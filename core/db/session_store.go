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

// SetAdminEligible records the LDAP-admin grant on the session row (auth.RoleStore).
//
// The session row is written first by Put at login, and markAdminEligible runs immediately after, so the
// UPDATE finds its row. A missing row is not an error: the only way to reach it is a session that was
// revoked between the two, and re-creating a row for a revoked session would resurrect it.
func (s *SessionStore) SetAdminEligible(ctx context.Context, id string, eligible bool) error {
	_, err := s.p.Pool.Exec(ctx,
		`UPDATE operator_sessions SET admin_eligible = $2 WHERE session_id = $1`, id, eligible)
	if err != nil {
		return fmt.Errorf("db: session set admin_eligible: %w", err)
	}
	return nil
}

// AdminEligible reads the stored grant. Unknown/revoked ids are (false, nil) — fail closed, and
// observationally identical to a session that never held the role (the same discipline as Get).
func (s *SessionStore) AdminEligible(ctx context.Context, id string) (bool, error) {
	var eligible bool
	err := s.p.Pool.QueryRow(ctx,
		`SELECT admin_eligible FROM operator_sessions WHERE session_id = $1`, id).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: session admin_eligible: %w", err)
	}
	return eligible, nil
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

// The durable store implements the optional role capability; the compile-time assertion is what makes the
// wiring real rather than hopeful — auth.SessionAuthenticator only consults it via a type assertion, so a
// signature drift here would silently fall back to the process-local map and reintroduce the defect.
var _ auth.RoleStore = (*SessionStore)(nil)
