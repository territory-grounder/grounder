package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/safety"
)

// EvalInput is the typed action description the engine evaluates a policy over (design.md `policy.Engine`).
// It uses the estate-object-model + policy-dimension fields (Host/Resource/Groups/DeviceClass/OpClass/Argv/
// Territory/Reversible) to match rules against the ONE shared grammar, and CARRIES the spec/001 risk Band,
// the bound Confidence, and the active Mode that the composed Decide consumes (Band in step 4 band
// composition, Confidence in step 3 the confidence clamp, Mode carried into the PolicyDecision for the audit
// + the downstream actuation gate).
type EvalInput struct {
	OpClass     string   // semantic op-class (the allow side of a match).
	Argv        string   // raw command string (the deny side of a match).
	Host        string   // canonical host name.
	Resource    string   // named resource id.
	Groups      []string // estate group memberships.
	DeviceClass string   // device-class (e.g. "cisco-asa").
	Territory   string   // estate territory.
	Reversible  bool     // whether the action is reversible.

	Confidence float64     // bound model confidence (clamped in step 3, REQ-1507).
	Band       safety.Band // the action's spec/001 risk band (composed in step 4, REQ-1509/1510).
	Mode       Mode        // the active global autonomy mode (carried into the decision; the mode zero value is Shadow).

	// ActionID, ExternalRef, Principal are the NON-SECRET correlation/attribution keys threaded from the
	// interceptor so the audited policy_decision joins into the decision tracer's per-incident walk by BOTH
	// keys (spec/020 REQ-2005), rather than the empty columns migration 0019 left. They carry NOTHING that
	// composes a verdict — Decide's outcome is identical with them empty (a non-interceptor caller leaves them
	// ""); they only ride into the audit projection. ActionID is the content-hashed manifest id (INV-07),
	// ExternalRef the normalized incident trigger, Principal the acting identity — never argv/host/credential.
	ActionID    string
	ExternalRef string
	Principal   string
}

// Decision is the structured, NON-SECRET projection of the Rego deny-overrides BASE evaluation — the verdict
// the fixed module resolved BEFORE the composition stages tightened it. It carries the base Verdict, the id
// of the rule that determined it (MatchedRuleID — empty when no rule matched and the fail-closed default
// applied), and a human-readable Reason for the console packet-tracer. The composed, required-field result
// of a full evaluation is PolicyDecision; this base projection is carried inside PolicyDecision.Audit().Base.
type Decision struct {
	Verdict       Verdict
	MatchedRuleID string
	Reason        string
}

// MatchedRule is one rule that matched the action, projected NON-SECRET to its stable id + its declared
// verdict. The FULL list of these (DecisionAudit.MatchedRules) makes a deny-overrides decision explainable —
// the winning verdict is the most-restrictive over this set — WITHOUT changing the outcome. It is bounded and
// TYPED (an id + a closed-enum verdict), never free text, and carries no argv/host/credential.
type MatchedRule struct {
	ID      string
	Verdict Verdict
}

// DecisionAudit bundles the NON-SECRET sub-records each composition stage produced, so one PolicyDecision
// carries the full provenance of every stage that shaped the verdict (the audit projection of REQ-1518). It
// holds NO secret: every sub-record is a plain verdict/number/bool/string projection (no argv, host, or
// credential). This projection IS the serialized decision detail — it is the payload the LedgerAuditSink
// (audit.go) persists onto the tamper-evident governance ledger, so a hardening field added here lands in the
// audit trail with NO new DB column or migration (REQ-1522).
type DecisionAudit struct {
	Base             Decision      // the Rego deny-overrides base projection (verdict, matched rule, reason) — step 2.
	NeverAutoFloor   bool          // whether the constitutional never-auto floor (INV-09, step 0) applied to the action.
	Refine           RefineRecord  // the confidence + rate clamp record (step 3).
	Compose          ComposeRecord // the band-composition record (step 4).
	GraduatedVerdict Verdict       // the verdict after the graduation hook (step 5).
	Floor            FloorRecord   // the execution deny-floor record (step 6).

	// BundleVersion is the content-derived identity of the operator rule bundle this decision was evaluated
	// over (REQ-1522) — a fingerprint of the rules-as-data, so the audit trail records EXACTLY which rule set
	// authorized or refused the action. Empty only when no bundle was loaded (a nil/uninitialised engine).
	BundleVersion string
	// MatchedRules is the FULL list of rules that matched the action (REQ-1522), each projected to its id +
	// verdict. The resolved verdict is the deny-overrides winner over this set; surfacing the whole set makes
	// the decision explainable without changing the outcome. Empty when nothing matched (fail-closed default).
	MatchedRules []MatchedRule

	// ActionID, ExternalRef, Principal are the NON-SECRET correlation/attribution keys carried from EvalInput
	// (spec/020 REQ-2005) so a persisted policy_decision joins the decision-tracer walk by both keys. Empty for
	// a non-interceptor caller (e.g. a console preview) — exactly as before.
	ActionID    string
	ExternalRef string
	Principal   string
}

