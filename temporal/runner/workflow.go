package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/screen"
	tg "github.com/territory-grounder/grounder/temporal"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
)

// RunnerResult is the terminal state of a Runner workflow run.
type RunnerResult struct {
	ExternalRef string
	ExecClass   string // the execution topology chosen before context construction (execclass.Class)
	Band        string
	ActionID    string
	// Attribution is the actor-attribution taxonomy the attribute step resolved (spec/023) — "unattributable"
	// when no admissible evidence exists, "" only on a pre-feature/pre-deploy history (the step version-guarded
	// off). The WHO-CAUSED-THIS answer carried on every session record. ActorEvidence is the minimized,
	// redacted evidence blob persisted with it (REQ-2311/2313).
	Attribution   string
	ActorEvidence []byte
	// SecurityEscalated is the attribute step's SECURITY disposition bit (attribution.SecurityEscalate →
	// AttributeResult.Security), carried verbatim so a consumer can distinguish the TG-466
	// intrusion-suspicious escalation from a GENERIC escalate that also lands POLL_PAUSE — the eval's
	// mechanical check reads THIS, not a taxonomy+band proxy that both dispositions could satisfy (TG-533
	// review finding, 2026-08-25). False on pre-feature histories exactly like Attribution's "".
	SecurityEscalated bool
	Proposed          bool
	EvidenceIDs       []string // the tool-result ids the proposal (or grounded stop) cited (INV-11) — surfaced for the ledger/console/eval
	Conclusion        string   // the agent's grounded no-action rationale on a stop (REQ-1008) — untrusted DATA for the record
	// StopReason is the ORCHESTRATOR's own account of why the loop halted without a proposal (migration 0044) —
	// trusted, and deliberately separate from Conclusion, which is untrusted agent free-text (INV-08).
	StopReason string
	Prediction string  // the committed machine prediction rendered judge-readable (TG-61) — surfaced so the terminal record + live judge cron score falsifiable_prediction; empty when nothing was proposed
	Predicted  bool    // a machine prediction was committed at the gate (a proposal reached the prediction gate) — false for a grounded stop
	Confidence float64 // the agent's emitted 0..1 proposal confidence (core/proposal), carried to the durable triage record for the decision tracer + calibration (spec/020 REQ-2003, observability only); 0 for a grounded stop
	// PromptVersion / SeedHash / ModelTier are the session's prompt/seed/model provenance for the decision
	// tracer (spec/020 REQ-2009): the trusted-preamble version, the SHA-256 fingerprint of the composed seed
	// (hash only — never the seed text; INV-13), and the LLM tier the loop ran on. Observability only.
	PromptVersion string
	SeedHash      string
	ModelTier     string
	// DecisionTier is the tier that produced the TERMINAL proposal/stop — WHICH MODEL DECIDED (TG-198).
	// ModelTier above answers only which model did the READING. Observability only.
	DecisionTier string
	// StepCount is the agent loop's read-only investigation cycle count — benchmark axis A6a (decision
	// efficiency). Observability only; surfaced for the eval scorer + decision tracer.
	StepCount int
	// Trajectory is the ordered tool path (agent.TrajectoryStep, ArgsKey already digested in the activity)
	// carried out so the offline eval scorer can grade the ordered path with the same deterministic veto the
	// runtime uses (TG-525, trajectory_grounded). Observability only; re-enters no gate (INV-08).
	Trajectory []agent.TrajectoryStep
	// DecisionMillis is the agent loop's WALL-CLOCK time to the terminal decision — benchmark axis A6b
	// (TG-205), the time half of the MTTR axis nothing in TG measured. Computed in the activity (a workflow
	// must not read the clock) and carried here so every terminal record — proposal, shadow proposal or
	// grounded stop — can publish it. Observability only.
	DecisionMillis int64
	// TriageRecorded reports that SOME recordTriage call durably recorded this session (TG-81 borrow 1) —
	// the fact the synthetic-terminal wrapper consults so a session that dies mid-flight still leaves a
	// terminal frame, and a session that already recorded is never double-written. Observability only.
	TriageRecorded bool
	// Diagnosis is the typed, source-bound CLAIM the session produced (core/proposal, TG-201), already bound
	// by the agent loop against the ToolResult ids the ORCHESTRATOR captured — never against the model's own
	// assertion (INV-11). Carried onto the terminal record so the asynchronous judge can score it hours
	// later, when the transcript is long gone: that is the only way `diagnosis_grounded` grades anything.
	// Zero value for a session that bound no claim. DATA only — it vetoes no proposal and releases nothing
	// (INV-08); a model can be wrong about what contradicts what, and the value is that the claim becomes
	// CHECKABLE, not that it becomes authority.
	Diagnosis proposal.Diagnosis
	// DegradedCapabilities is the set of TG's OWN dependency capabilities that were degraded when this session
	// ran (TG-394 slice 3) — computed in the activity over the live estate graph (a workflow may not read
	// external state) and carried here so every terminal record stamps WHY a lexical-only investigation looked
	// the way it did. A controlled, non-secret vocabulary (capability names). Nil for a pre-feature history or
	// an unwired deployment. Observability only — it re-enters no gate.
	DegradedCapabilities []string
	PollBuilt            bool
	Notified             bool   // the governance notice/poll was delivered to the human channel (notice/poll bands)
	Mutated              bool   // MUST be false in Phase 0/1 (the Runner stops at propose)
	Vote                 string // the human decision on a POLL_PAUSE proposal: "approved" | "denied" | "timeout" | "" (no poll)
	Outcome              string
}

// VoteSignalName is the Temporal signal a human approval/deny vote arrives on. The vote intake surface
// (REQ-518) signals the workflow keyed by external_ref — the decision id the poll was built for, so the
// vote binds to exactly the decision it answers (INV-12, no global cursor, no misattribution).
const VoteSignalName = "approval-vote"

// VoteSignal is the serializable human decision delivered to a waiting Runner. Voter is the
// SERVER-authenticated operator identity (the session principal), never a client-supplied claim.
// ActionID is the sealed action the human is deciding on: a vote decides ONLY when it names the
// session's gated action (INV-12 — the approval binds to the action, not merely the session ref), so a
// blind/premature/stale vote can never release an action the human did not see. A mismatched vote is
// ledger-recorded and ignored; the wait continues.
type VoteSignal struct {
	Approve  bool
	Voter    string
	ActionID string
}

// VoterAdmitted reports whether voter is a MEMBER of the poll's approve_by set (spec/015 REQ-1516) — the
// check that decides whether an authenticated operator may release THIS action.
//
// EXPORTED so the composition root's own oracle can assert its expanded approve_by set against the RULE THE
// BINARY ENFORCES rather than a copy re-typed inside a test. A copy proves the copy is right and says nothing
// about the shipped path — which is exactly how a control ships unreachable (see ladderRungFor's note).
//
// TG-254: nothing performed this check anywhere. /v1/vote authenticated the operator, rate-limited them, and
// signalled the workflow; the workflow bound the vote to the sealed action id and then approved. So ANY
// authenticated operator — an intern, a read-only auditor, a compromised session — could approve ANY governed
// action. `core/policy.MayApprove` existed and had zero production callers.
//
// It is PURE and operates only on the set the gate ACTIVITY resolved (recorded in history), so it is safe in
// workflow code and replays identically forever — a live identity lookup here would make the same vote
// admissible on Monday and refused on Tuesday for the same history, which Temporal forbids.
//
// FAIL CLOSED in every direction:
//   - an EMPTY (or all-blank) approve_by admits NOBODY. REQ-1516 admits a vote only when the voter is a
//     MEMBER of the set; an empty set has no members. This is the same direction policy.MayApprove takes
//     (core/policy/identity_test.go `_EmptyApproveBy_Denied`) and the opposite of the TG-254 defect.
//   - an empty voter id admits nothing (an unnamed actor cannot approve).
//
// ITS ANSWER IS ONLY BINDING WHEN THE BUNDLE DECLARES AN APPROVER REGIME. This predicate cannot tell "the
// operator named approvers and you are not one" from "this deployment has never named an approver for
// anything" — both arrive as an empty set. The caller supplies that missing fact from
// GateResult.ApproveByConfigured and skips admission entirely for an unconfigured bundle, because refusing
// on a bundle that declares no approver anywhere makes EVERY poll unvotable rather than making one poll
// safe. Keeping the distinction OUT of here is deliberate: this stays a pure membership test with one
// reading, and the policy question lives where the policy is known.
//
// A `group:` entry can only be matched by an identity backend that can enumerate membership, which workflow
// code deliberately has none of — so a group entry admits nobody HERE. It is not silently dropped: the entry
// keeps the set non-empty, the refusal is recorded, and expanding a group to its concrete members is the
// composition root's job at gate time (Deps.ApproveByFor), which the worker does over the spec/016 credential
// human plane (cmd/worker/approve_by_wiring.go: expandApproveBy) — so a bundle spelled `group:sre-oncall`
// arrives here already carrying its members as `user:` entries. Fail-closed either way, and the direction of
// the error is "a real approver is asked to re-try", never "a stranger executed".
func VoterAdmitted(approveBy []string, voter string) bool {
	v := strings.TrimSpace(voter)
	if v == "" {
		return false
	}
	for _, raw := range approveBy {
		e := strings.TrimSpace(raw)
		switch {
		case e == "":
			continue // a blank entry names nobody
		case strings.HasPrefix(e, "group:"):
			continue // membership is not resolvable in workflow code — see above
		case strings.HasPrefix(e, "user:"):
			e = strings.TrimSpace(strings.TrimPrefix(e, "user:"))
		}
		// An unprefixed entry is matched against the id, exactly as policy.parseApproveByEntry treats it.
		if e != "" && strings.EqualFold(e, v) {
			return true
		}
	}
	return false
}

// VoteWait is how long a POLL_PAUSE session waits for the human vote before recording a timeout and
// standing down (the poll is never silently approved — an ignored poll denies by default, INV-12).
// The predecessor's operator answered 0/824 polls: an expiry that escalates, not blocks forever, is the
// realistic design. Temporal timers make the long wait durable (it survives worker restarts/deploys).
const VoteWait = 24 * time.Hour

// voteRecoveryRecheckInterval (C2) is how often a WAITING POLL_PAUSE poll re-checks whether the incident's
// subject already RECOVERED on its own (TG's OWN captured recovery push, RecoveredSince over ingest_transition).
// A self-resolved incident's poll is moot — closing it obsolete within one recheck beats parking for VoteWait
// (24h) and leaving a stale open decision. Compiled (deterministic — workflow code reads no env).
const voteRecoveryRecheckInterval = 10 * time.Minute

// clearSettleWindow is how long the LEGACY single-shot close-out waits after an executed remediation before
// re-observing whether the incident's host went quiet (the ConfirmedClear check). It must exceed the
// active-alert reader's poll cycle (LibreNMS polls every ~5 min) so the observation reflects post-remediation
// truth rather than a stale still-firing snapshot or a single transient recovery. Durable Temporal timer; the
// runner workflow sets no execution timeout, so the wait never risks a workflow-level timeout. Retained for
// deterministic replay of pre-`confirmed-clear-retry` histories; new sessions use the bounded-retry below.
const clearSettleWindow = 6 * time.Minute

// clearRecheckInterval / clearMaxWindow (TG-124): the single-shot check at a FIXED clearSettleWindow missed
// real clears — a device-down RECOVERY (the guest boots → LibreNMS re-polls it UP → the alert clears) can take
// longer than 6 min, so the one-shot re-observed while the alert was still firing and never confirmed, and the
// novelty writeback NEVER fired (0 de-novels observed live). The bounded-retry re-observes every
// clearRecheckInterval up to clearMaxWindow, confirming on the FIRST genuinely-quiet reading and failing closed
// if never quiet within the bound. It tolerates variable recovery latency without ever weakening the safety
// contract (ConfirmedClear still requires an ACTUAL server-side quiet observation, never the model's word).
const clearRecheckInterval = 3 * time.Minute
const clearMaxWindow = 30 * time.Minute

// clearConfirmReadings debounces the clear: a clear is confirmed ONLY after this many CONSECUTIVE quiet
// readings, so a FLAPPING host (momentarily quiet between polls, then re-alerting) can never false-confirm —
// a re-alert resets the counter. Two readings clearRecheckInterval apart also span the reader's ~5-min poll
// cycle, so the confirmation reflects a stable post-remediation quiet rather than a single transient snapshot.
const clearConfirmReadings = 2

// noticeBody renders the governance notice as DATA for the human channel (the backend redacts credentials
// and PII before posting). Pure and deterministic (INV-08 — a notice is never interpolated as control), so
// it is safe in workflow code.
// handoffEligible decides whether a proposal-less terminal warrants paging a human (TG-386). It fires ONLY on
// a SUBSTANTIVE conclusion (non-empty — an empty-conclusion stop is either an infra failure or a trivial
// stand-down, and paging on every one fires on 35.8% of sessions), AND EITHER the session escalated because
// it hit the handoff limit (it explicitly ran out of autonomous options) OR it ran the DEEP_INVESTIGATION
// path (the full lane the pve03 root-cause session took). Conclusion is untrusted agent free-text (INV-08)
// used only to decide WHETHER to page; the body the operator reads is rendered by handoffBody, and delivery
// is armed separately (Deps.HandoffNotify).
func handoffEligible(conclusion, execClass, outcome string) bool {
	if strings.TrimSpace(conclusion) == "" {
		return false
	}
	return outcome == "escalated:handoff-limit" || execClass == string(execclass.DeepInvestigation)
}

// handoffBody renders the proposal-less-handoff page: the incident, the agent's grounded conclusion (DATA —
// the notifier backend redacts credentials/PII before posting), the orchestrator's trusted stop reason, and
// the ref. It carries NO action because there is none — the message is "a human is needed", not an approval.
func handoffBody(alertRule, host, conclusion, stopReason, externalRef string) string {
	return fmt.Sprintf("[handoff] %s on %s — no safe automated action; a human is needed.\nconclusion: %s\nstopped: %s\nref=%s",
		alertRule, host, strings.TrimSpace(conclusion), stopReason, externalRef)
}

