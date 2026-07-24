package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
	"github.com/territory-grounder/grounder/core/schema"
)

// EscalationStore is the pgx-backed durable dropped-escalation requeue lane over escalation_queue
// (migration 0004). It is the durable sibling of persist.EscalationQueue: an enqueued requeue survives a
// restart, so a dropped escalation is never lost to a crash. A requeue RE-ENTERS the gated pipeline (via the
// caller's Signaler) — this store only records the attempt, status, and next-eligibility.
type EscalationStore struct{ p *Pool }

// NewEscalationStore returns a Postgres-backed escalation-queue store.
func NewEscalationStore(p *Pool) *EscalationStore { return &EscalationStore{p: p} }

// Enqueue records a dropped escalation as pending, due at eligibleAt. The seq is DB-assigned (IDENTITY). An
// empty external_ref is rejected (fail closed — a requeue with no correlation key can never re-enter the
// gated pipeline).
func (s *EscalationStore) Enqueue(ctx context.Context, ref string, attempts int, eligibleAt time.Time) (persist.EscalationItem, error) {
	if ref == "" {
		return persist.EscalationItem{}, persist.ErrEmptyRef
	}
	v, err := schema.Stamp(schema.TableEscalationQueue)
	if err != nil {
		return persist.EscalationItem{}, err
	}
	var seq int64
	err = s.p.QueryRow(ctx, `
		INSERT INTO escalation_queue (external_ref, attempts, status, eligible_at, schema_version)
		VALUES ($1, $2, 'pending', $3, $4) RETURNING seq`,
		ref, attempts, eligibleAt, int(v)).Scan(&seq)
	if err != nil {
		return persist.EscalationItem{}, fmt.Errorf("db: enqueue escalation %s: %w", ref, err)
	}
	return persist.EscalationItem{Seq: seq, ExternalRef: ref, Attempts: attempts, Status: persist.EscalPending, EligibleAt: eligibleAt, SchemaVersion: v}, nil
}

// DuePending returns the pending escalations whose eligible_at has arrived, oldest first — the batch the
// requeue driver should re-enter into the gated pipeline.
func (s *EscalationStore) DuePending(ctx context.Context, now time.Time) ([]persist.EscalationItem, error) {
	rows, err := s.p.Query(ctx, `
		SELECT seq, external_ref, attempts, eligible_at, schema_version
		FROM escalation_queue WHERE status = 'pending' AND eligible_at <= $1 ORDER BY seq`, now)
	if err != nil {
		return nil, fmt.Errorf("db: due-pending escalations: %w", err)
	}
	defer rows.Close()
	var out []persist.EscalationItem
	for rows.Next() {
		var it persist.EscalationItem
		var sv int
		if err := rows.Scan(&it.Seq, &it.ExternalRef, &it.Attempts, &it.EligibleAt, &sv); err != nil {
			return nil, err
		}
		it.Status = persist.EscalPending
		it.SchemaVersion = schema.Version(sv)
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkFired transitions a pending escalation to 'fired' after it re-entered the gated pipeline. It is a
// bounded status transition (pending → fired), the only mutation this lane makes.
func (s *EscalationStore) MarkFired(ctx context.Context, seq int64) error {
	_, err := s.p.Exec(ctx, "UPDATE escalation_queue SET status = 'fired' WHERE seq = $1 AND status = 'pending'", seq)
	if err != nil {
		return fmt.Errorf("db: mark escalation %d fired: %w", seq, err)
	}
	return nil
}
