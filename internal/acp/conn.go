// Package acp is a minimal client for the Agent Client Protocol
// (https://agentclientprotocol.com): newline-delimited JSON-RPC 2.0 over an
// agent process's stdio. AgentShiftboard uses it to drive agent CLIs whose
// documented integration is ACP (Hermes, D-29). It implements only what the
// pane client needs: initialize, session/new, session/resume,
// session/prompt, session/cancel, session/update notifications, and
// session/request_permission requests. It never stores what it carries.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ProtocolVersion is the ACP major version this client speaks.
const ProtocolVersion = 1

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// ErrClosed is returned for calls on a connection whose agent went away.
var ErrClosed = errors.New("the agent closed the connection")

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// Handler receives what the agent sends on its own initiative.
type Handler interface {
	// Notify handles a notification such as session/update. It runs on
	// the read loop, in order, and must not block.
	Notify(method string, params json.RawMessage)
	// Request handles a request such as session/request_permission. It
	// runs on its own goroutine and may wait (for the user).
	Request(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)
}

// Conn is one client connection to an agent.
type Conn struct {
	w       io.Writer
	wmu     sync.Mutex
	h       Handler
	mu      sync.Mutex
	next    int64
	pending map[int64]chan message
	done    chan struct{}
	err     error
}

// NewConn starts reading r. Writes go to w. Call Wait to learn when the
// agent's output ends.
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	c := &Conn{w: w, h: h, pending: map[int64]chan message{}, done: make(chan struct{})}
	go c.read(r)
	return c
}

// Done is closed when the agent's output ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue // not a protocol line
		}
		switch {
		case m.Method != "" && m.ID != nil:
			id := *m.ID
			go func() {
				result, rerr := c.h.Request(context.Background(), m.Method, m.Params)
				c.reply(id, result, rerr)
			}()
		case m.Method != "":
			c.h.Notify(m.Method, m.Params)
		case m.ID != nil:
			var id int64
			if json.Unmarshal(*m.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	c.mu.Lock()
	c.err = sc.Err()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) write(m message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

func (c *Conn) reply(id json.RawMessage, result any, rerr *RPCError) {
	m := message{ID: &id}
	if rerr != nil {
		m.Error = rerr
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			m.Error = &RPCError{Code: -32603, Message: "internal error"}
		} else {
			m.Result = b
		}
	}
	_ = c.write(m)
}

// Call sends a request and decodes its result into out (which may be nil).
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	raw := json.RawMessage(fmt.Sprint(id))
	if err := c.write(message{ID: &raw, Method: method, Params: b}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: b})
}
