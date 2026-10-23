package policy

// Identical-pipeline invariant guard — spec/015 task T-015-5 (REQ-1501). This is the executable proof of the
// paradigm-rule-8 invariant: the investigation pipeline — ingest → reason → rationale → propose →
// risk-classify → policy-evaluate — SHALL be IDENTICAL across all four global modes (Shadow / HITL /
// Semi-auto / Full-auto); the active mode SHALL govern ONLY the actuation branch (whether a resolved action
// may auto-execute) and SHALL NOT alter any earlier stage.
//
// Where the pipeline lands in code: the reason/rationale/propose/risk-classify stages produce the sealed
// action + spec/001 Band + confidence that are handed to policy.Engine.Decide as an EvalInput; Decide is the
// policy-evaluate stage. The load-bearing, code-level restatement of REQ-1501 is therefore:
//
//	Engine.Decide resolves the SAME PolicyDecision verdict / composed band / approve_by for a given action
//	REGARDLESS of the active Mode. The ONLY thing the mode changes is downstream of the decision — whether
//	that verdict is allowed to auto-execute — which is the actuation branch (T-015-13), NOT the decision.
//
// WHY the invariant holds for the current Decide (design.md "Mode / verdict decision procedure"): Decide
// composes, most-restrictive-first, the constitutional never-auto floor (step 0), the Rego deny-overrides
// base verdict (step 2), param inheritance + the confidence/rate clamps (step 3), band composition (step 4),
// graduation (step 5) and the execution deny-floor (step 6) — every one of which is a function of the ACTION
// (op-class, argv, host, reversibility, band, confidence) and the operator rule DATA, and NONE of which reads
// the active Mode. `in.Mode` enters Decide at exactly one place: it is CARRIED verbatim into the returned
// PolicyDecision.Mode as the audit record of which mode was active — a recorded field, never a branch
// condition. The mode's actuation semantics ("does this `auto` actually actuate?" = mode ∈ {Semi,Full}) live
// at the actuation chokepoint (mode.MayAutoActuate, T-015-13), strictly AFTER and OUTSIDE Decide. This guard
// asserts that property by construction: it re-runs Decide across all four modes and requires the verdict,
// the composed band, and the approve_by set to be byte-identical — only PolicyDecision.Mode is permitted to
// differ, because that field's whole job is to record the active mode.
//
// A mode-dependent pipeline is a GOVERNANCE BUG (it would let the mode silently change what the system
// concludes, not merely whether it acts), so a violation is FAIL-CLOSED: AssertModeInvariant returns a typed
// error naming the divergence, and the boot SelfTest REFUSES to start a worker whose pipeline is
// mode-dependent — exactly as the actuation interceptor's SelfTest refuses to boot an unwired chain.
//
// Provenance: [F] · [R] paradigm-rule 8. See spec/015-policy-engine requirements.md REQ-1501 and design.md
// (`policy.Mode`: "the mode governs ONLY the actuation branch — pipeline_guard.go asserts the ingest → reason
// → rationale → propose → risk-classify stages take the same code path in every mode").

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/territory-grounder/grounder/core/safety"
)

// ErrPipelineModeDependent is the typed, fail-closed invariant violation (REQ-1501): the pipeline resolved a
// DIFFERENT decision under different modes, so the mode governs more than the actuation branch. Callers test
// it with errors.Is; the concrete *ModeDivergence naming the diverging field/modes/values unwraps to it.
var ErrPipelineModeDependent = errors.New("policy: pipeline is mode-dependent — the mode governs more than the actuation branch (REQ-1501 violation)")

// ModeDecider is the narrow behavioral slice the guard depends on: resolve an EvalInput to a PolicyDecision.
// *Engine satisfies it. Depending on the interface (not the concrete *Engine) is what lets the guard's own
// oracle inject a deliberately mode-DEPENDENT stub to prove the guard actually detects a violation (it is not
// a vacuous no-op). NOTHING in production may implement this to branch on in.Mode — that is the very bug the
// guard exists to catch.
type ModeDecider interface {
	Decide(ctx context.Context, in EvalInput) (PolicyDecision, error)
}

