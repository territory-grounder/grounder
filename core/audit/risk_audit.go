package audit

import (
	"errors"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
)

// RiskAudit is the immutable per-classification record (session_risk_audit) — the single source of
// truth for WHY a session auto-proceeded. Every field is a required output of the classifier's
// decision function, so omitting one is a Go type error at the call site (INV-19). It is de-identified
// (normalized signals, not raw transcript) and part of the audit spine (never TTL-purged).
type RiskAudit struct {
	ExternalRef          string            // correlation key (ADR-0010)
	RiskLevel            string            // e.g. "low", "high", "novel-incident"
	Band                 safety.Band       // AUTO / AUTO_NOTICE / POLL_PAUSE
	AutoApproved         bool              // true only for {AUTO, AUTO_NOTICE}
	AutoProceedOnTimeout bool              // ALWAYS false in TG — a poll never proceeds on timeout
	NotifyRequired       bool              // AUTO_NOTICE sets this (out-of-band veto notice)
	OperatorOverride     bool              // an operator forced the band
	Signals              map[string]string // signals_json — the normalized signals that drove the band
	PlanHash             string            // joins to the prediction gate (spec/002)
	ActionID             string            // the content-hashed action this classification is bound to (INV-07)
	SchemaVersion        schema.Version    // stamped on write from the canonical registry (REQ-505)
}

// ErrIncompleteAudit fails closed when a risk-audit record omits a load-bearing field.
var ErrIncompleteAudit = errors.New("audit: session_risk_audit record missing a required field (external_ref, action_id, risk_level)")

// AppendRiskAudit writes exactly ONE session_risk_audit record per classification and appends its
// governance decision to the hash-chained ledger (REQ-503, INV-19). It stamps the schema version,
// forces auto_proceed_on_timeout=false (TG never proceeds on a timed-out poll), and rejects an
// incomplete record. It returns the stamped record and the ledger entry it produced.
func AppendRiskAudit(l *Ledger, a RiskAudit) (RiskAudit, LedgerEntry, error) {
	if a.ExternalRef == "" || a.ActionID == "" || a.RiskLevel == "" {
		return RiskAudit{}, LedgerEntry{}, ErrIncompleteAudit
	}
	v, err := schema.Stamp(schema.TableSessionRiskAudit)
	if err != nil {
		return RiskAudit{}, LedgerEntry{}, err
	}
	a.SchemaVersion = v
	a.AutoProceedOnTimeout = false // invariant: a poll never proceeds on timeout

	// Persist the full de-identified classification row through the durable sink (when attached) BEFORE the
	// ledger entry, so the detail is stored or the decision does not record — fail closed on a sink error.
	if l.riskSink != nil {
		if err := l.riskSink.PersistRiskAudit(a); err != nil {
			return RiskAudit{}, LedgerEntry{}, err
		}
	}

	// The band decides whether autonomy was withheld — a POLL_PAUSE is the channel saying "no".
	withheld := a.Band == safety.BandPollPause
	entry, err := l.Append(GovDecision{
		Decision: "classify:" + a.Band.String(),
		Reason:   a.RiskLevel,
		ActionID: a.ActionID,
		Withheld: withheld,
	})
	if err != nil {
		return RiskAudit{}, LedgerEntry{}, err
	}
	return a, entry, nil
}
