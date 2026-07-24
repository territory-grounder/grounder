package risk

import (
	"github.com/territory-grounder/grounder/core/safety"
)

// Classify is the deterministic three-band admission gate. Its steps are ordered MOST-RESTRICTIVE
// FIRST so the mechanical floor can never be composed away by a later permissive branch (the
// safety-composition invariant, GOVERNED-BEHAVIORS I1). Every unhandled/error path yields the Band
// zero value — POLL_PAUSE — so the classifier fails closed by construction (REQ-006).
//
// Decision procedure (spec/001 design):
//  1. never-auto floor OR unknown/irreversible mutation class → POLL_PAUSE (REQ-004). No later step lifts this.
//  2. no committed prediction, a deviation verdict, a novel-incident class, OR a high-risk alert category
//     (maintenance/security-incident/deployment) → POLL_PAUSE (REQ-003, REQ-007).
//  3. silent_cognition_guard active and an AUTO-RESOLVE lacks bound evidence → strip AUTO-RESOLVE, poll (REQ-008).
//  4. reversible-mixed on a criticality-tier host, or a wide predicted blast-radius → AUTO_NOTICE + notify (REQ-002).
//  5. low-risk / reversible-and-prediction-eligible, below threshold, non-critical host → AUTO (REQ-001).
func Classify(in GatedInput) Decision {
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

	// Step 2 — no committed prediction, a deviation, or a novel incident → POLL_PAUSE (REQ-003, REQ-007).
	if !in.HasPrediction {
		return poll(d, "no-committed-prediction")
	}
	// A deviation — or ANY verdict the mechanical verifier did not validly produce — is treated as a
	// deviation and never auto-resolves (safety's "an unknown verdict is treated as a deviation" rule).
	if in.HasVerdict && (in.Verdict == safety.VerdictDeviation || !safety.ValidVerdict(in.Verdict)) {
		return poll(d, "verdict-deviation-or-invalid")
	}
	if in.NovelIncident {
		return poll(d, "ood-novel-incident")
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
