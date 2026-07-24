package policy

// Audit-every-decision — spec/015 task T-015-11 (REQ-1518). Every Engine.Decide evaluation is a governance
// decision, and INV-19 requires that NO policy decision execute without a persisted audit record. The engine
// already builds the NON-SECRET audit projection inside the returned PolicyDecision (engine.go's
// PolicyDecision.Audit()); this file adds the EMISSION SEAM around Decide so that projection reaches a sink on
// EVERY call — auto, approve, and deny alike — without touching the Decide composition itself.
//
// The seam is deliberately a WRAPPER, not a change to Decide: AuditedEngine calls the unmodified Engine.Decide
// (or, when the admin toggle has disabled the engine, the engine-off deferral in warn.go) and then emits
// exactly ONE decision record. Emission is BEST-EFFORT for availability (a sink failure must not change the
// security decision, mirroring the credential resolver's audit append in core/credential/audit.go) but is
// NEVER silent: a missing sink or an append failure is traced, so a governance-audit gap always leaves a
// record in the logs.
//
// The DURABLE pgx sink + the append-only policy_decision migration are a later leaf (T-015-12); this leaf
// defines the AuditSink interface, an in-memory fake for the oracles, and a thin adapter onto the existing
// tamper-evident governance ledger (core/audit) so the "appended to the ledger" contract is provable now.
//
// Provenance: [O] INV-19 (every governance decision is a required, persisted output) · [R] paradigm-rule 4.
// See spec/015-policy-engine requirements.md REQ-1518 and design.md (`policy.Engine`).

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/audit"
)

// AuditSink durably appends ONE non-secret policy-decision record per evaluation (REQ-1518, INV-19). The pgx
// impl writes T-015-12's append-only policy_decision table; the in-memory MemAuditSink drives the CI oracles
// and the LedgerAuditSink adapts the existing tamper-evident governance ledger. The record it receives is the
// PolicyDecision, whose audit projection is secret-free by construction (no argv, host, or credential — see
// engine.go's DecisionAudit). Append is best-effort at the call site: a write failure must not change the
// already-resolved security decision, but the failure is traced (never silently swallowed).
type AuditSink interface {
	AppendPolicyDecision(ctx context.Context, d PolicyDecision) error
}

// MemAuditSink is the in-memory AuditSink fake for oracle tests (CI has no DB). It records every appended
// decision in order and can be primed with a fixed append error to exercise the best-effort / trace path. It
// is safe for concurrent use.
type MemAuditSink struct {
	mu        sync.Mutex
	records   []PolicyDecision
	appendErr error
}

// NewMemAuditSink returns an empty in-memory sink.
func NewMemAuditSink() *MemAuditSink { return &MemAuditSink{} }

// WithAppendError primes the sink to fail every AppendPolicyDecision with err (to test the "append fails →
// decision stands but the failure is traced" path). It still records the decision so tests can assert the
// record was OFFERED to the sink even though the durable write failed.
func (s *MemAuditSink) WithAppendError(err error) *MemAuditSink {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendErr = err
	return s
}

// AppendPolicyDecision records the decision and returns the primed error (nil by default).
func (s *MemAuditSink) AppendPolicyDecision(_ context.Context, d PolicyDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, d)
	return s.appendErr
}

// Records returns a COPY of the appended decisions in order (for the console + tests).
func (s *MemAuditSink) Records() []PolicyDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PolicyDecision(nil), s.records...)
}