// PolicyDecision is the REQUIRED-FIELD result of one Engine.Decide evaluation (design.md `policy.Engine`,
// REQ-1518). Every field is required by construction: the fields are UNEXPORTED and the ONLY way to build a
// PolicyDecision is NewPolicyDecision, whose signature names every field — so a caller that forgets a field
// is a COMPILE error, which is how the persistence contract (INV-19) is enforced at the type level. Read the
// fields through the accessor methods; mutate nothing.
type PolicyDecision struct {
	verdict       Verdict
	matchedRuleID string
	composedBand  safety.Band
	approveBy     []string
	mode          Mode
	reason        string
	audit         DecisionAudit
}

// NewPolicyDecision is the ONLY constructor for a PolicyDecision — it requires every field, so producing a
// decision with a missing field cannot compile (REQ-1518, INV-19 at the type level). approveBy is copied
// defensively so the decision cannot be mutated through the caller's slice.
func NewPolicyDecision(verdict Verdict, matchedRuleID string, composedBand safety.Band, approveBy []string, mode Mode, reason string, audit DecisionAudit) PolicyDecision {
	return PolicyDecision{
		verdict:       verdict,
		matchedRuleID: matchedRuleID,
		composedBand:  composedBand,
		approveBy:     append([]string(nil), approveBy...),
		mode:          mode,
		reason:        reason,
		audit:         audit,
	}
}

// Verdict is the fully-composed verdict (auto / approve / deny) after every stage.
func (d PolicyDecision) Verdict() Verdict { return d.verdict }

// MatchedRuleID is the id of the rule that determined the base verdict (empty when the fail-closed default applied).
func (d PolicyDecision) MatchedRuleID() string { return d.matchedRuleID }

// ComposedBand is the action's spec/001 risk band composed into the decision (the "band" of the audit row, REQ-1518).
func (d PolicyDecision) ComposedBand() safety.Band { return d.composedBand }

// ApproveBy is the matched rule's approve_by principals for a resolved `approve` (empty otherwise). A COPY.
func (d PolicyDecision) ApproveBy() []string { return append([]string(nil), d.approveBy...) }

// Mode is the active global autonomy mode carried into the decision for the audit + the downstream actuation gate.
func (d PolicyDecision) Mode() Mode { return d.mode }

// Reason is the human-readable packet-tracer explanation of the composed decision.
func (d PolicyDecision) Reason() string { return d.reason }

// Audit is the NON-SECRET bundle of every stage's sub-record (the persistence projection, REQ-1518).
func (d PolicyDecision) Audit() DecisionAudit { return d.audit }

// BundleVersion is the content-derived identity of the operator rule bundle this decision was evaluated over
// (REQ-1522) — carried on EVERY decision so the audit trail names exactly which rule set authorized/refused it.
func (d PolicyDecision) BundleVersion() string { return d.audit.BundleVersion }

// MatchedRules is the FULL, bounded, typed list of rules that matched the action (deny-overrides provenance,
// REQ-1522). A defensive COPY — mutating it does not mutate the decision.
func (d PolicyDecision) MatchedRules() []MatchedRule {
	return append([]MatchedRule(nil), d.audit.MatchedRules...)
}

// ActionID, ExternalRef, Principal are the NON-SECRET correlation/attribution keys carried from EvalInput
// (spec/020 REQ-2005) — the audit writer persists them so a policy_decision joins the decision-tracer walk by
// both keys. Empty for a non-interceptor caller.
func (d PolicyDecision) ActionID() string    { return d.audit.ActionID }
func (d PolicyDecision) ExternalRef() string { return d.audit.ExternalRef }
func (d PolicyDecision) Principal() string   { return d.audit.Principal }

