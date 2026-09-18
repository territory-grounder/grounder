package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/territory-grounder/grounder/core/policy"
)

// PolicyDecisionWriteStore is the pgx-backed policy.AuditSink: it appends one immutable, NON-SECRET
// policy_decision row per Engine.Decide evaluation (spec/015 REQ-1518, INV-19, migration 0019). Parameters
// are always bound ($1) — no string-built SQL. Only non-secret projection fields are written: the matched
// rule id, the resolved verdict, the composition band_mode + composed risk band, the resolved min_confidence,
// a non-secret action_id binding, and the active mode. The runtime role holds no UPDATE/DELETE on the table
// (0019 REVOKE), so an appended decision is tamper-resistant like the rest of the accountability spine.
//
// NEVER A SECRET: the policy.PolicyDecision projection is secret-free BY CONSTRUCTION — it carries no argv,
// host, or credential (see core/policy engine.go DecisionAudit). No column here can receive one. The
// plan_hash and principal columns default to empty until the actuation interceptor threads a bound action +
// acting principal through (T-015-13); the action_id written here is the same NON-SECRET synthetic identifier
// the LedgerAuditSink uses, so the durable audit and the ledger audit agree.
type PolicyDecisionWriteStore struct{ p *Pool }

// NewPolicyDecisionWriteStore returns the Postgres-backed policy-decision audit writer.
func NewPolicyDecisionWriteStore(p *Pool) *PolicyDecisionWriteStore {
	return &PolicyDecisionWriteStore{p: p}
}

// AppendPolicyDecision inserts one policy_decision row. It writes ONLY the non-secret projection fields. A
// write error is returned; the AuditedEngine treats the append as best-effort (a projection error never
// changes the already-resolved security decision, but the failure is traced — never silently swallowed).
func (s *PolicyDecisionWriteStore) AppendPolicyDecision(ctx context.Context, d policy.PolicyDecision) error {
	a := d.Audit()
	// matched_rules is the FULL deny-overrides provenance list (REQ-1522, REQ-2004), projected NON-SECRET to
	// {id, verdict} per rule — never argv/host/credential (INV-13). Marshalled to jsonb so a persisted decision
	// joins back to EXACTLY which rules matched, not just the winning one.
	matched := d.MatchedRules()
	proj := make([]matchedRuleJSON, len(matched))
	for i, m := range matched {
		proj[i] = matchedRuleJSON{ID: m.ID, Verdict: string(m.Verdict)}
	}
	matchedJSON, err := json.Marshal(proj)
	if err != nil {
		return fmt.Errorf("db: policy_decision marshal matched_rules (rule %q): %w", d.MatchedRuleID(), err)
	}
	// Prefer the REAL content-hashed action_id threaded from the interceptor (spec/020 T-020-3); fall back to
	// the synthetic rule:verdict id for a non-interceptor caller (e.g. a console preview) so the row still names
	// its decision. principal + external_ref are the acting identity + the correlation key the decision tracer
	// joins the walk by (REQ-2005) — empty until the interceptor threads them, exactly as before.
	actionID := d.ActionID()
	if actionID == "" {
		actionID = policyActionID(d)
	}
	_, err = s.p.Pool.Exec(ctx, `
		INSERT INTO policy_decision
			(rule_id, verdict, band_mode, composed_band, min_confidence, action_id, plan_hash, principal, mode, bundle_version, matched_rules, reason, external_ref, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1)`,
		d.MatchedRuleID(), string(d.Verdict()), string(a.Compose.BandMode), d.ComposedBand().String(),
		a.Refine.MinConfidence, actionID, "", d.Principal(), d.Mode().String(),
		d.BundleVersion(), string(matchedJSON), d.Reason(), d.ExternalRef())
	if err != nil {
		return fmt.Errorf("db: policy_decision insert (rule %q verdict %q): %w", d.MatchedRuleID(), d.Verdict(), err)
	}
	return nil
}

// matchedRuleJSON is the NON-SECRET jsonb projection of one matched rule persisted on
// policy_decision.matched_rules (REQ-2004): the stable rule id + its declared verdict, never argv/host/credential.
type matchedRuleJSON struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
}

// policyActionID derives the NON-SECRET synthetic action identifier written on a decision row until the
// interceptor threads a real content-hashed action_id through (T-015-13) — the SAME projection the
// LedgerAuditSink appends to the governance ledger, so the durable audit and the ledger audit name the same
// decision. It carries no secret: it is only the matched rule id + the resolved verdict.
func policyActionID(d policy.PolicyDecision) string {
	rule := d.MatchedRuleID()
	if rule == "" {
		rule = "no-rule"
	}
	return "policy-decision:" + rule + ":" + string(d.Verdict())
}

// MemPolicyDecisionStore is the in-memory policy.AuditSink twin for the CI oracles (no Postgres). It records
// every appended decision so a test can assert exactly which non-secret fields were written — and that no
// secret ever leaks into a row. Concurrency-safe.
type MemPolicyDecisionStore struct {
	mu      sync.Mutex
	Records []policy.PolicyDecision
}

// NewMemPolicyDecisionStore returns an empty in-memory policy-decision audit twin.
func NewMemPolicyDecisionStore() *MemPolicyDecisionStore { return &MemPolicyDecisionStore{} }

// AppendPolicyDecision records the decision (append).
func (m *MemPolicyDecisionStore) AppendPolicyDecision(_ context.Context, d policy.PolicyDecision) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Records = append(m.Records, d)
	return nil
}

// compile-time proof both implementations satisfy the policy AuditSink interface.
var (
	_ policy.AuditSink = (*PolicyDecisionWriteStore)(nil)
	_ policy.AuditSink = (*MemPolicyDecisionStore)(nil)
)
