package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/execclass"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/risk"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/screen"
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
	Proposed      bool
	EvidenceIDs   []string // the tool-result ids the proposal (or grounded stop) cited (INV-11) — surfaced for the ledger/console/eval
	Conclusion    string   // the agent's grounded no-action rationale on a stop (REQ-1008) — untrusted DATA for the record
	Prediction    string   // the committed machine prediction rendered judge-readable (TG-61) — surfaced so the terminal record + live judge cron score falsifiable_prediction; empty when nothing was proposed
	Predicted     bool     // a machine prediction was committed at the gate (a proposal reached the prediction gate) — false for a grounded stop
	Confidence    float64  // the agent's emitted 0..1 proposal confidence (core/proposal), carried to the durable triage record for the decision tracer + calibration (spec/020 REQ-2003, observability only); 0 for a grounded stop
	// PromptVersion / SeedHash / ModelTier are the session's prompt/seed/model provenance for the decision
	// tracer (spec/020 REQ-2009): the trusted-preamble version, the SHA-256 fingerprint of the composed seed
	// (hash only — never the seed text; INV-13), and the LLM tier the loop ran on. Observability only.
	PromptVersion string
	SeedHash      string
	ModelTier     string
	PollBuilt     bool
	Notified      bool   // the governance notice/poll was delivered to the human channel (notice/poll bands)
	Mutated       bool   // MUST be false in Phase 0/1 (the Runner stops at propose)
	Vote          string // the human decision on a POLL_PAUSE proposal: "approved" | "denied" | "timeout" | "" (no poll)
	Outcome       string
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
func noticeBody(band safety.Band, alertRule, host, op, opClass, externalRef string) string {
	return fmt.Sprintf("[%s] %s on %s — proposed: %s (%s). ref=%s", band, alertRule, host, op, opClass, externalRef)
}