// collapseConclusion renders the durable record for a member that COLLAPSED into a cluster (TG-376): the
// cluster it joined, the elected subject investigating on its behalf, the rule that elected them, and the
// cluster's member/host NAMES threaded from CorrelateResult (the []string names TG-385 (b) returned to the
// workflow). Pure and deterministic (INV-08) — it is DATA on the triage row, never control. The names are a
// bounded, non-secret projection (hosts + the MaxMembers-capped ref list), so a wide cascade cannot balloon
// the record.
func collapseConclusion(cor CorrelateResult) string {
	hosts := strings.Join(cor.HostNames, ",")
	members := strings.Join(cor.MemberRefs, ",")
	return fmt.Sprintf("correlation: attached as evidence to cluster %d — elected subject %q investigates the cascade (rule %s); this member opens no session (TG-376). hosts=[%s] members=[%s]",
		cor.ClusterID, cor.ElectedRef, cor.ElectRule, hosts, members)
}

func noticeBody(band safety.Band, alertRule, host, op, opClass, externalRef string) string {
	line := fmt.Sprintf("[%s] %s on %s — proposed: %s (%s). ref=%s", band, alertRule, host, op, opClass, externalRef)
	if band != safety.BandPollPause {
		return line
	}
	// A POLL_PAUSE notice ASKS FOR A VOTE, so it must say how to cast one.
	//
	// It did not. The body described the proposal and stopped, while the resolver requires the reply's
	// first two whitespace fields to be exactly a verb and this decision id ("approve INC-123"). An
	// approver reading the old notice had no way to learn that: "approve" alone is rejected for citing no
	// decision, and "yes please approve INC-123" is rejected because field two is "please". An unanswered
	// poll then times out to DENY after the vote window — so a human who WANTED to approve, and said so in
	// their own words, produced a denial.
	//
	// This mattered nowhere until the notifier was wired on 2026-08-02; before that the notice reached no
	// operator at all. Making the channel live is what made the missing instruction a live defect.
	return line + "\n\nPick an option on the poll above, or reply in this room with exactly:\n" +
		"  approve " + externalRef + "\n" +
		"  deny " + externalRef + "\n" +
		"(the decision id must be the second word. You can also decide in the console. " +
		"No answer within the vote window is recorded as DENY.)"
}

// riskLevel maps an incident severity to a coarse risk level for the audit row.
func riskLevel(sev ingest.Severity) string {
	switch sev {
	case ingest.SeverityCritical:
		return "high"
	case ingest.SeverityWarning:
		return "medium"
	default:
		return "low"
	}
}

// autoResolveMarked reports whether the proposal carried an [AUTO-RESOLVE] marker. The marker rides in the
// model's approval_choice, parsed as DATA and never trusted as authority — the classifier's silent-cognition
// guard, not this flag, decides the band. Mirrors the predecessor's /[AUTO-RESOLVE]/ match on the model
// output: it keys on the marker itself, so a marked proposal that binds no evidence (including zero cited ids)
// is still recognized as an auto-resolve request and stripped to POLL_PAUSE.
func autoResolveMarked(approvalChoice string) bool {
	return strings.Contains(strings.ToUpper(approvalChoice), "AUTO-RESOLVE")
}

