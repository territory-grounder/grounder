package policy

// Band composition — spec/015 task T-015-6 (REQ-1509/1510). This is step 4 of the mode/verdict decision
// procedure (design.md): AFTER the fixed deny-overrides evaluation and the confidence/rate clamps have
// produced a policy Verdict, the engine COMPOSES that verdict with the action's own spec/001 risk band.
//
// Two composition modes, selected per rule by band_mode (carried on Params.BandMode, validated in T-015-2):
//
//   - respect (the default, and the "" inherit case): emit the MORE-RESTRICTIVE of {the policy verdict, the
//     risk band mapped to a verdict}. A POLL_PAUSE risk band therefore VETOES a permissive `auto` policy
//     verdict (REQ-1509) — the safety classifier can only ever RAISE scrutiny above the policy, never lower
//     it.
//   - force: apply the POLICY verdict even when it is LESS restrictive than the risk band, and stamp a
//     double-confirmation warning on the record (REQ-1510, design.md: "force returns the policy verdict and
//     stamps the double-warn flag"). This is the operator's explicit, audited "warn, don't block" choice.
//
// The load-bearing safety property (INV-09): NEITHER mode can lift the constitutional mechanical never-auto
// floor. That floor lives BENEATH this engine (core/safety.IsNeverAuto / IsDestructiveOp / !Reversible, and
// design.md step 0) and is enforced in defense-in-depth at the classifier and the actuation adapter
// regardless of any policy. This function additionally clamps a force→auto composition back to `approve`
// WHENEVER the floor applies and records the clamp, so that even an operator's force override can never make
// the engine emit `auto` for a floor-class action. core/safety is composed over READ-ONLY here — this file
// adds no floor, mutates no safety state, and holds zero diff against core/safety.
//
// Restrictiveness ordering used (most-restrictive first): deny > approve (human vote / checkpoint) > auto.
// The safety Band maps onto a verdict FLOOR — the least-restrictive verdict the band still permits: BandAuto
// and BandAutoNotice permit unattended autonomous execution → `auto`; BandPollPause (which requires a human)
// → `approve`; any invalid/zero band → `approve` (fail closed; the zero Band is BandPollPause by design). A
// risk band never maps to `deny`: `deny` is a policy-only REFUSAL, whereas the strictest stance the safety
// band takes is "pause for a human" (approve). NOTE (spec deviation): the task sketch left BandAutoNotice's
// mapping open; this follows core/safety's documented band semantics (AutoNotice = "act autonomously but
// notify in parallel", i.e. it permits autonomy) → `auto`, so only POLL_PAUSE forces the checkpoint. The
// bound acceptance oracle (force over an AUTO_NOTICE band) passes because force applies the policy verdict
// and stamps the double-warn either way; the genuine loosening-override (Overridden=true) is proven by the
// force-under-POLL_PAUSE unit test.
//
// Provenance: [O] INV-09 (the floor is inviolable) · [R] paradigm-rule 8 (graded, warn-don't-block).
// See spec/015-policy-engine REQ-1509/1510.

import (
	"fmt"

	"github.com/territory-grounder/grounder/core/safety"
)

// verdictRank is the restrictiveness rank of a verdict — higher is MORE restrictive. An unknown/invalid
// verdict ranks as `deny` (the most restrictive) so an unmapped verdict fails closed to the safest path.
func verdictRank(v Verdict) int {
	switch v {
	case VerdictAuto:
		return 0
	case VerdictApprove:
		return 1
	case VerdictDeny:
		return 2
	default:
		return 2 // unmapped → most restrictive (fail closed, INV-09).
	}
}

// bandFloorVerdict maps a spec/001 safety risk band onto the LEAST-restrictive verdict the band still
// permits. BandAuto / BandAutoNotice permit unattended autonomous execution (`auto`); BandPollPause requires
// a human (`approve`); any invalid value falls through to `approve` (fail closed — the zero Band is
// BandPollPause). A band never maps to `deny`: refusal is a policy-only verdict, not a safety-band stance.
func bandFloorVerdict(b safety.Band) Verdict {
	switch b {
	case safety.BandAuto, safety.BandAutoNotice:
		return VerdictAuto
	default:
		return VerdictApprove // BandPollPause and any invalid/zero value → require a human (fail closed).
	}
}

// moreRestrictive returns whichever of the two verdicts is more restrictive (higher rank). On an equal rank
// it returns a (the policy verdict), which is harmless because equal rank means the same restrictiveness.
func moreRestrictive(a, b Verdict) Verdict {
	if verdictRank(a) >= verdictRank(b) {
		return a
	}
	return b
}

// ComposeRecord is the NON-SECRET projection of one band-composition, produced as a required output so the
// composition is fully auditable (this feeds the policy_decision audit row in T-015-12; here it is only
// returned, never persisted). Every field is a plain enum/bool/string — there is no credential, no argv, and
// no host-secret in it. It records what the engine saw (PolicyVerdict, SafetyBand, BandVerdict, BandMode),
// what it emitted (Composed), and WHY a force override or a floor clamp changed the outcome (Overridden /
// OverriddenBand / FloorClamped / DoubleWarn) so the double-audit obligation of REQ-1510 is discharged.
type ComposeRecord struct {
	PolicyVerdict  Verdict     // the verdict the policy engine resolved (pre-composition).
	SafetyBand     safety.Band // the action's own spec/001 risk band.
	BandVerdict    Verdict     // SafetyBand mapped onto its verdict floor (bandFloorVerdict).
	BandMode       BandMode    // the RESOLVED composition mode actually applied (respect / force).
	Composed       Verdict     // the emitted composed verdict.
	Overridden     bool        // true WHEN force applied a verdict LESS restrictive than the band's floor.
	OverriddenBand safety.Band // the band force overrode (meaningful only WHEN Overridden is true).
	FloorClamped   bool        // true WHEN a force→auto result was clamped back to approve by the floor.
	DoubleWarn     bool        // true WHENEVER force was applied — the double-confirmation warning (REQ-1510).
	Reason         string      // human-readable explanation for the console packet-tracer.
}

