package coord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrBadMCPToken is returned for an MCP token that matches no session:
// unknown, replaced by a newer one, or revoked when its session ended.
// It never contains the token.
var ErrBadMCPToken = errors.New("unknown or revoked AgentShiftboard MCP token")

// hashMCPToken is the only form of a token the coordinator stores.
func hashMCPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueMCPToken creates a fresh random token for a session's connection to
// the project's shared MCP server (D-28) and returns it. Only its SHA-256
// hash is stored; it replaces any earlier token of the session, so a
// resumed session's previous token stops working. The caller passes the
// plaintext only to the session's MCP configuration, never to task state,
// logs, or messages.
func (c *Coordinator) IssueMCPToken(ctx context.Context, sessionID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	res, err := c.DB.ExecContext(ctx, `UPDATE sessions SET mcp_token_hash=? WHERE id=?`, hashMCPToken(token), sessionID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("unknown session %s", sessionID)
	}
	return token, nil
}

// SessionForToken returns the session an MCP token belongs to. The lookup
// is by hash, so comparing it leaks nothing about the token. A token is
// revoked in the same transaction that records its session as stopped or
// failed (and by CloseSession through that observation), so a token issued
// before the session ended is refused here. A session that is being
// resumed gets a new token while its record still says stopped, until its
// CLI reports again; that token is accepted, so the CLI can connect at
// startup. Tool calls still refuse a session recorded as ended
// (CheckMCPCaller).
func (c *Coordinator) SessionForToken(ctx context.Context, token string) (Actor, error) {
	if token == "" {
		return User, ErrBadMCPToken
	}
	var id string
	err := c.DB.QueryRowContext(ctx, `SELECT id FROM sessions WHERE mcp_token_hash=? AND mcp_token_hash!=''`, hashMCPToken(token)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return User, ErrBadMCPToken
	}
	if err != nil {
		return User, err
	}
	return Actor{SessionID: id}, nil
}

// RevokeMCPToken clears a session's MCP token, for a launcher whose CLI
// exited without the session being recorded as ended again.
func (c *Coordinator) RevokeMCPToken(ctx context.Context, sessionID string) error {
	_, err := c.DB.ExecContext(ctx, `UPDATE sessions SET mcp_token_hash='' WHERE id=?`, sessionID)
	return err
}

// SharedMCPInUse reports whether any session holds a token for the shared
// MCP server: a live session configured for it, or one being launched or
// resumed. Tokens are revoked when a session ends, so ended sessions never
// count.
func (c *Coordinator) SharedMCPInUse(ctx context.Context) (bool, error) {
	var n int
	err := c.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE mcp_token_hash!=''`).Scan(&n)
	return n > 0, err
}