// RunnerWorkflow is the deterministic session orchestrator. It drives investigate → classify → gate →
// STOP-at-propose. The execute/verify activities are invoked but no-op in a non-actuating mode (as throughout Phase 0/1),
// so the workflow ends at a sealed, classified, gated proposal with no estate mutation. The workflow
// body contains CONTROL FLOW ONLY — every side effect is an activity, and no activity executes an OS
// command (INV-21). Action identity is threaded unchanged: the action_id derived here is asserted by
// the gate activity's sealed ActionManifest (INV-07).
func runSession(ctx workflow.Context, env ingest.IncidentEnvelope) (RunnerResult, error) {
	// Base ActivityOptions for the ordinary read-only pipeline activities — a BOUNDED RetryPolicy so a
	// persistently-failing activity surfaces (or the discarded best-effort `.Get` returns) after a few
	// attempts instead of retrying forever under Temporal's unbounded default (see budget.go). The two
	// hazardous classes (record-vote, execute) and the long-running investigate set their OWN options below.
	ctx = workflow.WithActivityOptions(ctx, runnerActivityOptions())
	var a *Activities // nil receiver — used only for activity-name resolution

	// budgetStart anchors the session wall-clock budget (a runaway-time backstop; see budget.go). A pure,
	// deterministic time read — no command, safe on replay and harmless for pre-budget histories, which
	// simply never consult it (the budget control flow is GetVersion-guarded below).
	budgetStart := workflow.Now(ctx)

	res := RunnerResult{ExternalRef: env.ExternalRef, Outcome: "stop"}

	// 0) topology decision — choose the execution class BEFORE any expensive context is built, so a cheap
	// incident need not pay the full lifecycle a hard correlated one needs. Recorded for the console, the
	// ledger and evaluation; the per-class fast/deep paths consume it as they are built.
	//
	// This line is the PRE-CORRELATION default, and it is what a pre-TG-169 workflow history keeps (the
	// stage below is version-guarded). It is deliberately not the last word: see step 0.75.
	res.ExecClass = string(legacyExecClassFor(env))

	// 0.5) tier-1 suppression (spec/005) — a declared maintenance/chaos freeze, a known transient, or a
	// duplicate must NOT spend a triage session. The Runner stops early, read-only, with no proposal. Fail
	// open: no chain wired ⇒ investigate. This is the first gate so a suppressible alert costs no model spend.
	var sup SuppressResult
	if err := workflow.ExecuteActivity(ctx, a.SuppressActivity, env).Get(ctx, &sup); err != nil {
		return res, err
	}
	if sup.Suppressed {
		res.Outcome = "suppressed"
		return res, nil
	}

	// 0.75) CORRELATION (TG-169) — decide whether this incident is ONE system in trouble or SEVERAL, from
	// the alerts TG actually admitted around it (cross-source, cross-host, time-windowed), and route on
	// that instead of on one upstream severity field.
	//
	// THE DEFECT THIS REPLACES. `Correlated` — the flag that sends an incident down the deep path — was
	// `env.Severity == critical`. Severity is a property of ONE alert and cannot answer a question about
	// the RELATIONSHIP between alerts. Live: 2,434 of 2,995 admitted alerts are critical, so 81% of
	// incidents claimed to "span multiple systems" on no evidence at all. The reverse error costs more: a
	// genuine multi-host cascade assembled from WARNINGS — many weak signals, no single critical — set
	// Correlated=false and was routed to the cheapest reasoning TG has.
	//
	// It runs AFTER suppression on purpose: a suppressed alert (freeze, known transient, duplicate) opens
	// no session, and paying a database read to route a session that will not exist is the same waste
	// tier-1 suppression exists to avoid. It runs BEFORE investigate because that is the expensive step the
	// topology decision is FOR.
	//
	// VERSION-GUARDED: a workflow history from before this shipped has no such activity event, so it keeps
	// the step-0 legacy class and replays byte-identically. FAIL-SAFE on error: the activity itself never
	// returns one (it degrades internally and says so on the record), so this branch handles only a
	// Temporal-level failure — and it keeps the legacy class rather than failing a session over a routing
	// refinement.
	//
	// memberCtx is the elected-cluster member evidence investigate receives (TG-465 part 2). Zero — no
	// cluster block, today's seed byte-for-byte — unless the correlation stage below affirmatively found a
	// correlated window with THIS incident as its elected causal subject. A pre-correlation history, a
	// failed stage, and a collapsed member (which returns before investigate) all leave it zero.
	var memberCtx ClusterMemberContext
	// fastHeal is TG-496 fix (c)'s routing signal — set true only when the correlation stage confirmed an
	// isolated, non-critical, observed-STOPPED pve-liveness guest-down (cor.FastHeal). Hoisted here (like
	// memberCtx) so the investigate step below can read it; a pre-correlation history, a failed stage, or a
	// collapsed member all leave it false, so the fast-path never fires without a confirmed correlation verdict.
	var fastHeal bool
	if workflow.GetVersion(ctx, "correlation-stage", workflow.DefaultVersion, 1) >= 1 {
		var cor CorrelateResult
		if err := workflow.ExecuteActivity(ctx, a.CorrelateActivity, correlateInputFor(env)).Get(ctx, &cor); err != nil {
			workflow.GetLogger(ctx).Warn("correlation stage failed; keeping the pre-correlation execution class",
				"ref", env.ExternalRef, "class", res.ExecClass, "err", err)
		} else {
			if execclass.Valid(execclass.Class(cor.ExecClass)) {
				// VALIDATED before adoption: an unknown class would select no skills and satisfy no floor, so
				// a future activity returning "" or garbage must fall back rather than route into a class that
				// exists nowhere.
				res.ExecClass = cor.ExecClass
			}

			// 0.8) CASCADE COLLAPSE (TG-376) — the outcome of the durable cluster identity + causal election.
			// This incident is a correlated MEMBER of a durably-identified cluster but NOT its elected causal
			// subject, so it attaches as EVIDENCE to the elected subject and opens NO investigation session.
			// This is the whole fix for 1.000 alerts/session: on the 2026-08-06 pve03 storm 157 alerts opened
			// 157 sessions because nothing said "you are one member of a cascade whose subject is already being
			// investigated". cor.Elected is false ONLY for a correlated member that joined a durable cluster
			// (cluster_id>0) and lost the election — an uncorrelated incident, the elected subject itself, or a
			// deployment with no durable cluster store all keep Elected=true and investigate as before.
			//
			// It runs BEFORE investigate for the same reason the correlation stage does: investigate is the
			// expensive step this decision is FOR. A collapsed member still writes its durable triage row
			// (outcome collapsed:cluster-member, no proposal, no model spend) so the attachment is auditable —
			// it is not a silent drop, it is one session standing in for the storm.
			//
			// Version-guarded on its OWN marker so an in-flight/replaying history that never scheduled this
			// branch replays byte-identically (a pre-change history has the marker at DefaultVersion and skips
			// the collapse entirely). The elected subject falls through to investigate below, unchanged.
			if workflow.GetVersion(ctx, "cluster-collapse", workflow.DefaultVersion, 1) >= 1 &&
				cor.Correlated && !cor.Elected {
				res.Outcome = "collapsed:cluster-member"
				res.Conclusion = collapseConclusion(cor)
				res.StopReason = "cluster-collapse"
				res.TriageRecorded = recordTriage(ctx, a, env, res, "", "", nil)
				return res, nil
			}
			// TG-465 part 2: the elected subject falls through to investigate KNOWING the cluster it
			// represents — the member host names TG-385 already returned here now reach the seed. A pure
			// projection (memberContextFor gates on Correlated && Elected), so an uncorrelated incident
			// stays zero and composes today's seed.
			memberCtx = memberContextFor(cor)
			// TG-496 fix (c): adopt the confirmed-guest-down deterministic-heal signal. Read only for the
			// elected subject that falls through to investigate (a collapsed member returned above), and gated
			// by confirmedGuestDownHeal's fail-closed precondition, so it is true only for a confirmed heal case.
			fastHeal = cor.FastHeal
		}
	}

	// 1) investigate — the read-only agent loop proposes (or stops/escalates without a proposal). The loop can
	// legitimately run for minutes: up to HandoffHalt (10) cycles, each a model call (a reasoning model, plus
	// the fail-over tax when the primary is rate-limited) and read-only tool calls. The default 1-minute
	// activity timeout truncates it — an eval run over real NL incidents found EVERY triage timing out to an
	// empty session (no band, no proposal). So investigate gets a StartToClose sized to the cycle bound. It is
	// read-only (INV-21), so a single bounded retry on a transient model blip is safe (unlike execute).
	investigateCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy:         investigateRetryPolicy(), // ONE bounded retry + deterministic-error short-circuit (budget.go)
	})
	// THE DECIDED CLASS IS THREADED, NOT RE-DERIVED (TG-169). The investigate activity chooses the model
	// tier (the MECH-402 floor) and composes the skill seed from the execution class; both used to
	// recompute it from the envelope. That was harmless only while the class WAS a pure function of the
	// envelope. It no longer is — so a re-derivation would be a second, different decision wearing the
	// first one's name: the workflow would record DEEP_INVESTIGATION for a warning cascade while the
	// investigation it launched read on the cheap tier, with nothing disagreeing with anything. Pure data
	// on the same activity call, so no new GetVersion is needed (same pattern as the verify payload
	// below): an old history replays from its RECORDED result, and an in-flight task scheduled without the
	// argument deserializes it as "" — which classFor resolves back to the legacy envelope rule.
	//
	// memberCtx (TG-465 part 2) rides the SAME contract, appended LAST for the same reason the class was:
	// pure data on the same activity call, no new GetVersion. An old history replays from its RECORDED
	// result; an in-flight task scheduled without the argument deserializes it as the zero value — which
	// composes today's seed byte-for-byte (no <cluster_members> block).
	// TG-496 fix (c) — the deterministic guest-down auto-heal fast-path. CorrelateActivity confirmed a
	// pve-liveness guest-down observed STOPPED, isolated and non-critical (cor.FastHeal), so dispatch a
	// deterministic start-guest PROPOSAL that BYPASSES the agent loop — restoring auto-heal for the
	// highest-value reversible case under ANY brain (the post-08-08 Mistral brain collapsed the loop's
	// proposal propensity; TG-496). The activity RE-CONFIRMS the observed-stopped precondition and DECLINES
	// (Proposed=false) if the guest recovered in between, so the workflow falls back to the normal loop
	// (never the SkipsAgent no-action shortcut) — a heal we cannot synthesize never silently drops a
	// confirmed-down guest. On the confirmed path the proposal flows UNCHANGED through the classify / gate /
	// commit-confirm arm / mode-chokepoint chain below; the TG-378 seal gate re-confirms not-running at commit.
	//
	// Version-guarded like every added stage: a pre-change history recorded no such activity (and an old
	// CorrelateResult deserializes FastHeal=false), so it replays byte-identically.
	var inv InvestigateResult
	dispatchedHeal := false
	if workflow.GetVersion(ctx, "deterministic-guest-heal", workflow.DefaultVersion, 1) >= 1 && fastHeal {
		var fh InvestigateResult
		herr := workflow.ExecuteActivity(investigateCtx, a.DeterministicGuestHealActivity, env).Get(ctx, &fh)
		switch {
		case herr != nil:
			// Dispatch failure (infra) — fall back to the FULL agent loop, the safe direction. Downgrade the
			// class so the fallback investigation and its record agree (STANDARD_AGENT, the pre-fast-path class).
			workflow.GetLogger(ctx).Warn("deterministic guest-heal dispatch failed; falling back to standard investigate",
				"ref", env.ExternalRef, "err", herr)
			res.ExecClass = string(execclass.StandardAgent)
		case fh.Proposed:
			inv = fh
			dispatchedHeal = true
		default:
			// DECLINED — the guest was not confirmed stopped at emission (recovered/unknown). Route to the
			// normal loop rather than a no-action skip: an unconfirmable guest deserves the full investigation.
			workflow.GetLogger(ctx).Info("deterministic guest-heal declined; falling back to standard investigate",
				"ref", env.ExternalRef, "reason", fh.Reason)
			res.ExecClass = string(execclass.StandardAgent)
		}
	}
	if !dispatchedHeal {
		if err := workflow.ExecuteActivity(investigateCtx, a.InvestigateActivity, env, res.ExecClass, memberCtx).Get(ctx, &inv); err != nil {
			// THE INVESTIGATION ITSELF FAILED — record it, then propagate (MECH-003).
			//
			// This path previously returned bare, writing NO triage row, NO ledger entry and NO escalation: a
			// critical incident whose investigation crashed left a failed Temporal workflow and nothing else,
			// so it was invisible to the judge, the console, the eval and every operator surface. The branch
			// twenty lines below already refuses to conflate "a model call failed" with "no action warranted"
			// — this closes the same gap one level up, where the session did not even reach an outcome.
			//
			// The predecessor escalates outright here when severity is critical, precisely so a crashed
			// investigation can never silently drop one (its ERR trap). This branch keeps the DURABLE,
			// judge-visible record (the error is propagated unchanged, so Temporal still fails the workflow and
			// its retry/alerting behaviour is untouched) rather than paging on an INFRASTRUCTURE crash. The
			// substantive-conclusion page that TG-386 wired lives on the no-proposal path below, where the session
			// reached a real outcome — a crashed investigation has no grounded conclusion to hand a human, so the
			// Temporal failure + durable row remain the right signal here (the old "TG cannot page yet, MECH-007"
			// rationale is stale: the Matrix notifier is wired now, but a crash is deliberately not paged).
			//
			// Version-gated on its own key: an in-flight workflow that already took this path recorded no such
			// activity, and replaying it must not invent one.
			if workflow.GetVersion(ctx, "investigate-failure-record", workflow.DefaultVersion, 1) >= 1 {
				res = failedInvestigateResult(res, err)
				res.TriageRecorded = recordTriage(ctx, a, env, res, "", "", nil)
			}
			return res, err
		}
	}
	// Decision-tracer session provenance (spec/020 REQ-2009): stamp the composed-seed fingerprint, the
	// trusted-preamble version, and the model tier onto the result so EVERY terminal record (proposal, grounded
	// stop, or escalate) carries which prompt/seed/tier composed the session. Observability only — no gate reads it.
	res.SeedHash = inv.SeedHash
	res.PromptVersion = inv.PromptVersion
	res.ModelTier = inv.ModelTier
	// TG-198: and WHICH TIER DECIDED. Copied here, beside the investigate tier and before the propose/stop
	// branch, so it reaches the terminal record on every path a session can end on — the same reason the
	// diagnosis copy below sits here rather than in whichever branch happened to be edited. Pure field copy:
	// no command, safe on replay, no version marker needed (a replaying history simply carries "").
	res.DecisionTier = inv.DecisionTier
	res.StepCount = inv.StepCount
	res.Trajectory = inv.Trajectory // TG-525: the digested ordered tool path, out to the eval scorer
	// TG-394 slice 3: and WHICH of TG's own dependency capabilities were degraded when this session ran. Copied
	// here beside the step count, before the propose/stop branch, so the stamp reaches the terminal record on
	// EVERY path a session can end on — a lexical-only investigation must be legible whether it proposed, stood
	// down, or diverted to shadow. Pure field copy — no command, safe on replay (a replaying history carries nil).
	res.DegradedCapabilities = inv.DegradedCapabilities
	// TG-205: and HOW LONG the decision took (axis A6b). Copied here beside the step count, before the
	// propose/stop branch, for the same reason DecisionTier is: a wall-clock that reached only the propose
	// path would leave the axis blind to exactly the sessions that spent the most time and decided not to
	// act. Pure field copy — no command, safe on replay (a replaying history carries 0, i.e. unmeasured).
	res.DecisionMillis = inv.DecisionMillis
	// TG-201: carry the typed claim onto the result HERE — once, before the branch — so it reaches the
	// terminal record on EVERY path the investigation can end on (proposal, shadow proposal, grounded stop,
	// handoff), not only the one that happened to be edited. A claim that survives only the propose branch
	// would leave the diagnosis dimension blind to exactly the sessions where the agent decided not to act.
	// Zero value when nothing was proposed and the model bound no diagnosis. Pure field copy — no command,
	// safe on replay.
	res.Diagnosis = inv.Proposal.Diagnosis
	if !inv.Proposed {
		// No usable proposal ⇒ end the session with no action, no manifest, no prediction, no poll (REQ-1104).
		// Distinguish an ESCALATE handoff from an ordinary grounded stop: an escalate reaching here means the
		// agent hit the cycle/poll handoff limit without ever producing a validated action (the loop returns
		// OutcomeEscalate with the ZERO-value proposal — the only OutcomeEscalate that lands on the no-proposal
		// path; the low-confidence escalate carries a real action and stays proposed). Record it as
		// `escalated:handoff-limit`, NOT `no-proposal:stop` ("no action warranted"), so the judge/console/eval
		// see a human handoff rather than a silently-swallowed escalation.
		res.Outcome = "no-proposal:" + inv.Outcome
		if inv.Outcome == agent.OutcomeEscalate.String() {
			res.Outcome = "escalated:handoff-limit"
		}
		res.Conclusion = inv.Conclusion
		res.EvidenceIDs = inv.ConclusionEvidence
		// WHY it stopped, recorded beside WHAT it concluded. Without this a model-call failure and a considered
		// "no action warranted" are the same row, and 31.5% of sessions land here.
		res.StopReason = inv.Reason
		res.TriageRecorded = recordTriage(ctx, a, env, res, "", "", inv.SkillLoads)
		// TG-386: route a SUBSTANTIVE proposal-less terminal to the human channel. The one investigation of 156
		// that named dc1pve03 as the cascade root cause concluded "no guest-level action is safe — this
		// needs a human on the PVE host", reached this branch, wrote its row and RETURNED — the notify call sat
		// ~200 lines downstream in the propose path, gated on a band this path never sets, so the conclusion
		// reached a Postgres row and stopped, eleven hours before a human found it. Filtered (handoffEligible)
		// so a bare no-action stop does not page, and gated INERT at the activity (Deps.HandoffNotify) so
		// opening this new outward channel is a deliberate operator decision. Version-gated: a replaying
		// pre-change history took no such activity and must not invent one. Best-effort — the session already
		// concluded, so a notifier outage records non-delivery rather than failing the workflow.
		if workflow.GetVersion(ctx, "handoff-notify", workflow.DefaultVersion, 1) >= 1 &&
			handoffEligible(res.Conclusion, res.ExecClass, res.Outcome) {
			hi := NotifyInput{
				DecisionID: env.ExternalRef,
				Handoff:    true,
				Body:       handoffBody(env.AlertRule, env.Host, res.Conclusion, res.StopReason, env.ExternalRef),
			}
			var hn NotifyResult
			if err := workflow.ExecuteActivity(ctx, a.NotifyActivity, hi).Get(ctx, &hn); err == nil {
				res.Notified = hn.Delivered
			}
		}
		return res, nil
	}

	// action identity for the proposed action (pure, deterministic — safe in workflow code).
	actionID, err := inv.Proposal.Action.ID()
	if err != nil {
		return res, err
	}
	planHash := PlanHash(env.ExternalRef, actionID)

	// The ATTRIBUTE step (spec/023, REQ-2300): ask WHO is the actor behind the observed change before
	// classifying. Deterministic (reader evidence → taxonomy → rules-as-data disposition), advisory, and
	// safe-direction-only. Version-guarded: pre-existing histories (no marker) skip the step entirely, so the
	// three classifier flags stay false and old histories replay byte-identically (REQ-2303's inert default).
	var attr AttributeResult
	if workflow.GetVersion(ctx, "actor-attribution", workflow.DefaultVersion, 1) >= 1 {
		if err := workflow.ExecuteActivity(ctx, a.AttributeActivity, AttributeInput{
			Host: env.Host, FaultClass: inv.Proposal.Action.OpClass, Site: env.Site,
		}).Get(ctx, &attr); err != nil {
			return res, err
		}
		res.Attribution = attr.Finding.Taxonomy.String()
		res.SecurityEscalated = attr.Security
		// Persist the minimized evidence blob with the taxonomy (REQ-2311) — actor, verb, timestamp, ref only,
		// never raw log lines (REQ-2313). Marshaling is pure/deterministic (safe in workflow code); a marshal
		// failure drops the blob but never the taxonomy.
		if eb, merr := json.Marshal(attr.Finding.Evidence); merr == nil {
			res.ActorEvidence = eb
		}
		// REQ-2302: the platform's own actuation identity already remediated this (target, fault class) inside
		// the self-recognition window ⇒ terminate already-remediated — NO new actuation, but the terminal row +
		// governance record are written (Constitution 4.15: a session is done only when its knowledge is written
		// back), never a silent drop.
		if attr.SelfNoop {
			res.Outcome = "already-remediated"
			res.Conclusion = "actor-attribution: TG's own actuation identity already remediated this (target, fault class) within the self-recognition window — no re-actuation"
			res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
			reconcileTerminal(ctx, a, env, ReconcileInput{
				ActionID: actionID, HasTerminalResult: true,
				Host: env.Host, AlertRule: env.AlertRule, Action: inv.Proposal.Action.Op,
			})
			return res, nil
		}
	}

	// 1.7) THE SHADOW DIVERT (spec/026 REQ-2603, epic TG-227 plane 1): a proposal whose op_class matched
	// NOTHING in the actuation registry diverts HERE — before classify, before the gate seals a manifest,
	// before NotifyActivity, before the pending-decision projection, and before the vote wait. The old
	// behavior flowed such proposals into the REAL approval lane, where a human could be polled to approve
	// an action that could never execute (the ground-map hazard spec/026 removes). The shadow terminal
	// records the triage row (outcome `proposed:shadow`), appends ONE withheld propose:open ledger
	// decision, and feeds the earned-catalog occurrence seam — and seals nothing: the never-executable
	// chain (nil sealedArgv → empty-argv leaf refusal → never-auto floor → mode chokepoint, REQ-2608) is
	// upstream of this branch and untouched by it.
	//
	// The predicate was resolved in the INVESTIGATE ACTIVITY (inv.OpClassRegistered) so this branch is
	// replay-deterministic: the registry lookup's answer is in history, and when spec/028's composed
	// embedded+overlay registry makes the lookup deployment-dependent, replays still see the recorded
	// answer. GetVersion-guarded: pre-existing histories (no marker) keep the old lane byte-identically.
	if workflow.GetVersion(ctx, "shadow-propose", workflow.DefaultVersion, 1) >= 1 && !inv.OpClassRegistered {
		res.Outcome = "proposed:shadow"
		res.Proposed = true
		res.ActionID = actionID
		res.Conclusion = inv.Proposal.Rationale
		res.EvidenceIDs = inv.Proposal.EvidenceIDs
		res.Confidence = inv.Confidence
		// One attempt, like the vote record: the activity appends a ledger decision, and a Temporal
		// activity is at-least-once — a retry would forge a SECOND propose:open chain entry for one
		// proposal. Failing loses a shadow record (the honest direction); the triage insert is
		// first-wins-idempotent regardless.
		shadowCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		shadowErr := workflow.ExecuteActivity(shadowCtx, a.ShadowProposalActivity, ShadowProposalInput{
			ActionID: actionID,
			Target:   inv.Proposal.Action.Target,
			Row: judge.TriageRow{
				ExternalRef: env.ExternalRef,
				Host:        env.Host,
				AlertRule:   env.AlertRule,
				Band:        res.Band, // "" — the shadow lane never classifies; honestly absent, not invented
				Outcome:     res.Outcome,
				Proposed:    true,
				Op:          inv.Proposal.Action.Op,
				OpClass:     inv.Proposal.Action.OpClass,
				StepCount:   res.StepCount,
				// TG-205: the shadow lane records the wall-clock too (axis A6b). It is the lane where TG
				// reasons about a fault class it has no registered op for — the SLOWEST reasoning it does,
				// so excluding it would systematically understate time-to-decision.
				DecisionMillis: res.DecisionMillis,
				EvidenceIDs:    res.EvidenceIDs,
				Conclusion:     res.Conclusion,
				UndoSketch:     inv.Proposal.UndoSketch,
				Confidence:     res.Confidence,
				Attribution:    res.Attribution,
				// REQ-2610: the structured actor evidence REUSES the migration-0035 column — the same
				// minimized blob the attribute step produced rides the shadow row unchanged.
				ActorEvidence: res.ActorEvidence,
				SkillLoads:    inv.SkillLoads,
				PromptVersion: res.PromptVersion,
				SeedHash:      res.SeedHash,
				ModelTier:     res.ModelTier,
				DecisionTier:  res.DecisionTier, // TG-198: which tier DECIDED, not which one read
				// TG-201: the shadow lane records the claim too. It is the lane where TG proposes a fix it has
				// no registered op-class for — the LEAST-charted reasoning it does, and therefore the reasoning
				// most worth grading. Excluding it would leave the diagnosis dimension blind to exactly the
				// sessions the open-proposal plane exists to accrue evidence from.
				Diagnosis:         res.Diagnosis,
				DiagnosisRecorded: true,
				// TG-394 slice 3: the shadow lane records the degraded-capability set too — it is the lane where TG
				// reasons over an unregistered fault class, precisely when knowing its retrieval ran lexical-only
				// matters most for grading the reasoning later.
				DegradedCapabilities: res.DegradedCapabilities,
				// TG-527: the ordered tool path persists on the shadow row too — the least-charted reasoning
				// TG does is exactly the trajectory most worth re-judging later.
				Trajectory: trajectoryRows(res.Trajectory),
				CreatedAt:  workflow.Now(ctx).UTC(),
			},
		}).Get(ctx, nil)
		// REQ-2605 is a law: if the shadow activity failed (which after its single attempt means no
		// propose:open chain entry), the run fails LOUDLY rather than reporting a clean proposed:shadow
		// terminal that the governance chain never saw. Safe pre-deploy change inside the same version
		// marker: no production history with "shadow-propose">=1 exists before this branch merges.
		if shadowErr != nil {
			return res, shadowErr
		}
		return res, nil
	}

	// 2) classify the proposed action → band + one session_risk_audit row.
	var decision risk.Decision
	ci := ClassifyInput{
		ExternalRef: env.ExternalRef,
		ActionID:    actionID,
		PlanHash:    planHash,
		RiskLevel:   riskLevel(env.Severity),
		AlertRule:   env.AlertRule,
		OpClass:     inv.Proposal.Action.OpClass,
		Op:          inv.Proposal.Action.Op,
		Host:        inv.Proposal.Action.Target,
		// The STATED rationale, carried for the TG-317 cross-check against the sealed target above. Data
		// only: it is compared token-wise and never executed, and a disagreement escalates the band rather
		// than refusing — the reviewer who reads the prose is the one who can adjudicate it.
		Rationale: inv.Proposal.Rationale,
		// IncidentHost is the ingest-validated alerted device (env.Host) — the STABLE novelty subject.
		// Host above remains the action target (what the action TOUCHES) for blast-radius / criticality /
		// canary / self-protected / evidence-relevance signals; novelty keys on the subject instead, because
		// the target is an LLM-expressed string that varies per proposal (guest vs PVE node) for the same
		// fault, so keying novelty on it makes de-novel fail to transfer (TG-124). Subject = the alerted device.
		IncidentHost: env.Host,
		Reversible:   inv.Proposal.Action.Reversible,
		// Screen the untrusted model output for prompt-injection/jailbreak (pure, deterministic — safe in
		// workflow code). A hit forces POLL_PAUSE: an injected instruction may be steering the proposal.
		Jailbreak: screen.IsJailbreak(inv.Proposal.Rationale + " " + inv.Proposal.ApprovalChoice),
		// A mutating action targeting a stateful workload (DB/queue/store/statefulset) never auto-resolves.
		Stateful: safety.IsStatefulWorkload(actionSafetyParts(inv.Proposal.Action)...),
		// Derive destructiveness from the ACTUAL op (+ the model's rationale), overriding a model that
		// under-declares its op_class — "a plan cannot hide a mutation".
		Destructive: safety.IsDestructiveOp(append(actionSafetyParts(inv.Proposal.Action), inv.Proposal.Rationale)...),
		// The evidence the proposal cited + the orchestrator-captured tool results it must bind to (INV-11
		// silent-cognition guard). The model's citation is DATA; the classifier verifies it against captures.
		EvidenceIDs: inv.Proposal.EvidenceIDs,
		ToolResults: inv.ToolResults,
		// The [AUTO-RESOLVE] marker parsed from the model's approval_choice — DATA, never authority. Deriving it
		// from the marker (not from whether any evidence id was merely cited) is what lets the silent-cognition
		// guard catch a marked auto-resolve that binds ZERO evidence, the case the predecessor's Runner suppresses.
		AutoResolveMarked: autoResolveMarked(inv.Proposal.ApprovalChoice),
		// A high-risk alert category (maintenance/security-incident/deployment) forces a poll by default:
		// these classes END in an infra change regardless of how reversible each op looks. The category rides
		// in on the normalized `category` provider label; an absent/unknown category is not high-risk (safe —
		// the mechanical floor still governs it).
		HighRiskCategory: safety.HighRiskCategory(env.Labels["category"]),
		// The actor-attribution dispositions from the attribute step (spec/023) — safe-direction only;
		// zero when the step is version-guarded off (pre-existing histories) or unattributable (REQ-2303).
		AttributionStandDown: attr.StandDown,
		AttributionSecurity:  attr.Security,
		AttributionEscalate:  attr.Escalate,
		Attribution:          res.Attribution,
	}
	if err := workflow.ExecuteActivity(ctx, a.ClassifyActivity, ci).Get(ctx, &decision); err != nil {
		return res, err
	}
	res.Band = decision.Band.String()

	// TG-80 P2-6: the KILLED terminal. When the output screen hit (the classifier polled with
	// poll_reason=jailbreak-detected) and the deployment ARMED the kill (TG_SCREEN_KILL_TERMINAL, read
	// in the activity — a workflow must not touch the environment), the session ends HERE with a
	// first-class hostile terminal: no gate, no manifest, no vote for a proposal an injected
	// instruction may be steering. Unarmed (the shipped default) or on any activity error, the path
	// falls through to today's POLL_PAUSE flow byte-identically. The hostile disposition
	// ("jailbreak-output" / "repeat-offender:<n>") was computed at classify and rides Decision.Signals
	// onto the terminal outcome and the audit row either way.
	if decision.Signals["poll_reason"] == "jailbreak-detected" &&
		workflow.GetVersion(ctx, "screen-kill-terminal", workflow.DefaultVersion, 1) >= 1 {
		disp := decision.Signals["hostile_disposition"]
		if disp == "" {
			disp = "jailbreak-output"
		}
		var sk ScreenKillResult
		if err := workflow.ExecuteActivity(ctx, a.ScreenKillActivity,
			ScreenKillInput{ExternalRef: env.ExternalRef, ActionID: actionID, Disposition: disp}).Get(ctx, &sk); err == nil && sk.Armed {
			res.Outcome = "killed:hostile-output — " + disp
			res.StopReason = "screen-kill"
			recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
			return res, nil
		}
	}

	// 3) gate — commit the prediction, seal the content-hashed ActionManifest, build the poll.
	var gate GateResult
	gi := GateInput{Proposal: inv.Proposal, Band: decision.Band, PlanHash: planHash, Site: env.Site, CommonCause: predict.SiblingsEligible(env.AlertRule)}
	if err := workflow.ExecuteActivity(ctx, a.GateActivity, gi).Get(ctx, &gate); err != nil {
		return res, err
	}
	// the gate's sealed manifest must bind exactly the action we derived (INV-07).
	if gate.ActionID != actionID {
		res.Outcome = "action_id mismatch — re-gate"
		return res, nil
	}
	res.ActionID = gate.ActionID
	res.PollBuilt = gate.PollBuilt
	res.Proposed = true
	// Surface the evidence the proposal cited (the classifier already bound it against the captured tool
	// results at INV-11) so the ledger, console and eval can see WHAT grounded the action — not just that it
	// was proposed. This is a read of already-committed data; it does not participate in action identity.
	res.EvidenceIDs = inv.Proposal.EvidenceIDs
	// Surface the machine prediction the gate just committed (TG-61) so the terminal TriageRow carries it and
	// the LIVE judge cron scores falsifiable_prediction over the same rendered line the offline eval judges —
	// reaching the gate IS the commit, so Predicted is true here. Also a pure read of committed data.
	res.Prediction = gate.Prediction
	res.Predicted = true
	// Carry the PROPOSAL's rationale into the record's conclusion — the investigation's grounded reasoning for
	// THIS action. agent.Result.Conclusion is set ONLY on a stop/handoff (agent/loop.go), so without this a
	// proposing triage recorded an EMPTY conclusion — the "hollow proposal" the judge scored ~1.15/5 (TG-79 G1).
	// The reasoning existed in the Proposal all along; it was simply never copied to the durable record.
	res.Conclusion = inv.Proposal.Rationale
	// Carry the agent's emitted confidence into the durable record for the decision tracer + calibration
	// (spec/020 REQ-2003). This reads InvestigateResult.Confidence — the SAME agent.Result.Confidence the loop
	// used LIVE to gate stop/escalate/propose — NOT the nested proposal.Confidence field (a key the model is
	// never asked to emit, so it was always the zero-value 0 → session_triage.confidence was 0 on every row).
	//
	// It is no longer observability-only. TG-408: the SAME inv.Confidence is now threaded into ExecuteInput
	// and on into actuate.Request, which is the policy min_confidence clamp's input (REQ-1507). That field
	// was never assigned anywhere in the tree, so the clamp compared 0.0 against every threshold — a
	// fail-closed control that could only ever fire, and therefore could never be switched on.
	res.Confidence = inv.Confidence

	// 3.4) spec/030 T-030-4: THE RECIPE LOOKUP AT THE PROPOSE TERMINAL. A POLL_PAUSE proposal's op-class
	// may select a declared multi-step recipe (pure lookup, REQ-3001 — the model chose an op-class, never
	// a plan); a match widens what the poll OFFERS the human from the single action to the whole plan,
	// bound by the content-addressed plan_id (REQ-3002). Every compose failure direction is Matched=false
	// — the single action as today. Version-guarded: pre-existing histories never scheduled the compose
	// activity; and with the shipped-empty catalog (REQ-3007) new histories compose nothing either, so the
	// lane is structurally present and behaviorally inert until an owner-reviewed recipe lands (T-030-6).
	// AUTO/AUTO_NOTICE sessions never compose: a plan ALWAYS takes the one human vote, and hijacking an
	// auto-band action into a wider plan would change actuation posture without a ruling.
	var plancomp ComposePlanResult
	if decision.Band == safety.BandPollPause && workflow.GetVersion(ctx, "plan-compose", workflow.DefaultVersion, 1) >= 1 {
		if err := workflow.ExecuteActivity(ctx, a.ComposePlanActivity, ComposePlanInput{
			OpClass: inv.Proposal.Action.OpClass, Target: inv.Proposal.Action.Target,
			Params: inv.Proposal.Action.Params, ExternalRef: env.ExternalRef,
		}).Get(ctx, &plancomp); err != nil {
			plancomp = ComposePlanResult{} // the lookup is an OFFER, never a gate — fail toward the single action
		}
	}
	// planBindID is what the ONE vote must name (INV-12): the sealed single action, or — when a plan is
	// offered — the plan token, so an approval minted for different content can never release this plan.
	planBindID := gate.ActionID
	if plancomp.Matched {
		planBindID = "plan:" + plancomp.PlanID
	}

	// 3.5) deliver the governance notice to on-call for the notice/poll bands — AUTO_NOTICE pages, POLL_PAUSE
	// solicits an approval vote (CONSTITUTION: "AUTO_NOTICE/POLL_PAUSE route to the configured on-call
	// group"). AUTO is the silent lane (no page). Best-effort and fire-and-forget: paging is the Phase-0/1
	// human channel (not an estate mutation), and NotifyActivity is fail-open so a notifier outage never
	// fails the Runner. The vote-consuming resume is Phase 2.
	if decision.Band != safety.BandAuto {
		ni := NotifyInput{
			DecisionID: env.ExternalRef,
			Body:       noticeBody(decision.Band, env.AlertRule, inv.Proposal.Action.Target, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, env.ExternalRef),
			Approval:   decision.Band == safety.BandPollPause,
		}
		if ni.Approval {
			// The poll MENU. One option per proposed plan, then the two governance outs.
			//
			// TG's agent proposes exactly ONE action today, so this renders as a three-option poll. The
			// shape is a LIST because the interesting decision is between candidate remediations, and
			// when the agent begins emitting alternatives they arrive here as extra plan options with no
			// change to the notifier, the wire format, or the vote binding.
			ni.Choices = approvalChoices(env.ExternalRef, []string{
				fmt.Sprintf("%s %s on %s", inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.Proposal.Action.Target),
			})
			if plancomp.Matched {
				// The plan poll presents the WHOLE sequence as the one offer (REQ-3002): every step and its
				// compensation, one approve/deny. It replaces the single-action option — the plan is what the
				// vote decides, and two options with one binary vote would make the approval ambiguous.
				ni.Body = fmt.Sprintf("TRANSACTION PLAN %s (%d steps, all-or-nothing, auto-revert on failure) for %s:\n%s",
					plancomp.Recipe, len(plancomp.Steps), env.ExternalRef, strings.Join(plancomp.PollLines, "\n"))
				ni.Choices = approvalChoices(env.ExternalRef, []string{
					fmt.Sprintf("PLAN %s: %s", plancomp.Recipe, strings.Join(plancomp.PollLines, "; ")),
				})
			}
		}
		var notified NotifyResult
		if err := workflow.ExecuteActivity(ctx, a.NotifyActivity, ni).Get(ctx, &notified); err != nil {
			return res, err
		}
		res.Notified = notified.Delivered
	}

	// 3.6) the vote-consuming wait (REQ-1105, INV-12): a POLL_PAUSE proposal WAITS for the authenticated
	// human vote the poll solicited — the missing half of governed autonomy. Approve releases exactly this
	// action (threaded as Approved into the interceptor's admission gate); deny stands the session down; an
	// ignored poll times out to DENY (never a silent approval). Every human decision is recorded on the
	// hash-chained ledger (INV-19) via an activity. The signal is keyed to THIS workflow (external_ref = the
	// decision id), so a vote cannot misattribute across sessions. Versioned so pre-vote histories replay.
	approved := false
	approvedVoter := "" // the ONE vote's voter — carried into the plan hand-off (spec/030 T-030-4)
	if decision.Band == safety.BandPollPause {
		// project the open POLL_PAUSE decision so the console can LIST it (REQ-519) — best-effort, fail-open:
		// a read convenience, never the authority (the vote is delivered to THIS workflow directly). Guarded
		// by its own change marker so pre-existing histories replay deterministically.
		if workflow.GetVersion(ctx, "pending-projection", workflow.DefaultVersion, 1) >= 1 {
			pd := PendingDecisionInput{
				ExternalRef: env.ExternalRef,
				ActionID:    planBindID,
				Approaches:  []string{fmt.Sprintf("%s %s on %s", inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.Proposal.Action.Target)},
				Prediction:  noticeBody(decision.Band, env.AlertRule, inv.Proposal.Action.Target, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, env.ExternalRef),
				Reversible:  inv.Proposal.Action.Reversible,
				Site:        env.Site,
				OpenedAt:    workflow.Now(ctx),
			}
			if plancomp.Matched {
				// The projection row IS the vote intake's server-side binding (the console and the matrix
				// lane both resolve the action id to vote on FROM this row) — so for a plan it must carry
				// the plan token and render every step + compensation (REQ-3002).
				pd.Approaches = plancomp.PollLines
				pd.Prediction = fmt.Sprintf("TRANSACTION PLAN %s — %d steps, all-or-nothing, one approval, auto-revert on any step failure", plancomp.Recipe, len(plancomp.Steps))
				pd.Reversible = true // admission requires every step SafelyCompensatable (REQ-3004)
			}
			_ = workflow.ExecuteActivity(ctx, a.RecordPendingActivity, pd).Get(ctx, nil)
		}
		// Record the triage NOW — the investigation + proposal is COMPLETE at this point. The durable
		// judge/flywheel/benchmark record must NOT be hostage to the human EXECUTION vote (up to VoteWait=24h):
		// the vote decides whether to EXECUTE; the triage already happened. RecordTriage's ON CONFLICT DO NOTHING
		// makes the later terminal record a harmless no-op, so "proposed" here is the triage disposition — the
		// vote disposition lives on the ledger + pending projection, not this row (TG-79 G1 / benchmark unblock).
		if workflow.GetVersion(ctx, "triage-record-at-propose", workflow.DefaultVersion, 1) >= 1 {
			res.Outcome = "proposed"
			res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
		}
		if v := workflow.GetVersion(ctx, "vote-wait", workflow.DefaultVersion, 1); v >= 1 {
			// The human's TERMINAL decision must be DURABLE before anything proceeds, and never
			// double-recorded: this record runs with at most ONE attempt — a record failure fails the
			// session closed (stood down, reconciled by a human) rather than risking a duplicate ledger
			// append on retry. This fail-closed record is used ONLY for the operator's own approve/deny/
			// timeout/abandon decision — never for attacker-reachable noise (see below), so a transient
			// ledger blip during a flood cannot be weaponized to tear down a legitimately-waiting session.
			recCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout: time.Minute,
				RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
			})
			record := func(decision, voter string) error {
				var rec RecordVoteResult
				return workflow.ExecuteActivity(recCtx, a.RecordVoteActivity,
					RecordVoteInput{Decision: decision, ActionID: planBindID, ExternalRef: env.ExternalRef, Voter: voter}).Get(ctx, &rec)
			}

			// Wait for the vote that names THIS sealed action. A vote naming any other action id (blind,
			// premature, stale, or misdirected) is IGNORED and only COUNTED — the wait continues, so a
			// buffered pre-poll approval can never release an action the human did not see (INV-12). Misbound
			// votes are NOT ledger-written per-vote and schedule NO activity: doing so let an authenticated
			// operator flood a waiting session's history/ledger unboundedly (Temporal terminating the session,
			// voiding deny-by-default) and let a misbound vote timed to a ledger blip kill the session. They
			// are summarized in ONE record at the end. A sustained flood past maxMisbound stands the poll down
			// as an abandonment (deny — fail-closed), bounding history: an attacker can at worst force a DENY,
			// never an approval, and it is audited as abuse.
			const maxMisbound = 64
			// maxRefused bounds the SAME abuse for votes that name the right action but come from a
			// non-approver (spec/015 REQ-1516). It mirrors maxMisbound deliberately: an attacker's ceiling is
			// a DENY, never an approval, and the flood is audited rather than absorbed silently.
			const maxRefused = 64
			voteCh := workflow.GetSignalChannel(ctx, VoteSignalName)
			timedOut := false
			var vote VoteSignal
			gotVote := false
			misbound := 0
			timer := workflow.NewTimer(ctx, VoteWait)
			// Session wall-clock BUDGET (design-wisdom #8): race a total-time deadline against the vote wait —
			// the vote wait is the session's one long-parking surface. WHEN the budget is exhausted before a
			// decision arrives, stop to the SAME orphaned-poll hand-off a timeout uses (never park past the
			// ceiling). In production the budget (VoteWait + compute headroom) never fires before the 24h vote
			// timeout, so the poll window is fully honored — it bites only a session already over its total
			// ceiling. GetVersion-guarded so pre-existing histories (which never scheduled this timer) replay
			// deterministically: an in-flight poll started before this change adds no budget branch on replay.
			budgetOut := false
			budgetActive := workflow.GetVersion(ctx, "wallclock-budget", workflow.DefaultVersion, 1) >= 1
			var budgetTimer workflow.Future
			if budgetActive {
				budgetTimer = workflow.NewTimer(ctx, budgetRemaining(ctx, budgetStart))
			}
			// OBSOLETE-ON-RECOVERY (C2): while the poll waits for a human, periodically re-check whether the
			// incident's SUBJECT already recovered on its own (a provider recovery push TG captured after the
			// poll opened — RecoveredSince over ingest_transition, TG's OWN evidence, never the model, INV-11).
			// A self-resolved incident's poll is MOOT, so close it obsolete (a fail-closed stand-down, NEVER an
			// approval — no actuation) rather than parking for VoteWait. Version-guarded so pre-existing
			// histories (no marker ⇒ DefaultVersion) never schedule the recheck timer/activity and replay
			// byte-identically; a nil RecoveredSince seam makes the recheck inert (it never fires obsolete).
			obsoleted := false
			openedAt := workflow.Now(ctx)
			useObsolete := workflow.GetVersion(ctx, "vote-wait-obsolete-on-recovery", workflow.DefaultVersion, 1) >= 1
			var recoveryTimer workflow.Future
			if useObsolete {
				recoveryTimer = workflow.NewTimer(ctx, voteRecoveryRecheckInterval)
			}
			// APPROVER ADMISSION (spec/015 REQ-1516, TG-254): a vote that correctly names the sealed action
			// still only DECIDES it when the voter is a MEMBER of the poll's approve_by set — resolved at gate
			// time and carried here in history. Before this, the ActionID bind was the ONLY check, so any
			// authenticated operator could release any governed action. That is the whole defect.
			//
			// ...but ONLY WHEN THE BUNDLE DECLARES AN APPROVER REGIME (gate.ApproveByConfigured). On a bundle
			// where no rule carries approve_by, every poll resolves an EMPTY set, so enforcing would refuse
			// approve AND deny from everyone and converge every session on `human:timeout` after VoteWait — an
			// invisible brick of the poll lane, which on an actuating deployment is worse than the defect it
			// closes. Unconfigured therefore admits, LOUDLY (a ledger record per admitted vote, below).
			//
			// Version-guarded because it changes the behaviour of an IN-FLIGHT poll: a poll that opened before
			// this change recorded a GateResult with neither ApproveBy nor ApproveByConfigured, so on replay it
			// would take the unconfigured branch and schedule a ledger activity its history never contained. An
			// old history (no marker, so DefaultVersion) skips this block entirely and replays byte-identically.
			// The marker stays at version 1 rather than advancing: version 1 has never been merged or deployed,
			// so no history anywhere was recorded under the enforce-always reading it originally carried.
			refused := 0
			admissionActive := workflow.GetVersion(ctx, "vote-approver-admission", workflow.DefaultVersion, 1) >= 1
			for !gotVote && !timedOut && !budgetOut && !obsoleted && misbound < maxMisbound && refused < maxRefused {
				var recv VoteSignal
				received := false
				recheck := false
				sel := workflow.NewSelector(ctx)
				sel.AddReceive(voteCh, func(c workflow.ReceiveChannel, _ bool) {
					c.Receive(ctx, &recv)
					received = true
				})
				sel.AddFuture(timer, func(workflow.Future) { timedOut = true })
				if budgetActive {
					sel.AddFuture(budgetTimer, func(workflow.Future) { budgetOut = true })
				}
				if useObsolete {
					sel.AddFuture(recoveryTimer, func(workflow.Future) { recheck = true })
				}
				sel.Select(ctx)
				if recheck {
					// The recovery-recheck timer fired: has the subject recovered since the poll opened?
					var recovered bool
					// ★ AlertRule IS REQUIRED AND WAS OMITTED HERE. RecoveredSinceActivity fails CLOSED on an
					// empty AlertRule — deliberately, so an unrelated flapping rule on the same host cannot
					// confirm THIS incident's recovery. This call site passed only Host and Since, so the
					// recheck always answered "not recovered", `obsoleted` never fired, and every poll parked
					// until VoteWait expired. The sibling call at the clear-confirm belt below has always
					// passed it; these two disagreed and nothing noticed.
					//
					// Measured live 2026-07-29: 138 open decisions, oldest ref ~4,000 sessions stale, and the
					// governance ledger's entire recent tail was `human:timeout` — with every target guest
					// already running for 6–12h. The self-recovery closure this branch exists to perform had
					// never once executed.
					if err := workflow.ExecuteActivity(ctx, a.RecoveredSinceActivity,
						RecoveredSinceInput{Host: env.Host, AlertRule: env.AlertRule, Since: openedAt}).Get(ctx, &recovered); err == nil && recovered {
						obsoleted = true
					} else {
						recoveryTimer = workflow.NewTimer(ctx, voteRecoveryRecheckInterval) // re-arm the next recheck
					}
					continue
				}
				if !received {
					continue // the vote-wait timer or the budget deadline fired
				}
				if recv.ActionID != planBindID {
					// counted only — never a per-vote activity/ledger write (the round-1 DoS). For a plan
					// offer this is ALSO the plan-identity bind (REQ-3002): a vote naming the sealed single
					// action — or any stale plan — was minted for content the human did not decide here.
					misbound++
					continue
				}
				if admissionActive && !VoterAdmitted(gate.ApproveBy, recv.Voter) {
					if gate.ApproveByConfigured {
						// CONFIGURED BUNDLE, NOT AN APPROVER: the vote is REFUSED and the poll KEEPS WAITING —
						// it must not resolve, because resolving it (either way) would let a non-member decide
						// the action's fate, which is the same authority they are being denied. Unlike a
						// misbound vote this IS ledger-written per vote: a stranger attempting to release a
						// governed action is a security event and must be visible by voter, not summarized into
						// a count. Best-effort (`_ =`) on purpose — this is attacker-reachable, so a ledger blip
						// timed to a flood must never tear down a legitimately waiting session (the same
						// reasoning the misbound branch documents above).
						_ = record("human:vote-refused:not-in-approve-by", recv.Voter)
						refused++ // bounded like misbound, so a non-member cannot grow history without limit
						continue
					}
					// UNCONFIGURED BUNDLE: NO rule declares approve_by, so the operator has expressed no
					// opinion about who may approve and this admission is a NO-OP — the vote proceeds exactly
					// as it did before REQ-1516 was enforced. Refusing here instead would resolve an EMPTY set
					// for EVERY poll, refuse approve and deny alike, and time every POLL_PAUSE session out at
					// `human:timeout` after VoteWait: "any operator can approve anything" traded for "NO
					// operator can approve anything, invisibly", on a deployment that actuates.
					//
					// BUT NOT SILENTLY — a silent no-op is the failure mode this repo keeps repeating. The vote
					// that WOULD have been refused is recorded by voter, so the exposure is COUNTABLE from the
					// ledger (how many approvals landed with no approver regime, and from whom) instead of
					// being visible only in a boot log nobody re-reads. Best-effort for the same reason the
					// refusal above is: an accounting write must never fail a session that is deciding.
					_ = record("human:vote-admitted-unconfigured", recv.Voter)
				}
				vote, gotVote = recv, true
			}

			switch {
			case gotVote && vote.Approve:
				approved = true
				approvedVoter = vote.Voter
				res.Vote = "approved"
				if err := record("human:approve", vote.Voter); err != nil {
					return res, err
				}
			case gotVote:
				res.Vote = "denied"
				if err := record("human:deny", vote.Voter); err != nil {
					return res, err
				}
			case refused >= maxRefused:
				res.Vote = "refused"
				// A sustained flood of votes from principals who are not in approve_by. Every one was already
				// recorded by voter above; this is the terminal stand-down naming the abuse. Fail-closed
				// (a DENY), like the misbound flood — a non-approver can at worst cost the action a human
				// re-open, never gain an approval they were refused.
				if err := record("human:poll-abandoned:excess-refused-votes", ""); err != nil {
					return res, err
				}
			case misbound >= maxMisbound:
				res.Vote = "abandoned"
				// a flooded/abused poll denies by default (fail-closed), recorded once naming the abuse.
				if err := record("human:poll-abandoned:excess-misbound-votes", ""); err != nil {
					return res, err
				}
			case budgetOut:
				res.Vote = "budget-exceeded"
				// the session exhausted its total wall-clock budget while awaiting a decision — stand down
				// fail-closed (deny by default, never a silent approval) and record it once naming the budget
				// stop, so a runaway session is audited, not silently swallowed. Same at-most-once record path.
				if err := record("session:budget-exceeded", ""); err != nil {
					return res, err
				}
			case obsoleted:
				res.Vote = "obsolete"
				// the incident's SUBJECT recovered on its own while the poll awaited a human — the decision is
				// MOOT. Stand down fail-closed (NOT an approval — no actuation is authorized) and record it once
				// naming the self-recovery, so a self-resolved incident closes its poll instead of parking for
				// VoteWait and accumulating as a stale open decision. The recovery is TG's OWN captured evidence.
				if err := record("human:poll-obsolete:subject-recovered", ""); err != nil {
					return res, err
				}
			default:
				res.Vote = "timeout"
				// an ignored poll denies by default — never a silent approval
				if err := record("human:timeout", ""); err != nil {
					return res, err
				}
			}

			// Drain any votes buffered AT decision time (bounded — ReceiveAsync never waits): a vote that
			// arrived after the loop exited (e.g. a matching vote racing a timeout, or a corrective vote
			// after the first decision) is counted as "late" so it is never a 202-accepted-yet-unrecorded
			// receipt (INV-19). It changes nothing — the terminal decision above stands (first vote wins, no
			// revocation) — but it is accounted for.
			late := 0
			for {
				var extra VoteSignal
				if !voteCh.ReceiveAsync(&extra) {
					break
				}
				late++
			}
			// Accountability without per-vote growth: summarize ignored votes (misbound during the wait +
			// any late-arriving) in ONE best-effort record (a summary failure must NOT fail a session that
			// already reached its terminal decision — the authorization above is what must be durable).
			if (misbound > 0 || late > 0) && res.Vote != "abandoned" && res.Vote != "refused" {
				_ = record(fmt.Sprintf("human:votes-ignored:misbound=%d,late=%d", misbound, late), "")
			}

			// resolve the console projection with the terminal outcome (REQ-519) — best-effort, fail-open,
			// under the SAME change marker as the open above so pre-existing histories replay deterministically.
			// It runs for BOTH approve and deny/timeout/abandon, so a decided decision leaves the approvals list.
			if workflow.GetVersion(ctx, "pending-projection", workflow.DefaultVersion, 1) >= 1 {
				_ = workflow.ExecuteActivity(ctx, a.ResolvePendingActivity, ResolvePendingInput{
					ExternalRef: env.ExternalRef,
					ActionID:    planBindID,
					Outcome:     res.Vote,
					ResolvedAt:  workflow.Now(ctx),
				}).Get(ctx, nil)
			}

			// spec/020 T-020-4 (REQ-2006): record the human approval choice on the sealed manifest — for BOTH
			// approve and deny/timeout/abandon, so the tracer shows how the action resolved. Observe-only, fail-open.
			// NOT for a plan offer: the vote decided the PLAN, not the sealed single action (whose manifest
			// stays an unresolved audit artifact of the proposal); each plan step's own manifest carries its
			// own lifecycle through the chain.
			if !plancomp.Matched {
				backfillManifest(ctx, a, env.ExternalRef, gate.ActionID, res.Vote, "")
			}

			if !approved {
				res.Outcome = "proposal " + res.Vote + " — stood down without mutation"
				// A budget-exceeded stop is a runaway-session hand-off, not an ordinary poll disposition —
				// surface it distinctly (like the cycle-limit "escalated:*") so the judge/console/eval see a
				// wall-clock budget stop, never a silent human timeout.
				if res.Vote == "budget-exceeded" {
					res.Outcome = "escalated:budget-exceeded"
				}
				res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
				// Terminal close-out (spec/003): an UNANSWERED poll — a timeout OR a budget-exceeded stop — is
				// an ORPHANED poll: the reconciler flags it for a delayed re-check and reconcileTerminal hands it
				// off to the escalation lane, so an unresolved incident is re-examined and converges to a human,
				// never silently dropped (the reconcile→escalation hand-off). A denied/abandoned poll is
				// human-owned and left open (To Verify). Fail-safe; the chokepoint still refuses in a non-actuating mode.
				reconcileTerminal(ctx, a, env, ReconcileInput{
					ActionID:          gate.ActionID,
					Band:              decision.Band,
					HasTerminalResult: true,
					PollUnanswered:    res.Vote == "timeout" || res.Vote == "budget-exceeded",
				})
				return res, nil
			}
		}
	}

	// 3.7) spec/030 T-030-4: THE APPROVED PLAN LANE. The one vote above approved the WHOLE plan (bound
	// to plan:<plan_id>), so the session hands the sequence to the saga child and the single action
	// NEVER executes — the plan replaced it in the offer the human decided. The child re-enters the
	// same governed machinery for every step (its own classify, its own sealed manifest, the unchanged
	// interceptor chain on the actuation queue); a step failure compensates in reverse, and a failed
	// compensation pages + trips inside the child (REQ-3004/3005). Reachable only when plancomp.Matched,
	// which is version-guarded above and impossible until a recipe is declared.
	if plancomp.Matched {
		cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			WorkflowID: "plan/" + env.ExternalRef, // one plan lane per session; the plan_id binds content, the ref binds the session
		})
		var pres TransactionPlanResult
		perr := workflow.ExecuteChildWorkflow(cctx, TransactionPlanWorkflow, TransactionPlanInput{
			PlanID: plancomp.PlanID, Recipe: plancomp.Recipe, ExternalRef: env.ExternalRef,
			Site: env.Site, RiskLevel: riskLevel(env.Severity), Host: env.Host,
			Steps:       plancomp.Steps,
			EvidenceIDs: inv.Proposal.EvidenceIDs, ToolResults: inv.ToolResults,
			ApprovedVoter: approvedVoter,
		}).Get(ctx, &pres)
		if perr != nil {
			// The child died session-fatally (its own terminal frame + ledger narration already recorded
			// what it could). Surface the fatal terminal; nothing here retries a mutation.
			res.Outcome = "plan:session-fatal — " + perr.Error()
		} else {
			res.Outcome = fmt.Sprintf("plan:%s — %d/%d step(s) executed, %d compensated", pres.Terminal, pres.StepsExecuted, len(plancomp.Steps), pres.StepsCompensated)
			if len(pres.AppliedRemaining) > 0 {
				res.Outcome += fmt.Sprintf("; steps %v REMAIN APPLIED", pres.AppliedRemaining)
			}
			res.Mutated = pres.StepsExecuted > 0 || pres.StepsCompensated > 0
		}
		res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
		reconcileTerminal(ctx, a, env, ReconcileInput{
			ActionID:          gate.ActionID,
			Band:              decision.Band,
			HasTerminalResult: true,
		})
		return res, nil
	}

	// 4) execute + verify — routed through the wired-by-construction interceptor chain (spec/013), NOT a
	// direct OS call. In a non-actuating mode the chain refuses at GuardMutation and records the refusal, so the
	// Runner still stops at propose — but through the REAL governed path, not around it. Mutated stays false
	// until the mutation key is turned (Phase-2, behind the proven gate). Approved carries the human vote
	// (INV-12): only a voted POLL_PAUSE action reaches the interceptor's admission gate with it true.
	// A mutation is NEVER automatically retried: Temporal activities are at-least-once, and re-running the
	// actuator on a timeout/blip would execute the estate change N times under ONE human approval. The
	// execute activity gets exactly one attempt; an ambiguous failure surfaces as a failed session for a
	// human to reconcile (the idempotent short-circuit inside the activity also refuses a re-run whose
	// verdict already exists). Fail closed, not fail again.
	//
	// TG-153 — THE ACTUATION QUEUE. This is the ONLY activity in the Runner that can reach a credential
	// which mutates the estate, so it is the only one pinned to tg.TaskQueueActuate. Everything above it
	// (investigate, in particular, which drives the LLM agent over untrusted alert/syslog/host content)
	// stays on tg.runner. Pinning the queue here is what lets an operator run the two on DIFFERENT
	// PROCESSES: the triage process never polls tg.actuate, so it is not "refused" the actuation
	// credential — it is never handed the work, and it does not hold the key to do it with.
	//
	// Behaviour-preserving for every existing deployment: TG_CREDENTIAL_PLANE defaults to `both`, in which
	// ONE worker process polls tg.runner AND tg.actuate (cmd/worker/main.go), so this dispatch is served by
	// the same process, with the same deps, as before. Replay-safe: the Go SDK matches a replayed
	// ScheduleActivityTask on activity id, not on task queue, so histories recorded before this change
	// replay unchanged.
	execCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           tg.TaskQueueActuate,
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	ei := ExecuteInput{
		ActionID: gate.ActionID, ExternalRef: env.ExternalRef, PlanHash: planHash, Site: env.Site, TargetHost: inv.Proposal.Action.Target, Approved: approved,
		// Thread the investigation's cited evidence + orchestrator-captured tool results into the execute
		// activity so the interceptor's evidence gate binds real grounding (INV-11) — the same evidence the
		// classifier admitted. In a non-actuating mode this changes nothing (the chain still refuses at its gate).
		EvidenceIDs: inv.Proposal.EvidenceIDs, ToolResults: inv.ToolResults,
		// The FRESH per-incident classification band (TG-126): the interceptor's 1b admission gate enforces THIS
		// live band rather than the sealed manifest's frozen first-seal band. The manifest is content-addressed
		// by action_id and sealed first-wins, so a re-classified later incident of the same action shape reloads a
		// stale frozen band — decision.Band carries the CURRENT classification the admission gate must honor.
		Band: decision.Band,
		// The agent's emitted proposal confidence — the SAME inv.Confidence the loop already used live to
		// gate stop/escalate/propose and that is persisted on the triage row. It is the policy
		// min_confidence clamp's input (REQ-1507); until TG-408 the field was never assigned anywhere, so
		// the clamp compared 0.0 against every threshold and could only ever fire.
		Confidence: inv.Confidence,
	}
	// spec/029 T-029-2 (REQ-2901): the ARMED REVERT, armed BEFORE the effect executes. For a
	// commit-confirmed-eligible op-class (registry data, T-029-1) this durably records the revert window
	// (commit_confirm row + ledger append) and starts — and CONFIRMS STARTED — the CommitConfirmWorkflow
	// child holding the one durable Temporal timer, all before ExecuteActivity may run. Failure at either
	// step REFUSES the forward: an armed revert that cannot be durably recorded is the pve03 shape wearing
	// a timer, so the effect is withheld (fail closed) and the session terminates like a denied poll.
	// Eligibility lives inside the arm activity (recorded result), never in workflow code — a registry
	// change on redeploy can never make an old history replay differently. Version-guarded: pre-feature
	// histories skip the whole block and replay byte-identically. The child is ABANDONED at parent close
	// so the window outlives this session; its confirm/inverse routing is T-029-3 (interim elapse records
	// elapsed_unconfirmed loudly, no inverse — eligible classes stay undeployed until T-029-3 merges).
	ccArmed := false
	var ccChild workflow.ChildWorkflowFuture
	if workflow.GetVersion(ctx, "commit-confirm-arm", workflow.DefaultVersion, 1) >= 1 {
		refuseForward := func(detail string) (RunnerResult, error) {
			workflow.GetLogger(ctx).Warn("commit-confirm: refusing forward effect — armed revert unarmable (REQ-2901 fail closed)",
				"action_id", gate.ActionID, "detail", detail)
			res.Outcome = "refused:commit-confirm-unarmable — forward effect withheld (REQ-2901 fail closed)"
			res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
			reconcileTerminal(ctx, a, env, ReconcileInput{
				ActionID:          gate.ActionID,
				Band:              decision.Band,
				HasTerminalResult: true,
			})
			return res, nil
		}
		var armRes ArmCommitConfirmResult
		if err := workflow.ExecuteActivity(ctx, a.ArmCommitConfirmActivity, ArmCommitConfirmInput{
			ActionID:    gate.ActionID,
			ExternalRef: env.ExternalRef,
			OpClass:     inv.Proposal.Action.OpClass,
			TargetHost:  inv.Proposal.Action.Target,
			Site:        env.Site,
			PlanHash:    planHash,
			// The fired inverse's authorization basis (T-029-3), captured at THIS instant: the
			// fresh per-incident band and the human vote's outcome — both already in scope here,
			// after the vote block, before the effect. The interceptor still judges the inverse
			// fresh; this is what the request will truthfully CARRY. String() (never a raw int
			// conversion): the row is queried by humans, and the seal parses it back fail-closed.
			Band:      decision.Band.String(),
			Approved:  approved,
			AlertRule: env.AlertRule,
			// The guest-param for the REQ-2908 no-op read — resolved guest-first exactly like the
			// seal gate (the two fields are independently LLM-populated and can diverge).
			GuestParam: inv.Proposal.Action.Params["guest"],
		}).Get(ctx, &armRes); err != nil {
			return refuseForward("durable arm failed: " + err.Error())
		}
		if armRes.NoOpRefused {
			// REQ-2908: a free no-op — the estate already holds the desired end state. A clean
			// stand-down (recorded, reconciled), not a failure and not a mutation.
			workflow.GetLogger(ctx).Info("commit-confirm: refused no-op (empty diff)", "action_id", gate.ActionID, "reason", armRes.NoOpReason)
			res.Outcome = "refused:no-op — " + armRes.NoOpReason
			res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
			reconcileTerminal(ctx, a, env, ReconcileInput{
				ActionID: gate.ActionID, Band: decision.Band, HasTerminalResult: true,
			})
			return res, nil
		}
		if armRes.MandateRefused {
			// REQ-2905 (T-029-4): a canary/staged-postured class without commit-confirmed
			// eligibility may not execute at all — no armed revert, no forward.
			return refuseForward("canary mandate: " + armRes.MandateReason)
		}
		if armRes.Eligible {
			ccCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				// Deterministic id: one live window per (incident, action). A concurrent duplicate
				// start (the same action re-delivered while a window is live) fails here and refuses
				// the forward — never two timers over one effect.
				WorkflowID:        "commit-confirm:" + env.ExternalRef + ":" + gate.ActionID,
				ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
			})
			ccChild = workflow.ExecuteChildWorkflow(ccCtx, CommitConfirmWorkflow, CommitConfirmInput{
				ActionID:      gate.ActionID,
				ExternalRef:   env.ExternalRef,
				WindowSeconds: armRes.WindowSeconds,
				AlertRule:     env.AlertRule, // TG-461: incident signature for the service-fault durable-substitute confirm
			})
			// Wait for STARTED (not completed): the timer must exist before the effect does. If the
			// child cannot start, the recorded row is a claim with no timer behind it — mark it
			// aborted (best effort) and refuse the forward.
			if err := ccChild.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
				if aerr := workflow.ExecuteActivity(ctx, a.ResolveCommitConfirmActivity, ResolveCommitConfirmInput{
					ActionID:    gate.ActionID,
					ExternalRef: env.ExternalRef,
					State:       db.CommitConfirmAborted,
					Detail:      "child timer failed to start: " + err.Error(),
				}).Get(ctx, nil); aerr != nil {
					workflow.GetLogger(ctx).Warn("commit-confirm: abort-after-failed-child-start also failed — row stays armed",
						"action_id", gate.ActionID, "error", aerr.Error())
				}
				return refuseForward("child timer failed to start: " + err.Error())
			}
			ccArmed = true
		}
	}
	// actionAt: the HEAL instant — the correct lower bound for "a provider recovery caused by OUR action". Captured
	// immediately BEFORE ExecuteActivity (a recovery before this proves nothing about our heal, per the belt's
	// documented intent). The ConfirmedClear belt below anchors its RecoveredSince window HERE, not at the later
	// clear-confirm block: that block's workflow.Now() overshoots the heal by the full verify latency, so a FAST
	// recovery landing during verify would be wrongly excluded (the openwebui01 writeback miss). workflow.Now is a
	// deterministic clock read (no history event) so capturing it costs nothing for replay.
	actionAt := workflow.Now(ctx)
	var exec ExecuteResult
	if err := workflow.ExecuteActivity(execCtx, a.ExecuteActivity, ei).Get(ctx, &exec); err != nil {
		// An AMBIGUOUS failure (timeout/blip may mean executed-but-unreported) with a live armed
		// revert: the dead-man stays HOT deliberately — the child window elapses on its own and
		// records it (T-029-3 routes that elapse through the terminus consult). Aborting here
		// would stand down the one guard covering a maybe-executed mutation.
		return res, err
	}
	if ccArmed && !exec.Executed {
		// The chain REFUSED (mode chokepoint / a gate) — the effect provably did not execute, so the
		// armed window covers nothing: stand it down. Best-effort — a lost signal leaves the row
		// armed and the child elapses into the loud interim state instead (never a fired inverse for
		// a non-action: T-029-3's elapse consult reads the terminus, which has no execution).
		if err := ccChild.SignalChildWorkflow(ctx, CommitConfirmSignalName, CommitConfirmResolve{
			Kind:   "abort",
			Detail: "forward effect did not execute (chain refused): " + exec.Note,
		}).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Warn("commit-confirm: abort signal failed — window resolves by elapse instead",
				"action_id", gate.ActionID, "error", err.Error())
		}
	}
	res.Mutated = exec.Executed
	// TG-80 P1-4: a CANCELLED effect is a terminal of its own. The leaf signalled the remote command dead
	// before closing the transport and the interceptor recorded Cancelled; there is nothing to verify (the
	// effect provably did not complete) and the session record must say "cancelled", not "proposed" or a
	// generic refusal. Version-guarded so in-flight histories replay byte-identically.
	if exec.Cancelled && workflow.GetVersion(ctx, "cancel-terminal", workflow.DefaultVersion, 1) >= 1 {
		res.Outcome = "cancelled:remote-killed — " + exec.Note
		res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
		return res, nil
	}
	// Thread THIS session's execution fact into the verify read. Pure data on the same activity call — the
	// activity sequence is unchanged, so no workflow.GetVersion is needed; only the payload differs, and an old
	// history replays from its RECORDED result rather than re-running the activity.
	vi := ei
	vi.Executed = exec.Executed
	var ver VerifyResult
	if err := workflow.ExecuteActivity(ctx, a.VerifyActivity, vi).Get(ctx, &ver); err != nil {
		return res, err
	}
	// spec/020 T-020-4 (REQ-2006): record the post-execution mechanical verdict on the sealed manifest so the
	// tracer shows how the action resolved. In a non-actuating mode the verify activity reports nothing verified
	// (empty verdict ⇒ backfillManifest no-ops), so this is inert-but-complete until Phase 2. Observe-only.
	backfillManifest(ctx, a, env.ExternalRef, gate.ActionID, "", safety.Verdict(ver.Verdict))
	// spec/029 T-029-3 (REQ-2902): NUDGE the armed window now that the terminus has spoken. The signal
	// carries NO trusted data — the child consults the DURABLE terminus record itself (confirm-from-the-
	// terminus-only), so a lost nudge, a dead parent, and a sweep-adopted orphan all converge on the same
	// read; this only collapses the fast path from "window elapse" to "right now". Version-guarded
	// independently of the arm block (in-flight histories armed under v1 must replay byte-identically).
	if ccArmed && exec.Executed && workflow.GetVersion(ctx, "commit-confirm-confirm-route", workflow.DefaultVersion, 1) >= 1 {
		if err := ccChild.SignalChildWorkflow(ctx, CommitConfirmSignalName, CommitConfirmResolve{
			Kind: "confirm-input",
		}).Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Warn("commit-confirm: post-verify nudge failed — the window resolves by its own elapse consult",
				"action_id", gate.ActionID, "error", err.Error())
		}
	}

	res.Outcome = "proposed"
	res.TriageRecorded = recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.SkillLoads)
	// ConfirmedClear producer (TG-124): the novelty-writeback + auto-close ladder needs an ORCHESTRATOR-
	// captured confirmation that the incident's ORIGINAL (host, alert_rule) actually cleared — not the agent's
	// word (INV-11), and not the mechanical verdict (a match EXCLUDES the target's own alert, so it can never
	// mean the original condition cleared). Only an action we actually EXECUTED can have cleared the estate, so
	// re-observe the live post-state once here (via the same PostStateObserve reader the verifier diffs) and
	// set ConfirmedClear from it. Fail-closed: any error / no reader ⇒ stays false ⇒ the reconciler HOLDS the
	// incident To Verify (never a false auto-close). Version-guarded so pre-existing histories replay
	// deterministically (old histories skip the observe and keep ConfirmedClear=false, exactly as before).
	confirmedClear := false
	if workflow.GetVersion(ctx, "confirmed-clear-observe", workflow.DefaultVersion, 1) >= 1 && res.Mutated {
		// Re-observe the INCIDENT's own host (env.Host — where the original alert fired), NOT the action target:
		// ConfirmedClear means "the condition that opened this incident cleared". For a self-heal the two are the
		// same host; sourcing it from env.Host stays correct if a future remediation targets a different host than
		// the one alerting. The novelty writeback below keeps its own (action-target, rule) key. A fetch/activity
		// error is NEVER a clear (fail-closed).
		observeCleared := func() bool {
			var clr ClearResult
			if err := workflow.ExecuteActivity(ctx, a.ObserveClearedActivity, ClearInput{
				Host: env.Host, AlertRule: env.AlertRule, Site: env.Site,
			}).Get(ctx, &clr); err == nil {
				return clr.Cleared
			}
			return false
		}
		if workflow.GetVersion(ctx, "confirmed-clear-retry", workflow.DefaultVersion, 1) >= 1 {
			// BOUNDED-RETRY clear-check (TG-124): re-observe every clearRecheckInterval up to clearMaxWindow so a
			// device-down recovery that clears LATER than a fixed settle window is still confirmed. Confirm on the
			// FIRST quiet reading; fail closed if never quiet within the bound (the incident stays To Verify). A
			// stale still-firing read just triggers another retry — it can never false-confirm. Durable Temporal
			// timers; a cancelled timer breaks the loop with ConfirmedClear=false (fail-closed).
			//
			// BELT (TG-124 Plan B, version-guarded): the re-pull can LAG past the bound when the LibreNMS poller
			// is slow to recover a stuck alert (the observed writeback miss). So each tick ALSO consults TG's OWN
			// durable capture of the provider recovery PUSH (RecoveredSince over ingest_transition, populated by
			// the front door). A captured recovery at/after execAt counts exactly like a quiet re-pull reading —
			// preserving the consecutive-reading debounce — so a real recovery confirms even when the re-pull is
			// blind. Guarded by a NEW change-id: old histories (no marker ⇒ DefaultVersion) never call the belt
			// activity and replay byte-identically; a nil RecoveredSince seam makes the belt inert (today's
			// behavior). execAt excludes any recovery captured BEFORE our action (it proves nothing about it).
			execAt := workflow.Now(ctx)
			useRecovered := workflow.GetVersion(ctx, "confirmed-clear-recovered-since", workflow.DefaultVersion, 1) >= 1
			// The RecoveredSince window anchors at the HEAL (actionAt), NOT here at execAt: execAt is captured after
			// Execute AND Verify, so a fast recovery pushed DURING verify is < execAt and would be silently excluded —
			// then if the re-pull also stays non-quiet (e.g. concurrent alert variants keep the device-down alert
			// firing), the session never confirms and the novelty writeback is lost (observed live: openwebui01, whose
			// recovery captured at 07:32:05 but whose clear-confirm reconciled 07:35:21 before it could count). Anchoring
			// at actionAt still excludes any pre-heal recovery (the documented invariant) while including the post-heal
			// recovery our action actually caused. Version-guarded (change-id recovered-since-anchor-heal): old histories
			// (no marker ⇒ DefaultVersion) keep Since=execAt so the RecoveredSinceActivity input replays byte-identically.
			recoverySince := execAt
			if workflow.GetVersion(ctx, "recovered-since-anchor-heal", workflow.DefaultVersion, 1) >= 1 {
				recoverySince = actionAt
			}
			recoveredSince := func() bool {
				if !useRecovered {
					return false
				}
				var ok bool
				if err := workflow.ExecuteActivity(ctx, a.RecoveredSinceActivity, RecoveredSinceInput{Host: env.Host, AlertRule: env.AlertRule, Since: recoverySince}).Get(ctx, &ok); err == nil {
					return ok
				}
				return false // a belt error is never a recovery (fail-closed)
			}
			deadline := execAt.Add(clearMaxWindow)
			// THE BELT MAY SUPPLY AT MOST ONE OF THE REQUIRED READINGS (change-id clear-confirm-belt-once).
			// recoveredSince() is MONOTONE: its `since` is pinned before the loop, so once ONE qualifying recovery
			// row exists it returns true on EVERY tick — which silently defeated the flap debounce below. A host
			// whose own rule kept firing still reached clearConfirmReadings purely on the belt, falsifying this
			// file's own guarantee that "a FLAPPING host can never false-confirm — a re-alert resets the counter".
			// Capping the belt at one reading keeps its purpose (covering a lagging re-pull) while requiring at
			// least one DIRECT server-side quiet observation before a clear is confirmed. Version-guarded because
			// skipping the call changes the activity sequence; old histories keep the uncapped behaviour.
			beltCapped := workflow.GetVersion(ctx, "clear-confirm-belt-once", workflow.DefaultVersion, 1) >= 1
			quiet := 0
			beltUsed := false
			for {
				if serr := workflow.Sleep(ctx, clearRecheckInterval); serr != nil {
					break // cancelled ⇒ fail-closed
				}
				cleared := observeCleared()
				if !cleared && !(beltCapped && beltUsed) {
					// BELT: TG's own captured recovery confirms even when the re-pull lags.
					if recoveredSince() {
						cleared, beltUsed = true, true
					}
				}
				if cleared {
					quiet++
					if quiet >= clearConfirmReadings {
						confirmedClear = true
						break
					}
				} else {
					quiet = 0 // a re-alert (and no captured recovery) resets the debounce (flap protection)
				}
				if !workflow.Now(ctx).Before(deadline) {
					break // bound exhausted ⇒ fail-closed (stays false)
				}
			}
		} else {
			// Legacy single-shot (pre-`confirmed-clear-retry` histories replay deterministically): wait one settle
			// window, then re-observe once. A cancelled timer skips the check (ConfirmedClear stays false).
			if serr := workflow.Sleep(ctx, clearSettleWindow); serr == nil {
				confirmedClear = observeCleared()
			}
		}
	}
	// Persist the orchestrator-confirmed clear onto the triage row (benchmark axis A3 heal-success numerator,
	// migration 0039). confirmedClear is only finalized HERE — after the bounded clear-observe loop — which is
	// why it rides a follow-up UPDATE rather than the earlier recordTriage insert (that carried only mutated,
	// known at record time). Guarded on res.Mutated (only an actuated heal has a clear to confirm) and by its
	// OWN change marker so pre-existing histories replay deterministically (no marker ⇒ DefaultVersion ⇒ the
	// activity is skipped). Best-effort: MarkTriageClearedActivity is fail-open, and the future's error is
	// discarded — a scorer-instrumentation write must NEVER fail the session terminus. OBSERVABILITY ONLY.
	if workflow.GetVersion(ctx, "triage-confirmed-clear-persist", workflow.DefaultVersion, 1) >= 1 && res.Mutated {
		_ = workflow.ExecuteActivity(ctx, a.MarkTriageClearedActivity, MarkClearInput{
			ExternalRef: env.ExternalRef, Cleared: confirmedClear,
			// Back-fill the A3 denominator on the same terminus. This block is already gated on res.Mutated,
			// so it is true here — but pass it explicitly rather than implying it, because the whole defect
			// being fixed was a value assumed to have been recorded earlier and silently not.
			Mutated: res.Mutated,
		}).Get(ctx, nil)
	}
	// Terminal close-out (spec/003) at the workflow END: drive the band-aware reconciler over the finished
	// session — it transitions the incident's tracker ticket and records the close-out decision on the ledger.
	// With ConfirmedClear now sourced above, a verified-clean, confirmed-clear auto session auto-closes to Done
	// AND de-novels its (host, rule); anything unexecuted, unconfirmed, or non-match is conservatively left
	// OPEN (To Verify) — a deviation is never auto-closed (deviation→never-auto). Fail-safe (tracker + ledger
	// writes only, never the estate).
	// Writeback/auto-close verdict SOURCE (TG-124 fix): use the FRESH per-execution verdict (exec.Verdict —
	// ComputeVerdict diffed against THIS run's real post-state) rather than the frozen first-wins verdict
	// VerifyActivity reads back (ver.Verdict). action_verdict is content-addressed by action_id AND append-only
	// (INSERT … ON CONFLICT DO NOTHING), so a re-cycled (host, op) inherits the verdict of the action's FIRST
	// execution: a stale partial/deviation then PERMANENTLY blocks the de-novel writeback for every later clean
	// re-cycle, AND could false-authorize an auto-close of a genuinely-deviating re-cycle that inherited an old
	// match. exec.Verdict reflects what actually happened this session. On a FIRST execution the two are identical
	// (the store's first write == this verdict), so only re-cycles change — toward correctness. VerifyActivity
	// still runs (verdict metric + the tracer spine's per-action-shape row). Version-guarded so pre-existing
	// histories replay byte-identically (old marker ⇒ DefaultVersion ⇒ keep ver.Verdict).
	reconHasVerdict, reconVerdict := ver.Verified, safety.Verdict(ver.Verdict)
	if workflow.GetVersion(ctx, "writeback-fresh-verdict", workflow.DefaultVersion, 1) >= 1 {
		reconHasVerdict, reconVerdict = exec.Verdict != "", safety.Verdict(exec.Verdict)
	}
	// TG-483 (TG-146 C1): the deviation verdict above is FROZEN at the interceptor's ~1s post-execution
	// read, and the ConfirmedClear loop watches only the incident's own host — so a collateral cascade our
	// heal caused on a SIBLING, surfacing after the verdict read but within the settle window, graded clean.
	// Re-check HERE, at the terminus, after that loop has actually consumed the settle window: scan the
	// action target's blast-radius members (guest-first anchor, the REQ-2908 resolution) for (host, rule)
	// pairs that FIRST surfaced since the heal, against TG's own durable capture. Positive-only: a true
	// reading blocks auto-close AND graduation credit (reconcile + the ladder read the same bit); nil/error
	// (no graph, no reader, unobservable) changes nothing — the residual being closed is the false CLEAN,
	// and blind reads must not convert every terminus into a deviation hold. Version-guarded: old histories
	// (no marker ⇒ DefaultVersion) never schedule the activity and replay byte-identically.
	collateralObserved := false
	if workflow.GetVersion(ctx, "terminus-collateral-recheck", workflow.DefaultVersion, 1) >= 1 && res.Mutated {
		anchor := strings.TrimSpace(inv.Proposal.Action.Params["guest"])
		if anchor == "" {
			anchor = strings.TrimSpace(inv.Proposal.Action.Target)
		}
		var col ObserveCollateralResult
		if err := workflow.ExecuteActivity(ctx, a.ObserveCollateralActivity, ObserveCollateralInput{
			Anchor: anchor, ExcludeHost: env.Host, ExcludeRule: env.AlertRule, Since: actionAt,
		}).Get(ctx, &col); err == nil && col.Observed != nil && *col.Observed {
			collateralObserved = true
			logCollateralHits(ctx, env.ExternalRef, col.Hits)
		}
	}
	reconcileTerminal(ctx, a, env, ReconcileInput{
		ActionID:          gate.ActionID,
		Band:              decision.Band,
		HasVerdict:        reconHasVerdict,
		Verdict:           reconVerdict,
		HasTerminalResult: true,
		Executed:          res.Mutated,
		ConfirmedClear:    confirmedClear,
		// TG-483: the terminus collateral re-check's positive reading — blocks auto-close and graduation.
		CollateralObserved: collateralObserved,
		// The op-class whose graduation this terminus feeds (REQ-1223). The PROMOTE is decided here, not at
		// execute: `confirmedClear` is the only signal in this workflow observed AFTER the monitoring surface
		// has had time to refresh.
		OpClass: inv.Proposal.Action.OpClass,
		// Novelty writeback signature (TG-124): key the resolved-incident lesson on the STABLE incident subject
		// env.Host (the ingest-validated alerted device) — the SAME identity ClassifyInput.IncidentHost carries
		// into the novelty read, the same identity the clear confirmation was observed against (ClearInput.Host),
		// and the same convention the pred-ik-* seed corpus + the retrieval plane query by. AlertRule is
		// env.AlertRule; Action is the op that resolved it. Keying on env.Host (not the LLM-expressed
		// inv.Proposal.Action.Target, which alternates guest vs PVE node per proposal) is what makes de-novel
		// transfer: a verified-clean, confirmed-clear resolution de-novels the (subject, rule) so its next
		// occurrence self-heals hands-off regardless of how the next proposal expresses its target.
		Host:      env.Host,
		AlertRule: env.AlertRule,
		Action:    inv.Proposal.Action.Op,
	})
	return res, nil
}

