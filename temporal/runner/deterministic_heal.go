package runner

// TG-496 fix (c) — the DETERMINISTIC guest-down auto-heal fast-path.
//
// THE GAP. A confirmed guest-down (the pve-liveness detector observes a managed Proxmox guest transition
// running→stopped) used to classify STANDARD_AGENT and run the FULL agent loop to reach a start-guest
// proposal. After the 2026-08-08 brain swap (opus-cc → Mistral, TG-293) that loop's proposal PROPENSITY
// collapsed: two live drills, two no-proposals — the model no longer grounded a heal it once grounded
// routinely (the July fault-campaign week healed these exact guests). Detection stayed excellent (~36s); the
// DECISION half went cold.
//
// THE FIX. For the ONE unambiguous, reversible, confirmed case — a pve-liveness Device-Down whose guest is
// observed STOPPED — wire TG-42's unwired KnownProcedure/Reversible classifier signals so the incident
// routes to a DETERMINISTIC start-guest proposal synthesized WITHOUT the agent loop. It restores auto-heal
// under ANY brain because it does not depend on the model grounding a diagnosis.
//
// WHY IT IS SOUND, NOT RECKLESS (every existing safety control is preserved — this only adds a classification
// signal and a proposal-emission branch; it changes NOTHING downstream of the proposal):
//   - LOAD-BEARING CONFIRMED-STOPPED PRECONDITION, enforced at THREE fail-closed points: (1) classification
//     (confirmedGuestDownHeal, below — gates whether to PROPOSE); (2) emission (DeterministicGuestHealActivity
//     re-confirms — a guest that recovered in between declines back to the normal loop, never a blind start);
//     (3) the existing TG-378 seal gate (core/predict.checkStatePrecondition: start-guest declares
//     requires_target_state=not-running, re-read at COMMIT — unknown/running refuses the seal). A flap never
//     auto-proposes a restart.
//   - MODE CHOKEPOINT unchanged: the proposal traverses the same chain; Shadow ⇒ no actuate.
//   - COMMIT-CONFIRMED ARMING unchanged: start-guest is commit-confirmed eligible (opschema, 600s window), so
//     the armed stop-guest revert is recorded BEFORE the effect and the forward refuses if unarmable.
//   - !CRITICAL guard: a criticality-tier (P0) guest never takes the fast path (confirmedGuestDownHeal checks
//     the operator-declared set), mirroring execclass.Classify's own guard.
//   - HONEST RECORD: the disposition is labelled execclass:deterministic-heal — model/decision tier
//     "deterministic-heal", provenance in SkillLoads — never a fabricated agent investigation.

import (
	"context"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/proposal"
	pveliveness "github.com/territory-grounder/grounder/modules/ingest/pveliveness"
)

const (
	// deterministicHealProvenance labels every fast-path disposition (the synthesized proposal's SkillLoads
	// and — via the workflow — the terminal record), so a deterministic guest-down heal is legible as such
	// and can never be read as a grounded agent investigation (the honest-record invariant, TG-496 fix c).
	deterministicHealProvenance = "execclass:deterministic-heal"
	// deterministicHealTier is the honest model/decision tier stamp: NO model decided this proposal. Kept
	// distinct from any real LLM tier so a scorecard cannot mistake a deterministic heal for a model call.
	deterministicHealTier = "deterministic-heal"
	// deterministicHealStartGuest is the op-class the fast-path emits. A const, not a literal scattered at the
	// emit + oracle sites, so the two cannot drift (the opschema builder is the compiled authority).
	deterministicHealStartGuest = "start-guest"
	// outcomeDeterministicHealDeclined is the terminal a declined heal carries (the confirmed-stopped
	// precondition did not re-hold at emission). It is discarded when the workflow falls back to the loop —
	// present only so a decline is legible if ever recorded.
	outcomeDeterministicHealDeclined = "deterministic-heal-declined"
)