// ComposeBand composes a policy Verdict with an action's spec/001 safety risk Band under the rule's band_mode
// and returns the emitted Verdict plus a NON-SECRET ComposeRecord (REQ-1509/1510). It is a PURE function: no
// I/O, no mutation of any safety state, deterministic for identical inputs.
//
// neverAuto is the constitutional mechanical-floor signal for this action (design.md step 0):
// safety.IsNeverAuto(op) || !reversible || safety.IsDestructiveOp(op, op_class), computed by the caller from
// core/safety (see NeverAutoApplies). WHEN it is true, no composition may emit `auto` — even under force —
// so a force→auto is clamped back to `approve` and the clamp is recorded (INV-09). The floor sits beneath
// this engine; this clamp is defense-in-depth, not the sole enforcement.
//
// Fail-closed determinism: an unset ("" inherit) OR any invalid band_mode resolves to respect (the safer
// path); an unmapped policy verdict is treated as `deny` (the most restrictive) via verdictRank.
func ComposeBand(policyVerdict Verdict, safetyBand safety.Band, mode BandMode, neverAuto bool) (Verdict, ComposeRecord) {
	// Resolve the mode fail-closed: ONLY an explicit `force` engages the override; "" (inherit), respect, and
	// any unknown value all resolve to respect (the more-restrictive, safer composition).
	resolved := BandRespect
	if mode == BandForce {
		resolved = BandForce
	}

	bandV := bandFloorVerdict(safetyBand)
	rec := ComposeRecord{
		PolicyVerdict: policyVerdict,
		SafetyBand:    safetyBand,
		BandVerdict:   bandV,
		BandMode:      resolved,
	}

	// Normalize an unmapped/invalid policy verdict to the most restrictive (deny) before composing.
	pv := policyVerdict
	if !validVerdict(pv) {
		pv = VerdictDeny
	}

	var result Verdict
	if resolved == BandForce {
		// force: apply the POLICY verdict even when it is less restrictive than the band; always stamp the
		// double-confirmation warning (REQ-1510).
		result = pv
		rec.DoubleWarn = true
		if verdictRank(pv) < verdictRank(bandV) {
			// The force override actually LOOSENED the outcome below what the band would have allowed — the
			// operator's explicit, audited choice. Record the band it overrode (the double-audit).
			rec.Overridden = true
			rec.OverriddenBand = safetyBand
		}
	} else {
		// respect: emit the more-restrictive of {policy verdict, band floor}. The band can only raise
		// scrutiny (REQ-1509); it never loosens the policy verdict.
		result = moreRestrictive(pv, bandV)
	}

	// INV-09 — the constitutional never-auto floor is inviolable and sits BENEATH the engine: no policy, mode,
	// template, or force override may make a floor-class action resolve to `auto`. Clamp any `auto` result
	// back to `approve` when the floor applies, and record it. This runs for BOTH modes as defense-in-depth
	// (under respect an `auto` result requires both policy=auto AND band permits auto; under force it can
	// arise from the override) — the floor never yields, regardless of band_mode.
	if neverAuto && result == VerdictAuto {
		result = VerdictApprove
		rec.FloorClamped = true
	}

	rec.Composed = result
	rec.Reason = composeReason(rec)
	return result, rec
}

// composeReason renders the human-readable packet-tracer explanation for a ComposeRecord.
func composeReason(rec ComposeRecord) string {
	switch {
	case rec.FloorClamped:
		return fmt.Sprintf("force band_mode applied %q but the constitutional never-auto floor (INV-09) clamped it to %q — the floor is inviolable beneath the engine",
			rec.PolicyVerdict, rec.Composed)
	case rec.Overridden:
		return fmt.Sprintf("force band_mode applied the policy verdict %q over the more-restrictive %s risk band (double-confirmation warning recorded, REQ-1510)",
			rec.Composed, rec.SafetyBand)
	case rec.BandMode == BandForce:
		return fmt.Sprintf("force band_mode applied the policy verdict %q (band %s did not restrict it; double-confirmation warning recorded)",
			rec.Composed, rec.SafetyBand)
	default:
		return fmt.Sprintf("respect band_mode emitted the more-restrictive of policy %q and %s risk band → %q",
			rec.PolicyVerdict, rec.SafetyBand, rec.Composed)
	}
}

// NeverAutoApplies reports whether the constitutional mechanical never-auto floor (INV-09, design.md step 0)
// applies to an action, composing READ-ONLY over core/safety: an op on the never-auto class floor, an
// irreversible action, or a destructive op (independent of the model-declared op-class) can never resolve to
// `auto`. Callers pass the result to ComposeBand as its neverAuto argument. It is provided here so band.go is
// the single place band composition consults the floor; it adds nothing to core/safety and mutates nothing.
func NeverAutoApplies(in EvalInput) bool {
	return safety.IsNeverAuto(in.OpClass) || !in.Reversible || safety.IsDestructiveOp(in.Argv, in.OpClass)
}
