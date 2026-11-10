package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// RiskAuditStore is the pgx-backed, append-only writer for session_risk_audit (migration 0003). It
// implements audit.RiskAuditSink, so the durable Ledger persists the full de-identified classification row
// alongside the ledger's decision summary. The DB CHECK pins auto_proceed_on_timeout=false as a structural
// invariant, independent of this writer.
type RiskAuditStore struct{ p *Pool }

// NewRiskAuditStore returns a Postgres-backed session_risk_audit writer.
func NewRiskAuditStore(p *Pool) *RiskAuditStore { return &RiskAuditStore{p: p} }

var _ audit.RiskAuditSink = (*RiskAuditStore)(nil)

// LatestFor reads the classifier's DURABLE record for (externalRef, actionID): the band it decided and the plan
// hash it committed (TG-660 part c). ExecuteActivity binds the band/plan it executes under to this row rather
// than to the workflow input. exact=true when a row names this very action. A transaction plan's steps carry their
// own action ids but one classification per session, so when no row names the action the latest row for the
// session answers with exact=false (the caller admits that only for a step it has bound to the plan) — a session
// with NO classification at all answers ok=false, and the caller refuses. An unparseable stored band fails closed
// to POLL_PAUSE.
func (s *RiskAuditStore) LatestFor(ctx context.Context, externalRef, actionID string) (safety.Band, string, bool, bool, error) {
	var band, planHash string
	exact := true
	err := s.p.QueryRow(ctx, `
		SELECT band::text, COALESCE(plan_hash, '') FROM session_risk_audit
		WHERE external_ref = $1 AND action_id = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, externalRef, actionID).Scan(&band, &planHash)
	if errors.Is(err, pgx.ErrNoRows) {
		exact = false
		err = s.p.QueryRow(ctx, `
			SELECT band::text, COALESCE(plan_hash, '') FROM session_risk_audit
			WHERE external_ref = $1
			ORDER BY created_at DESC, id DESC LIMIT 1`, externalRef).Scan(&band, &planHash)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return safety.BandPollPause, "", false, false, nil
	}
	if err != nil {
		return safety.BandPollPause, "", false, false, fmt.Errorf("db: risk audit for %s: %w", externalRef, err)
	}
	return bandFromText(band), planHash, exact, true, nil
}

// bandFromText inverts safety.Band.String(): the two actuating spellings map back, anything else — including a
// spelling this binary does not know — is POLL_PAUSE (fail closed).
func bandFromText(s string) safety.Band {
	switch s {
	case safety.BandAuto.String():
		return safety.BandAuto
	case safety.BandAutoNotice.String():
		return safety.BandAutoNotice
	}
	return safety.BandPollPause
}

// PersistRiskAudit inserts one classification row. signals marshals to jsonb (sorted keys via json.Marshal on
// a map is not order-stable, but jsonb normalizes on store — the row content is what matters here).
func (s *RiskAuditStore) PersistRiskAudit(a audit.RiskAudit) error {
	signals := a.Signals
	if signals == nil {
		signals = map[string]string{}
	}
	sj, err := json.Marshal(signals)
	if err != nil {
		return fmt.Errorf("db: marshal signals: %w", err)
	}
	_, err = s.p.Exec(context.Background(), `
		INSERT INTO session_risk_audit
		  (external_ref, risk_level, band, auto_approved, auto_proceed_on_timeout, notify_required,
		   operator_override, signals_json, plan_hash, action_id, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)`,
		a.ExternalRef, a.RiskLevel, a.Band.String(), a.AutoApproved, a.AutoProceedOnTimeout, a.NotifyRequired,
		a.OperatorOverride, string(sj), a.PlanHash, a.ActionID, int(a.SchemaVersion))
	if err != nil {
		return fmt.Errorf("db: persist risk audit %s: %w", a.ExternalRef, err)
	}
	return nil
}
