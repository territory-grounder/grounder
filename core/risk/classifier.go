package risk

import (
	"github.com/territory-grounder/grounder/core/safety"
	"strconv"
)

// Classify is the deterministic three-band admission gate. Its steps are ordered MOST-RESTRICTIVE
// FIRST so the mechanical floor can never be composed away by a later permissive branch (the
// safety-composition invariant, GOVERNED-BEHAVIORS I1). Every unhandled/error path yields the Band
// zero value — POLL_PAUSE — so the classifier fails closed by construction (REQ-006).
//
// Decision procedure (spec/001 design):
//  1. never-auto floor OR unknown/irreversible mutation class → POLL_PAUSE (REQ-004). No later step lifts this.
//  2. no committed prediction, a recent same-family deviation verdict on the target, a novel-incident class,
//     OR a high-risk alert category (maintenance/security-incident/deployment) → POLL_PAUSE (REQ-003,
//     REQ-007, REQ-015).
//  3. silent_cognition_guard active and an AUTO-RESOLVE lacks bound evidence → strip AUTO-RESOLVE, poll (REQ-008).
//  4. reversible-mixed on a criticality-tier host, or a wide predicted blast-radius → AUTO_NOTICE + notify (REQ-002).
//  5. low-risk / reversible-and-prediction-eligible, below threshold, non-critical host → AUTO (REQ-001).
//  6. THE BAND FLOOR — a declared floor is composed over whatever steps 1-5 computed, in the SAFE
//     DIRECTION ONLY (spec/028 REQ-2809, spec/026 REQ-2611). It may only ever raise the approval bar.
//
// The floor is applied HERE, as a wrapper over the whole procedure, rather than inside any one branch. The
// steps above return early from a dozen places, and a clamp copied into each of them is a clamp that will be
// missed by the thirteenth. Wrapping makes "no path escapes the floor" a property of the control flow instead
// of a property of everyone remembering.
func Classify(in GatedInput) Decision {
	return applyBandFloor(in, classify(in))
}

// applyBandFloor composes a declared band floor over a computed decision, SAFE DIRECTION ONLY.
//
// safety.Band is ordered most-restrictive-first (BandPollPause = 0 < BandAutoNotice < BandAuto), so "raise
// the bar" is the numeric minimum. That ordering is not incidental — it is the same property that makes the
// zero value fail closed — and this clamp is written to depend on it explicitly.
//
// TWO CALLERS, ONE SEAM. The graduation ladder sets an AUTO_NOTICE floor for a class at the auto_notice rung
// (spec/028 REQ-2809: the class acts, but never unobserved); the actor-evidence policy sets a POLL_PAUSE
// floor for authored-action evidence (spec/026 REQ-2611: a human already touched this, so ask). They are the
// same operation at different heights, and composing the band in ONE typed deterministic gate is the point —
// a second place that adjusts bands is a second place they can be adjusted downward.
func applyBandFloor(in GatedInput, d Decision) Decision {
	if !in.BandFloorApplies {
		return d
	}
	if in.BandFloor >= d.Band {
		// The floor is at or below the computed bar — nothing to raise. Notably this is the branch that
		// makes the seam SAFE: a floor can never make a POLL_PAUSE into an AUTO.
		return d
	}
	switch in.BandFloor {
	case safety.BandAutoNotice:
		// The action still happens; the difference is that someone finds out. Everything else about the
		// decision — AutoApproved, AutoResolve — is preserved, because lowering those would silently convert
		// "act and page" into "do not act", which is a different (and unrequested) refusal.
		d.Band = safety.BandAutoNotice
		d.NotifyRequired = true
		if d.Signals != nil {
			d.Signals["band_floor"] = "auto_notice"
		}
		return d
	default:
		// Any other declared floor — in practice POLL_PAUSE — routes through the same fail-closed poll the
		// mechanical steps use, so a floored decision is indistinguishable from one the gate polled itself.
		// `default` rather than an explicit BandPollPause case on purpose: an unrecognised floor value must
		// land on the MOST restrictive outcome, not fall through unclamped.
		return poll(d, "band-floor-"+bandFloorReason(in))
	}
}

// bandFloorReason names WHY a floor was declared, so the audit row says something an operator can act on
// rather than merely "a floor applied". The caller supplies it; an unset reason still records the floor.
func bandFloorReason(in GatedInput) string {
	if in.BandFloorReason == "" {
		return "declared"
	}
	return in.BandFloorReason
}

