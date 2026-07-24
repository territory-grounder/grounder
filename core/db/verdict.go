package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
)

// VerdictStore is the pgx-backed, APPEND-ONLY writer for action_verdict (migration 0004) — the durable pair
// of the prediction store, completing the predict → verdict spine. One verdict per bound action (PK
// action_id, first-wins); written ONLY by the deterministic verifier (INV-10). An invalid verdict is
// rejected at the boundary — the enum column would reject it too, but failing here is a clearer error.
type VerdictStore struct{ p *Pool }

// NewVerdictStore returns a Postgres-backed action-verdict writer.
func NewVerdictStore(p *Pool) *VerdictStore { return &VerdictStore{p: p} }

// ErrInvalidVerdict is returned when a verdict outside {match, partial, deviation} is committed.
var ErrInvalidVerdict = errors.New("db: verdict is not one of match/partial/deviation")

// Commit appends the mechanical verdict for an action. A duplicate action_id is ignored (append-only,
// first-wins) — a verdict is never overwritten.
func (s *VerdictStore) Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error {
	if !safety.ValidVerdict(v) {
		return ErrInvalidVerdict
	}
	ver, err := schema.Stamp(schema.TableActionVerdict)
	if err != nil {
		return err
	}
	_, err = s.p.Exec(ctx, `
		INSERT INTO action_verdict (action_id, plan_hash, verdict, target_host, site, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (action_id) DO NOTHING`,
		actionID, planHash, string(v), targetHost, site, int(ver))
	if err != nil {
		return fmt.Errorf("db: commit verdict %s: %w", actionID, err)
	}
	return nil
}

// Get returns the committed verdict for an action, ok=false when none exists.
func (s *VerdictStore) Get(ctx context.Context, actionID string) (safety.Verdict, bool, error) {
	var v string
	err := s.p.QueryRow(ctx, "SELECT verdict FROM action_verdict WHERE action_id = $1", actionID).Scan(&v)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("db: get verdict %s: %w", actionID, err)
	}
	return safety.Verdict(v), true, nil
}
