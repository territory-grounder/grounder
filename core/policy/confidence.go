package policy

// min_confidence clamp + the combined refine step — spec/015 task T-015-3 (REQ-1507, REQ-1508). This is
// step 3 of the mode/verdict decision procedure (design.md): AFTER the fixed deny-overrides evaluation has
// produced a policy Verdict, the engine TIGHTENS that verdict when the action's bound confidence is below the
// resolved min_confidence, and (in ratelimit.go) when the auto-execution rate would exceed the rule's
// rate_limit. Both clamps are one-directional: they can only make the verdict MORE restrictive (auto→approve,
// route to a human), NEVER less. They sit ABOVE the constitutional mechanical never-auto floor (INV-09), which
// runs beneath the engine regardless; a clamp only ever raises scrutiny and can never bypass that floor.
//
// The confidence clamp is a PURE function (ClampConfidence): no I/O, no state, deterministic. The combined
// Refine is a method on the RateGovernor (ratelimit.go) because the rate half carries the only state; it
// applies the pure confidence clamp FIRST (so a low-confidence auto never even consumes a rate-budget slot),
// then the rate governor. Order is deliberate and cannot loosen: each stage only ever tightens.
//
// FAIL-CLOSED (INV-09): an out-of-range or NaN confidence is treated as BELOW any positive threshold, so a
// garbage confidence clamps auto→approve rather than slipping through as auto. A non-positive/unset threshold
// means "no confidence gate" (the default 0.60 is resolved UPSTREAM via RuleSet.EffectiveParams / the
// global-default rule, REQ-1507 — this function honours whatever resolved threshold it is given, and 0/unset
// is an explicit no-gate).
//
// Provenance: [F] · [O] INV-11 (confidence gate) · [O] INV-09 (only tightens, never lifts the floor).
// See spec/015-policy-engine REQ-1507.

import (
	"fmt"
	"math"
)

// ClampRecord is the NON-SECRET projection of one confidence clamp, produced as a required output so the clamp
// is fully auditable (it feeds the RefineRecord and, later, the policy_decision audit row in T-015-12; here it
// is only returned, never persisted). Every field is a plain verdict/float/bool/string — there is no
// credential, no argv, and no host-secret in it.
type ClampRecord struct {
	VerdictIn     Verdict // the verdict presented to the clamp.
	VerdictOut    Verdict // the verdict after the clamp (== VerdictIn unless Clamped).
	Confidence    float64 // the action's bound confidence (as seen; may be NaN/out-of-range).
	MinConfidence float64 // the resolved min_confidence threshold applied (0/unset ⇒ no gate).
	Clamped       bool    // true WHEN the gate tightened auto→approve.
	Reason        string  // human-readable explanation for the console packet-tracer.
}

// ClampConfidence tightens a policy Verdict when the action's bound confidence is below the resolved
// min_confidence (REQ-1507). It is PURE and deterministic.
//
// Semantics — it ONLY EVER tightens (auto→approve), NEVER loosens:
//   - Only an `auto` verdict is subject to the gate. `approve`/`deny` are already at or above the human bar,
//     so they pass through unchanged (clamping them would be a no-op at best and a loosening never happens).
//   - A non-positive/unset (or NaN) minConfidence means NO confidence gate — the verdict passes through.
//   - Fail-closed (INV-09): a NaN or out-of-[0,1] confidence is treated as BELOW any positive threshold, so a
//     garbage confidence clamps auto→approve rather than being admitted as auto.
func ClampConfidence(verdict Verdict, confidence float64, minConfidence float64) (Verdict, ClampRecord) {
	rec := ClampRecord{
		VerdictIn:     verdict,
		VerdictOut:    verdict,
		Confidence:    confidence,
		MinConfidence: minConfidence,
	}

	// No gate: a non-positive, unset, or NaN threshold is "no confidence gate". `!(minConfidence > 0)` catches
	// 0, negatives, AND NaN (NaN > 0 is false) in one guard.
	if !(minConfidence > 0) {
		rec.Reason = "no confidence gate (min_confidence unset / <= 0)"
		return verdict, rec
	}

	// The gate only applies to `auto`; approve/deny are already at/above the human bar and only ever pass through.
	if verdict != VerdictAuto {
		rec.Reason = fmt.Sprintf("confidence gate does not apply to %q (already at/above the human bar)", verdict)
		return verdict, rec
	}

	// Fail-closed: NaN or out-of-range confidence is treated as below any positive threshold.
	below := math.IsNaN(confidence) || confidence < 0 || confidence > 1 || confidence < minConfidence
	if below {
		rec.VerdictOut = VerdictApprove
		rec.Clamped = true
		rec.Reason = fmt.Sprintf("confidence %s < min_confidence %.4g → clamped auto→approve (route to a human)",
			confStr(confidence), minConfidence)
		return VerdictApprove, rec
	}

	rec.Reason = fmt.Sprintf("confidence %.4g >= min_confidence %.4g → auto retained", confidence, minConfidence)
	return verdict, rec
}

