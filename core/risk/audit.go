package risk

import (
	"github.com/territory-grounder/grounder/core/audit"
)

// ClassifyAndAudit runs the classifier and appends exactly one session_risk_audit row to the
// hash-chained governance ledger (composing spec/001's decision with spec/006's audit contract,
// REQ-503/INV-19). The audit row records the band, the signals that produced it, and the bound
// action_id, so an [AUTO-RESOLVE] is provably bound to the exact action it was classified against
// (INV-07). It returns the Decision, the stamped audit record, and the ledger entry.
func ClassifyAndAudit(l *audit.Ledger, in GatedInput) (Decision, audit.RiskAudit, audit.LedgerEntry, error) {
	d := Classify(in)
	ra := audit.RiskAudit{
		ExternalRef:      in.ExternalRef,
		RiskLevel:        d.RiskLevel,
		Band:             d.Band,
		AutoApproved:     d.AutoApproved,
		NotifyRequired:   d.NotifyRequired,
		OperatorOverride: false,
		Signals:          d.Signals,
		PlanHash:         d.PlanHash,
		ActionID:         d.ActionID,
	}
	stamped, entry, err := audit.AppendRiskAudit(l, ra)
	if err != nil {
		return d, audit.RiskAudit{}, audit.LedgerEntry{}, err
	}
	return d, stamped, entry, nil
}