// confirmedGuestDownHeal decides TG-496 fix (c) eligibility from the correlation stage: is this a pve-liveness
// Device-Down whose guest is CONFIRMED observed-stopped, isolated (not a correlated cascade), and not on the
// criticality tier? EVERY arm fails CLOSED — a wrong "yes" would auto-propose an actuation, so the default is
// always the safe, normal agent path:
//
//   - a CORRELATED incident is a cascade member; it takes the deep path (and execclass.Classify's Correlated
//     short-circuit outranks KnownProcedure anyway), never a fast heal;
//   - only the pve-liveness SOURCE + its Device-Down rule qualify — a slow LibreNMS device-down under the same
//     rule name (an SNMP/ICMP blip, not necessarily a stopped guest) does not;
//   - a CRITICALITY-TIER (P0) guest is never silently fast-healed (the !critical guard, from the
//     operator-declared config-not-code set);
//   - THE LOAD-BEARING PRECONDITION: the guest must be OBSERVED stopped in the guest_liveness projection the
//     TG-378 seal gate itself reads (a.D.Gate.GuestRunning). An unwired reader, a never-observed / stale /
//     paused guest (ok=false), or a RUNNING guest (a flap that already came back) all yield "no fast path".
func (a *Activities) confirmedGuestDownHeal(ctx context.Context, in CorrelateInput, correlated bool) bool {
	if correlated {
		return false // a cascade member takes the deep path, never a deterministic single-guest heal
	}
	if in.SourceID != pveliveness.SourceType || in.AlertRule != pveliveness.DeviceDownRule {
		return false // scoped to the TG-native, edge-triggered running→stopped liveness detector only
	}
	if in.Host == "" {
		return false // no guest to heal
	}
	// The !critical guard, established from the operator-declared criticality set (never a hostname in code).
	// nil ⇒ nothing is P0 (the prod default when no tier is declared) — matching the classifier's own guard.
	if a.D.CriticalityTier != nil && a.D.CriticalityTier(in.Host) {
		return false
	}
	// THE LOAD-BEARING PRECONDITION. Reuse the SAME guest-liveness reader the TG-378 seal gate enforces, so
	// the fast-path can never propose a heal the seal gate would refuse. Fail closed on an unwired reader.
	if a.D.Gate == nil || a.D.Gate.GuestRunning == nil {
		return false
	}
	running, _, ok := a.D.Gate.GuestRunning(ctx, in.Host)
	return ok && !running // confirmed stopped: observed, fresh, and not running
}

