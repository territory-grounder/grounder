package db

import (
	"context"
	"fmt"

	"github.com/territory-grounder/grounder/core/regime"
)

// RegimeAuditWriteStore is the pgx-backed regime.AuditSink: it appends one immutable row per regime
// resolution, job launch, and completed deferred verify (spec/017 REQ-1715, migration 0020). Parameters are
// always bound ($1) — no string-built SQL. Only NON-SECRET fields are written; the regime.*Row types are
// secret-free by construction (INV-13) — the only credential material an ActuationRow carries is a
// config.SecretRef REFERENCE (token_ref), never the value, and its writer rejects a non-reference token. The
// runtime role holds no UPDATE/DELETE on the three tables (0020 REVOKE), so an appended row is tamper-
// resistant like the rest of the accountability spine.
type RegimeAuditWriteStore struct{ p *Pool }

// NewRegimeAuditWriteStore returns the Postgres-backed regime-audit writer.
func NewRegimeAuditWriteStore(p *Pool) *RegimeAuditWriteStore { return &RegimeAuditWriteStore{p: p} }

// AppendResolution inserts one regime_resolution row (one per lane selection, REQ-1715).
func (s *RegimeAuditWriteStore) AppendResolution(ctx context.Context, r regime.ResolutionRow) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO regime_resolution
			(target, regime, lane, rule_id, outcome, schema_version)
		VALUES ($1, $2, $3, $4, $5, 1)`,
		r.Target, string(r.Regime), string(r.Lane), r.RuleID, string(r.Outcome))
	if err != nil {
		return fmt.Errorf("db: regime_resolution insert (target %q): %w", r.Target, err)
	}
	return nil
}

// AppendActuation inserts one regime_actuation row (one per job launch, REQ-1715). token_ref carries only the
// SecretRef REFERENCE — never the token value (INV-13).
func (s *RegimeAuditWriteStore) AppendActuation(ctx context.Context, r regime.ActuationRow) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO regime_actuation
			(action_id, lane, job_template_id, op_class, job_id, token_ref, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		r.ActionID, string(r.Lane), r.JobTemplateID, r.OpClass, r.JobID, string(r.TokenRef))
	if err != nil {
		return fmt.Errorf("db: regime_actuation insert (action %q): %w", r.ActionID, err)
	}
	return nil
}

// AppendDeferredVerdict inserts one deferred_verdict row (one per completed deferred verify, REQ-1715).
func (s *RegimeAuditWriteStore) AppendDeferredVerdict(ctx context.Context, r regime.DeferredVerdictRow) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO deferred_verdict
			(action_id, job_id, status, verdict, graduation, schema_version)
		VALUES ($1, $2, $3, $4, $5, 1)`,
		r.ActionID, r.JobID, string(r.Status), string(r.Verdict), string(r.Graduation))
	if err != nil {
		return fmt.Errorf("db: deferred_verdict insert (action %q): %w", r.ActionID, err)
	}
	return nil
}

// compile-time proof the pgx store satisfies the regime.AuditSink port.
var _ regime.AuditSink = (*RegimeAuditWriteStore)(nil)
