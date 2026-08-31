package runner

// RollbackWorkflow is the operator-facing MANUAL ROLLBACK lane (TG-462): an operator triggers the INVERSE of a
// previously-EXECUTED forward action, and the inverse traverses the SAME governed actuation chain every forward
// mutation does — admission → never-auto floor → structure → evidence → territory → verifiability → policy →
// breaker → MODE CHOKEPOINT → execute → verify. It is not a bypass and it is not a shortcut: it seals a fresh,
// content-hashed inverse ActionManifest, binds the FORWARD execution record as its captured evidence, classifies
// the inverse at the fail-safe POLL_PAUSE band (a human must approve), and only then hands the sealed inverse to
// the interceptor with actuate.Request.InvertsActionID set to the forward action id (the TG-404 machinery).
//
// Because the inverse rides the mode chokepoint like any other mutation, it is INERT under Shadow/HITL (the
// default, live posture): the chain refuses at the mode chokepoint long before any effect, so delivering this
// lane adds no actuation capability the operator has not already, deliberately, escalated the mode into.
//
// WHY A DEDICATED EXECUTE STEP, not a reuse of ExecuteActivity. The forward execute derives its argv from the
// sealed op-class via sealEffect → spec.Argv (the FORWARD argv). A rollback must run spec.RollbackArgv — for a
// class whose forward is not its own inverse (start-service → `systemctl stop`, start-container → `docker stop`)
// re-running the forward argv would undo NOTHING while the ledger recorded a rollback. So the rollback derives
// its own compensating argv from the SAME registry and hands it to the SAME interceptor, never touching
// activities.go (the forward execute path) or any core/ decision surface.
//
// Provenance: [O] INV-07 (sealed content-hashed inverse), INV-09 (mutation off — inert under Shadow), INV-11
// (the forward execution record is the bound, captured evidence), INV-12 (a human approval binds the release),
// TG-404 (InvertsActionID durable inverse record), TG-462 (this lane), TG-464 (armed: the effect-presence
// necessity probe here + the rollback-shape execution path on the ssh leaf). [R] paradigm-rules 4/8.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/territory"
	"github.com/territory-grounder/grounder/core/verify"
	tg "github.com/territory-grounder/grounder/temporal"
)

// RollbackInput is the operator's manual-rollback order. It carries the FORWARD action's already-resolved,
// non-secret identity — loaded by the endpoint seam from the sealed forward manifest + the forward
// action_execution record — so the workflow re-derives the inverse and re-validates reversibility from
// AUTHORITATIVE data rather than trusting the request body. RollbackExternalRef is the NEW incident key the
// rollback session is filed under (distinct from the forward's), so the inverse's audit and vote bind to THIS
// operator request; ForwardExternalRef scopes the idempotency read to the forward incident.
type RollbackInput struct {
	ForwardActionID     string            // the previously-EXECUTED forward action being undone (InvertsActionID target)
	ForwardOpClass      string            // the forward op-class — the registry key the inverse derives from
	ForwardOp           string            // the forward op hint (for a legible, DISTINCT inverse action identity)
	ForwardTarget       string            // the host/resource the forward action touched (the inverse's target)
	ForwardParams       map[string]string // the forward's structured params (the inverse's compensating argv slots)
	ForwardReversible   bool              // whether the forward action was sealed reversible (a fail-closed precondition)
	ForwardSite         string            // the forward action's site (scopes the post-state observer)
	ForwardExternalRef  string            // the forward incident (idempotency + evidence attribution)
	RollbackExternalRef string            // the NEW incident key this rollback session is filed under
	Operator            string            // the SERVER-authenticated admin operator who requested the rollback
}

// RollbackResult is the terminal state of a manual-rollback run.
type RollbackResult struct {
	ForwardActionID string
	InverseActionID string // the sealed inverse's own content-hashed id (distinct from the forward it undoes)
	Band            string // always POLL_PAUSE — a manual rollback is human-approved by construction
	Vote            string // "approved" | "denied" | "timeout" — the operator's release decision
	Executed        bool   // MUST be false under Shadow/HITL (the chain refuses at the mode chokepoint)
	Outcome         string
	Reason          string
}

// RollbackVoteWait bounds how long the rollback poll waits for the approving vote before standing down (an
// unanswered rollback poll is DENIED — never a silent approval, INV-12). Distinct name from the Runner's
// VoteWait so the two lanes' timers read independently in history.
const RollbackVoteWait = 24 * time.Hour

