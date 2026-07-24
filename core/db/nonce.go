package db

import (
	"context"
	"time"
)

// PgNonceStore implements core/auth.NonceStore against Postgres. A replayed (source,nonce) is
// detected by a UNIQUE violation on insert. Expired nonces are pruned by a Temporal schedule (P1-9).
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