// Len returns the number of decisions offered to the sink (including those whose durable write was primed to
// fail — the point is that no decision path is un-audited).
func (s *MemAuditSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// LedgerAuditSink adapts the existing tamper-evident, hash-chained governance ledger (core/audit.Ledger, via
// the same narrow ledgerAppender seam the mode controller uses) into an AuditSink. It projects the PolicyDecision
// to the ledger's four required GovDecision fields — the resolved verdict, the packet-tracer reason, a bound
// decision id, and the withheld flag — so a real policy decision is appended to the ONE governance chain
// (REQ-1518) with no forked audit trail. It writes NO secret: the projection is derived solely from the
// non-secret decision fields.
type LedgerAuditSink struct{ ledger ledgerAppender }

// NewLedgerAuditSink adapts a ledgerAppender (an *audit.Ledger satisfies it) into an AuditSink.
func NewLedgerAuditSink(l ledgerAppender) *LedgerAuditSink { return &LedgerAuditSink{ledger: l} }

// AppendPolicyDecision projects the decision to a GovDecision and appends it to the governance ledger.
func (s *LedgerAuditSink) AppendPolicyDecision(_ context.Context, d PolicyDecision) error {
	if s == nil || s.ledger == nil {
		return nil
	}
	_, err := s.ledger.Append(govDecisionFor(d))
	return err
}

// govDecisionFor is the NON-SECRET projection of a PolicyDecision onto the ledger's required-field GovDecision.
// ActionID prefers the REAL content-hashed action_id threaded from the interceptor (spec/020 T-020-3) so the
// governance-ledger audit and the pgx policy_decision row name the SAME decision; it falls back to the synthetic
// rule:verdict identifier for a non-interceptor caller (a console preview) — the ledger rejects an empty required
// field (fail closed), so ActionID stays non-empty either way. Withheld is true whenever autonomy was withheld
// (any verdict other than `auto` routes to a human or refuses — the "one channel allowed to say no"). The Reason
// carries the SERIALIZED decision detail — the loaded bundle version + the full matched-rule list (REQ-1522) —
// so the persisted governance-ledger row records exactly which rule set authorized/refused the action, with NO
// new column or migration.
func govDecisionFor(d PolicyDecision) audit.GovDecision {
	rule := d.MatchedRuleID()
	if rule == "" {
		rule = "no-rule"
	}
	// Same real-or-synthetic action_id selection the pgx policy_decision writer uses (core/db policyActionID),
	// so the durable audit and the ledger audit agree on the decision id.
	actionID := d.ActionID()
	if actionID == "" {
		actionID = "policy-decision:" + rule + ":" + string(d.Verdict())
	}
	return audit.GovDecision{
		Decision: "policy:" + string(d.Verdict()),
		Reason:   decisionDetail(d),
		ActionID: actionID,
		Withheld: d.Verdict() != VerdictAuto,
	}
}

// decisionDetail is the NON-SECRET serialized detail/rationale payload persisted with a decision (REQ-1522): the
// content-derived bundle version that authorized/refused the action, the packet-tracer reason, and the FULL
// matched-rule list (deny-overrides provenance). It carries no secret — the bundle version is a content hash and
// the matched rules are non-secret id + closed-enum-verdict labels.
func decisionDetail(d PolicyDecision) string {
	bundle := d.BundleVersion()
	if bundle == "" {
		bundle = "none"
	}
	return fmt.Sprintf("bundle=%s; %s; matched=%s", bundle, d.Reason(), matchedRulesString(d.MatchedRules()))
}

// matchedRulesString renders the full matched-rule list as a bounded, typed "[id:verdict,...]" projection.
func matchedRulesString(ms []MatchedRule) string {
	if len(ms) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.ID+":"+string(m.Verdict))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// AuditedEngine wraps an Engine so that every resolved decision is emitted to an AuditSink exactly once
// (audit-every-decision, REQ-1518). It optionally carries the admin EngineToggle (warn.go): when the toggle has
// DISABLED the engine, Decide does NOT run the Rego composition but instead defers to the mechanical floors
// (engine-off ⇒ `approve`, never `auto`; the constitutional never-auto floor still clamps beneath) — and that
// engine-off decision is audited too, so disabling the engine is not a way to escape the audit trail. It does
// NOT change the Decide composition; it only wraps it. Safe for concurrent use (its own fields are immutable
// after construction; the wrapped Engine and toggle are concurrency-safe).
type AuditedEngine struct {
	engine *Engine
	sink   AuditSink
	toggle *EngineToggle // nil ⇒ the engine is always enabled (no admin override layer).
	logf   func(format string, args ...any)
}

// NewAuditedEngine wraps engine with sink. A nil sink is permitted but degraded: decisions still resolve, but
// each un-recordable decision is TRACED (audit-every-decision cannot be satisfied without a sink, so the gap is
// loud, never silent). Attach the admin toggle with WithToggle and a logger with WithLogf.
func NewAuditedEngine(engine *Engine, sink AuditSink) *AuditedEngine {
	return &AuditedEngine{engine: engine, sink: sink}
}

// WithToggle attaches the admin engine toggle (warn.go) and returns the wrapper for chaining. A nil toggle
// leaves the engine always-enabled.
func (a *AuditedEngine) WithToggle(t *EngineToggle) *AuditedEngine {
	if a != nil {
		a.toggle = t
	}
	return a
}

// WithLogf attaches the optional tracer used to record a missing sink / a best-effort append failure and
// returns the wrapper. nil ⇒ silent (but a nil logger is discouraged: a silent governance-audit gap is exactly
// what REQ-1518 forbids).
func (a *AuditedEngine) WithLogf(logf func(string, ...any)) *AuditedEngine {
	if a != nil {
		a.logf = logf
	}
	return a
}

// Decide resolves an action and emits EXACTLY ONE audit record for the result (REQ-1518). It composes with,
// but never alters, Engine.Decide:
//
//   - a nil/uninitialised wrapper or engine fails closed to `approve` (route to a human) — and still audits it;
//   - WHEN the admin toggle has DISABLED the engine for the active mode, it returns the engine-off deferral
//     (warn.go engineOffDecision: `approve`, never `auto`, the never-auto floor recorded beneath) — audited;
//   - OTHERWISE it calls the unmodified Engine.Decide and audits whatever it returns (including the fail-closed
//     `approve` Decide itself produces on a Rego-eval error).
//
// Every one of these paths emits precisely one record, so no decision path is un-audited. A sink failure does
// not change the returned decision (best-effort) but is traced.
func (a *AuditedEngine) Decide(ctx context.Context, in EvalInput) (PolicyDecision, error) {
	if a == nil || a.engine == nil {
		dec := failClosedDecision(in, "", "uninitialised audited engine — fail-closed to approve")
		if a != nil {
			a.emit(ctx, dec)
		}
		return dec, nil
	}

	// Admin engine toggle: when the engine is disabled for the active mode, DEFER to the mechanical floors
	// rather than running the composition. Engine-off is NOT a bypass — the verdict is `approve` and the
	// constitutional never-auto floor still clamps beneath (proven in warn_test.go). The loaded bundle version
	// is still recorded (REQ-1522) so an engine-off decision names which rule set was in force.
	if a.toggle != nil && !a.toggle.Effective(in.Mode) {
		dec := engineOffDecision(in, a.engine.BundleVersion())
		a.emit(ctx, dec)
		return dec, nil
	}

	dec, err := a.engine.Decide(ctx, in)
	// Audit EVERY decision, including the fail-closed `approve` Decide returns alongside a Rego-eval error —
	// the decision still resolved and must be recorded before it is acted on.
	a.emit(ctx, dec)
	return dec, err
}

// emit appends the decision to the sink exactly once, best-effort. A missing sink or an append failure is
// TRACED — a governance-audit gap is never silently swallowed (REQ-1518). The decision has already resolved;
// this never changes it.
func (a *AuditedEngine) emit(ctx context.Context, d PolicyDecision) {
	if a.sink == nil {
		a.log("policy: NO audit sink configured — policy decision NOT recorded (verdict=%s rule=%q); REQ-1518 requires a sink",
			d.Verdict(), d.MatchedRuleID())
		return
	}
	if err := a.sink.AppendPolicyDecision(ctx, d); err != nil {
		a.log("policy: governance audit append FAILED (decision STANDS, verdict=%s rule=%q): %v",
			d.Verdict(), d.MatchedRuleID(), err)
	}
}

func (a *AuditedEngine) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}