// recordTriage persists the compact terminal triage record (REQ-1106) — the durable judge spine's
// input. It runs at every terminal outcome that spent a triage session (the no-proposal stop, the
// stood-down poll decision, the completed proposal) but NOT for a suppressed incident (no session was
// spent, there is nothing to judge). Best-effort and fail-open end to end: the activity swallows write
// errors and the workflow discards the future's error — a record failure must NEVER fail the session
// (judge scoring is asynchronous and read-only over the record). Guarded by its own change marker so
// pre-existing histories replay deterministically.
func recordTriage(ctx workflow.Context, a *Activities, env ingest.IncidentEnvelope, res RunnerResult, op, opClass string, skillLoads []string) bool {
	if workflow.GetVersion(ctx, "triage-record", workflow.DefaultVersion, 1) < 1 {
		return false
	}
	_ = workflow.ExecuteActivity(ctx, a.RecordTriageActivity, judge.TriageRow{
		ExternalRef: env.ExternalRef,
		Host:        env.Host,
		AlertRule:   env.AlertRule,
		Band:        res.Band,
		Outcome:     res.Outcome,
		Proposed:    res.Proposed,
		Op:          op,
		OpClass:     opClass,       // axis A5: the canonical op-class (migration 0036), "" for a no-proposal stop
		StepCount:   res.StepCount, // axis A6a: read-only investigation cycle count (migration 0037)
		// TG-205 — axis A6b: how LONG the decision took (migration 0058). A6 is defined as MTTR and every
		// implementation measured steps, so no AXIS surface reported time (the only clock was the cumulative
		// tg_agent_run_seconds_total counter); this is the first durable PER-INCIDENT wall-clock TG keeps.
		// 0 = the loop never ran, and the scorer excludes it rather than reading it as an instant decision.
		DecisionMillis: res.DecisionMillis,
		StopReason:     res.StopReason, // WHY a no-proposal session halted (migration 0044) — orchestrator-computed
		Mutated:        res.Mutated,    // axis A3 denominator: did TG actuate an estate mutation (migration 0039)
		EvidenceIDs:    res.EvidenceIDs,
		Conclusion:     res.Conclusion,
		Prediction:     res.Prediction, // TG-61: the committed prediction the live judge cron scores
		Predicted:      res.Predicted,
		// TG-201: the typed claim + the flag saying this build RECORDED it. DiagnosisRecorded is true for
		// every row this writer produces, empty claim or not — "the agent bound nothing" is a gradeable fact,
		// while a row from before migration 0056 (NULL column) means the field did not exist and the judge
		// must score it N/A rather than fail it retroactively.
		Diagnosis:         res.Diagnosis,
		DiagnosisRecorded: true,
		// TG-394 slice 3: the degraded-capability set at session time (migration 0082) — so a lexical-only
		// investigation is legible on the record when the graph has since recovered. Observability only.
		DegradedCapabilities: res.DegradedCapabilities,
		Confidence:           res.Confidence, // spec/020 REQ-2003: the agent's emitted proposal confidence (observability)
		// spec/023 REQ-2311: the WHO-CAUSED-THIS taxonomy + its minimized evidence blob (observability only).
		Attribution:   res.Attribution,
		ActorEvidence: res.ActorEvidence,
		SkillLoads:    skillLoads,
		// spec/020 REQ-2009: the session's prompt/seed/model provenance (observability only).
		PromptVersion: res.PromptVersion,
		SeedHash:      res.SeedHash,
		ModelTier:     res.ModelTier,
		// TG-198: the tier that produced the TERMINAL proposal/stop. ModelTier above is the INVESTIGATION
		// tier; before this column every row claimed "fast" decided, including the ones the TG-60 nudge
		// handed to the reasoner.
		DecisionTier: res.DecisionTier,
		// TG-527: the ordered, digested tool path (migration 0104) — so the trajectory_grounded axis can
		// score this session from the DB later, not only inside the eval harness's process.
		Trajectory: trajectoryRows(res.Trajectory),
		CreatedAt:  workflow.Now(ctx).UTC(),
	}).Get(ctx, nil)
	return true
}

