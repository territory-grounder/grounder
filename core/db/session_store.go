package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/auth"
)

// SessionStore is the pgx-backed, DURABLE implementation of auth.SessionStore (REQ-508): browser
// operator sessions persist across grounder restarts/redeploys, so a valid cookie keeps working instead
// of forcing a re-login on every deploy (the in-memory store's limitation). Logout stays authoritative
// (Revoke deletes the row); Get returns found=false for unknown OR revoked ids — observationally
// identical. Parameters are always bound ($1) — no string-built SQL.
type SessionStore struct{ p *Pool }

// NewSessionStore returns the Postgres-backed operator-session store.
func NewSessionStore(p *Pool) *SessionStore { return &SessionStore{p: p} }

// Put registers (or refreshes) a session id → operator/expiry mapping.
func (s *SessionStore) Put(ctx context.Context, id, operator string, expires time.Time) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO operator_sessions (session_id, operator, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (session_id) DO UPDATE SET operator = EXCLUDED.operator, expires_at = EXCLUDED.expires_at`,
		id, operator, expires)
	if err != nil {
		return fmt.Errorf("db: session put: %w", err)
	}
	return nil
}

// Get resolves a session id; unknown and revoked are both found=false.
func (s *SessionStore) Get(ctx context.Context, id string) (string, time.Time, bool, error) {
	var operator string
	var expires time.Time
	err := s.p.Pool.QueryRow(ctx,
		`SELECT operator, expires_at FROM operator_sessions WHERE session_id = $1`, id).
		Scan(&operator, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("db: session get: %w", err)
	}
	return operator, expires, true, nil
}

// Revoke deletes a session id (logout); revoking an unknown id is a no-op.
func (s *SessionStore) Revoke(ctx context.Context, id string) error {
	_, err := s.p.Pool.Exec(ctx, `DELETE FROM operator_sessions WHERE session_id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: session revoke: %w", err)
	}
	return nil
}

// compile-time proof the durable store satisfies the auth seam its in-memory oracle also satisfies.
var _ auth.SessionStore = (*SessionStore)(nil)