// DeterministicGuestHealActivity synthesizes the start-guest PROPOSAL for a confirmed guest-down (TG-496 fix
// c), bypassing the agent loop — ZERO model calls. It is dispatched by RunnerWorkflow only when
// CorrelateActivity set cor.FastHeal (the confirmed-stopped, isolated, non-critical pve-liveness case).
//
// It RE-CONFIRMS the load-bearing precondition at emission time (defence in depth: the guest may have
// recovered between correlation and here). If the guest is no longer confirmed-stopped it DECLINES
// (Proposed=false) and the workflow routes the incident back to the normal agent loop — the safe direction,
// never a blind start. On the confirmed path it returns a proposal that flows UNCHANGED through the same
// shadow-divert / classify / gate / commit-confirm arm / mode-chokepoint chain every proposal does:
//   - the mode chokepoint still gates actuation (Shadow ⇒ none);
//   - start-guest is commit-confirmed eligible, so the armed stop-guest revert is recorded BEFORE the effect;
//   - the TG-378 seal gate re-reads guest liveness at commit (requires_target_state=not-running) and refuses
//     to seal a start for a guest observed running or unknown.
//
// OpClassRegistered is resolved HERE (opschema.Lookup) so the decision is replay-deterministic and day-zero
// correct: under the empty-catalog profile start-guest is unregistered ⇒ OpClassRegistered=false ⇒ the
// workflow's shadow-divert branch records it as an adviser-only shadow proposal that seals and executes nothing.
func (a *Activities) DeterministicGuestHealActivity(ctx context.Context, env ingest.IncidentEnvelope) (InvestigateResult, error) {
	guest := env.Host
	// RE-CONFIRM the precondition (fail closed to a decline → normal loop; never a blind start).
	if a.D.Gate == nil || a.D.Gate.GuestRunning == nil {
		return declineHeal("deterministic heal declined: no guest-state reader wired"), nil
	}
	running, prov, ok := a.D.Gate.GuestRunning(ctx, guest)
	if !ok || running {
		return declineHeal("deterministic heal declined: guest " + guest + " not confirmed stopped at emission (" + prov + ")"), nil
	}
	// TG-498: the observation above IS this proposal's grounding — CAPTURE it as evidence. The execute-side
	// evidence gate (interceptor gate 4, INV-11) refuses any mutating action whose EvidenceIDs bind to no
	// orchestrator-captured tool-result, and the deterministic lane synthesized its proposal with none — so
	// every deterministic heal refused at execute with "evidence unbound" and the armed commit-confirm window
	// correctly aborted (the first live traversal, 2026-08-15 03:57Z, diagnosed on TG-498). The observation is
	// orchestrator-made (this activity, zero model tokens — a STRONGER provenance than agent tool output) and
	// already the emission precondition; recording it as a ToolResult and citing it makes the evidence gate's
	// question answerable instead of bypassed.
	evidence := agent.ToolResult{
		ID:      "det-liveness-" + env.ExternalRef,
		Target:  guest,
		Output:  "guest " + guest + " confirmed NOT RUNNING via the guest_liveness projection (" + prov + ") at deterministic-heal emission — the confirmed-stopped precondition observation",
		Success: true,
	}

	action := manifest.Action{
		Target:     guest,
		OpClass:    deterministicHealStartGuest,
		Op:         "start",
		Params:     map[string]string{opschema.ParamGuest: guest},
		Reversible: true,
	}
	// Resolve the registry membership in the activity (replay determinism + day-zero correctness), exactly as
	// InvestigateActivity does for a model proposal.
	_, registered := opschema.Lookup(action.OpClass)

	p := proposal.Proposal{
		ExternalRef: env.ExternalRef,
		Action:      action,
		// The captured observation above — the evidence gate binds these ids against ToolResults (INV-11).
		EvidenceIDs: []string{evidence.ID},
		// Confidence 1.0 is deterministic certainty. NB (TG-408/REQ-1507): inv.Confidence is NO LONGER
		// observability-only — workflow.go threads it into actuate.Request, where the policy min_confidence
		// clamp reads it (core/policy/confidence.go). 1.0 is deliberate and correct here: a confirmed
		// observed-stopped guest is genuinely unambiguous (unlike an LLM self-report), so it is never clamped
		// auto→approve. INV-08 still holds — this value re-opens no directive grammar; it only feeds the clamp.
		Confidence: 1,
		Rationale: "deterministic guest-down heal (" + deterministicHealProvenance + "): guest " + guest +
			" observed STOPPED (" + prov + ") → start-guest (reversible; stop-guest is its armed inverse). " +
			"The agent-loop diagnosis grounding is bypassed for this one unambiguous, confirmed, reversible case; " +
			"the proposal still traverses the mode chokepoint, commit-confirmed arming, and the TG-378 observed-not-running seal gate.",
		Diagnosis: proposal.Diagnosis{
			RootCause: "guest " + guest + " observed stopped (pve-liveness Device-Down, confirmed via the guest_liveness projection)",
			Mechanism: "a managed Proxmox guest transitioned running→stopped; the registered, bounded, reversible remediation is start-guest",
		},
	}
	return InvestigateResult{
		Proposed:          true,
		Proposal:          p,
		Outcome:           agent.OutcomeProposed.String(),
		OpClassRegistered: registered,
		Confidence:        1,
		// The captured grounding observation (TG-498): workflow.go threads ToolResults + EvidenceIDs into
		// ExecuteInput generically, so the execute-side evidence gate binds exactly as for an agent proposal.
		ToolResults: []agent.ToolResult{evidence},
		// Honest provenance: no seed was composed and no model decided. SkillLoads carries the deterministic-
		// heal marker (not an agent seed); the tiers name "deterministic-heal", never an LLM tier.
		SkillLoads:   []string{deterministicHealProvenance},
		ModelTier:    deterministicHealTier,
		DecisionTier: deterministicHealTier,
		Reason:       "deterministic guest-down heal",
	}, nil
}

// declineHeal is the fail-closed non-proposal a deterministic heal returns when the confirmed-stopped
// precondition does not (re-)hold at emission time. Proposed=false makes the workflow fall back to the normal
// agent loop (downgrading the class to STANDARD_AGENT) — the safe direction, never a blind start.
func declineHeal(reason string) InvestigateResult {
	return InvestigateResult{
		Proposed:     false,
		Outcome:      outcomeDeterministicHealDeclined,
		Conclusion:   reason,
		Reason:       reason,
		SkillLoads:   []string{deterministicHealProvenance},
		ModelTier:    deterministicHealTier,
		DecisionTier: deterministicHealTier,
	}
}