// trajectoryRows converts the loop's typed trajectory to the judge's persisted twin (TG-527). Always
// non-nil so the store writes '[]' — "recorded, no steps" — never NULL, which means "pre-0104 row".
func trajectoryRows(steps []agent.TrajectoryStep) []judge.TrajectoryStep {
	out := make([]judge.TrajectoryStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, judge.TrajectoryStep{Tool: s.Tool, ArgsKey: s.ArgsKey})
	}
	return out
}

// backfillManifest records a sealed manifest's LATER lifecycle labels — the human approval choice (after the
// vote) and the post-execution mechanical verdict (after verify) — onto the already-sealed action_manifest row
// for the decision tracer (spec/020 T-020-4, REQ-2006). Best-effort and fail-open: the activity swallows write
// errors and the workflow discards the future's error — a backfill failure must NEVER fail the session.
// OBSERVE-ONLY: it writes only the two NON-HASHED columns, so it cannot tamper the sealed binding (INV-07) and
// it changes no gate/verdict/control flow. Guarded by its own change marker so pre-existing histories replay
// deterministically; a no-op when there is no action_id or nothing to backfill.
func backfillManifest(ctx workflow.Context, a *Activities, externalRef, actionID, approvalChoice string, verdict safety.Verdict) {
	if workflow.GetVersion(ctx, "manifest-backfill", workflow.DefaultVersion, 1) < 1 {
		return
	}
	if actionID == "" || (approvalChoice == "" && string(verdict) == "") {
		return
	}
	_ = workflow.ExecuteActivity(ctx, a.BackfillManifestActivity, BackfillManifestInput{
		ActionID:       actionID,
		ExternalRef:    externalRef,
		ApprovalChoice: approvalChoice,
		Verdict:        verdict,
	}).Get(ctx, nil)
}