// rollbackArgvFor is the REVERSIBILITY GATE + the compensating-argv derivation, in one pure, deterministic
// place (safe in workflow code and unit-testable without Temporal). It fails CLOSED in every direction:
//
//   - an op-class that is NOT cleanly reversible (tier ≠ low-reversible: medium / irreversible / vendor-critical)
//     is REFUSED. This is the "reversible-only" invariant — a rollback of a destructive/irreversible op has no
//     safe inverse (re-running the forward would re-destroy), so no accrual of approvals can lift it. It is the
//     same safe-direction floor the never-auto floor takes for the forward.
//   - a forward action that was not sealed reversible is REFUSED (the seal is the authority, not the tier alone).
//   - an op-class with no rollback argv (unregistered, or a required param missing/blank) is REFUSED.
//
// The tier check comes BEFORE the argv build on purpose: for an irreversible class RollbackArgv() falls back to a
// RE-RUN of the forward (INV-07's "a bound rollback, not a perfect one"), which for a prune/vacuum/delete would
// re-perform the destruction. Refusing on the tier first is what stops that. Neutralizing this check is the
// killing mutation the rollback test drives RED.
func rollbackArgvFor(spec opschema.OpClassSpec, forwardReversible bool, params map[string]string) ([]string, error) {
	if !forwardReversible {
		return nil, fmt.Errorf("rollback refused: forward action for op-class %q was not sealed reversible", spec.OpClass)
	}
	if spec.SafetyTier != opschema.TierLowReversible {
		return nil, fmt.Errorf("rollback refused: op-class %q is tier %q, not %q — a manual rollback is permitted "+
			"only for cleanly-reversible op-classes (medium / irreversible / vendor-critical have no safe inverse)",
			spec.OpClass, spec.SafetyTier, opschema.TierLowReversible)
	}
	// A low-reversible class is only SAFELY rollback-able if re-deriving its compensating argv actually UNDOES the
	// forward. Two ways that holds: the class DECLARES an explicit rollback_template (start-service → systemctl
	// stop), OR its op is a genuine idempotent-reconvergence verb (restart/reload) where re-running the forward
	// returns the target to the same good state. A low-reversible class with NEITHER — e.g. start-guest, op=start,
	// no declared rollback_template — falls through opschema.RollbackArgv to a RE-RUN of the FORWARD (`start` on an
	// already-started guest): a silent NO-OP reported as a rollback (the worst-case bug). Refuse it as not-safely-
	// reversible (fail closed), the same typed refusal an irreversible class takes → the endpoint returns 400.
	// Detected from the DECLARED template presence directly (len(spec.RollbackTemplate)), never inferred from
	// RollbackArgv==Argv (which is fragile).
	op := strings.ToLower(strings.TrimSpace(spec.Op))
	if len(spec.RollbackTemplate) == 0 && op != "restart" && op != "reload" {
		return nil, fmt.Errorf("rollback refused: op-class %q (tier %q, op %q) declares no rollback_template and its op "+
			"is not an idempotent-reconvergence verb (restart/reload) — re-deriving the compensating argv would RE-RUN "+
			"the forward (undoing nothing while reporting a rollback), so there is no safe inverse", spec.OpClass, spec.SafetyTier, spec.Op)
	}
	argv, err := spec.RollbackArgv(params)
	if err != nil {
		return nil, fmt.Errorf("rollback refused: no compensating argv for op-class %q: %w", spec.OpClass, err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("rollback refused: op-class %q produced an empty compensating argv (fail closed)", spec.OpClass)
	}
	return argv, nil
}

// ValidateRollback is the exported, pure reversibility authority the ENDPOINT SEAM calls for its synchronous
// pre-check (so an irreversible/unknown target is a fast 400/404 before any workflow starts) — the SAME
// rollbackArgvFor gate the seal activity re-runs as the authority. It returns the inverse's deterministic
// content-addressed action_id (so the endpoint can report it) or a fail-closed refusal. No I/O.
func ValidateRollback(in RollbackInput) (inverseActionID string, err error) {
	spec, ok := opschema.Lookup(in.ForwardOpClass)
	if !ok {
		return "", fmt.Errorf("rollback refused: op-class %q is not registered — no execution path", in.ForwardOpClass)
	}
	if _, err := rollbackArgvFor(spec, in.ForwardReversible, in.ForwardParams); err != nil {
		return "", err
	}
	return inverseActionFor(in).ID()
}

// inverseActionFor builds the sealed inverse's Action from the forward's identity. The inverse carries the SAME
// registry op-class, target and params (so the territory gate, the never-auto floor, and the compensating-argv
// derivation all read the SAME authoritative facts the forward was sealed under) but a DISTINCT op hint, so it
// gets its OWN content-addressed action_id — an inverse is its own execution, and InvertsActionID (a request-level
// REFERENCE, not the identity) names what it undoes. Reversible is true: the inverse of a cleanly-reversible
// action is itself cleanly reversible.
func inverseActionFor(in RollbackInput) manifest.Action {
	return manifest.Action{
		Target:     in.ForwardTarget,
		OpClass:    in.ForwardOpClass,
		Op:         "rollback:" + strings.TrimSpace(in.ForwardOp),
		Params:     in.ForwardParams,
		Reversible: true,
	}
}

// RollbackWorkflow drives seal → notify → human-approval wait → gated execute. The workflow body is CONTROL FLOW
// ONLY; every side effect is an activity, and the only activity that can reach an effect leaf
// (SealRollbackExecuteActivity) is pinned to the actuation queue and traverses the interceptor chain (so nothing
// here bypasses the chokepoint).
func RollbackWorkflow(ctx workflow.Context, in RollbackInput) (RollbackResult, error) {
	ctx = workflow.WithActivityOptions(ctx, runnerActivityOptions())
	var a *Activities // nil receiver — activity-name resolution only

	res := RollbackResult{
		ForwardActionID: in.ForwardActionID,
		Band:            safety.BandPollPause.String(),
		Outcome:         "rollback:stop",
	}

	// 1) SEAL the inverse manifest (re-validates reversibility, the authority; the endpoint's synchronous
	//    pre-check is UX only). A seal failure is a fail-closed refusal — nothing to approve, nothing to execute.
	var seal SealRollbackResult
	if err := workflow.ExecuteActivity(ctx, a.SealRollbackActivity, in).Get(ctx, &seal); err != nil {
		return res, err
	}
	if !seal.Sealed {
		res.Outcome = "rollback:refused-seal"
		res.Reason = seal.Reason
		return res, nil
	}
	res.InverseActionID = seal.InverseActionID

	// 2) NOTIFY on-call: a manual rollback is POLL_PAUSE, so it SOLICITS an approval vote (never a silent
	//    actuation). Best-effort/fail-open like the Runner's notify — a notifier outage never fails the session.
	ni := NotifyInput{
		DecisionID: in.RollbackExternalRef,
		Body: fmt.Sprintf("[POLL_PAUSE] MANUAL ROLLBACK requested by %s — undo %s %s on %s (inverse action %s). ref=%s",
			in.Operator, in.ForwardOp, in.ForwardOpClass, in.ForwardTarget, short12(seal.InverseActionID), in.RollbackExternalRef),
		Approval: true,
		Choices:  approvalChoices(in.RollbackExternalRef, []string{fmt.Sprintf("rollback %s on %s", in.ForwardOpClass, in.ForwardTarget)}),
	}
	var notified NotifyResult
	_ = workflow.ExecuteActivity(ctx, a.NotifyActivity, ni).Get(ctx, &notified)

	// 3) WAIT for the human approval that names THIS sealed inverse action (INV-12). Approve → release exactly
	//    this inverse; deny/timeout → stand down (no execute). Every terminal decision is recorded on the
	//    tamper-evident ledger. The approver-admission control (spec/015 REQ-1516) admits a vote only when the
	//    voter is a member of the inverse's approve_by set AND the bundle declares an approver regime — the SAME
	//    fail-closed rule the Runner enforces (VoterAdmitted), never re-decided here.
	approved, vote := rollbackVoteWait(ctx, a, in, seal)
	res.Vote = vote
	if !approved {
		res.Outcome = "rollback:" + vote
		res.Reason = "manual rollback not approved (" + vote + ") — no actuation"
		return res, nil
	}

	// 4) EXECUTE the sealed inverse through the FULL interceptor chain, with InvertsActionID set. Pinned to the
	//    actuation queue (the only rollback activity that can reach an effect leaf). One attempt: a mutation is
	//    never auto-retried under one approval (Temporal activities are at-least-once). Under Shadow/HITL the
	//    interceptor refuses at the mode chokepoint and Executed stays false — the lane is inert until an operator
	//    has escalated the mode, exactly like the forward lane.
	execCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           tg.TaskQueueActuate,
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var exec ExecuteResult
	if err := workflow.ExecuteActivity(execCtx, a.SealRollbackExecuteActivity, RollbackExecuteInput{
		In:              in,
		InverseActionID: seal.InverseActionID,
	}).Get(ctx, &exec); err != nil {
		return res, err
	}
	res.Executed = exec.Executed
	if exec.Executed {
		res.Outcome = "rollback:executed"
	} else {
		res.Outcome = "rollback:refused-gate"
	}
	res.Reason = exec.Note
	return res, nil
}

// rollbackVoteWait waits for the operator's approve/deny on the sealed inverse, binding the vote to the inverse
// action id (INV-12) and admitting an approver against the RECORDED approve_by set (spec/015 REQ-1516). It is a
// focused version of the Runner's vote-wait: it keeps the two safety-critical properties (bind-to-action and
// approver-admission), records the terminal decision to the ledger, and denies on timeout — an unanswered
// rollback poll is never a silent approval.
func rollbackVoteWait(ctx workflow.Context, a *Activities, in RollbackInput, seal SealRollbackResult) (approved bool, vote string) {
	recCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	record := func(decision, voter string) {
		var rec RecordVoteResult
		_ = workflow.ExecuteActivity(recCtx, a.RecordVoteActivity,
			RecordVoteInput{Decision: decision, ActionID: seal.InverseActionID, ExternalRef: in.RollbackExternalRef, Voter: voter}).Get(ctx, &rec)
	}

	voteCh := workflow.GetSignalChannel(ctx, VoteSignalName)
	timer := workflow.NewTimer(ctx, RollbackVoteWait)
	// Bound the audited history against a vote flood: an authenticated operator who names the wrong action, or is
	// not an approver, can at worst force a DENY, never an approval (fail closed) — mirroring the Runner's caps.
	const maxNoise = 64
	noise := 0
	timedOut := false
	for {
		var recv VoteSignal
		received := false
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(voteCh, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, &recv); received = true })
		sel.AddFuture(timer, func(workflow.Future) { timedOut = true })
		sel.Select(ctx)

		if timedOut {
			record("human:timeout", "")
			return false, "timeout"
		}
		if !received {
			continue
		}
		// The vote must NAME this sealed inverse action (a blind/premature/stale/misdirected vote is ignored and
		// only counted — the wait continues). This is what stops a buffered vote releasing an action the human
		// did not see (INV-12).
		if strings.TrimSpace(recv.ActionID) != seal.InverseActionID {
			if noise++; noise >= maxNoise {
				record("human:deny", "")
				return false, "denied"
			}
			continue
		}
		// APPROVER ADMISSION (spec/015 REQ-1516): under a CONFIGURED approver regime, only a member of the
		// inverse's approve_by set may release it. Under an UNCONFIGURED bundle admission is inert (today's
		// behaviour), and the admitted vote is still recorded. A non-approver's vote is refused-and-counted.
		if seal.ApproveByConfigured && !VoterAdmitted(seal.ApproveBy, recv.Voter) {
			if noise++; noise >= maxNoise {
				record("human:deny", "")
				return false, "denied"
			}
			continue
		}
		if recv.Approve {
			record("human:approve", recv.Voter)
			return true, "approved"
		}
		record("human:deny", recv.Voter)
		return false, "denied"
	}
}