// confStr renders a confidence for the audit reason, naming a NaN/out-of-range value explicitly so the reason
// makes the fail-closed clamp legible rather than printing a bare "NaN".
func confStr(c float64) string {
	switch {
	case math.IsNaN(c):
		return "NaN (out-of-range → fail-closed)"
	case c < 0 || c > 1:
		return fmt.Sprintf("%.4g (out-of-range → fail-closed)", c)
	default:
		return fmt.Sprintf("%.4g", c)
	}
}

// RefineRecord is the NON-SECRET projection of one combined refine (confidence clamp + rate governor), the
// audit trail for step 3 of the decision procedure. It records what was presented (VerdictIn, Confidence),
// what each stage decided (ConfidenceClamped, RateClamped, RateCount), the resolved knobs (MinConfidence,
// RateLimit, RateKey), and the emitted verdict (VerdictOut). Persistence is T-015-12; here it is only returned.
// Every field is a plain verdict/number/bool/string — no credential, argv, or host-secret.
type RefineRecord struct {
	VerdictIn         Verdict // the verdict presented to the refine step.
	VerdictOut        Verdict // the verdict after both clamps (rank >= VerdictIn — only tightens).
	Confidence        float64 // the action's bound confidence.
	MinConfidence     float64 // the resolved min_confidence threshold applied (0/unset ⇒ no gate).
	ConfidenceClamped bool    // true WHEN the confidence gate tightened auto→approve.
	RateLimit         int     // the resolved rate_limit per window applied (0/unset ⇒ no governor).
	RateKey           string  // the governor key the action counted against (op-class or "global").
	RateCount         int     // prior auto executions counted in the trailing window (before this action).
	RateClamped       bool    // true WHEN the rate governor tightened auto→approve.
	Reason            string  // human-readable explanation for the console packet-tracer.
}

// Refine applies step 3 of the decision procedure to a policy Verdict: the confidence clamp (REQ-1507) THEN
// the rate governor (REQ-1508). It is a method on *RateGovernor because the rate half carries the only state;
// the confidence half is pure. Both stages only ever TIGHTEN (auto→approve), so verdictRank(out) >=
// verdictRank(in) always holds — the refine step can never loosen a verdict.
//
// params supplies the ALREADY-RESOLVED knobs (min_confidence, rate_limit) — resolve inheritance from the
// global-default rule via RuleSet.EffectiveParams BEFORE calling Refine (REQ-1507). A nil pointer means the
// knob is unset ⇒ that stage is a no-op (no gate / no governor). opClass is the rate-governor key (empty ⇒
// the "global" key). A nil *RateGovernor is tolerated: it degrades to the confidence clamp alone.
func (g *RateGovernor) Refine(verdict Verdict, confidence float64, params Params, opClass string) (Verdict, RefineRecord) {
	minConf := 0.0
	if params.MinConfidence != nil {
		minConf = *params.MinConfidence
	}
	limit := 0
	if params.RateLimit != nil {
		limit = *params.RateLimit
	}

	rec := RefineRecord{
		VerdictIn:     verdict,
		VerdictOut:    verdict,
		Confidence:    confidence,
		MinConfidence: minConf,
		RateLimit:     limit,
		RateKey:       rateKey(opClass),
	}

	// Stage 1 — confidence clamp (pure, tightens only). A low-confidence auto becomes approve here and so never
	// reaches the rate governor as an auto, which correctly means it does NOT consume a rate-budget slot.
	v, cr := ClampConfidence(verdict, confidence, minConf)
	rec.ConfidenceClamped = cr.Clamped

	// Stage 2 — rate governor (stateful, tightens only). Nil-safe: a nil governor is a no-op governor.
	v2, rr := g.clamp(v, opClass, limit)
	rec.RateClamped = rr.Clamped
	rec.RateCount = rr.CountInWindow
	rec.VerdictOut = v2

	rec.Reason = refineReason(rec)
	return v2, rec
}

// refineReason renders the packet-tracer explanation for a combined refine.
func refineReason(rec RefineRecord) string {
	switch {
	case rec.ConfidenceClamped && rec.RateClamped:
		return fmt.Sprintf("confidence %s < %.4g AND rate cap %d/window met → clamped auto→approve",
			confStr(rec.Confidence), rec.MinConfidence, rec.RateLimit)
	case rec.ConfidenceClamped:
		return fmt.Sprintf("confidence %s < min_confidence %.4g → clamped auto→approve",
			confStr(rec.Confidence), rec.MinConfidence)
	case rec.RateClamped:
		return fmt.Sprintf("rate cap %d/window met (%d prior auto in window) → clamped auto→approve",
			rec.RateLimit, rec.RateCount)
	default:
		return fmt.Sprintf("no clamp: verdict %q retained", rec.VerdictOut)
	}
}
