package db

import (
	"context"
	"time"
)

// PgNonceStore implements core/auth.NonceStore against Postgres. A replayed (source,nonce) is
// detected by a UNIQUE violation on insert. No pruning schedule exists (the P1-9 claim that stood here was stale — verified 2026-08-25, no nonce-pruning workflow anywhere in the tree); expiry is enforced at CHECK time and growth is bounded by use. If growth ever matters, build the reaper and update core/db/retention_coverage_test.go's declaration with it.
// All queries are parameterized (INV-03).
type PgNonceStore struct{ p *Pool }

// NewNonceStore returns a Postgres-backed nonce store.
func NewNonceStore(p *Pool) *PgNonceStore { return &PgNonceStore{p: p} }

// SeenBefore records (sourceID,nonce) and reports whether it had already been seen. First write wins;
// a second write of the same pair reports true (replay). ts is stored for windowed pruning.
func (s *PgNonceStore) SeenBefore(ctx context.Context, sourceID, nonce string, ts time.Time) (bool, error) {
	ct, err := s.p.Exec(ctx,
		`INSERT INTO auth_nonce (source_id, nonce, seen_at) VALUES ($1,$2,$3)
		 ON CONFLICT (source_id, nonce) DO NOTHING`,
		sourceID, nonce, ts)
	if err != nil {
		return false, err
	}
	// 0 rows affected => the pair already existed => replay.
	return ct.RowsAffected() == 0, nil
}