// reconcileTerminal runs the terminal band-aware close-out lane (spec/003) at a session that reached a
// terminal outcome AND spent a proposal: it drives reconcile.Reconcile → CloseOut (a tracker ticket
// transition + a close-out ledger append) and hands an unresolved (orphaned-poll) decision off to the
// escalation requeue lane. Best-effort and fail-open end to end — the activity never returns an error and
// the workflow discards the future's error, so a close-out/hand-off failure NEVER fails a session that
// already reached its terminus. Nothing here mutates the estate (tracker + ledger + escalation-queue
// writes only). The workflow's own external_ref and workflow-execution id fill the session identity.
// Guarded by its own change marker so pre-existing histories replay deterministically.
func reconcileTerminal(ctx workflow.Context, a *Activities, env ingest.IncidentEnvelope, in ReconcileInput) {
	if workflow.GetVersion(ctx, "terminal-reconcile", workflow.DefaultVersion, 1) < 1 {
		return
	}
	in.ExternalRef = env.ExternalRef
	in.SessionID = workflow.GetInfo(ctx).WorkflowExecution.ID
	// Envelope-sourced enrichment for the novelty writeback's distilled precedent (TG-124). The load-bearing
	// (host, alert_rule) novelty SIGNATURE is set by the caller (Host = the action target, AlertRule =
	// env.AlertRule — mirrored from ClassifyInput so the WRITE key equals the READ key); Site/Summary are pure
	// retrieval enrichment and safe to source from the envelope here for every terminus.
	in.Site = env.Site
	in.Summary = env.Summary
	// The ladder credit is dispatched as its OWN activity pinned to ONE attempt. ReconcileActivity runs with
	// MaximumAttempts:4, and the ladder write is not idempotent — a retried reconcile could credit a single
	// confirmed heal up to four times, i.e. four of the five runs an op-class needs to reach AUTO. This mirrors
	// execCtx above, whose comment states the rule: a Temporal activity is at-least-once, so anything that must
	// happen exactly once gets exactly one attempt. Failing here LOSES a credit rather than manufacturing
	// three, which is the correct direction for an autonomy earn path.
	//
	// Version-guarded because it adds a command to the workflow history: a replaying execution started before
	// this change must not see an activity its history does not contain. Legacy executions leave SkipGraduation
	// false and keep the old inline write, so they neither double-credit nor silently lose one.
	if workflow.GetVersion(ctx, "graduation-single-attempt", workflow.DefaultVersion, 1) >= 1 {
		in.SkipGraduation = true
		gradCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Second,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		_ = workflow.ExecuteActivity(gradCtx, a.GraduationActivity, in).Get(ctx, nil)
	}
	_ = workflow.ExecuteActivity(ctx, a.ReconcileActivity, in).Get(ctx, nil)
}