// DeciderFactory builds a FRESH ModeDecider for one mode's evaluation. The guard uses a factory rather than a
// single shared decider so that any PER-ACTION MUTABLE state a real engine carries — the rate governor's
// trailing-minute counters (REQ-1508) and the graduation ladder's run history (REQ-1514) — starts IDENTICAL
// for every mode. That isolation is what makes the comparison sound: the ONLY variable across the four
// evaluations is in.Mode, so a divergence can be attributed to the mode and nothing else. A stateless engine
// (no rate governor, no graduation) may return the same instance every call — sharing is safe when there is
// no per-action state; the SelfTest does exactly that.
type DeciderFactory func(ctx context.Context) (ModeDecider, error)

// allModes is the closed set of four global modes the invariant quantifies over (REQ-1500).
var allModes = [...]Mode{ModeShadow, ModeHITL, ModeSemiAuto, ModeFullAuto}

// ModeDivergence is the concrete, typed evidence of a REQ-1501 violation: a decision field that changed
// between two modes for the same action. It unwraps to ErrPipelineModeDependent, so errors.Is(err,
// ErrPipelineModeDependent) holds while errors.As(err, **ModeDivergence) recovers the diverging field, the
// two modes, and the two values for the fail-closed boot log.
type ModeDivergence struct {
	Field  string // the decision field that diverged: "verdict", "composed_band", or "approve_by".
	ModeA  Mode   // the reference mode (the first mode evaluated).
	ModeB  Mode   // the mode whose decision differed from the reference.
	ValueA string // the reference mode's value of Field.
	ValueB string // the diverging mode's value of Field.
}

// Error renders the divergence for the fail-closed boot log / the returned error.
func (d *ModeDivergence) Error() string {
	return fmt.Sprintf("policy pipeline is mode-dependent: %s differs between mode %s (%s) and mode %s (%s) for an identical action — the mode must govern ONLY the actuation branch (REQ-1501)",
		d.Field, d.ModeA, d.ValueA, d.ModeB, d.ValueB)
}

// Unwrap ties the concrete divergence to the sentinel so errors.Is(err, ErrPipelineModeDependent) holds.
func (d *ModeDivergence) Unwrap() error { return ErrPipelineModeDependent }

// AssertModeInvariant is the guard (REQ-1501): it builds a fresh decider for EACH of the four global modes
// via factory, evaluates the SAME action `in` in every mode (overriding only in.Mode), and requires the
// resolved verdict, composed band, and approve_by set to be IDENTICAL across all four. Only
// PolicyDecision.Mode is permitted to differ — that field records the active mode for the audit, which is its
// purpose. On any divergence it returns a *ModeDivergence (which errors.Is-matches ErrPipelineModeDependent),
// so a mode-dependent pipeline is caught, named, and fails closed. A factory or Decide error is surfaced
// verbatim (fail closed — the guard never passes on an error it could not evaluate).
//
// It is usable as a boot-time self-test (see SelfTest) and as a runtime assertion around a specific action.
func AssertModeInvariant(ctx context.Context, factory DeciderFactory, in EvalInput) error {
	if factory == nil {
		return errors.New("policy: nil decider factory — cannot assert the pipeline mode-invariant")
	}

	var (
		refVerdict Verdict
		refBand    string
		refApprove []string
		refMode    Mode
		haveRef    bool
	)

	for _, mode := range allModes {
		d, err := factory(ctx)
		if err != nil {
			return fmt.Errorf("policy: pipeline mode-invariant: building decider for mode %s: %w", mode, err)
		}
		if d == nil {
			return fmt.Errorf("policy: pipeline mode-invariant: nil decider for mode %s", mode)
		}

		probe := in
		probe.Mode = mode
		dec, err := d.Decide(ctx, probe)
		if err != nil {
			return fmt.Errorf("policy: pipeline mode-invariant: deciding in mode %s: %w", mode, err)
		}

		verdict := dec.Verdict()
		band := dec.ComposedBand().String()
		approve := dec.ApproveBy()

		if !haveRef {
			refVerdict, refBand, refApprove, refMode, haveRef = verdict, band, approve, mode, true
			continue
		}
		if verdict != refVerdict {
			return &ModeDivergence{Field: "verdict", ModeA: refMode, ModeB: mode, ValueA: string(refVerdict), ValueB: string(verdict)}
		}
		if band != refBand {
			return &ModeDivergence{Field: "composed_band", ModeA: refMode, ModeB: mode, ValueA: refBand, ValueB: band}
		}
		if !slices.Equal(approve, refApprove) {
			return &ModeDivergence{Field: "approve_by", ModeA: refMode, ModeB: mode,
				ValueA: fmt.Sprintf("%v", refApprove), ValueB: fmt.Sprintf("%v", approve)}
		}
	}
	return nil
}

