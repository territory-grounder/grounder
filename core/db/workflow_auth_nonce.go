package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/temporal/wfauth"
)

// WorkflowAuthNonceStore is the pgx-backed wfauth.NonceStore (migration 0134): the durable replay guard behind
// the signed authorization envelope on admin-write workflows and approval votes (TG-660). One statement, bound
// parameters, no read-then-write: INSERT ... ON CONFLICT DO NOTHING returns one row when THIS call claimed the
// nonce and no row when it was already claimed — atomic across every worker process, which is what makes a
// replay against a second worker instance fail exactly like one against the first.
type WorkflowAuthNonceStore struct{ p *Pool }

// NewWorkflowAuthNonceStore returns the Postgres-backed nonce claim store.
func NewWorkflowAuthNonceStore(p *Pool) *WorkflowAuthNonceStore { return &WorkflowAuthNonceStore{p: p} }

var _ wfauth.NonceStore = (*WorkflowAuthNonceStore)(nil)

// Claim records nonce as seen. fresh=true ⇒ first presentation; fresh=false ⇒ a replay. Any error is returned
// as-is so the verifier refuses (never admits) when the store cannot answer.
func (s *WorkflowAuthNonceStore) Claim(ctx context.Context, nonce, kind string, expiresAt time.Time) (bool, error) {
	tag, err := s.p.Exec(ctx, `
		INSERT INTO workflow_auth_nonce (nonce, kind, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (nonce) DO NOTHING`,
		nonce, kind, expiresAt.UTC())
	if err != nil {
		return false, fmt.Errorf("db: workflow_auth_nonce claim: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
