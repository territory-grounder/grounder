package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/territory-grounder/grounder/core/auth"
)

// The policy read surface (spec/015 T-015-12, spec/006 read-surface): the console's window onto the Policy
// Engine's REAL persisted state (migration 0019) — the append-only per-decision audit (policy_decision), the
// single active autonomy mode (policy_mode), the per-op-class earned-autonomy ladder (policy_graduation), and
// the operator's active rules-as-data policy (policy_ruleset). It answers "what did the engine decide?",
// "which mode is active and what may it do?", "which op-classes have earned autonomy?", and "what are the
// operator's rules?".
//
// NEVER A SECRET (INV-13): every field on every DTO here is non-secret BY CONSTRUCTION — rule ids, verdicts,
// bands, op-classes, levels, counts, and rules-as-data (op-classes, host globs, verdicts, params). No response
// can carry key material, a credential, a secret, or an argv/host — the pgx read store selects ONLY the
// non-secret columns the 0019 tables hold, and these structs have no field that could receive one. An unwired
// store fails closed to 503; an empty spine reports an honest empty result, never a fabricated row (INV-15).
//
// Read-only: this surface never edits a rule or changes a mode (the console ASA editor / mode selector /
// packet-tracer is a SEPARATE follow-on MR, T-015-12 console). Mutation stays OFF.

// PolicyDecision is one policy_decision audit row: the NON-SECRET record of one Engine.Decide evaluation.
// Every field is decision METADATA only — never argv, host, credential, or secret.
type PolicyDecision struct {
	RuleID        string  `json:"rule_id,omitempty"`   // matched rule id; empty = fail-closed default (no rule matched)
	Verdict       string  `json:"verdict"`             // auto / approve / deny
	BandMode      string  `json:"band_mode,omitempty"` // respect / force; empty on a deny short-circuit
	ComposedBand  string  `json:"composed_band"`       // POLL_PAUSE / AUTO_NOTICE / AUTO
	MinConfidence float64 `json:"min_confidence"`      // the resolved min_confidence threshold applied
	ActionID      string  `json:"action_id,omitempty"` // non-secret bound action id (INV-07)
	PlanHash      string  `json:"plan_hash,omitempty"` // non-secret plan hash (INV-07); empty until threaded (T-015-13)
	Principal     string  `json:"principal,omitempty"` // acting principal (non-secret id); empty until threaded
	Mode          string  `json:"mode"`                // the active mode when the decision was made
	CreatedAt     string  `json:"created_at"`          // RFC3339
}

// PolicyDecisionsPage is the read-only decision-history view, newest first.
type PolicyDecisionsPage struct {
	Decisions []PolicyDecision `json:"decisions"`
}

// PolicyMode is the single active autonomy mode plus its HONEST posture (REQ-1500/1519): whether the mode was
// actually persisted (false → the fail-closed Shadow default is reported, never a fabricated actuating mode),
// and the mode's actuation semantics derived from the mode itself.
type PolicyMode struct {
	Mode              string `json:"mode"`                 // canonical mode name (Shadow when absent — fail closed)
	Persisted         bool   `json:"persisted"`            // whether a mode row exists (false → honest Shadow default)
	MayAutoActuate    bool   `json:"may_auto_actuate"`     // true ONLY for Semi-auto / Full-auto
	RequiresHumanVote bool   `json:"requires_human_vote"`  // true ONLY for HITL
	Posture           string `json:"posture"`              // human-readable honest posture line
}

// PolicyGraduationClass is one policy_graduation ladder row: an op-class's earned autonomy level and its
// consecutive verified-clean run count (non-secret).
type PolicyGraduationClass struct {
	OpClass       string `json:"op_class"`
	Level         string `json:"level"`           // approve / auto
	CleanRunCount int    `json:"clean_run_count"` // consecutive verified-clean runs toward promotion
	LastOutcome   string `json:"last_outcome"`    // unverified / verified_clean / deviated
	UpdatedAt     string `json:"updated_at"`      // RFC3339
}

// PolicyGraduationPage is the read-only per-op-class ladder view, ordered by op-class.
type PolicyGraduationPage struct {
	Classes []PolicyGraduationClass `json:"classes"`
}

// PolicyRuleMatch is the NON-SECRET projection of a rule's selector (rules-as-data). The estate object-model
// dimension is rendered as a single "kind:pattern" selector string (host / host_glob / group / device_class /
// resource); the policy-specific dimensions are plain fields.
type PolicyRuleMatch struct {
	Selector    string `json:"selector,omitempty"`     // e.g. "host:librespeed01" / "host_glob:nl*" / "group:edge"
	OpClass     string `json:"op_class,omitempty"`
	ArgvPattern string `json:"argv_pattern,omitempty"`
	Territory   string `json:"territory,omitempty"`
	Reversible  *bool  `json:"reversible,omitempty"`
}

// PolicyRule is one operator rule as DATA (REQ-1503) — non-secret: an id, a verdict, a match, tunable params,
// and the approve_by principal labels. It carries no secret.
type PolicyRule struct {
	ID            string          `json:"id"`
	Verdict       string          `json:"verdict"` // auto / approve / deny
	Match         PolicyRuleMatch `json:"match"`
	MinConfidence *float64        `json:"min_confidence,omitempty"`
	BandMode      string          `json:"band_mode,omitempty"`
	RateLimit     *int            `json:"rate_limit,omitempty"`
	ApproveBy     []string        `json:"approve_by,omitempty"`
	IsDefault     bool            `json:"is_default,omitempty"`
}