// failedInvestigateResult stamps the terminal record for a session whose INVESTIGATION ITSELF failed.
//
// Extracted as a pure function so the contract is testable without a Temporal environment — the
// alternative is an untested error path, which is precisely how this branch came to write no record at
// all for its entire life.
//
// StopReason is the ORCHESTRATOR's account of the failure, never agent text: the same trusted channel
// the no-proposal path uses, and deliberately distinct from Conclusion, so "the model call died" and "no
// action was warranted" can never become the same row.
func failedInvestigateResult(res RunnerResult, err error) RunnerResult {
	res.Outcome = "failed:investigate"
	reason := "investigate activity failed"
	if err != nil {
		reason += ": " + err.Error()
	}
	res.StopReason = reason
	return res
}

// approvalChoices builds the poll menu for a POLL_PAUSE decision: one option per candidate plan, then
// DENY and INVESTIGATE.
//
// Every answer id embeds the decision id (notifier.ChoiceID), because a Matrix poll response carries the
// chosen answer's id and nothing else. That is what lets a vote bind to exactly the decision it answers
// with no server-side event->decision map to drift (INV-12).
//
// "Investigate" is deliberately NOT approving. It is the option a human needs when the proposal is
// plausible but unproven, and folding it into consent would turn hesitation into authorization.
func approvalChoices(decisionID string, plans []string) []notifier.Choice {
	out := make([]notifier.Choice, 0, len(plans)+2)
	for i, p := range plans {
		verb := "plan"
		if len(plans) > 1 {
			verb = fmt.Sprintf("plan%d", i)
		}
		label := p
		if len(plans) > 1 {
			label = fmt.Sprintf("Plan %d — %s", i+1, p)
		} else {
			label = "Approve — " + p
		}
		out = append(out, notifier.Choice{ID: notifier.ChoiceID(verb, decisionID), Label: label, Approve: true})
	}
	out = append(out,
		notifier.Choice{ID: notifier.ChoiceID("deny", decisionID), Label: "Deny — stand down, change nothing"},
		notifier.Choice{ID: notifier.ChoiceID("investigate", decisionID), Label: "Investigate further — do not act yet"},
	)
	return out
}
