package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/audit"
)

// RiskAuditStore is the pgx-backed, append-only writer for session_risk_audit (migration 0003). It
// implements audit.RiskAuditSink, so the durable Ledger persists the full de-identified classification row
// alongside the ledger's decision summary. The DB CHECK pins auto_proceed_on_timeout=false as a structural
// invariant, independent of this writer.
type RiskAuditStore struct{ p *Pool }

// NewRiskAuditStore returns a Postgres-backed session_risk_audit writer.
func NewRiskAuditStore(p *Pool) *RiskAuditStore { return &RiskAuditStore{p: p} }

var _ audit.RiskAuditSink = (*RiskAuditStore)(nil)

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
