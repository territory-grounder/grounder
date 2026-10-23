package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/territory-grounder/grounder/core/auth"
)

// The policy packet-tracer surface (spec/015 TG-105): POST a HYPOTHETICAL candidate action, get back the
// composed verdict + the matched rule + the effective band + why, evaluated by the worker's REAL Policy
// Engine over the existing Temporal channel. It answers "may TG act on host X with op-class Y, and by which
// rule?" without touching anything.
//
// READ-ONLY (AuthReadOnly): it evaluates and returns, actuating nothing. The worker's trace decider is the
// BARE composed engine — NOT the audited one — so a hypothetical query writes NO policy_decision audit row,
// and it carries NO rate governor, so a trace neither consumes nor reflects live rate budget. That last fact
// is surfaced honestly on every response (rate_governor_simulated=false, and a note in the reason), because a
// composed `auto` here could still be rate-clamped to `approve` at real actuation time.
//
// NEVER A SECRET (INV-13): the request describes an action in the SAME non-secret grammar the rules match on
// (op-class, host, argv pattern, groups, device-class, territory); the response is decision metadata only
// (verdicts, a rule id, a band, a bundle fingerprint). No field can carry key material or a credential.

// PolicyTraceRequest is the hypothetical candidate action to evaluate. Every field is optional except that at
// least one of op_class or host must be present (the "op-class Y on host X" the tracer answers about); an
// empty band/mode fails closed (POLL_PAUSE / Shadow). The correlation keys (action_id/external_ref/principal)
// are intentionally absent — they compose no verdict and their only consumer is the audit row a trace never
// writes.
type PolicyTraceRequest struct {
	OpClass     string   `json:"op_class,omitempty"`
	Argv        string   `json:"argv,omitempty"`
	Host        string   `json:"host,omitempty"`
	Resource    string   `json:"resource,omitempty"`
	Groups      []string `json:"groups,omitempty"`
	DeviceClass string   `json:"device_class,omitempty"`
	Territory   string   `json:"territory,omitempty"`
	Reversible  bool     `json:"reversible,omitempty"`
	Confidence  float64  `json:"confidence,omitempty"`
	Band        string   `json:"band,omitempty"` // AUTO | AUTO_NOTICE | POLL_PAUSE
	Mode        string   `json:"mode,omitempty"` // Shadow | HITL | Semi-auto | Full-auto
}

// PolicyTraceMatchedRule is one rule that matched the traced action — its stable id + declared verdict.
type PolicyTraceMatchedRule struct {
	RuleID  string `json:"rule_id"`
	Verdict string `json:"verdict"`
}

// PolicyTraceResult is the packet-tracer answer: the composed verdict and the provenance behind it.
type PolicyTraceResult struct {
	Verdict        string                   `json:"verdict"`                  // composed auto/approve/deny
	MatchedRuleID  string                   `json:"matched_rule_id,omitempty"` // "" = fail-closed default (nothing matched)
	ComposedBand   string                   `json:"composed_band"`            // POLL_PAUSE / AUTO_NOTICE / AUTO
	ApproveBy      []string                 `json:"approve_by,omitempty"`     // principals for a resolved `approve`
	Mode           string                   `json:"mode"`                     // the mode carried into the decision
	Reason         string                   `json:"reason"`                   // packet-tracer explanation (+ rate-governor note)
	NeverAutoFloor bool                     `json:"never_auto_floor"`         // the constitutional never-auto floor applied
	BundleVersion  string                   `json:"bundle_version,omitempty"` // content-derived rule-bundle identity
	MatchedRules   []PolicyTraceMatchedRule `json:"matched_rules,omitempty"`  // full matched set (deny-overrides provenance)
	// RateGovernorSimulated is always false — the trace decider carries no rate governor. Served explicitly so
	// a console never has to infer it, and repeated in the reason so it travels with the text too.
	RateGovernorSimulated bool `json:"rate_governor_simulated"`
}

// PolicyTracer evaluates a hypothetical action against the worker's REAL Policy Engine and returns the
// composed decision. nil = the surface fails closed to 503 (no worker/Temporal wired). It authorizes nothing
// and writes nothing — the "may I?" question, never the act.
type PolicyTracer interface {
	Trace(ctx context.Context, req PolicyTraceRequest) (PolicyTraceResult, error)
}

// policyTraceHandler serves POST /v1/policy/trace (AuthReadOnly). Nil tracer = 503 fail-closed.
func (d Deps) policyTraceHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Tracer == nil {
		http.Error(w, "policy trace unavailable", http.StatusServiceUnavailable)
		return
	}
	var req PolicyTraceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "malformed policy trace request", http.StatusBadRequest)
		return
	}
	// A completely empty candidate is a mistake, not a question: require at least the op-class or the host the
	// packet-tracer is named for. Everything else is optional and fails closed at the engine.
	if strings.TrimSpace(req.OpClass) == "" && strings.TrimSpace(req.Host) == "" {
		http.Error(w, "describe the candidate action: at least op_class or host is required", http.StatusBadRequest)
		return
	}
	res, err := d.Tracer.Trace(r.Context(), req)
	if err != nil {
		// A trace evaluation that failed to reach the worker's engine is retryable, not a verdict — 503, never
		// a fabricated decision. (A refused action is a normal 200 carrying verdict "deny".)
		http.Error(w, "policy trace failed — retry", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// MemPolicyTracer is the in-memory PolicyTracer twin for the CI oracles (no Temporal/worker). It returns a
// canned result/error and records the last request so a test can assert the handler decoded + forwarded the
// candidate faithfully. Secret-free by construction — its only inputs are the non-secret DTOs.
type MemPolicyTracer struct {
	Result  PolicyTraceResult
	Err     error
	LastReq PolicyTraceRequest
	Calls   int
}

// Trace records the call and returns the canned result.
func (m *MemPolicyTracer) Trace(_ context.Context, req PolicyTraceRequest) (PolicyTraceResult, error) {
	m.Calls++
	m.LastReq = req
	if m.Err != nil {
		return PolicyTraceResult{}, m.Err
	}
	return m.Result, nil
}

// compile-time proof the in-memory twin satisfies the interface.
var _ PolicyTracer = (*MemPolicyTracer)(nil)