func classify(in GatedInput) Decision {
	d := Decision{
		RiskLevel:            in.RiskLevel,
		ActionID:             in.ActionID,
		PlanHash:             in.PlanHash,
		Signals:              in.Signals,
		AutoProceedOnTimeout: false, // invariant: a poll never proceeds on timeout
	}

	// Step 0 — a detected prompt-injection / jailbreak in the untrusted input is never an auto-resolvable op.
	// It forces the human circuit-breaker BEFORE any other reasoning (the predecessor's inline screen →
	// HIGH → POLL_PAUSE), because an injected instruction may be steering everything downstream.
	if in.Jailbreak {
		return poll(d, "jailbreak-detected")
	}

	// Step 1 — the inviolable mechanical never-auto floor (REQ-004). Enforced FIRST; non-configurable.
	// An op on the floor, or any action that is not proven reversible (zero value = Irreversible),
	// clamps to POLL_PAUSE. This is the mechanical realization that "unknown ⇒ never-auto".
	if safety.IsNeverAuto(in.OpClass) || in.Reversible == Irreversible {
		return poll(d, "irreversible-or-never-auto-floor")
	}

	// The model does not get to under-declare its own op. A server-side derivation of the ACTUAL command
	// (safety.IsDestructiveOp) overrides the model's stated op_class/reversibility: a proposal claiming
	// "restart-service" whose op is `dropdb prod` is on the floor. Enforces "a plan cannot hide a mutation".
	if in.ServerDestructive {
		return poll(d, "server-derived-destructive-op")
	}

	// A mutating action on a stateful workload (DB / queue / store / statefulset) never auto-resolves even
	// when reversible — a restart/scale during sync or quorum can lose data (SeaweedFS is replication-0).
	// A purely read-only op (fully Reversible) is exempt; anything that modifies clamps to POLL_PAUSE.
	if in.StatefulTarget && in.Reversible != Reversible {
		return poll(d, "stateful-workload-mutation")
	}

	// A restart/reload targeting the platform's OWN control-plane service is never auto-resolved even when
	// reversible: the mission lane runs INSIDE an orchestrated session, so auto-restarting the platform
	// mid-session can orphan the running reconcile. The platform-controller owns those restarts out-of-band;
	// the mission lane defers to a human — a deterministic, non-bypassable veto (the predecessor's
	// _SELF_PROTECTED_RESTART_RE conservative-carve blocker).
	if in.SelfProtectedRestart {
		return poll(d, "self-protected-control-plane-restart")
	}

	// Canary pin (REQ-009) — a deployment-declared canary (host, op) is forced to POLL_PAUSE so the FIRST
	// staged mutations require a human vote (never AUTO). It runs AFTER the inviolable mechanical floors
	// above (which record the more fundamental poll_reason when they also apply) but BEFORE the
	// auto-eligible branches below, so it can raise an otherwise-AUTO action to a poll — never lower one.
	// Inert by default: with no policy declared, in.CanaryPinned is always false and this is a no-op.
	if in.CanaryPinned {
		return poll(d, "canary-policy-pinned")
	}

	// Actor-attribution dispositions (spec/023, REQ-2301/2304/2310) — beside the canary pin, AFTER the
	// mechanical floors (which record the more fundamental reason when they also apply) and BEFORE the
	// auto-eligible branches: they raise an otherwise-AUTO action to a poll, never lower one. A security
	// signal leads (the most severe), then stand-down, then the escalate path. Unattributable sets none
	// of the three (the pre-feature ladder, REQ-2303).
	if in.AttributionSecurity {
		d = poll(d, "actor-attributed-suspicious")
		d.Signals["security_escalation"] = "true"
		return d
	}
	if in.AttributionStandDown {
		return poll(d, "actor-attributed-authorized")
	}
	if in.AttributionEscalate {
		return poll(d, "actor-attribution-escalate")
	}

	// RATIONALE vs ARGV (TG-317, TG-154 §2/T7). The model's stated prose named a host, and it is not the
	// one the sealed action touches. Beside the attribution branches and for the same reason: it raises an
	// otherwise-AUTO action to a poll and never lowers one.
	//
	// It POLLS rather than refuses, deliberately. This is a text heuristic, and a refusal on a heuristic
	// takes the estate offline over a wording change. A poll costs one human glance — and puts the
	// disagreement in front of the one person who reads the rationale, which is exactly where a
	// prose-says-one-thing-argv-does-another attack has to survive contact with a reviewer.
	if in.RationaleHostMismatch {
		d = poll(d, "rationale-names-a-different-host")
		// The detail rides the signals so it reaches the poll notice. Without it the reviewer sees a poll
		// reason and no way to check the claim, and an unadjudicatable poll gets approved on reflex.
		if in.RationaleMismatchDetail != "" {
			d.Signals["rationale_mismatch"] = in.RationaleMismatchDetail
		}
		return d
	}

	// LAST of the "who/what forbids this" reasons, deliberately. Every rule above names a SAFETY property of
	// this specific action (floor, destructive, stateful, self-protected, canary, attribution); this one names
	// a PROCESS state — the class has not yet earned autonomy. Ordering it after them keeps the recorded
	// poll_reason the strongest true one: an operator reading the audit row at 3am must see "a suspicious actor
	// touched this host", not "this verb is new". My first cut had it above them and an oracle caught the
	// demotion — the band was right and the REASON was a misreport, which on the security path is worse.
	// An op-class that has not earned autonomy needs a recorded human approval to execute at all (the policy
	// engine composes `approve`). Poll so that approval can actually be GIVEN: a refusal nobody was asked
	// about is a dead end, not a gate. Only ever raises review; inert when the resolver is unwired.
	if in.UngraduatedClass {
		return poll(d, "op-class-not-graduated")
	}

	// Step 2 — no committed prediction, a deviation, or a novel incident → POLL_PAUSE (REQ-003, REQ-007).
	if !in.HasPrediction {
		return poll(d, "no-committed-prediction")
	}
	// A deviation — or ANY verdict the mechanical verifier did not validly produce — is treated as a
	// deviation and never auto-resolves (safety's "an unknown verdict is treated as a deviation" rule).
	//
	// The verdict here is the target's most recent RELEVANT one (REQ-015): rule-family scoped and
	// recency-bounded, read from the durable actuation ledger. This is TG's own "a deviation can never
	// auto-resolve again" — the graduation ladder's demote-on-deviation — applied one step EARLIER, at
	// classification rather than only at graduation, so the very next same-family remediation on a host TG has
	// just deviated on meets a human instead of the class needing to fail again to be demoted. Absent or
	// unreadable evidence leaves HasVerdict false and this branch inert, so the rule can only RAISE review.
	if in.HasVerdict && (in.Verdict == safety.VerdictDeviation || !safety.ValidVerdict(in.Verdict)) {
		// Bind the reading to the record. poll() guarantees the Signals map, so this runs after it — and the
		// key is recorded ONLY for the decision this rule actually drove (the REQ-014 discipline), never
		// decorating every row, and omitted rather than written blank when absent.
		d = poll(d, "verdict-deviation-or-invalid")
		if in.VerdictKey != "" {
			d.Signals["prior_verdict_key"] = in.VerdictKey
		}
		d.Signals["prior_verdict"] = string(in.Verdict)
		return d
	}
	if in.NovelIncident {
		// Bind the evidence to the record. poll() guarantees the Signals map, so these run after it.
		d = poll(d, "ood-novel-incident")
		if in.NoveltyKey != "" {
			d.Signals["novelty_key"] = in.NoveltyKey
		}
		d.Signals["novelty_count"] = strconv.Itoa(in.NoveltyCount)
		return d
	}

	// A high-risk alert category (maintenance / security-incident / deployment) forces a poll regardless of
	// the plan's mechanical reversibility: a planned change, a containment (ban/shun/isolate), and a release
	// all END in an infra change by definition, so a human acks them even when each individual op reads as
	// reversible. Safe-direction only — this can RAISE review, never lower it (the predecessor's
	// category-high-risk default, restored as a first-class band driver).
	if in.HighRiskCategory {
		return poll(d, "high-risk-category-default")
	}

	// Step 3 — silent_cognition_guard: an AUTO-RESOLVE without bound evidence is stripped and polled
	// (REQ-008, INV-11). A bare marker with no orchestrator-captured, successful, recent, relevant
	// ToolResult is not admissible.
	if in.SilentCognitionGuard && in.AutoResolveMarked && !hasBoundEvidence(in.Evidence) {
		return poll(d, "auto-resolve-evidence-unbound")
	}

	// Step 4 — a criticality-tier host, a reversible-mixed action, or a wide predicted blast-radius →
	// AUTO_NOTICE, proceed with AUTO-RESOLVE, notify the on-call in parallel (REQ-002). A criticality-
	// tier (P0) host is NEVER silently AUTO: REQ-001 admits AUTO only OFF a criticality-tier host, so
	// the most permissive band a P0-host action can reach — even a fully-reversible one — is AUTO_NOTICE.
	if in.CriticalityTier || in.Reversible == ReversibleMixed || in.BlastRadiusWide {
		d.Band = safety.BandAutoNotice
		d.AutoApproved = true
		d.NotifyRequired = true
		d.AutoResolve = true
		return d
	}

	// Step 5 — low-risk / reversible-and-prediction-eligible, below the blast-radius threshold, not on
	// a criticality-tier host (that case was handled in step 4) → AUTO, marked AUTO-RESOLVE (REQ-001).
	d.Band = safety.BandAuto
	d.AutoApproved = true
	d.AutoResolve = true
	return d
}

// poll returns a fail-closed POLL_PAUSE decision: autonomy withheld, no AUTO-RESOLVE, notify the
// approver graph, never proceed on timeout. It records the reason as a signal for the audit row.
func poll(d Decision, reason string) Decision {
	d.Band = safety.BandPollPause
	d.AutoApproved = false
	d.AutoResolve = false
	d.NotifyRequired = true // the approver graph is notified of a pause
	d.AutoProceedOnTimeout = false
	if d.Signals == nil {
		d.Signals = map[string]string{}
	}
	d.Signals["poll_reason"] = reason
	return d
}