// Engine is the single policy-evaluation entry point the actuation interceptor (spec/013) consults before it
// decides auto / approve / deny for a classified, gated action. It holds the operator rule DATA and the
// prepared FIXED Rego evaluator, plus the INJECTED composition pieces (the rate governor, the graduation
// ladder, and the execution deny-floor). Each injected piece is NIL-SAFE: a nil piece makes that stage a
// pass-through — EXCEPT the constitutional never-auto floor, which is a pure function of the input threaded
// into band composition and therefore ALWAYS applies, so no engine (even one built with every piece nil) can
// ever emit `auto` for a floor-class action (INV-09). There is deliberately NO setter or constructor that
// accepts Rego source — operators supply only data (REQ-1503).
type Engine struct {
	ruleSet       RuleSet
	bundleVersion string        // content-derived identity of ruleSet, threaded into every decision (REQ-1522).
	eval          baseEvaluator // the fixed deny-overrides evaluator seam (real impl *evaluator, eval.go).
	rateGov       *RateGovernor // step 3 rate governor (nil ⇒ confidence clamp only, no rate governor).
	grad          *Ladder       // step 5 graduation ladder (nil ⇒ no graduation gating — pass-through).
	floor         *Floor        // step 6 execution deny-floor (nil ⇒ floors nothing — pass-through).
}

// baseEvaluator is the narrow seam Decide calls to run the FIXED deny-overrides Rego module over the
// pre-matched rule DATA. The real implementation is *evaluator (eval.go, the embedded audited module — there
// is still NO path from operator input to evaluator logic, REQ-1503). Behind an interface so the
// fail-closed-to-DENY-on-evaluator-error contract (REQ-1522) is lockable by an oracle that injects an
// evaluator which returns an error instead of a resolved verdict.
type baseEvaluator interface {
	evaluate(ctx context.Context, preparedRules []map[string]any) (regoResult, error)
}

// NewEngine builds an engine over an operator rule set (validated DATA — build it with ParseRuleSet or
// NewRule) and the embedded fixed evaluator. A nil/empty rule set is valid: with no rules every action
// resolves to the fail-closed default `approve` (route to a human), never `auto`. The composition pieces are
// attached with the With* options; unset pieces are nil-safe pass-throughs (the never-auto floor still applies).
func NewEngine(ctx context.Context, rs RuleSet) (*Engine, error) {
	ev, err := newEvaluator(ctx)
	if err != nil {
		return nil, err
	}
	return &Engine{ruleSet: rs, bundleVersion: bundleVersion(rs), eval: ev}, nil
}

// BundleVersion is the content-derived identity of the operator rule bundle this engine evaluates over
// (REQ-1522). It is deterministic over the rule DATA, so the same bundle always names the same version and a
// decision's recorded BundleVersion can be matched back to the rule set that produced it. A nil engine → "".
func (e *Engine) BundleVersion() string {
	if e == nil {
		return ""
	}
	return e.bundleVersion
}