// PolicyRulesPage is the active rules-as-data policy plus its non-secret metadata. Present is false when no
// ruleset has been persisted (an honest empty policy — every action resolves to the fail-closed default).
type PolicyRulesPage struct {
	Present   bool         `json:"present"`
	RuleCount int          `json:"rule_count"`
	UpdatedBy string       `json:"updated_by,omitempty"`
	UpdatedAt string       `json:"updated_at,omitempty"`
	Rules     []PolicyRule `json:"rules"`
}

// PolicyReader serves the four policy read views for the authenticated principal. All reads are over the real
// persisted projections; an empty spine returns an honest empty result. The pgx-backed impl selects ONLY
// non-secret columns; the in-memory MemPolicyReader drives the CI oracles (CI has no Postgres).
type PolicyReader interface {
	PolicyDecisions(ctx context.Context, p auth.Principal, limit int) ([]PolicyDecision, error)
	PolicyMode(ctx context.Context, p auth.Principal) (PolicyMode, error)
	PolicyGraduation(ctx context.Context, p auth.Principal) ([]PolicyGraduationClass, error)
	PolicyRules(ctx context.Context, p auth.Principal) (PolicyRulesPage, error)
}

// policyPageLimit bounds a single decisions read; the console pages the recent tail.
const policyPageLimit = 200

// policyPageDefault is the default decisions page when no ?limit= is supplied.
const policyPageDefault = 50

// policyDecisionsHandler serves GET /v1/policy/decisions?limit=N. The limit is capped at policyPageLimit
// before the read. Nil reader = 503 fail-closed.
func (d Deps) policyDecisionsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Policy == nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := policyPageDefault
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > policyPageLimit {
		limit = policyPageLimit
	}
	rows, err := d.Policy.PolicyDecisions(r.Context(), p, limit)
	if err != nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []PolicyDecision{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PolicyDecisionsPage{Decisions: rows})
}

// policyModeHandler serves GET /v1/policy/mode — the active mode + honest posture. Nil reader = 503.
func (d Deps) policyModeHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Policy == nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	m, err := d.Policy.PolicyMode(r.Context(), p)
	if err != nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// policyGraduationHandler serves GET /v1/policy/graduation — the per-op-class ladder. Nil reader = 503.
func (d Deps) policyGraduationHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Policy == nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	rows, err := d.Policy.PolicyGraduation(r.Context(), p)
	if err != nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []PolicyGraduationClass{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PolicyGraduationPage{Classes: rows})
}

// policyRulesHandler serves GET /v1/policy/rules — the active rules-as-data policy. Nil reader = 503.
func (d Deps) policyRulesHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Policy == nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	page, err := d.Policy.PolicyRules(r.Context(), p)
	if err != nil {
		http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
		return
	}
	if page.Rules == nil {
		page.Rules = []PolicyRule{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

// MemPolicyReader is the in-memory PolicyReader twin for the CI oracles (no Postgres). It holds canned,
// NON-SECRET DTOs and applies the (already-capped) decisions limit exactly as the pgx store does, recording
// the limit it last received so a test can assert the handler capped/forwarded it. It is secret-free by
// construction — its only inputs are the non-secret DTOs. Concurrency-safe.
type MemPolicyReader struct {
	mu         sync.Mutex
	Decisions  []PolicyDecision // newest-first
	Mode       PolicyMode
	Classes    []PolicyGraduationClass
	Rules      PolicyRulesPage
	Err        error // when set, every method returns it (drives the 503 oracle)
	LastLimit  int   // spy for the decisions cap oracle
}

// PolicyDecisions returns the canned decisions, newest-first, capped at limit; records the limit for the oracle.
func (m *MemPolicyReader) PolicyDecisions(_ context.Context, _ auth.Principal, limit int) ([]PolicyDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastLimit = limit
	if m.Err != nil {
		return nil, m.Err
	}
	out := make([]PolicyDecision, 0, len(m.Decisions))
	for _, d := range m.Decisions {
		out = append(out, d)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// PolicyMode returns the canned active-mode posture.
func (m *MemPolicyReader) PolicyMode(_ context.Context, _ auth.Principal) (PolicyMode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return PolicyMode{}, m.Err
	}
	return m.Mode, nil
}

// PolicyGraduation returns the canned per-op-class ladder rows.
func (m *MemPolicyReader) PolicyGraduation(_ context.Context, _ auth.Principal) ([]PolicyGraduationClass, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	return append([]PolicyGraduationClass(nil), m.Classes...), nil
}

// PolicyRules returns the canned active rules-as-data page.
func (m *MemPolicyReader) PolicyRules(_ context.Context, _ auth.Principal) (PolicyRulesPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return PolicyRulesPage{}, m.Err
	}
	return m.Rules, nil
}

// compile-time proof the in-memory twin satisfies the reader interface.
var _ PolicyReader = (*MemPolicyReader)(nil)