// short12 renders a short prefix of a content hash for a human-facing notice (never security-load-bearing).
func short12(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// ---- activities ----

// SealRollbackResult is the serializable outcome of the seal step.
type SealRollbackResult struct {
	Sealed          bool
	InverseActionID string
	Reason          string
	// ApproveBy / ApproveByConfigured are resolved ONCE here (like GateActivity) so the vote-wait admits an
	// approver from RECORDED history rather than a live policy read (Temporal determinism).
	ApproveBy           []string
	ApproveByConfigured bool
}

// SealRollbackActivity re-validates reversibility (the authority), derives the compensating argv, seals the
// content-hashed inverse ActionManifest durably (INV-07), and resolves who may approve it. A seal/record failure
// fails CLOSED: no durable authorization ⇒ nothing to approve or execute. It performs NO estate mutation.
func (a *Activities) SealRollbackActivity(ctx context.Context, in RollbackInput) (SealRollbackResult, error) {
	spec, ok := opschema.Lookup(in.ForwardOpClass)
	if !ok {
		return SealRollbackResult{Reason: "op-class " + in.ForwardOpClass + " is not registered — no execution path"}, nil
	}
	// The reversibility gate (the authority; the endpoint pre-check is UX). REFUSES fail-closed.
	if _, err := rollbackArgvFor(spec, in.ForwardReversible, in.ForwardParams); err != nil {
		return SealRollbackResult{Reason: err.Error()}, nil
	}
	inverse := inverseActionFor(in)
	// Seal the inverse at POLL_PAUSE. planHash/predictionHash are the seal's own keys (a manual rollback commits
	// no model prediction — the human approval, not a prediction, authorizes it); the interceptor's structure
	// gate asserts the sealed action identity, not a prediction row's existence.
	planHash := PlanHash(in.RollbackExternalRef, "")
	m, err := manifest.New(inverse, safety.BandPollPause, planHash, "")
	if err != nil {
		return SealRollbackResult{}, err
	}
	inverseID := m.ActionID
	m = m.WithProvenance(manifest.Provenance{IncidentRef: in.RollbackExternalRef})
	if a.D.ManifestSink != nil {
		if err := a.D.ManifestSink.Seal(ctx, m); err != nil {
			// Fail closed: the authorization must be durable before a human is asked to approve it.
			return SealRollbackResult{}, err
		}
	}
	out := SealRollbackResult{Sealed: true, InverseActionID: inverseID}
	// Resolve the approver regime for the inverse (spec/015 REQ-1516), exactly as GateActivity does for a poll.
	out.ApproveByConfigured = a.D.ApproveByConfigured
	if a.D.ApproveByFor != nil {
		out.ApproveBy = a.D.ApproveByFor(ctx, ApproveByQuery{
			OpClass: inverse.OpClass, Op: inverse.Op, Host: inverse.Target, Reversible: inverse.Reversible,
			Band: safety.BandPollPause, Site: in.ForwardSite, ActionID: inverseID, ExternalRef: in.RollbackExternalRef,
		})
	}
	return out, nil
}

// RollbackExecuteInput carries the identity the execute step reloads the sealed inverse from durable state with.
type RollbackExecuteInput struct {
	In              RollbackInput
	InverseActionID string
	// AutoFired marks a commit-confirmed revert (spec/029 T-029-3): fired by the armed window's
	// elapse consult, not by an operator vote. The request then carries ApprovedBasis — the
	// FORWARD action's recorded approval, captured durably at arm time — instead of the manual
	// lane's vote-gate constant. Zero values keep the manual lane byte-identical.
	AutoFired     bool
	ApprovedBasis bool
}

// buildRollbackRequest assembles the governed actuate.Request for the sealed inverse. It is a PURE function (no
// I/O beyond the injected observer closures) so a test asserts, directly, that the request reaching the
// interceptor carries InvertsActionID = the forward id, band = POLL_PAUSE, the compensating rollback argv, and a
// bound forward-record evidence — without needing the effect to fire. Every gate itself lives in the (unchanged,
// protected) interceptor; this only shapes the request the chain judges.
//
// The FORWARD execution record is the evidence (INV-11): TG's OWN durable record that the forward action ran is
// captured, successful, recent (relative to the operator's rollback request), and target-relevant (same host) —
// the bound grounding a compensating action cites.
//
// stillFaulted is the ROLLBACK-APPROPRIATE necessity probe (TG-464 gap B, superseding TG-462's deliberate
// nil). A manual rollback is an operator-initiated undo of a KNOWN action, not a fault-conditional heal, so
// the forward lane's necessity question ("is the original fault still visible?") is the wrong one; the
// rollback-appropriate question — owner-decided on the ticket — is "is the FORWARD EFFECT still present?":
// undoing a start is necessary and safe only while the started unit is still up, and once the effect has
// lapsed there is nothing to undo (a blind stop could down a unit someone else brought up meanwhile). The
// closure is INJECTED — built by SealRollbackExecuteActivity over the live alert reader (see
// forwardEffectPresent) — so this function stays pure. A nil closure (no reader wired) still fails CLOSED at
// the interceptor's necessity gate (the nil-seam refusal at core/actuate/interceptor.go gate 4i), never a
// silent pass, so an unwired deployment keeps exactly TG-462's inert posture.
func buildRollbackRequest(m *manifest.ActionManifest, rollbackArgv []string, in RollbackInput,
	approved bool, acknowledged map[territory.Territory]bool,
	observe func(context.Context) ([]verify.ObservedAlert, bool),
	preAnomalous func(context.Context) (map[string]bool, bool),
	hostSite verify.SiteAuthority,
	stillFaulted func(context.Context) (present bool, ok bool)) actuate.Request {
	return actuate.Request{
		Manifest:        m,
		ExternalRef:     in.RollbackExternalRef,
		InvertsActionID: in.ForwardActionID, // TG-404: this request is the INVERSE of the named forward action
		Gated:           true,               // a sealed, human-authorized inverse (the structure gate rejects only an UNSEALED/ungated action)
		Argv:            rollbackArgv,       // the COMPENSATING argv (spec.RollbackArgv), never the forward argv
		Evidence: []actuate.Evidence{{
			ToolResultID: "action_execution:" + in.ForwardActionID,
			Captured:     true, // TG's own durable record that the forward action executed
			Successful:   true,
			Recent:       true,
			Relevant:     true, // same target host as the forward action
		}},
		Observe:      observe,
		PreAnomalous: preAnomalous,
		HostSite:     hostSite,
		Acknowledged: acknowledged,
		Approved:     approved,     // the operator's recorded approval binds the release (INV-12)
		StillFaulted: stillFaulted, // the effect-presence probe above; nil refuses at the gate (fail closed)
		// The band the inverse manifest was SEALED at: BandPollPause for the manual lane (sealed so
		// by SealRollbackActivity — behavior unchanged), the forward's own band for a commit-
		// confirmed auto-fire (spec/029 T-029-3: the envelope that admitted the forward is the
		// basis its declared revert carries; the admission gate still enforces it fresh).
		Band: m.Band,
	}
}

// forwardEffectPresent builds the manual-rollback lane's necessity probe (TG-464 gap B): a StillFaulted-shaped
// closure the interceptor's gate 4i consumes, answering the ROLLBACK-appropriate question — is the FORWARD
// action's EFFECT still present (is there still something to undo)? The two lanes license OPPOSITE readings
// of the SAME live surface: a forward heal proceeds while the target still ALERTS (the fault persists); a
// rollback proceeds while the target is QUIET (the forward fix is holding, i.e. its effect is present).
// Re-using the reader the clear-check and the forward probe already trust is deliberate — a presence probe
// that disagreed with the clear-check would be a second, conflicting notion of "fixed".
//
// PRECISION, stated honestly: the rollback activity's deps expose NO host/unit-state reader (nothing like a
// `systemctl is-active` reaches this seam — only the alert read exists), so effect-presence is grounded on
// the best read available, at host-quiet granularity:
//   - target QUIET    ⇒ (present=true, ok=true) — proceed. No active alert on the target is the best grounded
//     evidence the forward fix is holding (the started unit is still up). Coarser than a unit probe: a unit
//     stopped out-of-band without raising an alert still reads quiet, and the ticket's blind-stop hazard (a
//     unit someone ELSE started meanwhile) is not discriminated at this granularity — the gate is a
//     pre-condition on the best available evidence, not an outcome proof.
//   - target ALERTING ⇒ (present=false, ok=true) — the gate's "no longer necessary" refusal. The target is
//     faulted again: either the forward effect itself lapsed (the started unit is back down — nothing to
//     undo) or the host is mid-incident, where a blind compensating stop is exactly the destabilizer to
//     refuse. Fail-closed in the refusal direction: an UNRELATED alert also refuses (resolve it first).
//   - read error      ⇒ (present=false, ok=false) — the gate's fail-closed read-error refusal. Unlike the
//     forward probe there is deliberately NO OpenIncidents-ledger belt here: the belt positively confirms a
//     fault is still OPEN, which rescues only the forward's proceed direction — the rollback's proceed
//     direction needs a positive QUIET, and an absence of ledger rows is not that (ingest silence proves
//     nothing).
//
// SCOPE (TG-461/TG-464 follow-up): this alert-lane probe is the necessity check for every reversible class
// EXCEPT a guest-power rollback, which rollbackNecessityProbe routes to guestEffectPresent — a real
// guest-running read over the guest_liveness projection, which the actuate plane CAN read where this alert
// surface is 403-scoped-out (the topology token has no alert-read; TG-461). The honest host-quiet caveat above
// therefore no longer governs guest ops.
func forwardEffectPresent(clearObserve func(context.Context, string, string) ([]verify.ObservedAlert, bool),
	target, site string) func(context.Context) (present bool, ok bool) {
	return func(nctx context.Context) (bool, bool) {
		obs, ok := clearObserve(nctx, target, site)
		if !ok {
			return false, false // unreadable surface — not a quiet, not a licence (TG-182 discipline)
		}
		for _, al := range obs {
			if strings.EqualFold(strings.TrimSpace(al.Host), strings.TrimSpace(target)) {
				return false, true // the target is actively alerting — the forward effect cannot be confirmed present
			}
		}
		return true, true // the target is quiet — the forward fix is holding, there is still something to undo
	}
}

// guestEffectPresent is the guest-power lane of the rollback necessity probe (TG-464 gap B, unblocked past
// TG-461). When the forward action was a guest-power op — its sealed inverse is a stop-guest, which opschema
// declares RequiresRunning (the blind-stop guard) — "is the forward EFFECT present?" is precisely "is the guest
// still RUNNING?", which the guest_liveness projection (TG-378) answers directly. This resolves the honest
// host-quiet PRECISION caveat above for guest ops (a real target-state read), AND it is the surface the ACTUATE
// plane can actually read: the LibreNMS active-alert read forwardEffectPresent uses is 403-scoped-out there
// (TG-461), so a guest rollback grounded on the alert surface can only fail closed. It is the SAME reader the
// seal-time state precondition (core/predict.checkStatePrecondition) trusts, so the two notions of "running"
// cannot disagree.
//
//   - guest RUNNING     ⇒ (present=true, ok=true) — the started guest is still up; the undo is meaningful.
//   - guest NOT running ⇒ (present=false, ok=true) — the forward effect has lapsed (the guest is already down);
//     nothing to undo, and a blind stop of an already-stopped guest is refused.
//   - state unestablished (stale/absent projection, or no reader) ⇒ (present=false, ok=false) — fail closed: an
//     unestablished state is not a confirmation of presence (unknown is not still-running; TG-378 discipline).
func guestEffectPresent(guestRunning func(context.Context, string) (running bool, provenance string, ok bool),
	guest string) func(context.Context) (present bool, ok bool) {
	return func(nctx context.Context) (bool, bool) {
		running, _, ok := guestRunning(nctx, guest)
		if !ok {
			return false, false // state could not be established — not a licence to fire the inverse (TG-378)
		}
		return running, true // RUNNING ⇒ effect present; STOPPED ⇒ effect lapsed (nothing to undo)
	}
}

// serviceEffectPresent is the SERVICE lane of the rollback necessity probe (TG-464 close-out). For a
// service-lifecycle inverse (start-service → systemctl stop, restart/reload re-run) "is the forward EFFECT
// present?" is precisely "is the unit still RUNNING?" — which a positive `systemctl is-active` read over the
// ACTUATION identity answers directly. This is the surface the actuate plane can actually read: the LibreNMS
// active-alert read forwardEffectPresent uses is 403-scoped-out there (TG-461 — the topology token has no
// alert-read), so a service rollback grounded on the alert surface could only fail closed, and the manual
// rollback's ONLY eligible classes (rollbackArgvFor: template / restart / reload — all service-lifecycle)
// were structurally un-executable on a split deployment. Same three-way contract as guestEffectPresent:
//
//   - unit ACTIVE       ⇒ (present=true, ok=true) — the started/restarted unit is still up; the undo is
//     meaningful (the stop compensates a fix that is holding).
//   - unit NOT active   ⇒ (present=false, ok=true) — the forward effect has lapsed (the unit is already
//     down); nothing to undo, and a blind stop of an already-stopped unit is refused.
//   - state unestablished (transport error, host-key/auth failure, guard denial) ⇒ (present=false, ok=false)
//     — fail closed: an unreadable unit state is not a licence to fire the stop (TG-182 discipline).
func serviceEffectPresent(serviceActive func(ctx context.Context, host, unit string) (active bool, ok bool),
	host, unit string) func(context.Context) (present bool, ok bool) {
	return func(nctx context.Context) (bool, bool) {
		active, ok := serviceActive(nctx, host, unit)
		if !ok {
			return false, false // unit state could not be established — not a licence to fire the stop
		}
		return active, true // ACTIVE ⇒ effect present; inactive ⇒ effect lapsed (nothing to undo)
	}
}

// rollbackNecessityProbe selects the rollback's StillFaulted necessity probe for a sealed inverse. A guest-power
// rollback (the inverse declares RequiresRunning — a stop-guest) takes the guest_liveness lane, which the actuate
// plane can read (TG-461); a service-lifecycle inverse takes the systemctl-is-active lane over the actuation SSH
// identity (TG-464 — the same plane-readability fix for the manual lane's only eligible classes); every other
// reversible class keeps the live active-alert lane TG-462 built. All fail closed by construction, and when no
// reader for the selected lane is wired the probe falls through — ultimately to nil, the interceptor's own "no
// re-check wired" gate-4i refusal (TG-462's inert posture), never a silent pass.
func rollbackNecessityProbe(inverseSpec opschema.OpClassSpec, inverseSpecKnown bool, guest string,
	guestRunning func(context.Context, string) (running bool, provenance string, ok bool),
	serviceActive func(ctx context.Context, host, unit string) (active bool, ok bool), unit string,
	clearObserve func(context.Context, string, string) ([]verify.ObservedAlert, bool),
	target, site string) func(context.Context) (present bool, ok bool) {
	if inverseSpecKnown && inverseSpec.RequiresTargetState == opschema.RequiresRunning && guestRunning != nil && strings.TrimSpace(guest) != "" {
		return guestEffectPresent(guestRunning, guest)
	}
	// Selection is by FAMILY (service-lifecycle), disjoint from the guest lane (guest ops are guest-lifecycle,
	// and only their stop side declares RequiresRunning). Requires a wired reader AND a non-empty unit param —
	// anything less falls through to the alert lane / nil seam exactly as before this lane existed.
	if inverseSpecKnown && inverseSpec.Family == opschema.FamilyServiceLifecycle && serviceActive != nil && strings.TrimSpace(unit) != "" {
		return serviceEffectPresent(serviceActive, target, unit)
	}
	if clearObserve != nil {
		return forwardEffectPresent(clearObserve, target, site)
	}
	return nil
}

// SealRollbackExecuteActivity reloads the sealed inverse manifest (INV-07 — the authoritative action from durable
// state, never a re-serialized copy), asserts its identity, builds the governed request, and runs it through the
// SAME interceptor chain (or regime lane) forward mutations use. It MUTATES the estate only when the mode
// chokepoint permits — inert under Shadow/HITL. Pinned to the actuation queue by the workflow.
func (a *Activities) SealRollbackExecuteActivity(ctx context.Context, rin RollbackExecuteInput) (ExecuteResult, error) {
	in := rin.In
	routed := a.D.RegimeEngine != nil && a.D.LaneEffect != nil
	if a.D.Interceptor == nil && !routed {
		// Oracle / no-DB path: assert the gate for parity and stop (never a silent execute).
		if a.D.Mutation != nil {
			if err := a.D.Mutation.GuardMutation(); err != nil {
				return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "mutation disabled (read-only)"}, nil
			}
		}
		return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "no interceptor wired"}, nil
	}
	if a.D.Manifests == nil {
		return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "no manifest store"}, nil
	}
	m, ok, err := a.D.Manifests.Get(ctx, rin.InverseActionID)
	if err != nil {
		return ExecuteResult{}, err
	}
	if !ok || m == nil {
		return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "no sealed inverse manifest"}, nil
	}
	if err := m.Assert(rin.InverseActionID); err != nil {
		return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "inverse manifest assertion failed — refused: " + err.Error()}, nil
	}
	// The compensating argv, re-derived under the matching gate. A refusal here is fail-closed —
	// the inverse never reaches the effect leaf with a forward or empty argv. Two shapes
	// (spec/029 T-029-3): a CLASS inverse's sealed action carries the INVERSE class (start-guest's
	// revert is a first-class stop-guest action), so its argv is that class's OWN compiled builder;
	// a SELF-inverse keeps the forward class and re-derives through rollbackArgvFor exactly as
	// TG-462 built it. Discriminated on durable facts (the sealed manifest's class vs the forward's).
	spec, specOK := opschema.Lookup(m.Action.OpClass)
	if !specOK {
		return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "op-class not registered at execute — refused"}, nil
	}
	var rollbackArgv []string
	if !strings.EqualFold(strings.TrimSpace(m.Action.OpClass), strings.TrimSpace(in.ForwardOpClass)) {
		if spec.SafetyTier != opschema.TierLowReversible {
			return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "inverse class tier is not low-reversible — refused"}, nil
		}
		argv, aerr := spec.Argv(m.Action.Params)
		if aerr != nil || len(argv) == 0 {
			return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: fmt.Sprintf("inverse class argv derivation refused: %v", aerr)}, nil
		}
		rollbackArgv = argv
	} else {
		argv, rerr := rollbackArgvFor(spec, in.ForwardReversible, m.Action.Params)
		if rerr != nil {
			return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: rerr.Error()}, nil
		}
		rollbackArgv = argv
	}
	// The post-execution observer (fail-closed: no reader ⇒ (nil,false), which the verifiability gate refuses on).
	observe := func(octx context.Context) ([]verify.ObservedAlert, bool) {
		if a.D.PostStateObserve == nil {
			return nil, false
		}
		return a.D.PostStateObserve(octx, in.ForwardTarget, in.ForwardSite)
	}
	var preAnomalous func(context.Context) (map[string]bool, bool)
	if a.D.OpenIncidents != nil {
		preAnomalous = func(pctx context.Context) (map[string]bool, bool) { return a.D.OpenIncidents(pctx, time.Now().UTC()) }
	}
	// The rollback-appropriate necessity probe (TG-464 gap B): built here and injected so buildRollbackRequest
	// stays pure. rollbackNecessityProbe routes a guest-power rollback (the sealed inverse is a stop-guest,
	// opschema RequiresRunning) to the guest_liveness projection — the actuate plane CAN read it, whereas the
	// LibreNMS active-alert surface is 403-scoped-out there (TG-461); every other class keeps the live
	// active-alert read. A nil seam (no reader wired) is the interceptor's own "no re-check wired" fail-closed
	// refusal, distinct from a wired-but-unreadable probe. The guest identity mirrors checkStatePrecondition
	// (core/predict): the sealed inverse's guest param, then its Target.
	var guestRunning func(context.Context, string) (running bool, provenance string, ok bool)
	if a.D.Gate != nil {
		guestRunning = a.D.Gate.GuestRunning
	}
	guest := m.Action.Params["guest"]
	if guest == "" {
		guest = m.Action.Target
	}
	// The service lane's unit identity is the sealed inverse's own unit param (the SAME authoritative fact the
	// compensating argv renders from) — never re-derived, so the probe and the stop it licenses cannot diverge.
	unit := m.Action.Params[opschema.ParamUnit]
	stillFaulted := rollbackNecessityProbe(spec, specOK, guest, guestRunning, a.D.ServiceActive, unit, a.D.ClearObserve, in.ForwardTarget, in.ForwardSite)
	// The manual lane reaches here only past its vote gate (approved=true, sealed at POLL_PAUSE);
	// the auto-fired lane (spec/029) carries the forward's own recorded basis, and its manifest was
	// sealed at the forward's band — buildRollbackRequest reads the band off the manifest, so each
	// lane's request truthfully reflects its authorization. The interceptor judges both fresh.
	approved := true
	if rin.AutoFired {
		approved = rin.ApprovedBasis
	}
	req := buildRollbackRequest(m, rollbackArgv, in, approved, a.D.Acknowledged, observe, preAnomalous, a.D.HostSite, stillFaulted)

	var out actuate.Outcome
	if routed {
		var lane regime.Lane
		var lerr error
		if reg, byKind := effectKindRegime(m.Action.OpClass); byKind {
			l, wired := a.D.RegimeEngine.LaneForRegime(reg)
			if !wired {
				lerr = fmt.Errorf("regime %q (effect kind of op-class %q) has no wired lane", reg, m.Action.OpClass)
			} else {
				lane = l
			}
		} else {
			lane, lerr = a.D.RegimeEngine.SelectLane(credential.Target{Host: in.ForwardTarget})
		}
		if lerr != nil {
			return ExecuteResult{Executed: false, ActionID: rin.InverseActionID, Note: "regime: no effect lane for inverse — refused: " + lerr.Error()}, nil
		}
		out, err = a.D.LaneEffect.Apply(ctx, lane, req)
	} else {
		out, err = a.D.Interceptor.Do(ctx, req)
	}
	if err != nil {
		return ExecuteResult{}, err // unwired chain (fail loud)
	}
	return ExecuteResult{Executed: out.Executed, ActionID: rin.InverseActionID, Verdict: string(out.Verdict), Note: out.Reason}, nil
}