// bundleVersion computes the deterministic, NON-SECRET content fingerprint of an operator rule set — the
// "bundle version" recorded on every decision (REQ-1522). It hashes a CANONICAL projection of the rules-as-data
// (ordered rules; each rule's id, verdict, match dimensions, params, approve_by, default flag; plus the
// global-default params), so a cosmetic change does not move the version but any change to an evaluated field
// does. It carries no secret: the rule DATA is non-secret (op-classes, host globs, verdicts, params).
func bundleVersion(rs RuleSet) string {
	type canonMatch struct {
		SelectorKind    string `json:"sk,omitempty"`
		SelectorPattern string `json:"sp,omitempty"`
		OpClass         string `json:"op,omitempty"`
		ArgvPattern     string `json:"argv,omitempty"`
		Territory       string `json:"terr,omitempty"`
		Reversible      *bool  `json:"rev,omitempty"`
	}
	type canonParams struct {
		MinConfidence *float64 `json:"mc,omitempty"`
		BandMode      string   `json:"bm,omitempty"`
		RateLimit     *int     `json:"rl,omitempty"`
	}
	type canonRule struct {
		ID        string      `json:"id"`
		Verdict   string      `json:"v"`
		Match     canonMatch  `json:"m"`
		Params    canonParams `json:"p"`
		ApproveBy []string    `json:"ab,omitempty"`
		IsDefault bool        `json:"d,omitempty"`
	}
	canonP := func(p Params) canonParams {
		return canonParams{MinConfidence: p.MinConfidence, BandMode: string(p.BandMode), RateLimit: p.RateLimit}
	}
	set := struct {
		Default canonParams `json:"default"`
		Rules   []canonRule `json:"rules"`
	}{Default: canonP(rs.Default), Rules: make([]canonRule, 0, len(rs.Rules))}
	for _, r := range rs.Rules {
		cm := canonMatch{
			OpClass: r.Match.OpClass, ArgvPattern: r.Match.ArgvPattern,
			Territory: r.Match.Territory, Reversible: r.Match.Reversible,
		}
		if r.Match.Selector != nil {
			cm.SelectorKind = string(r.Match.Selector.Kind)
			cm.SelectorPattern = r.Match.Selector.Pattern
		}
		set.Rules = append(set.Rules, canonRule{
			ID: r.ID, Verdict: string(r.Verdict), Match: cm, Params: canonP(r.Params),
			ApproveBy: r.ApproveBy, IsDefault: r.IsDefault,
		})
	}
	b, err := json.Marshal(set)
	if err != nil {
		// A rule set is plain data with no unmarshalable field, so this cannot happen; fail closed to a
		// stable non-empty sentinel rather than an empty version if it ever did.
		return "sha256:unencodable"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// WithRateGovernor injects the step-3 rate governor and returns the engine for chaining. A nil governor is a
// pass-through: the confidence clamp still applies (it is pure), only the rate half no-ops.
func (e *Engine) WithRateGovernor(g *RateGovernor) *Engine {
	if e != nil {
		e.rateGov = g
	}
	return e
}

// WithGraduation injects the step-5 graduation ladder and returns the engine for chaining. A nil ladder is a
// pass-through: no op-class is graduation-gated (an `auto` rule verdict is not downgraded on that account).
func (e *Engine) WithGraduation(l *Ladder) *Engine {
	if e != nil {
		e.grad = l
	}
	return e
}

// WithFloor injects the step-6 execution deny-floor and returns the engine for chaining. A nil floor floors
// nothing. The POLICY floor being absent does NOT lift the constitutional never-auto floor beneath it.
func (e *Engine) WithFloor(f *Floor) *Engine {
	if e != nil {
		e.floor = f
	}
	return e
}

// Decide resolves an action to a required-field PolicyDecision by composing the pieces in the design.md
// most-restrictive-first order (REQ-1518). Each stage only ever TIGHTENS the verdict:
//
//	step 0  constitutional never-auto floor  — NeverAutoApplies(in), threaded into band composition; ALWAYS applies.
//	step 2  Rego deny-overrides base verdict — the fixed module over the operator rule DATA (a deny SHORT-CIRCUITS).
//	        (params inheritance)             — RuleSet.EffectiveParams resolves the matched rule's min_confidence/rate/band.
//	step 3  confidence + rate clamps         — RateGovernor.Refine tightens a low-confidence / over-rate auto → approve.
//	step 4  band composition                 — ComposeBand(respect|force) with the never-auto floor clamp (inviolable).
//	step 5  graduation                       — GraduatedVerdict downgrades an ungraduated `auto` → approve.
//	step 6  execution deny-floor             — ApplyFloor floors a floor-class action to deny at execute.
//	        approve_by                       — a resolved `approve` carries the matched rule's approve_by principals.
//
// The active Mode is CARRIED into the decision for the audit + the downstream actuation gate; the mode's
// actuation semantics ("does this auto actually actuate?" = mode ∈ {Semi,Full}) belong to the actuation
// chokepoint (T-015-13), NOT to Decide — the verdict here is the policy authorization only.
//
// Fail-closed: a nil/uninitialised engine → `approve` (route to a human — the engine is not wired, so the
// warn-don't-block default applies); a Rego evaluator ERROR → `deny` (REFUSE — a broken evaluator cannot be
// warned-around, so the one action fails closed to the most-restrictive verdict) and the error is surfaced.
// NEVER `auto` on any error path, and NEVER a permissive verdict on an evaluator error (REQ-1522, INV-09).
func (e *Engine) Decide(ctx context.Context, in EvalInput) (PolicyDecision, error) {
	if e == nil || e.eval == nil {
		// A zero/uninitialised engine fails closed to a human route rather than panicking or auto-executing.
		// No bundle was loaded, so the recorded bundle version is empty.
		return failClosedDecision(in, "", "uninitialised engine — fail-closed to approve"), nil
	}

	// Step 0 — the constitutional never-auto floor. It is a PURE function of the input (not an injected piece),
	// so it can never be nil'd away; it is threaded into band composition where it always clamps an `auto`.
	neverAuto := NeverAutoApplies(in)
	// Record the loaded bundle version on EVERY decision this engine emits (REQ-1522).
	audit := DecisionAudit{NeverAutoFloor: neverAuto, BundleVersion: e.bundleVersion,
		ActionID: in.ActionID, ExternalRef: in.ExternalRef, Principal: in.Principal}

	// Step 2 — Rego deny-overrides base verdict + the matched rule (for its params + approve_by) + the FULL
	// matched-rule list (deny-overrides provenance, REQ-1522).
	base, matched, matchedRules, err := e.evaluateBase(ctx, in)
	if err != nil {
		// The evaluator returned an ERROR, not a resolved verdict — fail closed to DENY (refuse this action),
		// carrying the loaded bundle version. Never `approve`, never `auto` (REQ-1522, INV-09).
		return failClosedDenyDecision(in, e.bundleVersion, "rego evaluation error — fail-closed to DENY (refuse)"), err
	}
	audit.Base = base
	audit.MatchedRules = matchedRules
	verdict := base.Verdict

	// A deny SHORT-CIRCUITS (deny-overrides, REQ-1504): it is the most-restrictive verdict and no later,
	// only-tightening stage can lift it, so we resolve immediately and skip the refine/band/graduation/floor
	// stages. The band the decision records is still the action's own risk band.
	if verdict == VerdictDeny {
		audit.GraduatedVerdict = VerdictDeny
		return NewPolicyDecision(VerdictDeny, base.MatchedRuleID, in.Band, nil, in.Mode,
			"deny short-circuits: "+base.Reason, audit), nil
	}

	// Resolve the matched rule's effective params (inheritance from the global default + the 0.60 floor, REQ-1507).
	params := e.ruleSet.EffectiveParams(matched)

	// Step 3 — confidence clamp + rate governor (auto→approve). Refine is nil-safe on a nil governor: the pure
	// confidence clamp still applies, only the rate half no-ops.
	verdict, audit.Refine = e.rateGov.Refine(verdict, in.Confidence, params, in.OpClass)

	// Step 4 — band composition with the inviolable never-auto floor. ComposeBand is a pure package function
	// (NOT an injected piece) so it — and the never-auto clamp inside it — ALWAYS runs (INV-09).
	verdict, audit.Compose = ComposeBand(verdict, in.Band, params.BandMode, neverAuto)

	// Step 5 — graduation: an ungraduated `auto` downgrades to `approve`. A nil ladder is a pass-through.
	if e.grad != nil {
		verdict = e.grad.GraduatedVerdict(ctx, in.OpClass, verdict)
	}
	audit.GraduatedVerdict = verdict

	// Step 6 — execution deny-floor: a floor-class action floors to `deny` at execute. A nil floor is a pass-through.
	verdict, audit.Floor = e.floor.ApplyFloor(verdict, in)

	// approve_by — for a resolved `approve`, carry the matched rule's approve_by principals (REQ-1516 vote path).
	var approveBy []string
	if verdict == VerdictApprove {
		approveBy = matched.ApproveBy
	}

	return NewPolicyDecision(verdict, base.MatchedRuleID, in.Band, approveBy, in.Mode,
		decideReason(verdict, audit), audit), nil
}

// failClosedDecision builds the fail-closed `approve` PolicyDecision (route to a human) used on the nil-engine
// and admin-engine-off paths — a NON-error deferral where warn-don't-block routes to a human. It NEVER emits
// `auto`, records whether the never-auto floor applies, and carries the loaded bundle version (empty when no
// bundle is loaded) so even a deferral names which rule set was in force (REQ-1522).
func failClosedDecision(in EvalInput, bundleVer, reason string) PolicyDecision {
	base := Decision{Verdict: VerdictApprove, Reason: reason}
	return NewPolicyDecision(VerdictApprove, "", in.Band, nil, in.Mode, reason,
		DecisionAudit{Base: base, NeverAutoFloor: NeverAutoApplies(in), GraduatedVerdict: VerdictApprove, BundleVersion: bundleVer,
			ActionID: in.ActionID, ExternalRef: in.ExternalRef, Principal: in.Principal})
}

// failClosedDenyDecision builds the fail-closed `deny` PolicyDecision used ONLY on the evaluator-ERROR path: a
// broken evaluator cannot produce a trustworthy verdict, so the one action is REFUSED (deny) rather than let
// through on a permissive default (REQ-1522, INV-09). It NEVER emits `auto` or `approve`, records the never-auto
// floor, and carries the loaded bundle version. This is a distinct control from the warn-don't-block `approve`
// deferral above: an operator's warn-don't-block posture governs actions they wrote no deny for, NOT a failure
// of the evaluator itself.
func failClosedDenyDecision(in EvalInput, bundleVer, reason string) PolicyDecision {
	base := Decision{Verdict: VerdictDeny, Reason: reason}
	return NewPolicyDecision(VerdictDeny, "", in.Band, nil, in.Mode, reason,
		DecisionAudit{Base: base, NeverAutoFloor: NeverAutoApplies(in), GraduatedVerdict: VerdictDeny, BundleVersion: bundleVer,
			ActionID: in.ActionID, ExternalRef: in.ExternalRef, Principal: in.Principal})
}

// evaluateBase runs the fixed Rego deny-overrides module over the operator rule DATA and returns the base
// Decision projection plus the matched Rule (its params + approve_by feed the later stages). Matching uses
// the ONE shared estate-object-model grammar (Match.matches → credential.Match) computed in Go; the Rego
// module applies only the order-independent deny-overrides combination over the matched rules' verdicts.
func (e *Engine) evaluateBase(ctx context.Context, in EvalInput) (Decision, Rule, []MatchedRule, error) {
	prepared := make([]map[string]any, 0, len(e.ruleSet.Rules))
	index := make(map[string]Rule, len(e.ruleSet.Rules))
	for _, r := range e.ruleSet.Rules {
		if r.IsDefault {
			// The global-default rule contributes params (inheritance), not a verdict match — it must never
			// itself resolve an action, so it is not offered to the deny-overrides combination.
			continue
		}
		index[r.ID] = r
		prepared = append(prepared, map[string]any{
			"id":      r.ID,
			"verdict": string(r.Verdict),
			"matched": r.Match.matches(in),
		})
	}

	res, err := e.eval.evaluate(ctx, prepared)
	if err != nil {
		return Decision{}, Rule{}, nil, err
	}
	// The FULL list of matched rules, each projected to its id + declared verdict (REQ-1522). The order is the
	// ruleset order (the module returns matched_ids in input order); the resolved verdict is the deny-overrides
	// winner over this set. Every matched id is a non-default rule present in index.
	matchedRules := make([]MatchedRule, 0, len(res.matchedIDs))
	for _, mid := range res.matchedIDs {
		matchedRules = append(matchedRules, MatchedRule{ID: mid, Verdict: index[mid].Verdict})
	}
	id := pickRuleID(res)
	return Decision{Verdict: res.verdict, MatchedRuleID: id, Reason: reasonFor(res)}, index[id], matchedRules, nil
}

// decideReason renders the composed packet-tracer summary naming each stage's contribution.
func decideReason(final Verdict, a DecisionAudit) string {
	return fmt.Sprintf(
		"composed verdict %q — base %q; never-auto-floor=%v; confidence/rate→%q; band→%q; graduation→%q; floor→%q",
		final, a.Base.Verdict, a.NeverAutoFloor, a.Refine.VerdictOut, a.Compose.Composed, a.GraduatedVerdict, a.Floor.ExecutionVerdict)
}

// pickRuleID returns the id of the rule that determined the verdict: the first matching deny for a deny
// (deny-overrides provenance), else the first matching rule carrying the winning verdict, else empty (the
// fail-closed default applied because nothing matched).
func pickRuleID(res regoResult) string {
	if res.verdict == VerdictDeny && len(res.denyIDs) > 0 {
		return res.denyIDs[0]
	}
	if len(res.winningIDs) > 0 {
		return res.winningIDs[0]
	}
	return ""
}

func reasonFor(res regoResult) string {
	switch {
	case res.verdict == VerdictDeny:
		return fmt.Sprintf("deny-overrides: rule %q denied (%d matching rule(s))", pickRuleID(res), len(res.matchedIDs))
	case len(res.matchedIDs) == 0:
		return "no matching rule — fail-closed default: route to human (approve)"
	default:
		return fmt.Sprintf("rule %q resolved %q (%d matching rule(s))", pickRuleID(res), res.verdict, len(res.matchedIDs))
	}
}