// execClassFor makes the execution-topology decision from the cheap signals available BEFORE any expensive
// context is built (the external audit's "topology decision made before expensive context construction").
// Today the only pre-context signal is the envelope severity; as tier-1 suppression (known-pattern), the
// estate graph (criticality tier) and correlation detection land upstream, their signals feed
// execclass.Input to sharpen the routing. The decision is deterministic (INV-08) and fail-safe.
func execClassFor(env ingest.IncidentEnvelope) execclass.Class {
	in := execclass.Input{}
	if env.Severity == ingest.SeverityCritical {
		// A critical incident warrants the thorough path until finer correlation/pattern signals exist.
		in.Correlated = true
	}
	return execclass.Classify(in)
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
// STOP-at-propose. The execute/verify activities are invoked but no-op under mutation OFF (Phase 0/1),
// so the workflow ends at a sealed, classified, gated proposal with no estate mutation. The workflow
// body contains CONTROL FLOW ONLY — every side effect is an activity, and no activity executes an OS
// command (INV-21). Action identity is threaded unchanged: the action_id derived here is asserted by
// the gate activity's sealed ActionManifest (INV-07).
func RunnerWorkflow(ctx workflow.Context, env ingest.IncidentEnvelope) (RunnerResult, error) {
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
	res.ExecClass = string(execClassFor(env))

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
	var inv InvestigateResult
	if err := workflow.ExecuteActivity(investigateCtx, a.InvestigateActivity, env).Get(ctx, &inv); err != nil {
		return res, err
	}
	// Decision-tracer session provenance (spec/020 REQ-2009): stamp the composed-seed fingerprint, the
	// trusted-preamble version, and the model tier onto the result so EVERY terminal record (proposal, grounded
	// stop, or escalate) carries which prompt/seed/tier composed the session. Observability only — no gate reads it.
	res.SeedHash = inv.SeedHash
	res.PromptVersion = inv.PromptVersion
	res.ModelTier = inv.ModelTier
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
		recordTriage(ctx, a, env, res, "", inv.SkillLoads)
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
			recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.SkillLoads)
			reconcileTerminal(ctx, a, env, ReconcileInput{
				ActionID: actionID, HasTerminalResult: true,
				Host: env.Host, AlertRule: env.AlertRule, Action: inv.Proposal.Action.Op,
			})
			return res, nil
		}
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
		Stateful: safety.IsStatefulWorkload(inv.Proposal.Action.Target, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass),
		// Derive destructiveness from the ACTUAL op (+ the model's rationale), overriding a model that
		// under-declares its op_class — "a plan cannot hide a mutation".
		Destructive: safety.IsDestructiveOp(inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.Proposal.Rationale),
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
	// OBSERVABILITY ONLY — it does NOT feed the actuation-path policy min_confidence clamp (a separate reviewed
	// change; the clamp reads r.Confidence at the interceptor, which is fed a hardwired 0 at the execute build site).
	res.Confidence = inv.Confidence

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
	if decision.Band == safety.BandPollPause {
		// project the open POLL_PAUSE decision so the console can LIST it (REQ-519) — best-effort, fail-open:
		// a read convenience, never the authority (the vote is delivered to THIS workflow directly). Guarded
		// by its own change marker so pre-existing histories replay deterministically.
		if workflow.GetVersion(ctx, "pending-projection", workflow.DefaultVersion, 1) >= 1 {
			_ = workflow.ExecuteActivity(ctx, a.RecordPendingActivity, PendingDecisionInput{
				ExternalRef: env.ExternalRef,
				ActionID:    gate.ActionID,
				Approaches:  []string{fmt.Sprintf("%s %s on %s", inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, inv.Proposal.Action.Target)},
				Prediction:  noticeBody(decision.Band, env.AlertRule, inv.Proposal.Action.Target, inv.Proposal.Action.Op, inv.Proposal.Action.OpClass, env.ExternalRef),
				Reversible:  inv.Proposal.Action.Reversible,
				Site:        env.Site,
				OpenedAt:    workflow.Now(ctx),
			}).Get(ctx, nil)
		}
		// Record the triage NOW — the investigation + proposal is COMPLETE at this point. The durable
		// judge/flywheel/benchmark record must NOT be hostage to the human EXECUTION vote (up to VoteWait=24h):
		// the vote decides whether to EXECUTE; the triage already happened. RecordTriage's ON CONFLICT DO NOTHING
		// makes the later terminal record a harmless no-op, so "proposed" here is the triage disposition — the
		// vote disposition lives on the ledger + pending projection, not this row (TG-79 G1 / benchmark unblock).
		if workflow.GetVersion(ctx, "triage-record-at-propose", workflow.DefaultVersion, 1) >= 1 {
			res.Outcome = "proposed"
			recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.SkillLoads)
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
					RecordVoteInput{Decision: decision, ActionID: gate.ActionID, ExternalRef: env.ExternalRef, Voter: voter}).Get(ctx, &rec)
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
			for !gotVote && !timedOut && !budgetOut && !obsoleted && misbound < maxMisbound {
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
					if err := workflow.ExecuteActivity(ctx, a.RecoveredSinceActivity,
						RecoveredSinceInput{Host: env.Host, Since: openedAt}).Get(ctx, &recovered); err == nil && recovered {
						obsoleted = true
					} else {
						recoveryTimer = workflow.NewTimer(ctx, voteRecoveryRecheckInterval) // re-arm the next recheck
					}
					continue
				}
				if !received {
					continue // the vote-wait timer or the budget deadline fired
				}
				if recv.ActionID != gate.ActionID {
					misbound++ // counted only — never a per-vote activity/ledger write (the round-1 DoS)
					continue
				}
				vote, gotVote = recv, true
			}

			switch {
			case gotVote && vote.Approve:
				approved = true
				res.Vote = "approved"
				if err := record("human:approve", vote.Voter); err != nil {
					return res, err
				}
			case gotVote:
				res.Vote = "denied"
				if err := record("human:deny", vote.Voter); err != nil {
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
			if (misbound > 0 || late > 0) && res.Vote != "abandoned" {
				_ = record(fmt.Sprintf("human:votes-ignored:misbound=%d,late=%d", misbound, late), "")
			}

			// resolve the console projection with the terminal outcome (REQ-519) — best-effort, fail-open,
			// under the SAME change marker as the open above so pre-existing histories replay deterministically.
			// It runs for BOTH approve and deny/timeout/abandon, so a decided decision leaves the approvals list.
			if workflow.GetVersion(ctx, "pending-projection", workflow.DefaultVersion, 1) >= 1 {
				_ = workflow.ExecuteActivity(ctx, a.ResolvePendingActivity, ResolvePendingInput{
					ExternalRef: env.ExternalRef,
					ActionID:    gate.ActionID,
					Outcome:     res.Vote,
					ResolvedAt:  workflow.Now(ctx),
				}).Get(ctx, nil)
			}

			// spec/020 T-020-4 (REQ-2006): record the human approval choice on the sealed manifest — for BOTH
			// approve and deny/timeout/abandon, so the tracer shows how the action resolved. Observe-only, fail-open.
			backfillManifest(ctx, a, gate.ActionID, res.Vote, "")

			if !approved {
				res.Outcome = "proposal " + res.Vote + " — stood down without mutation"
				// A budget-exceeded stop is a runaway-session hand-off, not an ordinary poll disposition —
				// surface it distinctly (like the cycle-limit "escalated:*") so the judge/console/eval see a
				// wall-clock budget stop, never a silent human timeout.
				if res.Vote == "budget-exceeded" {
					res.Outcome = "escalated:budget-exceeded"
				}
				recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.SkillLoads)
				// Terminal close-out (spec/003): an UNANSWERED poll — a timeout OR a budget-exceeded stop — is
				// an ORPHANED poll: the reconciler flags it for a delayed re-check and reconcileTerminal hands it
				// off to the escalation lane, so an unresolved incident is re-examined and converges to a human,
				// never silently dropped (the reconcile→escalation hand-off). A denied/abandoned poll is
				// human-owned and left open (To Verify). Fail-safe + mutation OFF.
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

	// 4) execute + verify — routed through the wired-by-construction interceptor chain (spec/013), NOT a
	// direct OS call. Under mutation OFF the chain refuses at GuardMutation and records the refusal, so the
	// Runner still stops at propose — but through the REAL governed path, not around it. Mutated stays false
	// until the mutation key is turned (Phase-2, behind the proven gate). Approved carries the human vote
	// (INV-12): only a voted POLL_PAUSE action reaches the interceptor's admission gate with it true.
	// A mutation is NEVER automatically retried: Temporal activities are at-least-once, and re-running the
	// actuator on a timeout/blip would execute the estate change N times under ONE human approval. The
	// execute activity gets exactly one attempt; an ambiguous failure surfaces as a failed session for a
	// human to reconcile (the idempotent short-circuit inside the activity also refuses a re-run whose
	// verdict already exists). Fail closed, not fail again.
	execCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	ei := ExecuteInput{
		ActionID: gate.ActionID, ExternalRef: env.ExternalRef, PlanHash: planHash, Site: env.Site, TargetHost: inv.Proposal.Action.Target, Approved: approved,
		// Thread the investigation's cited evidence + orchestrator-captured tool results into the execute
		// activity so the interceptor's evidence gate binds real grounding (INV-11) — the same evidence the
		// classifier admitted. Under mutation OFF this changes nothing (the chain still refuses at its gate).
		EvidenceIDs: inv.Proposal.EvidenceIDs, ToolResults: inv.ToolResults,
		// The FRESH per-incident classification band (TG-126): the interceptor's 1b admission gate enforces THIS
		// live band rather than the sealed manifest's frozen first-seal band. The manifest is content-addressed
		// by action_id and sealed first-wins, so a re-classified later incident of the same action shape reloads a
		// stale frozen band — decision.Band carries the CURRENT classification the admission gate must honor.
		Band: decision.Band,
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
		return res, err
	}
	res.Mutated = exec.Executed
	var ver VerifyResult
	if err := workflow.ExecuteActivity(ctx, a.VerifyActivity, ei).Get(ctx, &ver); err != nil {
		return res, err
	}
	// spec/020 T-020-4 (REQ-2006): record the post-execution mechanical verdict on the sealed manifest so the
	// tracer shows how the action resolved. Under mutation OFF the verify activity reports nothing verified
	// (empty verdict ⇒ backfillManifest no-ops), so this is inert-but-complete until Phase 2. Observe-only.
	backfillManifest(ctx, a, gate.ActionID, "", safety.Verdict(ver.Verdict))

	res.Outcome = "proposed"
	recordTriage(ctx, a, env, res, inv.Proposal.Action.Op, inv.SkillLoads)
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
				if err := workflow.ExecuteActivity(ctx, a.RecoveredSinceActivity, RecoveredSinceInput{Host: env.Host, Since: recoverySince}).Get(ctx, &ok); err == nil {
					return ok
				}
				return false // a belt error is never a recovery (fail-closed)
			}
			deadline := execAt.Add(clearMaxWindow)
			quiet := 0
			for {
				if serr := workflow.Sleep(ctx, clearRecheckInterval); serr != nil {
					break // cancelled ⇒ fail-closed
				}
				cleared := observeCleared()
				if !cleared {
					cleared = recoveredSince() // BELT: TG's own captured recovery confirms even when the re-pull lags
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
	reconcileTerminal(ctx, a, env, ReconcileInput{
		ActionID:          gate.ActionID,
		Band:              decision.Band,
		HasVerdict:        reconHasVerdict,
		Verdict:           reconVerdict,
		HasTerminalResult: true,
		Executed:          res.Mutated,
		ConfirmedClear:    confirmedClear,
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
func recordTriage(ctx workflow.Context, a *Activities, env ingest.IncidentEnvelope, res RunnerResult, op string, skillLoads []string) {
	if workflow.GetVersion(ctx, "triage-record", workflow.DefaultVersion, 1) < 1 {
		return
	}
	_ = workflow.ExecuteActivity(ctx, a.RecordTriageActivity, judge.TriageRow{
		ExternalRef: env.ExternalRef,
		Host:        env.Host,
		AlertRule:   env.AlertRule,
		Band:        res.Band,
		Outcome:     res.Outcome,
		Proposed:    res.Proposed,
		Op:          op,
		EvidenceIDs: res.EvidenceIDs,
		Conclusion:  res.Conclusion,
		Prediction:  res.Prediction, // TG-61: the committed prediction the live judge cron scores
		Predicted:   res.Predicted,
		Confidence:  res.Confidence, // spec/020 REQ-2003: the agent's emitted proposal confidence (observability)
		// spec/023 REQ-2311: the WHO-CAUSED-THIS taxonomy + its minimized evidence blob (observability only).
		Attribution:   res.Attribution,
		ActorEvidence: res.ActorEvidence,
		SkillLoads:    skillLoads,
		// spec/020 REQ-2009: the session's prompt/seed/model provenance (observability only).
		PromptVersion: res.PromptVersion,
		SeedHash:      res.SeedHash,
		ModelTier:     res.ModelTier,
		CreatedAt:     workflow.Now(ctx).UTC(),
	}).Get(ctx, nil)
}

// backfillManifest records a sealed manifest's LATER lifecycle labels — the human approval choice (after the
// vote) and the post-execution mechanical verdict (after verify) — onto the already-sealed action_manifest row
// for the decision tracer (spec/020 T-020-4, REQ-2006). Best-effort and fail-open: the activity swallows write
// errors and the workflow discards the future's error — a backfill failure must NEVER fail the session.
// OBSERVE-ONLY: it writes only the two NON-HASHED columns, so it cannot tamper the sealed binding (INV-07) and
// it changes no gate/verdict/control flow. Guarded by its own change marker so pre-existing histories replay
// deterministically; a no-op when there is no action_id or nothing to backfill.
func backfillManifest(ctx workflow.Context, a *Activities, actionID, approvalChoice string, verdict safety.Verdict) {
	if workflow.GetVersion(ctx, "manifest-backfill", workflow.DefaultVersion, 1) < 1 {
		return
	}
	if actionID == "" || (approvalChoice == "" && string(verdict) == "") {
		return
	}
	_ = workflow.ExecuteActivity(ctx, a.BackfillManifestActivity, BackfillManifestInput{
		ActionID:       actionID,
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
	_ = workflow.ExecuteActivity(ctx, a.ReconcileActivity, in).Get(ctx, nil)
}
