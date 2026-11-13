package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dlir2404/agent-shiftboard/internal/coord"
)

// HTTPHandler serves MCP over Streamable HTTP for every session of a
// project (D-28). Every request must carry `Authorization: Bearer <token>`
// with a token the coordinator issued to a session; anything else gets 401.
// A request acts as the token's session, never as the user. The MCP session
// a client opens is bound to the session that opened it: a request with
// another session's token is refused (the SDK compares TokenInfo.UserID).
// Tokens are never logged: the SDK's logger is off and refusals do not
// echo the token.
func HTTPHandler(version string, open Opener, ops *Ops) http.Handler {
	var mu sync.Mutex
	servers := map[string]*mcp.Server{}
	getServer := func(r *http.Request) *mcp.Server {
		ti := auth.TokenInfoFromContext(r.Context())
		if ti == nil || ti.UserID == "" {
			return nil // unreachable behind RequireBearerToken; the SDK answers 400
		}
		mu.Lock()
		defer mu.Unlock()
		s, ok := servers[ti.UserID]
		if !ok {
			actor := coord.Actor{SessionID: ti.UserID}
			s = newServer(version, open, ops, &actor)
			servers[ti.UserID] = s
		}
		return s
	}
	// No idle timeout: an agent may sit idle for days and still expects its
	// MCP session to be there.
	h := mcp.NewStreamableHTTPHandler(getServer, nil)
	verify := func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		c, err := open(ctx)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		a, err := c.SessionForToken(ctx, token)
		if errors.Is(err, coord.ErrBadMCPToken) {
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		if err != nil {
			return nil, err
		}
		// The token is checked again on every request, so this expiry
		// only satisfies the SDK.
		return &auth.TokenInfo{UserID: a.SessionID, Expiration: time.Now().Add(time.Hour)}, nil
	}
	return auth.RequireBearerToken(verify, nil)(h)
}