// selfTestInputs is the representative set of actions the boot SelfTest sweeps: one that resolves to each
// point of the verdict trinary plus a no-match action. Each must resolve IDENTICALLY in all four modes.
func selfTestInputs() []EvalInput {
	return []EvalInput{
		// Resolves to `auto` (reversible, high-confidence, auto-banded, non-floor) — the case a mode-dependent
		// pipeline would be MOST tempted to change (auto is the actuation-relevant verdict); it must not.
		{OpClass: "guard.auto", Reversible: true, Confidence: 1.0, Band: safety.BandAuto},
		// Resolves to `deny` (deny-overrides base verdict).
		{OpClass: "guard.deny", Reversible: true, Confidence: 1.0, Band: safety.BandAuto},
		// Resolves to `approve` (an explicit approve rule → carries approve_by).
		{OpClass: "guard.approve", Reversible: true, Confidence: 1.0, Band: safety.BandAuto},
		// No matching rule → the fail-closed default (`approve`, empty approve_by).
		{OpClass: "guard.nomatch", Reversible: true, Confidence: 1.0, Band: safety.BandAuto},
	}
}

// selfTestFactory builds the representative STATELESS engine used by SelfTest: a small operator rule set
// covering the verdict trinary, no rate governor and no graduation ladder (so it carries no per-action
// mutable state and one instance is safe to share across every mode). It is DATA-only — rules enter as data,
// never Rego — exactly as production requires (REQ-1503).
func selfTestFactory(ctx context.Context) (DeciderFactory, error) {
	auto, err := NewRule(Rule{ID: "guard-auto", Match: Match{OpClass: "guard.auto"}, Verdict: VerdictAuto})
	if err != nil {
		return nil, err
	}
	deny, err := NewRule(Rule{ID: "guard-deny", Match: Match{OpClass: "guard.deny"}, Verdict: VerdictDeny})
	if err != nil {
		return nil, err
	}
	approve, err := NewRule(Rule{ID: "guard-approve", Match: Match{OpClass: "guard.approve"}, Verdict: VerdictApprove, ApproveBy: []string{"group:sre-oncall"}})
	if err != nil {
		return nil, err
	}
	eng, err := NewEngine(ctx, RuleSet{Rules: []Rule{auto, deny, approve}})
	if err != nil {
		return nil, err
	}
	// The engine is stateless (no rate governor / graduation), so returning the SAME instance for every mode
	// is sound — there is no per-action state to isolate.
	return func(context.Context) (ModeDecider, error) { return eng, nil }, nil
}

// SelfTest asserts the identical-pipeline invariant (REQ-1501) on the REAL composed Engine.Decide across a
// representative sweep of actions (the verdict trinary + a no-match), and is meant to be called from the boot
// preflight: if it returns non-nil the worker MUST refuse to start, because a mode-dependent pipeline is a
// governance bug (fail closed), mirroring how core/actuate.Interceptor.SelfTest refuses to boot an unwired
// chain. It returns nil when the pipeline is provably mode-independent for every probed action.
func SelfTest(ctx context.Context) error {
	factory, err := selfTestFactory(ctx)
	if err != nil {
		return fmt.Errorf("policy: pipeline-guard self-test: building the representative engine: %w", err)
	}
	for _, in := range selfTestInputs() {
		if err := AssertModeInvariant(ctx, factory, in); err != nil {
			return fmt.Errorf("policy: pipeline-guard self-test failed for action %q: %w", in.Host, err)
		}
	}
	return nil
}
