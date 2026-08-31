// Package actuate is the wired-by-construction pre/post interception chain — the single actuation
// chokepoint through which every governed mutation must pass. Execute is reachable ONLY through the
// interceptor (the underlying actuator is an unexported field), so a mutating side effect cannot bypass
// the chain. Every failed check REFUSES loud (surfaces an error/refusal and records it), never
// observe-only via a swallowed exception. Mutation ships OFF (mode Shadow) and can be enabled only through an
// operator-authorized, preflight-gated mode transition into Semi-auto/Full-auto (the absorbed mutation gate).
//
// Provenance: [O] INV-09 (mutation off + never-auto floor at the adapter, defense in depth), INV-10
// (predict-before / mechanical verdict), INV-11 (evidence-bound), INV-21/S8-5 (wired-by-construction,
// fail loud, no dark control), spec/013 · [F] "deterministic orchestrator owns the effect channel" ·
// [R] paradigm-rules 4/8. This is Phase-2 behavior; mutation defaults off and the chain is proven here.
package actuate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/territory"
	"github.com/territory-grounder/grounder/core/trace"
	"github.com/territory-grounder/grounder/core/verify"
)

// Evidence is an orchestrator-captured tool-result reference. A mutating action is admissible only if it
// cites at least one bound evidence — captured by the orchestrator (never agent free-text), successful,
// recent, and target-relevant (INV-11).
type Evidence struct {
	ToolResultID string
	Captured     bool
	Successful   bool
	Recent       bool
	Relevant     bool
}

// Bound reports whether the evidence is admissible on all four axes.
func (e Evidence) Bound() bool { return e.Captured && e.Successful && e.Recent && e.Relevant }

func hasBoundEvidence(es []Evidence) bool {
	for _, e := range es {
		if e.Bound() {
			return true
		}
	}
	return false
}

// Request is one governed actuation request threaded through the interceptor.
type Request struct {
	Manifest *manifest.ActionManifest // the sealed, content-hashed action (INV-07)
	// ExternalRef is the NON-SECRET normalized incident trigger this action answers, threaded from the workflow
	// so the audited policy_decision joins the decision-tracer walk by external_ref (spec/020 REQ-2005). It rides
	// into the policy audit projection + the "runner:<ref>" principal, and it feeds NO verdict.
	//
	// SINCE TG-166a IT FEEDS EXACTLY ONE GATE: it is the SESSION KEY of the actuation-frequency governor. This
	// line used to read "It NEVER feeds a gate/verdict", which was true and is now half true — recorded here
	// rather than quietly amended, because "per-session rate limit" needs a session identity and external_ref
	// IS this system's session identity (session_triage is keyed by it, the workflow is one run per incident).
	// The governor treats an EMPTY ref as an unattributed session sharing ONE budget with every other
	// unattributed actuation, never as an exemption, so nothing about this field's absence can loosen a gate.
	ExternalRef string
	// InvertsActionID, when non-empty, marks this request the INVERSE of the named forward action — a
	// compensating rollback. It rides through to the per-execution record (inverts_action_id) so an executed
	// inverse is durable and queryable, not a log-string only (TG-404). "" is a forward action. It is NOT part
	// of the content-hashed action identity: an inverse has its own action_id (the rollback argv's hash); this
	// only names what it undoes. TG-82's auto-revert is the production caller that will set it; today it is
	// exercised by the end-to-end oracle (mutation is off, zero inverses have run).
	InvertsActionID string
	Gated           bool              // a committed prediction produced this (from the prediction gate)
	Argv            []string          // the FIXED argv vector — no shell, no string-built command (INV-02)
	Stdin           []byte            // validated stdin bytes
	Evidence        []Evidence        // orchestrator-captured tool-results (INV-11)
	Prediction      verify.Prediction // the committed prediction, for the post-execution verdict
	// Observe captures the estate's active alerts for the post-execution verdict. It returns (alerts, ok):
	// ok=false means the post-state could NOT be observed (e.g. a monitoring read error), so the verifier fails
	// CLOSED — no durable verdict, no graduation credit (TG-182), mirroring ClearObserve. ok=true with an empty
	// slice is a genuinely quiet estate (a healthy heal → match). A nil Observe (no observer wired at all) is
	// refused before execute by the verifiability gate (4c).
	Observe func(context.Context) ([]verify.ObservedAlert, bool) // captures observed alerts after execution
	// PreAnomalous captures the hosts that ALREADY held an open incident (a raise with no recovery, from the
	// durable ingest ledger) at the moment it is called — the verifier's HOST-level baseline arm. It is called
	// once at the baseline gate, immediately before execution, so the returned set is by construction
	// pre-action and cannot contain anything the action caused. ok=false means the set could not be read; the
	// gate treats an unestablished set exactly like an unestablished pair baseline (see the baseline gate),
	// NEVER as an empty one — (nil,false)≠(empty,true) is the seam the 2026-07-28 false deviation walked
	// through on the pair arm. nil (unwired: no DB) leaves the pair arm as the sole baseline, recorded at the
	// gate rather than silent.
	PreAnomalous func(context.Context) (map[string]bool, bool)
	// StillFaulted re-runs, at the LAST pre-effect instant, the observation that justified this action, and
	// answers ONE question: is the fault this action answers STILL VISIBLE? It returns (present, ok) — ok=false
	// means the re-check could not be performed at all.
	//
	// ★ WHY IT EXISTS (TG-166b). Every gate above proves the action is SAFE — reversible, allowlisted,
	// evidence-bound, predicted, policy-authorized, in-territory. Not one of them asks whether it is still
	// NECESSARY. The evidence that justified the mutation was captured during the investigation, minutes and a
	// model round-trip ago; between then and here the unit may have been restarted by a human, by config
	// management, or by systemd's own restart policy, and the alert may have cleared. The chain would still
	// fire the effect, because "was true when we looked" is the only thing it ever checked.
	//
	// ★ WHAT IT PROVES, AND WHAT IT DOES NOT — stated plainly because the honest answer is narrow:
	//   • It can only FALSIFY necessity, never establish it. present=false is strong: the justification is
	//     gone, so the gate refuses. present=true is weak: it means "not refuted", and the gate treats it as
	//     nothing more than that. Nothing here licenses an action; it only withdraws a licence.
	//   • It proves the fault was visible at T_recheck, not at T_execute. There is an irreducible race between
	//     the read and the effect and no amount of moving this line closer to Exec closes it. The value is that
	//     T_recheck is SECONDS before the effect instead of MINUTES, which is where the realistic drift lives.
	//   • It is only as sharp as the probe the caller supplies. TG's production wiring re-runs the SAME live
	//     active-alert read the clear-check uses (runner ClearObserve) and asks whether the target host is
	//     still alerting AT ALL — deliberately host-quiet rather than the exact (host, rule), exactly as
	//     ObserveClearedActivity argues: a host carrying a DIFFERENT alert must not read as clear. The cost of
	//     that coarseness is symmetrical and must not be oversold — a host quiet on the monitoring surface can
	//     still have the specific unit down, and a host that is alerting may be alerting about something this
	//     action does not fix.
	//   • It is NOT a substitute for the post-execution verifier, and it grants no graduation credit. It is a
	//     pre-condition, not an outcome.
	//
	// nil is NOT a pass. A request that reaches the effect with no necessity re-check wired is refused at gate
	// 4i, the same discipline gate 4c applies to a nil post-execution observer: we do not execute what we
	// cannot even attempt to check. An unwired seam would otherwise be the classic optional control — present
	// in the design, absent in the deployment, and invisible in both.
	StillFaulted func(context.Context) (present bool, ok bool)
	// CaptureState, when wired, snapshots the action target's rollback-relevant state at the LAST pre-effect
	// instant (TG-58), returning (snapshot, ok). ok=false means the state could not be read: the capture is
	// audited as a gap and execution PROCEEDS — pre-state capture is rollback PREP, not a safety gate. Unlike the
	// baseline (4c) and necessity (4i) gates, a missing capture does NOT make the forward action unsafe, only
	// un-restorable, which is a Phase-2 arming concern, not a reason to refuse a safe heal now. nil (the default,
	// and every caller until the Phase-2 rollback story is wired) ⇒ no capture, pre-effect path byte-identical.
	CaptureState func(context.Context) (PreState, bool)
	// HostSite is the ESTATE-DERIVED site vocabulary the verifier's coincidental-cross-site filter keys on
	// (spec/002 REQ-107; estate.Graph.SiteOf in production, threaded through the runner deps). The verdict
	// excludes a surprise candidate ONLY when this authority knows BOTH the candidate's site AND the target's
	// site and they differ — never from an alert's self-reported ingest label; an unknown-site host is NEVER
	// excluded. nil (no estate wired) excludes nothing, which is the pre-C4 fully fail-closed behavior. The
	// stakes are recorded at governance_ledger seq 6555: an unrelated 59-second sensor flap at the OTHER site
	// scored a deviation, demoted restart-container auto→approve and discarded ~80 hands-off clean runs.
	HostSite verify.SiteAuthority
	// Confidence is the bound model confidence carried into the policy engine's confidence clamp (REQ-1507).
	// The zero value (0.0) is below the engine's default min_confidence, so an unset confidence clamps a policy
	// `auto` to `approve` — fail closed (an unconfident action never auto-authorizes).
	Confidence float64
	// Acknowledged is the set of high-stakes territories whose operating manual was loaded this session — the
	// grounding prerequisite for the territory gate. A mutating action in an unacknowledged high-stakes
	// territory is refused (INV-21 territory control). Empty ⇒ nothing acknowledged.
	Acknowledged map[territory.Territory]bool
	// Approved records that a human approval vote authorized this action. A POLL_PAUSE-band action may
	// auto-execute ONLY when it is true — the vote binds the decision (INV-12). An AUTO/AUTO_NOTICE band was
	// already admitted by the classifier and needs no approval. Zero value = not approved (fail closed).
	Approved bool
	// Band is the CURRENT incident's classification band (the fresh spec/001 verdict — decision.Band for THIS
	// incident), threaded LIVE from the workflow and SEPARATE from r.Manifest.Band. BOTH band-sensitive controls
	// — the 1b human-approval admission and the 4d policy authorization below — read THIS band, not the sealed
	// manifest's band. Rationale (TG-126): the sealed ActionManifest is CONTENT-ADDRESSED by action_id and
	// persisted first-seal-wins (db.ManifestStore.Seal, ON CONFLICT (action_id) DO NOTHING), so its band is
	// FROZEN at the FIRST sealing of that action identity. A LATER incident of the same action SHAPE
	// re-classifies to a fresh band but cannot re-seal, so r.Manifest.Band is stale for every incident after the
	// first — it would wrongly BLOCK a fresh AUTO under a frozen POLL_PAUSE (the confirmed bug), and at 4d the
	// policy engine's band composition (ComposeBand) would floor an otherwise-`auto` graduated class to `approve`
	// and re-block the same fresh-AUTO incident; symmetrically, a frozen AUTO must never LEAK past a fresh
	// POLL_PAUSE at either gate. Admission AND authorization are per-INCIDENT-classification properties, so both
	// MUST read this fresh band. The zero value is safety.BandPollPause (the most-restrictive band, by design),
	// so an absent/unknown/zero fresh band FAILS CLOSED — it requires an approval and never auto-admits or
	// auto-authorizes. r.Manifest.Band is retained ONLY for the action's content-addressed IDENTITY + audit and
	// feeds NO admission or authorization decision.
	Band safety.Band
}

// Outcome is the result of an interception. A refused outcome carries the reason; an executed outcome
// carries the mechanical verdict written by the deterministic verifier (never the acting model).
type Outcome struct {
	Executed bool
	Refused  bool
	Reason   string
	Verdict  safety.Verdict
	ActionID string
	// RateLimited marks a refusal that came from the ACTUATION-FREQUENCY governor (TG-166a) rather than from
	// anything wrong with the action. It exists because a throttled actuation must not read as an unrelated
	// failure: without it the caller sees only Refused+Reason and a "the effect leaf exited 42" and a "you have
	// actuated this host three times in ten minutes" are the same shape to every consumer. The Reason string
	// also carries RefusalRateLimited for the human surfaces; this is the machine-readable half.
	RateLimited bool
	// Cancelled marks a refusal that came from the caller's CANCELLATION (deadline or explicit) landing while
	// the effect was RUNNING — the remote command was signalled dead over the SSH channel before the
	// transport closed (TG-80 P1-4, modules/actuation/ssh). Before this field every such abort collapsed
	// into the generic "execute failed" prose, indistinguishable from a bad host key or a dead link — and the
	// remote process was simply orphaned. Machine-readable half of RefusalCancelled; the effect did NOT
	// complete, so Executed stays false.
	Cancelled bool
	// AsyncHandle is the job handle an ASYNC lane's launch returned (spec/017 REQ-1709, TG-122 slice 0): the
	// AWX job id / the GitOps MR reference. It is a PREDICTION, not a success — the estate is untouched when
	// such a launch returns, and the deferred-verify channel (core/regime/asyncverify.go) is the sole author
	// of the eventual verdict. The interceptor itself NEVER sets this field (its chain is launch-shape
	// agnostic); regime.LaneEffect fills it after Do returns, from the handle its capture decorator observed
	// at the leaf, so the caller can bind the handle to the reserved pending-verification record. Empty for
	// every synchronous lane and for every refusal.
	AsyncHandle string
}

// RefusalRateLimited is the stable token every actuation-frequency refusal reason starts with, so an operator
// (and the console/tracer) can tell a THROTTLE from a failure without parsing prose. TG-166a.
const RefusalRateLimited = "actuation rate limit"

// RefusalCancelled is the stable token every cancellation-terminal reason starts with (TG-80 P1-4): the
// effect was cut short by the caller's deadline/cancel and the remote command was signalled dead, not
// abandoned. Stable so the console/tracer and the session record can tell a CANCELLED run from a failure.
const RefusalCancelled = "actuation cancelled"

var (
	// ErrGateUnwired is returned by SelfTest (and Do) when a collaborator is missing — a control that
	// cannot execute must fail LOUD and safe, never be left dark (S8-5).
	ErrGateUnwired = errors.New("actuate: interception chain is unwired (a governed control is missing) — refusing")

	// baselineRetryDelay is the single bounded backoff between the baseline gate's two attempts at each
	// baseline arm — sized for a transient monitoring-read blip (the observed failure was a one-second
	// window), not an outage. A var so oracles can shrink it; production never mutates it.
	baselineRetryDelay = 500 * time.Millisecond
)

// VerdictSink durably records the mechanical verdict for an executed action (the pgx db.VerdictStore
// satisfies it). The deterministic verifier is the only writer; the interceptor persists exactly one verdict
// per action_id after computing it. Optional — nil ⇒ the in-memory path (the verdict rides only the ledger).
type VerdictSink interface {
	Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error
}

// ExecutionSink durably records ONE ROW PER EXECUTION with the fresh verdict computed against THAT
// execution's post-state (the pgx db.ActionExecutionStore satisfies it).
//
// It exists because VerdictSink cannot answer "how did THIS run go?". action_id is content-addressed over the
// operation alone, and action_verdict is keyed by it first-wins — deliberately, since spec/012 relies on that
// row as the shape's FIRST verified outcome. The consequence is that re-running the same operation records
// nothing: measured live, 113 executions collapsed into 28 durable outcomes, so "N independent hands-off
// heals of class X" was unrecordable.
//
// Optional — nil ⇒ per-execution recording is dark and behaviour is byte-identical to before. A record failure
// is NEVER fatal to an execution that already happened: it is audited as a control gap, exactly like the
// durable-verdict path, because the mutation cannot be un-run.
type ExecutionSink interface {
	// invertsActionID (LAST param, "" for a forward action) names the forward action this execution undoes,
	// so an executed inverse leaves a durable, queryable record instead of a log-string only (TG-404).
	Record(ctx context.Context, actionID, externalRef, targetHost, site string, v safety.Verdict, verified bool, invertsActionID string) error
}

// PreState is a target-state snapshot captured at the LAST pre-effect instant (TG-58, a Phase-2 governed-autonomy
// prerequisite): the rollback-relevant state of the action's target BEFORE the mutation. The recorded inverse
// (execution_log, INV-07) says HOW to undo; this says WHAT the world was, so a Phase-2 applied-undo executor has
// a concrete state to restore TO. Opaque to the interceptor: the caller's CaptureState produces the target/
// op-class-specific snapshot; the chokepoint captures it and hands it to the durable sink, never interpreting it.
type PreState struct {
	Kind string // the op-class / target-kind the snapshot describes (e.g. "service", "guest") — for the consumer
	Data []byte // the opaque serialized snapshot the caller captured; the interceptor never reads its contents
}

// PreStateSink is the OPTIONAL durable writer for TG-58 pre-mutation state captures: given the action id the
// interceptor authorized and the snapshot taken immediately before the effect, it persists the pre-state so a
// future applied-undo executor can restore to it. Optional — nil ⇒ pre-state capture is DARK and the pre-effect
// path is byte-identical. A record failure is NEVER fatal to an execution that already happened (the mutation
// cannot be un-run): it is audited as a control gap, exactly like ExecutionSink.
type PreStateSink interface {
	RecordPreState(ctx context.Context, actionID string, s PreState) error
}

// ExecRecorder is an OPTIONAL capability of an effect-leaf actuator: given the action id the interceptor
// authorized and the argv it just executed, it derives that mutation's execution_log — the forward command
// and its compensating inverse (INV-07). AFTER a successful execute Do records this to the tamper-evident
// ledger, bound to the action_id, so a mutation is attributable and undoable — the effect leaf owns the
// inverse derivation, the interceptor owns the durable write. A read-only reference actuator does not
// implement it (there is nothing to record); the ssh mutating build does. A nil forward means "nothing
// mutating to record". WHILE mutation is off Do refuses before execute, so this is never reached.
type ExecRecorder interface {
	ExecLog(actionID string, command []string) (forward, rollback []string, err error)
}

// HostBound is an OPTIONAL capability of an effect-leaf actuator that executes on a SINGLE fixed host it does
// not receive per-action: the SSH mutating leaf wraps the argv as `identity@<configured-host>` and never reads
// the action's target, so an action admitted for host X would otherwise mis-execute on the configured host.
// An actuator that exposes ActuationHost lets the interceptor's host-match gate refuse a target mismatch
// (fail-closed). A non-empty value is the host every mutation of this leaf lands on; "" (or not implementing
// it) means the leaf is not single-host-bound (a per-target or resource-id leaf, e.g. the Proxmox/k8s leaves)
// and the gate is a no-op for it.
type HostBound interface {
	ActuationHost() string
}

// PolicyDecider is the narrow authorization seam the interceptor consults before it actuates (spec/015):
// policy.Engine.Decide (via *policy.AuditedEngine, which also audits every decision, REQ-1518). It resolves an
// action to auto/approve/deny by deny-overrides over the operator rule data — an INDEPENDENT control layer
// from the mechanical mode chokepoint (REQ-1521), never folded into it. Interface-typed so the interceptor is
// testable with a fake and never itself constructs a Rego engine.
type PolicyDecider interface {
	Decide(ctx context.Context, in policy.EvalInput) (policy.PolicyDecision, error)
}

// GraduationRecorder is the OPTIONAL earn-path seam (spec/013 REQ-1217, wiring spec/015 REQ-1514): AFTER a
// governed action EXECUTES and its post-state is VERIFIED, the interceptor feeds the run outcome to the
// per-op-class graduation ladder so a verified-clean run accrues toward `auto`. *policy.Ladder satisfies it.
// It is a WRITE-ONLY seam consulted STRICTLY AFTER a completed, verified, already-governed actuation — it
// authorizes NO action and gates NOTHING (the never-auto floor, the evidence/territory/verifiability gates,
// the policy verdict, the breaker, and the mode chokepoint all ran BEFORE execute). It NEVER re-runs or
// re-adjudicates verification: it consumes ONLY the deterministic verifier's verdict (INV-10), mapped to a
// RunOutcome at the boundary. The recorder is the SAME ladder the policy engine READS via GraduatedVerdict,
// so wiring it CLOSES the earn-loop — without this write the ladder dead-locks (no class ever records a clean
// run, so none graduates). A nil recorder is a documented no-op (the sync path simply does not advance the
// ladder — no regression); the real worker MUST wire it.
type GraduationRecorder interface {
	Record(ctx context.Context, opClass string, outcome policy.RunOutcome) (policy.RecordResult, error)
}

// Interceptor is the wired-by-construction actuation chain. Its actuator is UNEXPORTED, so the only way
// to reach Execute is through Do — the single chokepoint (S8-5).
type Interceptor struct {
	chokepoint *safety.Chokepoint // the mode-driven actuation chokepoint (the absorbed MutationGate, REQ-1520)
	actuator   actuation.Actuator
	ledger     *audit.Ledger
	verdicts   VerdictSink             // optional durable verdict writer; nil ⇒ the verdict rides only the ledger record
	executions ExecutionSink           // optional per-EXECUTION recorder; nil ⇒ only the first run of each action shape leaves a durable outcome
	preStates  PreStateSink            // optional TG-58 pre-mutation state recorder; nil ⇒ pre-state capture is dark, pre-effect path byte-identical
	breaker    *safety.MutationBreaker // optional armed breaker; a post-execution deviation/chain-gap forces Shadow
	admission  TargetAdmission         // optional DURABLE cross-process per-target admission + cooldown (TG-81 b2); nil ⇒ unarmed pass-through
	decider    PolicyDecider           // optional policy authorizer (spec/015); nil ⇒ pass-through (mode chokepoint still gates)
	modeNow    func() policy.Mode      // reads the active mode for the policy EvalInput; nil ⇒ ModeShadow (fail closed)
	// objectGroups resolves the target host's object-group membership for the policy EvalInput (TG-481, spec/016
	// REQ-1618). It MUST read the SAME operator-authored object-group store the credential resolver reads (the
	// worker hands both planes one db.EstateObjectGroupStore), so a group-scoped policy rule and a group-scoped
	// credential rule share ONE definition — never a second. nil ⇒ the policy engine sees no groups here, exactly
	// as before TG-481 (byte-identical); and since the default ruleset scopes no rule by group, wiring it is also
	// byte-identical until an operator authors a group-scoped rule (dormant-safe).
	objectGroups func(host string) []string
	// composer is the REQ-1604 authN layer (spec/016 T-016-5): resolves the target identity AFTER the
	// policy verdict, BEFORE anything executes. nil ⇒ today's deployment (the effect leaf's static
	// identity) — the gate row states that honestly and the chain is byte-identical.
	composer func(ctx context.Context, targetHost string) (ruleID string, err error)
	grad     GraduationRecorder    // optional graduation earn-path recorder (REQ-1217/1514); nil ⇒ the sync path does not advance the ladder
	gateSink trace.GateVerdictSink // optional OBSERVE-ONLY per-gate verdict trail (spec/020 T-020-7); nil ⇒ no-op, gate behavior identical
	// limiter is the per-session/per-target actuation-frequency + in-flight governor (TG-166a). It is NOT
	// optional and has no nil path: NewInterceptor installs a default-budget limiter by construction, so the
	// control cannot be forgotten at a wiring site. WithActuationLimiter swaps in a SHARED instance — which a
	// multi-interceptor composition root (cmd/worker builds one direct interceptor plus one per regime lane)
	// MUST do, or each lane gets its own private budget and the fleet cap silently becomes (lanes × cap).
	limiter *ActuationLimiter
}

// WithGateVerdictSink attaches the OBSERVE-ONLY per-interceptor-gate verdict trail (spec/020 T-020-7, REQ-2007)
// and returns the interceptor (chainable). The interceptor emits one ordered row per gate as it runs; the sink
// is a PURE SIDE EFFECT — an Emit error is swallowed and NEVER changes a gate outcome, and nothing reads the
// trail back to make a decision. A nil sink (the default) is a no-op: the chokepoint behaves identically.
func (i *Interceptor) WithGateVerdictSink(s trace.GateVerdictSink) *Interceptor {
	i.gateSink = s
	return i
}

// WithObjectGroupResolver wires the shared object-group membership resolver into the policy EvalInput (TG-481,
// spec/016 REQ-1618) and returns the interceptor (chainable). Pass the SAME resolution the credential plane
// uses (both read one db.EstateObjectGroupStore) so a group-scoped policy rule and a group-scoped credential
// rule consume one definition. A nil resolver (the default) leaves the policy decision seeing no groups —
// byte-identical to pre-TG-481.
func (i *Interceptor) WithObjectGroupResolver(fn func(host string) []string) *Interceptor {
	i.objectGroups = fn
	return i
}

// WithComposer wires the REQ-1604 authN compose layer (spec/016 T-016-5, credential.Composer.Compose):
// the target identity resolves as its OWN gate after authorization and before execute, so a target the
// operator declared no identity for refuses at a named control instead of deep inside an effect leaf.
// nil (the default) leaves the chain byte-identical.
func (i *Interceptor) WithComposer(fn func(ctx context.Context, targetHost string) (string, error)) *Interceptor {
	i.composer = fn
	return i
}

// resolveObjectGroups returns the target host's object-group names for the policy EvalInput, or nil when the
// resolver is unwired or the host is empty (nil-safe; an unwired interceptor decides exactly as before).
func (i *Interceptor) resolveObjectGroups(host string) []string {
	if i.objectGroups == nil || host == "" {
		return nil
	}
	return i.objectGroups(host)
}

// WithExecutionSink attaches a per-execution recorder and returns the interceptor (chainable). Without one,
// only the FIRST execution of each action shape leaves a durable outcome (action_verdict is keyed by the
// content-addressed action_id, first-wins), so repeated heals of the same shape are invisible.
func (i *Interceptor) WithExecutionSink(e ExecutionSink) *Interceptor {
	i.executions = e
	return i
}

// WithPreStateSink attaches a TG-58 pre-mutation state recorder and returns the interceptor (chainable). Without
// one — and without a Request.CaptureState hook — no pre-state is captured and the pre-effect path is
// byte-identical to before this seam existed. The concrete DB-backed sink and the op-class-specific CaptureState
// functions are the follow-on Phase-2 rollback wiring; this is the chokepoint seam they bind to.
func (i *Interceptor) WithPreStateSink(p PreStateSink) *Interceptor {
	i.preStates = p
	return i
}

// WithVerdictSink attaches a durable verdict writer and returns the interceptor (chainable).
func (i *Interceptor) WithVerdictSink(v VerdictSink) *Interceptor {
	i.verdicts = v
	return i
}

// WithMutationBreaker arms the interceptor with the mutation breaker (chainable). Once armed, a
// post-execution DEVIATION verdict or a chain-integrity gap records a trip; at the breaker's threshold
// (default 1 for the first canary) the mode is FORCED to Shadow in-process (ForceShadow) — the runtime kill the
// readiness review (§4.B.2/§4.B.3) required. It is INERT under Shadow: Do refuses at the mode chokepoint long
// before any execution, so no verdict is ever produced and the breaker is never touched today.
func (i *Interceptor) WithMutationBreaker(b *safety.MutationBreaker) *Interceptor {
	i.breaker = b
	return i
}

// WithPolicyDecider wires the policy authorization layer (spec/015 T-015-13): before it actuates, the
// interceptor consults decider.Decide and honors the resolved verdict per REQ-1506 — `auto` proceeds; `deny`
// refuses unconditionally; `approve` (the "route to a human vote" verdict) proceeds ONLY when the required
// human approval is recorded (Request.Approved), else refuses (fail closed). Honoring a recorded approval on
// an `approve` verdict is how an ungraduated op-class earns its clean runs toward `auto` (REQ-1514) — without
// it the graduation ladder dead-locks, since an unseen class always resolves to `approve`. modeNow reads the
// active global mode for the policy EvalInput (carried into the decision audit); a nil modeNow reads
// ModeShadow. This is an INDEPENDENT control from the mode chokepoint
// (REQ-1521): the policy verdict authorizes the individual action; the chokepoint decides whether the system
// is in an actuating posture at all. A nil decider leaves the interceptor a pass-through on this layer — the
// mechanical mode chokepoint still gates every mutation.
func (i *Interceptor) WithPolicyDecider(decider PolicyDecider, modeNow func() policy.Mode) *Interceptor {
	i.decider = decider
	i.modeNow = modeNow
	return i
}

// WithGraduationRecorder wires the graduation earn-path (spec/013 REQ-1217, spec/015 REQ-1514): AFTER a
// governed action executes and its post-state is VERIFIED, the interceptor records the run outcome to the
// per-op-class graduation ladder so a verified-clean run accrues toward `auto`. The recorder is the SAME
// *policy.Ladder the policy engine READS via GraduatedVerdict, so wiring it CLOSES the earn-loop: without this
// write the ladder dead-locks — no class ever records a clean run and none can graduate. It is a WRITE-ONLY
// seam consulted only on the post-verify tail; it authorizes nothing and weakens no gate (the never-auto
// floor, the evidence/territory/verifiability gates, the policy verdict, the breaker, and the mode chokepoint
// all ran BEFORE execute). A nil recorder leaves the sync path a documented no-op (the mode chokepoint still
// gates every execute; the awx-job async lane feeds the same ladder via its own deferred-verify sink).
// Chainable.
func (i *Interceptor) WithGraduationRecorder(g GraduationRecorder) *Interceptor {
	i.grad = g
	return i
}

// tripBreaker records a post-execution safety failure to the armed breaker, if one is wired. A nil breaker
// is a no-op. At the breaker's threshold this forces the mode to Shadow (the canary kill-switch). It is only
// ever reached AFTER an execution, which cannot happen under Shadow — so it is inert today.
func (i *Interceptor) tripBreaker(ctx context.Context, reason string) {
	if i.breaker == nil {
		return
	}
	_, _ = i.breaker.Trip(ctx, reason)
}

// NewInterceptor wires the chain. A nil collaborator is permitted at construction but fails SelfTest and
// Do loudly, so an unwired chain can never silently execute.
//
// The actuation-frequency governor (TG-166a) is installed HERE, not through an optional With… seam, and with
// the conservative DefaultActuationLimits. Every other governor in this chain is optional because its absence
// is a legitimate deployment shape (no policy engine ⇒ read-only; no breaker ⇒ no cross-process kill); an
// absent rate limit is never a legitimate shape, it is just the pre-TG-166 hole. Building it in means a
// composition root cannot forget it and there is no nil branch to reason about.
func NewInterceptor(chokepoint *safety.Chokepoint, actuator actuation.Actuator, ledger *audit.Ledger) *Interceptor {
	return &Interceptor{
		chokepoint: chokepoint,
		actuator:   actuator,
		ledger:     ledger,
		limiter:    NewActuationLimiter(nil),
	}
}

// WithActuationLimiter swaps in a SHARED actuation-frequency governor and returns the interceptor
// (chainable). A composition root that builds MORE THAN ONE interceptor — cmd/worker builds the direct
// native-ssh chain plus one per regime lane from the same builder — must pass the SAME limiter to all of
// them, otherwise each chain counts its own window and the per-session/per-target cap silently multiplies by
// the number of lanes. A nil argument is IGNORED (the constructor's default limiter stays), so this seam can
// never be used to remove the control: there is no "off".
func (i *Interceptor) WithActuationLimiter(l *ActuationLimiter) *Interceptor {
	if l != nil {
		i.limiter = l
	}
	return i
}

// ReachabilityProber is the OPTIONAL effect-leaf capability behind gate 4h3 (TG-81 b4; clean-room from
// h-network's all-or-nothing batch pre-flight, attribution: SOURCE-BENCHMARK-CATALOG): a leaf that can
// cheaply prove its TRANSPORT to the action's target before any estate-touching step. Today a manifest
// carries exactly one target, so "probe the whole blast radius, abort before touching anything" is a
// one-target loop; when fleet manifests land, the same gate iterates every target and one unreachable
// member aborts the batch whole. Structural type-assertion like HostBound/ExecRecorder: a leaf that
// cannot probe is a documented pass-through (the exec failure still fails closed downstream).
type ReachabilityProber interface {
	// ProbeReachable proves the transport to target without executing anything. ok=false carries the
	// operator-facing detail; the gate refuses on it.
	ProbeReachable(ctx context.Context, target string) (ok bool, detail string)
}

// WithTargetAdmission arms the DURABLE cross-process per-target admission + cooldown store (TG-81 b2,
// chainable). Like the mutation breaker and for the same reason, a nil argument leaves the seam unarmed
// (a legitimate deployment shape — the in-process limiter still governs); once armed, every failure
// mode REFUSES: a held claim, an active cooldown, and an unreachable store are indistinguishable at the
// gate and all fail closed. Multi-interceptor composition roots must pass the SAME store to every chain
// — that is the entire point of the seam being durable.
func (i *Interceptor) WithTargetAdmission(s TargetAdmission) *Interceptor {
	if s != nil {
		i.admission = s
	}
	return i
}

// canActuateNow reports whether the system is in an actuating posture, asked of the MODE CHOKEPOINT.
//
// It deliberately does NOT read i.modeNow. That reader is installed by WithPolicyDecider ALONGSIDE the
// decider, so in the exact failure this guard exists for — the policy engine failing to build, leaving the
// decider nil — modeNow is nil too. Keying on it would make the guard silently inert precisely when it is
// needed, which is worse than no guard because it reads as protection. The chokepoint, by contrast, is a
// REQUIRED collaborator (SelfTest refuses to boot without it) and is bound to the live mode authority, so it
// always knows the real posture.
func (i *Interceptor) canActuateNow() bool {
	return i.chokepoint != nil && i.chokepoint.MayActuate()
}

// SelfTest asserts every REQUIRED collaborator is wired — the mode chokepoint, the effect-leaf actuator, and
// the ledger. The boot preflight calls it (and Chokepoint.ProvePreflight only marks the preflight green when it
// passes); a nil collaborator fails loud so a dark control cannot be booted (INV-21/S8-5).
//
// The policy decider is NOT required here, but its absence is no longer a pass-through: Do REFUSES any action
// while the chokepoint reports an actuating posture and no decider is wired (roadmap P2-3). Boot is therefore
// still permitted without a policy engine — a read-only/Shadow deployment is legitimate — while an actuating
// one cannot proceed with its policy layer absent. Keeping the check at the actuation point rather than here
// is deliberate: it fails closed exactly where the harm would occur and leaves every read-only, oracle and
// in-memory construction working unchanged.
func (i *Interceptor) SelfTest() error {
	if i == nil || i.chokepoint == nil || i.actuator == nil || i.ledger == nil {
		return ErrGateUnwired
	}
	return nil
}

// Do runs the governed actuation chain in the spec/013 + spec/015 order: admission (poll-approval → never-auto
// floor (adapter, defense in depth) → structure gate (committed prediction + action_id) → evidence → territory
// → verifiability) → policy-authorize (Decide) → mode-chokepoint (may-actuate) → execute (the single
// chokepoint) → verify → audit. Credential-authenticate is resolved downstream in the effect leaf (already
// wired). Every failed check REFUSES loud and records the refusal to the ledger; it never swallows a check into
// an observe-only pass. The policy verdict and the mode chokepoint are INDEPENDENT fail-closed layers
// (REQ-1521), each of which alone can refuse. Returns an error only for an unwired chain (fail loud).
func (i *Interceptor) Do(ctx context.Context, r Request) (Outcome, error) {
	if err := i.SelfTest(); err != nil {
		return Outcome{}, err // an unwired chain fails loud, never executes
	}
	if r.Manifest == nil {
		// A structurally-invalid request is an inadmissible request, not an unwired chain. Error returns
		// are reserved strictly for an unwired chain (fail loud); an inadmissible request is a recorded
		// refusal so it is audited like every other refusal (INV-19), never a silent/observe-only pass.
		i.record("refuse", "", "nil manifest — no sealed action", true)
		return Outcome{Refused: true, Reason: "nil manifest — no sealed action"}, nil
	}
	actionID := r.Manifest.ActionID

	refuse := func(reason string) (Outcome, error) {
		i.record("refuse", actionID, reason, true)
		return Outcome{Refused: true, Reason: reason, ActionID: actionID}, nil
	}

	// spec/020 T-020-7 (REQ-2007/REQ-2001): the OBSERVE-ONLY per-gate verdict trail. emitGate appends ONE ordered
	// row as each gate resolves; refuseGate emits the refusing gate's row THEN refuses — so a refusal leaves the
	// refusing gate's row and NO phantom pass rows for gates past it. This is a PURE SIDE EFFECT: a nil sink is a
	// no-op (the interceptor behaves identically), and an Emit error is swallowed here — it can NEVER change a
	// gate outcome or let a refused action through, and nothing downstream reads the trail to make a decision.
	gateOrd := 0
	emitGate := func(gate, verdict, reason string) {
		gateOrd++
		if i.gateSink == nil {
			return
		}
		_ = i.gateSink.Emit(ctx, trace.GateVerdict{
			Ordinal: gateOrd, Gate: gate, Verdict: verdict, Reason: reason,
			ActionID: actionID, ExternalRef: r.ExternalRef,
		})
	}
	// emitGateMargin is emitGate for a gate that decided against a NUMERIC threshold: it records the signed
	// distance from that threshold (TG-178), so a decision within ε of its boundary is a reviewable case. It
	// advances the SAME ordinal counter as emitGate (each gate emits exactly one row), and is identically
	// OBSERVE-ONLY — the margin is a pure side-effect field, never read back, and a nil sink is a no-op.
	emitGateMargin := func(gate, verdict, reason string, margin float64) {
		gateOrd++
		if i.gateSink == nil {
			return
		}
		m := margin
		_ = i.gateSink.Emit(ctx, trace.GateVerdict{
			Ordinal: gateOrd, Gate: gate, Verdict: verdict, Reason: reason,
			ActionID: actionID, ExternalRef: r.ExternalRef, Margin: &m,
		})
	}
	refuseGate := func(gate, reason string) (Outcome, error) {
		emitGate(gate, "refuse", reason)
		return refuse(reason)
	}

	// 1b. Admission: a POLL_PAUSE-band incident may auto-execute ONLY with a recorded human approval — the vote
	//     binds the decision (INV-12). This reads the FRESH per-incident classification band (r.Band), NOT the
	//     sealed manifest's FROZEN first-seal band (r.Manifest.Band, TG-126): the band is a per-incident
	//     classification property, and the content-addressed manifest freezes the band at the FIRST sealing of an
	//     action identity (Seal ON CONFLICT DO NOTHING), so a re-classified later incident of the same action
	//     shape would otherwise be wrongly BLOCKED by a stale frozen POLL_PAUSE (the confirmed bug) — or, in the
	//     mirror case, wrongly ADMITTED under a stale frozen AUTO. An AUTO / AUTO_NOTICE fresh band was already
	//     admitted by the classifier and needs no approval; a POLL_PAUSE fresh band — INCLUDING an absent/zero
	//     band, which is safety.BandPollPause by design (fail closed) — that reached execute without an approval
	//     is a control gap, refused and recorded here. The frozen manifest band never admits nor blocks at 1b.
	if r.Band == safety.BandPollPause && !r.Approved {
		return refuseGate("admission", "poll-band action without a recorded approval")
	}
	emitGate("admission", "pass", "")
	// 2. The mechanical never-auto floor, enforced at the ADAPTER (defense in depth): an irreversible or
	//    floor-class op is refused even with mutation on (INV-09). No flag lifts this — including a model that
	//    UNDER-DECLARES its op_class: the floor also re-derives destructiveness from the ACTUAL command
	//    (safety.IsDestructiveOp over Op+OpClass), so a `kubectl delete pvc` sealed as a benign "restart-service"
	//    reversible=true cannot slip the chokepoint (the admission classifier applies this same override; the
	//    adapter floor must not be weaker). "A plan cannot hide a mutation."
	floorParts := r.Manifest.Action.SafetyParts()
	if safety.IsNeverAuto(r.Manifest.Action.OpClass) || !r.Manifest.Action.Reversible ||
		safety.IsDestructiveOp(floorParts...) {
		return refuseGate("never-auto-floor", "never-auto floor (adapter) — irreversible, floor-class, or server-derived destructive op")
	}
	emitGate("never-auto-floor", "pass", "")
	// 2b. The stateful floor at the ADAPTER (TG-146 A3, the ≥2-deep half). The classify-time floor now
	//     sees the params (temporal/runner actionSafetyParts → the SAME SafetyParts derivation), but a
	//     request that reaches this chain with a NON-VOTED band and a stateful identity in its params
	//     (Target "app01", unit "mariadb.service") is a band that was mis-recorded upstream — and for the
	//     awx/k8s/mcp/proxmox lanes this adapter is the only pre-effect depth (only the ssh leaf has its
	//     own check). A stateful mutation may proceed ONLY through the human-voted band: POLL_PAUSE
	//     passes (the vote happened), AUTO/AUTO_NOTICE refuse. Over-matching is the intended direction.
	if r.Band != safety.BandPollPause && safety.IsStatefulWorkload(floorParts...) {
		return refuseGate("stateful-floor", "stateful floor (adapter) — a stateful-workload mutation under a non-voted band; the band should have been POLL_PAUSE")
	}
	emitGate("stateful-floor", "pass", "")
	// 3. Structure gate: gate on the committed plan and action identity, not command strings. An
	//    ungated action (no committed prediction) or a tampered/substituted action id is refused (INV-06/07).
	if !r.Gated {
		return refuseGate("structure", "ungated — no committed prediction")
	}
	if err := r.Manifest.Assert(actionID); err != nil {
		return refuseGate("structure", "action_id mismatch — authorization is for a different action")
	}
	emitGate("structure", "pass", "")
	// 3b. Structure gate — actuation param schema (the op-class schema registry, core/actuate/opschema, ONE
	//     source of truth): a sealed action for a REGISTERED op-class whose structured params did not build an
	//     argv (an EMPTY argv means a required param such as `unit` is missing) is refused HERE at the structure
	//     gate with the schema's ACTIONABLE guidance (which param is missing) — rather than surfacing at execute
	//     as an opaque ErrEmptyArgv (the canary's original failure mode). It is gated on len(Argv)==0 so it
	//     fires ONLY on the real defect: an action whose argv DID build is governed by that argv (and the effect
	//     leaf's allowlist re-validates it), and an UNREGISTERED op-class is unchanged (its empty argv still
	//     fails closed at execute). The registry's ValidateArgs is EXACTLY as tolerant as the builder sealedArgv
	//     used (validator-tolerance == builder-tolerance), so this never rejects a param form the builder
	//     accepts — a stricter validator would be the ACI-tolerance regression. Fail-closed like every gate.
	if len(r.Argv) == 0 {
		if spec, ok := opschema.Lookup(r.Manifest.Action.OpClass); ok {
			if verr := opschema.ValidateArgs(spec, r.Manifest.Action.Params); verr != nil {
				return refuseGate("structure-schema", "structure — actuation param schema: "+verr.Error())
			}
			emitGate("structure-schema", "pass", "")
		}
	} else if spec, ok := opschema.Lookup(r.Manifest.Action.OpClass); ok && spec.Kind() == opschema.EffectAWXLaunch {
		// TG-152 L1 — the SYMMETRIC half of this gate. An awx-launch effect ALWAYS builds Argv=[LaunchVerb]
		// (len 1), so the len==0 arm above never fires for it: required-param presence was checked only at
		// PROPOSE time, and the params travel OUTSIDE the argv (as AWX extra_vars) where the effect leaf's
		// extravars validation checks that supplied keys are declared+typed but NOT that required keys are
		// PRESENT. Re-run the same registry validator here, at execute, exactly as the ssh-argv classes get —
		// validator-tolerance == builder-tolerance holds identically, so this never rejects a param form the
		// propose path accepted.
		if verr := opschema.ValidateArgs(spec, r.Manifest.Action.Params); verr != nil {
			return refuseGate("structure-schema", "structure — actuation param schema (awx-launch execute-time re-check): "+verr.Error())
		}
		emitGate("structure-schema", "pass", "")
	}
	// 4. Evidence gate: a mutating action must cite a bound orchestrator-captured tool-result (INV-11).
	if !hasBoundEvidence(r.Evidence) {
		return refuseGate("evidence", "evidence unbound — no captured tool-result")
	}
	emitGate("evidence", "pass", "")
	// 4b. Territory gate (the namesake control): a mutating action inside a high-stakes infrastructure
	//     territory (k8s/network/edge/pve/native/docker) may proceed only once that territory's operating
	//     manual has been acknowledged this session — the "grounding" prerequisite. A confirmed infra write the
	//     gate cannot place fails CLOSED. Read-only investigation never reaches here (this is the execute path).
	tg := territory.Gate{Acknowledged: r.Acknowledged}
	if res := tg.Permit(true, safety.IsDestructiveOp(r.Manifest.Action.Op, r.Manifest.Action.OpClass), r.Manifest.Action.Target, r.Manifest.Action.Op, r.Manifest.Action.OpClass); res.Decision == territory.Block {
		return refuseGate("territory", "territory gate — "+res.Reason)
	}
	emitGate("territory", "pass", "")
	// 4c. Verifiability gate: a mutating action may execute ONLY if an observer is WIRED (a non-nil Observe). With
	//     no observer at all, ComputeVerdict would run against a nil observation and return `match` for EVERY
	//     action — the verifier becomes theater. So a request with no observer is refused BEFORE it executes: we
	//     do not execute what we cannot even attempt to verify. A wired observer that later reports ok=false
	//     (the post-state could not be READ at verify time — e.g. a monitoring outage) is NOT refused here (the
	//     action already needs to run to be verifiable); instead it fails CLOSED at step 6 — verdict withheld,
	//     no graduation credit (TG-182). (In a non-actuating mode the chain refuses at step 1 long before here.)
	if r.Observe == nil {
		return refuseGate("verifiability", "unverifiable — no post-execution observer wired (cannot verify ⇒ will not execute)")
	}
	emitGate("verifiability", "pass", "")
	// 4d. Policy authorize (spec/015, REQ-1506): consult the policy engine's per-action verdict. The engine
	//     resolves auto / approve / deny by deny-overrides over the operator rule data (via the AuditedEngine,
	//     so the decision is audited, REQ-1518). The interceptor honors the verdict per its REQ-1506 MEANING —
	//     `approve` is "route to a human vote", NOT a permanent refusal:
	//       • deny    → refuse UNCONDITIONALLY (deny-overrides; no recorded approval lifts a deny).
	//       • approve → proceed ONLY when the required human vote is on file (r.Approved, bound by the RecordVote
	//                   path, INV-12); with NO recorded approval, refuse (fail closed — a second floor beneath the
	//                   admission gate at 1b). This is EXACTLY how an ungraduated op-class earns its clean runs
	//                   toward `auto` (REQ-1514): an unseen class fail-closes to `approve` (graduation.go), the
	//                   operator approves, THIS run executes and accrues one verified-clean run — so the ladder is
	//                   no longer dead-locked by an `approve` that could never be honored.
	//       • auto    → proceed (the class earned autonomy, or a rule granted it under verify-on-auto).
	//     Any other/unknown verdict fails closed (refuse). This is an INDEPENDENT control layer from the
	//     mechanical mode chokepoint below (REQ-1521): it authorizes THIS action; the chokepoint decides whether
	//     the system is in an actuating posture at all — even a proceed here cannot actuate at Shadow. A Rego-eval
	//     error fails closed. The never-auto floor (step 2) already refused any irreversible/destructive op
	//     BEFORE here, so honoring an approval can never let a floor-class mutation through.
	//
	//     A NIL DECIDER IS NO LONGER A PASS-THROUGH WHEN THE SYSTEM CAN ACTUATE (roadmap P2-3). It used to be,
	//     justified as "the mode chokepoint still gates" — true at Shadow, FALSE the moment an operator moves to
	//     Semi-auto, which is the live posture. With no decider in an actuating mode the whole policy layer
	//     vanishes silently: no graduation ladder, no per-op-class authorization, no confidence clamp, leaving
	//     only the chokepoint (which permits) and the never-auto floor.
	//
	//     That state is REACHABLE, not theoretical: cmd/worker/main.go logs "policy engine: build failed …
	//     actuation falls back to the mode chokepoint + never-auto floor only (fail closed)" and CONTINUES
	//     BOOTING. The "fail closed" in that message only holds at Shadow. So a malformed ruleset would have
	//     produced a worker that actuates with its policy layer absent — precisely the fail-OPEN class TG-182
	//     closed for the verifier.
	//
	//     Refusing HERE rather than at SelfTest is deliberate: it is fail-closed exactly where the harm would
	//     occur, and it leaves every read-only / Shadow construction (tests, oracles, the in-memory twins)
	//     working unchanged, because at Shadow the chokepoint refuses before execute anyway.
	if i.decider == nil && i.canActuateNow() {
		return refuse("policy engine not wired while the mode permits actuation — refusing (fail-closed: an " +
			"actuating posture without a policy authorizer has no graduation ladder, per-op-class authorization, " +
			"or confidence clamp)")
	}
	if i.decider != nil {
		mode := policy.ModeShadow
		if i.modeNow != nil {
			mode = i.modeNow()
		}
		dec, derr := i.decider.Decide(ctx, policy.EvalInput{
			OpClass: r.Manifest.Action.OpClass,
			Argv:    strings.Join(r.Argv, " "),
			Host:    r.Manifest.Action.Target,
			// The target's object-group membership, from the SAME shared store the credential resolver reads
			// (TG-481, REQ-1618): this is what lets a group-scoped policy rule govern live actuation with no
			// second definition. Empty when the resolver is unwired or the host is in no group — decision
			// byte-identical to pre-TG-481, and no default rule scopes by group.
			Groups:     i.resolveObjectGroups(r.Manifest.Action.Target),
			Reversible: r.Manifest.Action.Reversible,
			// The declared-inverse bit (spec/029 T-029-3): lets a rule grant the COMPENSATING
			// direction autonomy without widening the model-proposed path of the same op-class
			// (the stop-guest inverse_only rule). Structural, from the request — never a model input.
			InvertsForward: r.InvertsActionID != "",
			Confidence:     r.Confidence,
			// The FRESH per-incident band (r.Band), NOT the sealed manifest's frozen first-seal band (TG-126):
			// the policy engine composes the safety band with its verdict (spec/015 ComposeBand), so a stale
			// frozen POLL_PAUSE would floor an otherwise-`auto` graduated class to `approve` and RE-BLOCK a
			// fresh-AUTO incident here even after 1b admits it. Authorizing on the CURRENT classification (exactly
			// like 1b) lets a de-noveled + graduated incident self-heal hands-off, while a fresh POLL_PAUSE still
			// composes to `approve` (needs a human). Zero band ⇒ BandPollPause ⇒ compose `approve` ⇒ fail closed.
			Band: r.Band,
			Mode: mode,
			// spec/020 T-020-3 (REQ-2005): thread the NON-SECRET correlation/attribution keys so the audited
			// policy_decision joins the decision-tracer walk by BOTH action_id AND external_ref instead of the
			// empty columns migration 0019 left. These NEVER feed the verdict — Decide composes identically with
			// them empty; they only ride into the audit projection. ActionID is the sealed manifest's
			// content-hashed id (INV-07), ExternalRef the incident it answers, Principal the autonomous runner.
			ActionID:    r.Manifest.ActionID,
			ExternalRef: r.ExternalRef,
			Principal:   "runner:" + r.ExternalRef,
		})
		if derr != nil {
			return refuseGate("policy", "policy engine error — fail closed: "+derr.Error())
		}
		switch dec.Verdict() {
		case policy.VerdictAuto:
			// Authorized to auto-execute — proceed (still floored by the mode chokepoint below).
		case policy.VerdictApprove:
			if !r.Approved {
				return refuseGate("policy", "policy verdict approve — needs a human approval, none recorded (no auto-execute)")
			}
			// The `approve` verdict's required human vote is on file (INV-12): proceed. Record that a recorded
			// human approval — NOT an `auto` grant — is what authorized this action, so the ledger shows exactly
			// why an `approve`-verdict action was permitted to execute (audit clarity, INV-19).
			i.record("policy-approve-honored", actionID, "policy verdict approve authorized by a recorded human approval (INV-12)", false)
		case policy.VerdictDeny:
			return refuseGate("policy", "policy verdict deny — refused (deny-overrides; approval cannot lift a deny)")
		default:
			return refuseGate("policy", "policy verdict "+string(dec.Verdict())+" (unknown) — fail closed")
		}
		// TG-178: when the confidence gate is active (min_confidence > 0), record how far the bound confidence
		// was from it — Confidence - min_confidence, already on the non-secret Refine record. A small |margin|
		// means the action auto-authorized within ε of the auto→approve clamp: a boundary case worth review.
		// A gate with min_confidence unset (0) has no numeric threshold, so no margin. Purely observe-only.
		if ref := dec.Audit().Refine; ref.MinConfidence > 0 {
			emitGateMargin("policy", "pass", string(dec.Verdict()), ref.Confidence-ref.MinConfidence)
		} else {
			emitGate("policy", "pass", string(dec.Verdict()))
		}
		// TG-178: the band-composition margin — how far the policy verdict sat from the band's verdict floor,
		// in VERDICT RANKS (0 = the policy verdict landed exactly on the floor, the boundary case; ±1/±2 =
		// a full rank of headroom / a force override). A distinct policy sub-gate from the min_confidence clamp
		// above; observe-only, emitted on the same nil-tolerant sink and never read back into a gate outcome.
		cmp := dec.Audit().Compose
		emitGateMargin("policy-band", "pass", string(cmp.Composed), float64(cmp.BandMarginRank))
		// TG-178: the graduation margin — how many verified-clean runs the op-class was from its NEXT rung.
		// A climbing class sits at count−threshold ≤ −1 (−1 = one clean run short, the boundary case the
		// ticket names); a class already graduated, or not auto-eligible, has no rung to be short of and
		// records NO margin — a plain gate row, so a nil margin is never mistaken for an at-threshold 0.
		// A distinct policy sub-gate from the min_confidence clamp and the band; observe-only, emitted on the
		// same nil-tolerant sink AFTER the verdict resolved, and never read back into any gate outcome.
		if g := dec.Audit().Graduation; g.Present {
			emitGateMargin("graduation", "pass", "", float64(g.Margin))
		} else {
			emitGate("graduation", "pass", "")
		}
	}
	// 4d2. AuthN compose (spec/016 REQ-1604, T-016-5, TG-98): the target IDENTITY resolves as its own
	//      control layer — AFTER the policy verdict above, BEFORE anything executes — so authentication
	//      COMPOSES with authorization instead of hiding inside the effect leaf. When wired it fails
	//      closed: a target with no declared identity refuses HERE, at a named gate, with the resolution
	//      itself audited by the composer (credential.AuditedResolver → credential_resolution). A nil
	//      composer is today's deployment — the leaves authenticate with the static actuation identity —
	//      and the pass row says exactly that rather than implying a control that is not armed.
	if i.composer == nil {
		emitGate("authn-compose", "pass", "no composer wired — identity remains the effect leaf's static configuration")
	} else if ruleID, cerr := i.composer(ctx, r.Manifest.Action.Target); cerr != nil {
		return refuseGate("authn-compose", "authn compose — no identity resolves for this target (fail closed): "+cerr.Error())
	} else {
		emitGate("authn-compose", "pass", "identity rule "+ruleID)
	}
	// 4e. Cross-process kill (REQ-1210, design-wisdom #3 — the multi-worker canary prerequisite): honor the
	//     SHARED durable breaker BEFORE the mode chokepoint. A deviation or chain-integrity trip in ANY worker
	//     opened the breaker in the cross-process store; every worker reads that OPEN state HERE and
	//     force-Shadows its OWN mode before it actuates — so one worker's trip force-Shadows every sibling (the
	//     shared kill the multi-worker canary depends on, which a per-process breaker never delivered). It FAILS
	//     CLOSED: an unreadable breaker reads OPEN (Tripped) and refuses. It is a no-op while the shared breaker
	//     is closed and inert under Shadow (the mode chokepoint below refuses regardless), so it changes nothing
	//     today — it arms the guarantee for a later, operator-escalated canary. A nil breaker (unarmed) is a
	//     documented pass-through: the mode chokepoint still gates.
	if i.breaker != nil && i.breaker.Tripped(ctx) {
		// The reason says which is KNOWABLE here, nothing more. The old text asserted "a sibling worker
		// tripped" unconditionally — a hardcoded literal that actively misled the 2026-07-28 incident review
		// into hunting a second worker. The shared row does not say whose trip this is; the safety:breaker-trip
		// ledger entry does.
		i.chokepoint.ForceShadow("mutation breaker OPEN — cross-process kill (tripped by this or a sibling worker; see the safety:breaker-trip ledger entry)")
		return refuseGate("breaker", "mutation breaker OPEN — system-wide kill (tripped by this or a sibling worker; see the safety:breaker-trip ledger entry)")
	}
	if i.breaker != nil {
		emitGate("breaker", "pass", "")
	}
	// 4f. Mode chokepoint (the absorbed MutationGate, REQ-1520): the SOLE mechanical authority for "may this
	//     action actuate?" — `mode ∈ {Semi-auto, Full-auto} && preflight green`. In Shadow / HITL (the default),
	//     an un-bound mode, or a red preflight, this refuses EVERY mutation (the read-only floor the disabled
	//     gate held). It is the SOLE actuation authority and an INDEPENDENT floor beneath the policy verdict:
	//     even a policy `auto` cannot execute while the mode is not actuating — the negative control that no
	//     code path actuates at Shadow. (The host-match gate 4g below may still refuse a target mismatch after
	//     this passes; nothing weakens this floor.)
	if err := i.chokepoint.GuardMutation(); err != nil {
		return refuseGate("mode-chokepoint", "mutation disabled (read-only)")
	}
	emitGate("mode-chokepoint", "pass", "")
	// 4g. Host-match — a single-host-bound effect leaf (the SSH mutating leaf) runs the argv on its CONFIGURED
	//     host and never reads the action's target, so an action admitted for a DIFFERENT host would mis-execute
	//     on the configured host. Refuse on any target≠bound-host mismatch (fail-closed: an exact-string
	//     mismatch blocks the heal, never mis-routes it). A leaf that is not HostBound, or reports an empty
	//     host, is unaffected (the Proxmox/k8s/local leaves route by their own target/resource-id). This makes
	//     arming the single-host canary safe; per-target routing (fleet restart-service) is the follow-on.
	if hb, ok := i.actuator.(HostBound); ok {
		if ah := strings.TrimSpace(hb.ActuationHost()); ah != "" && ah != strings.TrimSpace(r.Manifest.Action.Target) {
			return refuseGate("host-match", fmt.Sprintf("effect leaf is bound to host %q but the action targets %q — refusing to mis-actuate on the wrong host", ah, strings.TrimSpace(r.Manifest.Action.Target)))
		}
		emitGate("host-match", "pass", "")
	}
	// 4h. ACTUATION-FREQUENCY GATE (TG-166a) — the per-session and per-target rate + in-flight-concurrency
	//     governor. Every gate ABOVE this line is per-ACTION: each of them asks "is THIS restart admissible?"
	//     and each of them can answer yes a hundred times in a row, because none of them can see a SEQUENCE.
	//     That is the hole this ticket names: a subverted agent emits an in-grammar, allowlisted, reversible,
	//     evidence-bound, target-relevant proposal to restart a unit, and repeats it. Nothing counted. The
	//     op-class rate_limit in core/policy is not this control and is not even wired (see limiter.go).
	//
	//     It sits HERE, after the last per-action gate and before the baseline reads, deliberately: it is the
	//     last decision that can be made without touching the estate, and the in-flight lease must cover the
	//     WHOLE pre-effect sequence — the baseline monitoring reads (which sleep and retry), the necessity
	//     probe, and Exec itself — because "two actuations in flight against one host" includes the seconds
	//     one of them spends reading. Consequently the budget is charged to any attempt that gets this far,
	//     including one later refused at the baseline or necessity gate. That is intended: an attempt that
	//     reaches this point has committed the estate to work on its behalf, and refunding failures would hand
	//     a subverted agent unlimited retries in exchange for making each one fail.
	//
	//     FAIL CLOSED: a nil limiter refuses (unreachable — NewInterceptor installs one, WithActuationLimiter
	//     ignores nil), an absent session/target key shares one bucket rather than exempting, and there is no
	//     way to spell an unlimited scope. VISIBLE: the refusal reason starts with RefusalRateLimited and
	//     carries the scope, key, count and cap, and Outcome.RateLimited marks it machine-readably — a
	//     throttled actuation must never read like "the effect leaf exited 42".
	lease, limitRefusal := i.limiter.Admit(r.ExternalRef, r.Manifest.Action.Target)
	if limitRefusal != "" {
		emitGate("actuation-limit", "refuse", limitRefusal)
		i.record("refuse", actionID, limitRefusal, true)
		return Outcome{Refused: true, RateLimited: true, Reason: limitRefusal, ActionID: actionID}, nil
	}
	// Release the in-flight slots on EVERY exit path below (refusal, execute failure, verdict, panic-free
	// return). A leaked slot is fail-closed — it refuses later actuations against that session/target — but it
	// is still a bug, so the release is bound to the function, not to a branch.
	defer lease.Release()
	// TG-178: the actuation-limit gate decided against a NUMERIC threshold (the trailing-window rate budget),
	// so it emits a signed margin like the policy and band gates — the rate-budget slack remaining after this
	// admission (0 = this was the last actuation before the frequency throttle, a reviewable boundary case).
	// Identically observe-only: a pure side-effect field, never read back to gate anything.
	emitGateMargin("actuation-limit", "pass", "", float64(lease.headroom))
	// 4h2. DURABLE TARGET ADMISSION (TG-81 b2) — the cross-process half of 4h. The lease above governs
	//      THIS process; the durable claim governs the ESTATE: one actuation in flight per target across
	//      every actuation-capable process, plus a cooldown after a disturbed effect (a failed or killed
	//      mutation leaves the target in an unknown state — the next hand must wait out the dust, not pile
	//      on). It sits directly after the in-process lease so both are held for the whole pre-effect
	//      sequence, and it FAILS CLOSED once armed: a held claim, an active cooldown and an unreachable
	//      store all refuse with the store's own reason (the h-ssh active-set posture, inverted — theirs
	//      admitted when its store was unreachable). A nil store is the documented unarmed pass-through,
	//      the mutation-breaker convention. execDisturbed is set ONLY by the effect-leaf failure paths
	//      below, so a refusal between here and the effect releases the claim with no cooldown.
	execDisturbed := false
	if i.admission != nil {
		if aerr := i.admission.Admit(ctx, r.Manifest.Action.Target, r.ExternalRef); aerr != nil {
			return refuseGate("target-admission", "durable target admission refused: "+aerr.Error())
		}
		defer func() { i.admission.Release(ctx, r.Manifest.Action.Target, r.ExternalRef, execDisturbed) }()
		emitGate("target-admission", "pass", "")
	}
	// 4h3. PRE-FLIGHT REACHABILITY (TG-81 b4) — when the effect leaf can prove its transport, prove it
	//      NOW, inside both leases and before the first estate-touching step (the baseline reads sleep and
	//      retry against monitoring; the probe is the cheaper, earlier abort). An unreachable target
	//      refuses cleanly — "aborted before touching anything" — instead of surfacing minutes later as
	//      an effect-leaf exit nobody can tell apart from a refused mutation. STRICT per-target: a
	//      probing leaf refuses an empty target outright (nothing to probe is not a pass). Deliberately
	//      AFTER the 4h budget charge — a free pre-lease refusal would hand a subverted agent unlimited
	//      retries against a down host — and its refusal releases the durable claim undisturbed. A leaf
	//      without the capability is a documented pass-through: the exec failure downstream still fails
	//      closed, this gate only moves the refusal earlier and names it.
	if rp, ok := i.actuator.(ReachabilityProber); ok {
		target := strings.TrimSpace(r.Manifest.Action.Target)
		if target == "" {
			return refuseGate("reachability", "a mutating action must name a probeable target — refusing the empty target rather than probing nothing")
		}
		reachable, detail := rp.ProbeReachable(ctx, target)
		if !reachable {
			return refuseGate("reachability", "pre-flight reachability probe failed for "+target+": "+detail+" — aborted before any estate-touching step")
		}
		// The pass DETAIL is carried: an unarmed leaf answers true with "transport not proven", and the
		// gate trail must say that rather than record a probe that never dialed.
		emitGate("reachability", "pass", detail)
	}
	// BASELINE GATE (TG-148 hardened after the 2026-07-28 false deviation, ledger 5153-5155): capture the
	// pre-action baselines NOW, immediately BEFORE the effect fires, and REFUSE to execute if none can be
	// established. The post-execution Observe (step 6) is estate-WIDE, so the baseline is the verifier's ONLY
	// temporal discrimination between "appeared because of my action" and "was already wrong somewhere" —
	// ObservedAlert carries no timestamp, and without a baseline the deviation test silently changes subject
	// from this action's cascade to everything wrong anywhere in the estate.
	//
	// The previous line here was `preObserved, _ := r.Observe(ctx)`, with a comment claiming a discarded read
	// error "only WIDENS what counts as a surprise (fail-safe)". That reasoning was INVERTED, and it fired:
	// in the one second where the pair baseline was unestablished and the post-read succeeded, a stale
	// uncleared alert on an unrelated host (harness-stopped 19:47, restarted 20:07, never re-polled) read as
	// the cascade of a start-guest — verdict deviation, breaker tripped estate-wide, op-class demoted,
	// actuation halted 1h49m on a manufactured verdict. spec/013's own licence for the instant demote+trip
	// ("the candidate set for a deviation is close to empty by construction... a deviation observed this fast
	// is real") is CONDITIONAL on the baseline it had just lost.
	//
	// Two independent arms, each with one bounded retry:
	//   - pair arm: r.Observe — the live monitoring snapshot, precise to (host,rule) but sharing the
	//     post-read's failure mode. ok is RESPECTED now, mirroring the post-read at step 6.
	//   - host arm: r.PreAnomalous — hosts holding an OPEN incident in the durable ingest ledger, coarse but
	//     failure-independent of the monitoring surface, anchored pre-execution by construction.
	// Either arm established ⇒ proceed (recorded; the established arm still separates pre-existing from
	// caused). BOTH unestablished ⇒ refuse: executing would recreate exactly the verdict-without-a-baseline
	// this gate exists to forbid, and refusal is the only fail-closed option that still exists pre-execute.
	// The same discipline as verifiability gate 4c — we do not execute what we cannot adjudicate.
	//
	// BACKSTOP FOR GATE 4c (TG-234). Every Observe call from here on assumes 4c already refused a nil
	// observer — which made the chain fail closed by ACCIDENT: with 4c deleted, the next line was a
	// nil-pointer panic, not a refusal. A panic produces no ledger entry, no verdict, no operator-legible
	// reason, and its behaviour under a partially-initialised chain is unspecified; it also made 4c's RED
	// mutation control non-discriminating (the control crashed instead of failing the assertion, proving
	// the gate load-bearing without proving WHAT it bears). This guard turns that crash path into a
	// DESIGNED refusal with its own distinct reason, so the floor degrades deliberately and 4c's real
	// contribution — refusing BEFORE any gate below it runs — is isolatable by mutation.
	// RED CONTROL EXECUTED (2026-08-03): gate 4c deleted → this backstop refused with
	// "verifiability-backstop" and NO panic; restored → refusal comes from 4c as "verifiability". The two
	// distinct reasons are what make the control discriminating.
	if r.Observe == nil {
		return refuseGate("verifiability-backstop",
			"unverifiable — nil observer reached the baseline step (gate 4c should have refused upstream; "+
				"fail closed by design, not by crash)")
	}
	preObserved, preOK := r.Observe(ctx)
	if !preOK {
		time.Sleep(baselineRetryDelay)
		preObserved, preOK = r.Observe(ctx)
	}
	var preAnomalous map[string]bool
	anomWired, anomOK := r.PreAnomalous != nil, false
	if anomWired {
		if preAnomalous, anomOK = r.PreAnomalous(ctx); !anomOK {
			time.Sleep(baselineRetryDelay)
			preAnomalous, anomOK = r.PreAnomalous(ctx)
		}
	}
	if !preOK && !anomOK {
		return refuseGate("baseline", fmt.Sprintf(
			"pre-action baseline unestablished after retry (pair-arm ok=%t, host-arm wired=%t ok=%t) — a verdict computed "+
				"without any baseline cannot separate this action's cascade from pre-existing faults, and a deviation it "+
				"manufactures trips the estate-wide breaker; refusing to execute what cannot be adjudicated", preOK, anomWired, anomOK))
	}
	if !anomOK {
		preAnomalous = nil // an unestablished host arm must never read as an EMPTY one (empty asserts "no host was anomalous")
	}
	if !preOK {
		preObserved = nil // same property on the pair arm — a failed read contributes nothing, not an empty snapshot
	}
	emitGate("baseline", "pass", fmt.Sprintf("pair-arm ok=%t n=%d; host-arm wired=%t ok=%t n=%d", preOK, len(preObserved), anomWired, anomOK, len(preAnomalous)))
	// 4i. NECESSITY GATE (TG-166b) — the last question the chain never asked. Everything above proves the
	//     action is SAFE: reversible, allowlisted, evidence-bound, predicted, policy-authorized, in-territory,
	//     on the right host, with a baseline. Nothing above asks whether it is still NEEDED. The evidence that
	//     justified this mutation was captured during the investigation — minutes and a model round-trip ago —
	//     and the chain would fire the effect on it regardless of what happened since. Between propose and here
	//     the unit can be restarted by a human, by config management, or by systemd's own Restart= policy, and
	//     the alert can clear. "Was true when we looked" was the only thing the gate ever checked.
	//
	//     This re-runs the justifying observation at the last pre-effect instant and acts on ONE direction of
	//     the answer. It can FALSIFY necessity, never establish it:
	//       • present=false ⇒ REFUSE. The justification is gone; mutating now is an unnecessary mutation, which
	//         is a real cost (a restart drops connections, resets counters, and earns graduation credit for a
	//         non-event) and the exact behaviour a subverted agent wants to repeat.
	//       • present=true  ⇒ "not refuted", nothing more. It licenses nothing; every gate above already did
	//         the licensing.
	//       • ok=false      ⇒ REFUSE. We could not check, so we do not mutate — the same discipline as the
	//         verifiability gate (4c) and the baseline gate above. A monitoring outage must not silently become
	//         an unconditional pass, which is what an `ok` this code ignored would be.
	//       • nil seam      ⇒ REFUSE. An unwired necessity check is the classic optional control: present in
	//         the design, absent in the deployment, invisible in both. Refusing here is the same choice gate 4c
	//         made for a nil observer, and it is fail-closed exactly where the harm would occur.
	//
	//     WHAT IT DOES NOT PROVE — the honest limits, so nobody reads this gate as more than it is. It proves
	//     the fault was visible at T_recheck, not at T_execute; the race between the read and the effect is
	//     irreducible and moving this line closer to Exec does not close it. Its sharpness is the caller's
	//     probe: TG's production wiring re-runs the SAME live active-alert read the clear-check trusts to
	//     auto-close an incident, and asks whether the target host is still alerting AT ALL — host-quiet, not
	//     the exact (host, rule), for the reason ObserveClearedActivity gives (a host carrying a DIFFERENT
	//     alert must not read as clear). So a quiet host can still have this specific unit down, and an
	//     alerting host may be alerting about something this action does not fix. It is a pre-condition, not an
	//     outcome, and it grants no graduation credit.
	if r.StillFaulted == nil {
		return refuseGate("necessity",
			"no execute-time fault re-check wired — refusing: the chain can prove this action SAFE but not that "+
				"it is still NECESSARY, and an unchecked necessity is an unwired control, not a passed one")
	}
	stillFaulted, necessityOK := r.StillFaulted(ctx)
	if !necessityOK {
		return refuseGate("necessity",
			"the fault could not be re-observed at execute time (read error) — refusing to mutate what we cannot "+
				"show is still needed; an unreadable probe is not a clear and is not a licence")
	}
	if !stillFaulted {
		return refuseGate("necessity", fmt.Sprintf(
			"NO LONGER NECESSARY — the fault this action answers is not visible on %q at the last pre-effect "+
				"instant (it cleared between the investigation that justified this mutation and now). Executing "+
				"would mutate a healthy target and credit the op-class for a non-event; the incident resolves "+
				"through the normal clear-check instead", strings.TrimSpace(r.Manifest.Action.Target)))
	}
	emitGate("necessity", "pass", "fault still visible at the last pre-effect instant (not refuted — this gate can only falsify necessity)")
	// 5. Execute — the single chokepoint. argv-only, no shell (INV-02).
	//
	//    TWO failure channels, both fail-closed. A Go error is a TRANSPORT failure (handshake, auth, deadline).
	//    A non-zero ExitCode is the REMOTE's own refusal or failure, which every mutating effect leaf returns
	//    deliberately as a RESULT rather than an error — the SSH leaf for a non-zero remote exit
	//    (ssh/native.go: "a RESULT the caller interprets, not an error"), the Proxmox leaf for a non-OK task
	//    exitstatus (proxmox/proxmox.go: "a failed mutation, not a Go error"). This IS that interpretation, and
	//    until now it did not exist: the Result was discarded with `_`, so NOTHING in the actuation path ever
	//    read ExitCode and a refused command was recorded `execute: pass`.
	//
	//    Why that is a fail-OPEN and not merely a logging gap: verify (step 6) scores the POST-STATE, not the
	//    effect. When the goal state already holds for some OTHER reason — the unit was never down, the alert
	//    was stale, an operator or another actor fixed it first — a refused mutation yields verdict `match`,
	//    which maps to OutcomeVerifiedClean and CREDITS THE OP-CLASS TOWARD AUTO. An op-class could therefore
	//    climb the graduation ladder to autonomous on actions its targets REFUSED. Graduation is the earn path
	//    (spec/015 REQ-1514); earning it from a non-event inverts it.
	//
	//    PROVEN LIVE, not hypothetical. On 2026-07-26T00:01:40Z the librespeed01 host guard denied
	//    `'systemctl' 'start' 'nginx.service'` with exit 42 (journal, tg-actuator-guard: "DENY"), and in the
	//    SAME SECOND TG appended `actuate:execute:match` to the governance ledger and advanced start-service to
	//    clean_run_count=1. Three records, one second apart, disagreeing about whether anything happened.
	// TG-58 PRE-MUTATION STATE CAPTURE — the LAST pre-effect instant, after every safety gate has passed
	// (baseline 4c, necessity 4i) and immediately before the effect fires: snapshot the target's rollback-relevant
	// state so a Phase-2 applied-undo has a concrete state to restore TO. DORMANT: a nil CaptureState hook captures
	// nothing and this block is byte-identical to before. NON-FATAL by design — pre-state capture is rollback PREP,
	// not a safety gate: a failed capture is audited and the action PROCEEDS (a missing capture makes the mutation
	// un-restorable, a Phase-2 arming concern, never unsafe). One bounded retry, mirroring the baseline arms.
	var preState PreState
	preStateOK := false
	if r.CaptureState != nil {
		if preState, preStateOK = r.CaptureState(ctx); !preStateOK {
			time.Sleep(baselineRetryDelay)
			preState, preStateOK = r.CaptureState(ctx)
		}
		if !preStateOK {
			i.record("pre-state-capture-gap", actionID, "pre-mutation state could not be captured before the effect — the action proceeds (rollback prep, not a safety gate); a Phase-2 applied-undo would lack a restore point", false)
		}
	}
	res, err := i.actuator.Exec(ctx, r.Argv, r.Stdin)
	if err != nil {
		// The effect FIRED and did not complete — whatever the class, the target's state is now unknown,
		// so the durable admission releases with the cooldown (TG-81 b2).
		execDisturbed = true
		// TG-80 P1-4: a cancellation that landed mid-effect is its own terminal. The SSH leaf wraps the
		// context error after signalling the remote command dead (TERM → KILL before the transport closes),
		// so errors.Is on the context sentinels classifies it without this chain importing the leaf. It is
		// still a refusal (nothing completed), but a CANCELLED one — the record must not read as "execute
		// failed" beside a bad host key, and the estate is not left with an orphaned process.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			out, rerr := refuseGate("execute", RefusalCancelled+": "+err.Error())
			out.Cancelled = true
			return out, rerr
		}
		return refuseGate("execute", "execute failed: "+err.Error())
	}
	if res.ExitCode != 0 {
		execDisturbed = true
		return refuseGate("execute", fmt.Sprintf(
			"effect leaf exited %d — the target refused or the mutation failed; no verdict is computed and no graduation credit is granted",
			res.ExitCode))
	}
	// 5c. NO-OP — the leaf changed NOTHING because the target was ALREADY in the requested state (REQ-1221).
	//     This is neither a failure nor a heal, and it is the one case the exit code cannot express: a real
	//     mutation and a no-op both exit 0. Treating it as a heal is a fail-OPEN of exactly the shape REQ-1220
	//     closes — the verifier scores the POST-STATE, the target is by definition already in goal state, so
	//     the verdict is `match` BY CONSTRUCTION, and `match` is the only promoting graduation outcome. An
	//     op-class would climb toward AUTO on mutations it never performed.
	//
	//     So the chain stops here: no manifest stage advances, no verdict is computed or persisted (INV-10),
	//     no execution row is written, and the ladder is not fed. `Executed` is FALSE because it is the honest
	//     answer — the runner maps it onto session_triage.mutated, and TG mutated nothing. The reconciler keys
	//     on the same flag, so a no-op correctly leaves nothing to reconcile. The incident stays open and
	//     resolves through the normal clear-check, which is right: whatever put the target in goal state was
	//     not this action, and credit belongs to whatever did.
	if res.NoOp {
		emitGate("execute", "no-op", "target already in the requested state")
		i.record("actuate:no-op", actionID, "effect leaf changed nothing — the target was already in the requested state; no verdict, no execution row, no graduation credit", false)
		return Outcome{Executed: false, ActionID: actionID, Reason: "no-op: the target was already in the requested state, so this action mutated nothing"}, nil
	}
	emitGate("execute", "pass", "")
	// TG-58: persist the pre-mutation snapshot for this EXECUTED mutation (the no-op and refused paths returned
	// above, so only a real mutation reaches here), bound to the same action_id the execution record uses, so a
	// future applied-undo executor can restore to it. NON-FATAL: a store failure is a control gap on an
	// already-executed action (it cannot be un-run), audited exactly like the execution-record path, never a crash.
	if i.preStates != nil && preStateOK {
		if perr := i.preStates.RecordPreState(ctx, actionID, preState); perr != nil {
			i.record("pre-state-record-failed", actionID, "executed but the pre-mutation state was not persisted: "+perr.Error(), false)
		}
	}
	// 5a. Record the execution_log (INV-07): an effect leaf that can derive a compensating inverse records ONE
	//     execution_log bound to THIS action id, so the mutation is attributable and undoable. Do owns the
	//     durable write — appending it to the tamper-evident ledger; the effect leaf only derives the forward +
	//     inverse. A read-only reference actuator implements nothing here (there is nothing to record). A
	//     derivation error on an already-executed action is a control gap (the execution stands — it cannot be
	//     un-run), recorded so the caller reconciles, never swallowed.
	if rec, ok := i.actuator.(ExecRecorder); ok {
		if fwd, rb, lerr := rec.ExecLog(actionID, r.Argv); lerr != nil {
			i.record("exec-log-failed", actionID, "executed but execution_log not derived: "+lerr.Error(), false)
		} else if len(fwd) > 0 {
			i.record("exec-log", actionID, "execution_log bound to action_id — forward["+strings.Join(fwd, " ")+"] rollback["+strings.Join(rb, " ")+"]", false)
		}
	}
	// 5b. Record the EXECUTED stage on the immutable manifest lifecycle chain (INV-07) — the action ran, so
	//     the chain advances. A record failure is a chain-integrity gap on an executed action (surfaced below).
	execRecErr := r.Manifest.Record(manifest.StageExecuted, r.Argv)
	// 6. Verify — the deterministic verifier writes the only verdict; the acting model has no write path
	//    (INV-10). A deviation is recorded and (by the reconciler) never auto-resolves. The verifiability gate
	//    (4c) guarantees Observe is non-nil here, so the verdict is computed against a REAL observation of the
	//    post-execution estate, never nil — a deviation is always computable.
	observed, observedOK := r.Observe(ctx)
	// ComputeVerdictDetailScoped is the sole author (ComputeVerdict is its enum projection). We take the
	// DETAIL (surprise hosts = the deviation triggers, rule mismatches = the partial triggers) so a DEVIATION's
	// exact cause is durably recorded in the ledger below (TG-148: action_verdict stores only the enum, leaving a
	// false-deviation — e.g. a pre-existing UNRELATED estate alert misread as a cascade surprise, since Observe is
	// estate-wide — undiagnosable post-hoc). The verdict is computed against BOTH baseline arms captured at the
	// baseline gate above AND the estate-derived site scope (REQ-107): a surprise candidate provably on the OTHER
	// site is coincidental background, while an unknown-site host still deviates (fail closed) — the mechanic
	// whose absence let a 59-second other-site sensor flap demote an op-class (ledger seq 6555).
	detail := verify.ComputeVerdictDetailScoped(r.Prediction, observed, preObserved, preAnomalous, r.HostSite)
	verdict := detail.Verdict
	// FAIL-CLOSED verifiability (TG-182): a non-nil observer can still report ok=false when the post-state could
	// NOT be READ (a monitoring outage). An empty observation computes `match` (verdict.go), which would launder
	// an UNVERIFIED mutation as verified-clean and advance graduation on zero evidence — the exact fail-OPEN the
	// clear-check already guards (ClearObserve returns ok=false on the same read error). So when unobserved we
	// WITHHOLD the durable verdict, label the gate `unverifiable`, and grant NO graduation credit. `verified`
	// gates every honesty surface below; when it is true the path is byte-identical to before.
	verified := observedOK
	verifyLabel := string(verdict)
	verifyReason := ""
	if !verified {
		verifyLabel, verifyReason = "unverifiable", "post-state unobservable (monitoring read error) — verdict withheld, no graduation credit"
	}
	// spec/020 T-020-7: the verify gate's mechanical verdict is the final ordered row of the trail
	// (match/partial/deviation, or `unverifiable` when the post-state could not be read) — observe-only.
	emitGate("verify", verifyLabel, verifyReason)
	// 6b. Record the VERIFIED stage, then verify the WHOLE chain binds this one action_id in lifecycle order
	//     (INV-07 — the "one immutable typed chain from evidence to verdict"). A broken chain on an action that
	//     already executed is surfaced (it cannot be un-executed) so the caller reconciles.
	verRecErr := r.Manifest.Record(manifest.StageVerified, verdict)
	if execRecErr != nil || verRecErr != nil || r.Manifest.VerifyChain() != nil {
		i.record("chain-integrity-gap:"+string(verdict), actionID, "executed but the manifest lifecycle chain did not record/verify", false)
		// A chain-integrity gap on an executed action is a trip-worthy safety event: arm the breaker so a
		// repeat (or, at threshold 1, this one) halts mutation in-process (§4.B.3).
		i.tripBreaker(ctx, "chain-integrity gap on "+actionID)
		// The per-run execution record STILL gets its best-effort write on this early return
		// (spec/029 T-029-3 review finding #1): before this line, a chain-gap execution left NO
		// durable executed-trace at all — StageExecuted unrecorded AND the 7b row skipped by this
		// return — so the commit-confirm consult read the run as never-executed and resolved its
		// armed window `aborted` over a live (breaker-tripping!) mutation. An execution that left
		// no trace is indistinguishable from one that never happened — the worse failure, here on
		// the exact path where the revert matters most.
		if i.executions != nil {
			if rerr := i.executions.Record(ctx, actionID, r.ExternalRef, r.Prediction.TargetHost, r.Prediction.Site, verdict, verified, r.InvertsActionID); rerr != nil {
				i.record("execution-record-failed:"+verifyLabel, actionID, "chain-gap execution also lost its per-run record: "+rerr.Error(), false)
			}
		}
		return Outcome{Executed: true, Verdict: verdict, ActionID: actionID, Reason: "manifest lifecycle chain gap"}, nil
	}
	// 7. Persist the mechanical verdict durably (INV-10) — the deterministic verifier is the only writer, so
	//    the verdict store records it exactly once per action_id. A persist failure is a control gap on an
	//    action that ALREADY executed: it is recorded as a refusal-shaped audit entry but the execution stands
	//    (we cannot un-execute), so the caller learns the verdict was not durably written and can reconcile.
	if !verified {
		// Unobservable post-state (TG-182): WITHHOLD the durable verdict rather than persist a false `match` — a
		// verdict we could not actually observe must not enter the scorecard/tracer as a clean result. Recorded
		// as a control gap, not a failure; the executed action stands (it cannot be un-run).
		i.record("verdict-withheld:unverifiable", actionID, "executed but the post-state could not be observed (monitoring read error) — verdict WITHHELD (fail-closed, not persisted as a false match)", false)
	} else if i.verdicts != nil {
		if err := i.verdicts.Commit(ctx, actionID, r.Prediction.PlanHash, r.Prediction.TargetHost, r.Prediction.Site, verdict); err != nil {
			i.record("verdict-persist-failed:"+string(verdict), actionID, "executed but verdict not durably written: "+err.Error(), false)
			if verdict == safety.VerdictDeviation {
				i.tripBreaker(ctx, "deviation verdict (verdict persist failed) on "+actionID)
			}
			return Outcome{Executed: true, Verdict: verdict, ActionID: actionID, Reason: "verdict not persisted"}, nil
		}
	}
	// 7b. Record THIS EXECUTION (roadmap P2-1). The durable verdict above is keyed by the content-addressed
	//     action_id first-wins, so re-running the same operation persists nothing — measured live, 113
	//     executions collapsed into 28 durable outcomes, which makes "N INDEPENDENT hands-off heals of class
	//     X" unrecordable. This appends one row per run carrying the FRESH verdict just computed against THIS
	//     execution's post-state, alongside (never replacing) the per-shape row.
	//
	//     Recorded on BOTH paths deliberately: an UNVERIFIABLE execution writes a NULL verdict with
	//     unverifiable=true, so "we executed and could not check" is durable rather than absent. An execution
	//     that left no trace at all is indistinguishable from one that never happened, which is the worse
	//     failure for an audit surface.
	//
	//     Best-effort by design: the action has ALREADY executed and cannot be un-run, so a recording failure
	//     is audited as a control gap and the outcome still stands — the same discipline as the verdict-persist
	//     path, minus the early return, because this record is evidence rather than a gate.
	if i.executions != nil {
		if err := i.executions.Record(ctx, actionID, r.ExternalRef, r.Prediction.TargetHost, r.Prediction.Site, verdict, verified, r.InvertsActionID); err != nil {
			i.record("execution-record-failed:"+verifyLabel, actionID, "executed but the per-execution record was not written: "+err.Error(), false)
		}
	}
	// 8. Audit — append the governed decision to the tamper-evident ledger (INV-19). On a DEVIATION, enrich the
	//    reason with the structured breakdown (TG-148 diagnostic): the surprise hosts are the deviation triggers,
	//    so a false-deviation is traceable to the exact unpredicted host(s) — e.g. a pre-existing unrelated estate
	//    alert (Observe is estate-wide) misread as a cascade — instead of an opaque "deviation".
	execReason := "governed actuation executed"
	if !verified {
		execReason = "governed actuation executed; post-state UNVERIFIABLE (monitoring read error) — no verdict, no graduation credit"
	} else if verdict == safety.VerdictDeviation {
		// baseline_ok/baseline (+ the host arm) make the ledger row diagnostic of its own most destructive
		// branch: the 2026-07-28 false deviation was UNDIAGNOSABLE post-hoc precisely because the row never
		// said what the verdict was computed against — three candidate mechanisms stayed live for hours.
		execReason = fmt.Sprintf("governed actuation executed; DEVIATION %s; observed=%d target_excluded=%q; "+
			"baseline_ok=%t baseline=%d pre_anomalous_ok=%t pre_anomalous=%d",
			detail.Summary(), len(observed), r.Prediction.TargetHost,
			preOK, len(preObserved), anomOK, len(preAnomalous))
	}
	i.record("execute:"+verifyLabel, actionID, execReason, false)
	// 8b. Arm the breaker on a DEVIATION: the mechanical verifier caught the post-state diverging from the
	//     committed prediction, so trip toward halting mutation (§4.B.3). A match/partial does not trip.
	if verdict == safety.VerdictDeviation {
		i.tripBreaker(ctx, "deviation verdict on "+actionID)
	}
	// 8c. Graduation earn-path (spec/013 REQ-1217, wiring spec/015 REQ-1514): feed the VERIFIED run outcome to
	//     the per-op-class graduation ladder so a clean governed actuation accrues toward `auto`. This is the
	//     ONLY new behavior on the executed tail — a post-verify WRITE of ladder state; it authorizes nothing
	//     and gates nothing (every control ran ABOVE). It is reached ONLY here on the executed+verified path
	//     (a refuse returns long before, so a refused/withheld action never touches the ladder), and the
	//     verifiability gate (4c) guarantees the post-state WAS verified against a real observation — so a
	//     `match` earns a clean run, a `deviation` demotes+resets, a `partial` breaks the streak. A record
	//     failure is NON-FATAL to the already-executed, already-audited action (it cannot be un-run): it is
	//     recorded as a control-gap note and swallowed. A nil recorder is a no-op (no regression). The mode
	//     mode chokepoint still gated the execute above, so nothing accrues until an operator escalates.
	i.recordGraduation(ctx, r.Manifest.Action.OpClass, actionID, verdict, verified)
	outVerdict := verdict
	if !verified {
		// No trustworthy verdict — signal "not verified" to the caller with an empty verdict (backfillManifest
		// no-ops on empty), so nothing downstream reads a `match` we never actually observed (TG-182).
		outVerdict = ""
	}
	return Outcome{Executed: true, Verdict: outVerdict, ActionID: actionID}, nil
}

// recordGraduation feeds ONE executed+verified run outcome to the graduation ladder (spec/013 REQ-1217,
// spec/015 REQ-1514), if a recorder is wired. It runs STRICTLY on the post-verify tail — after execute, verify,
// and the audit record — so it can only ADVANCE ladder state; it never authorizes or gates an action, and a
// caller reaches it only for an action that actually executed and was verified (a refusal returns before it).
// The verify verdict maps to a RunOutcome via policy.OutcomeFromVerdict(verdict, verified). This path acts on
// exactly ONE of them — a `deviation` (a verified post-state diverging from the committed prediction) →
// demote+reset — and is SILENT on the rest. A `match`, a `partial`, and an UNVERIFIABLE post-state
// (verified=false ⇒ OutcomeUnverified) are DEFERRED to the decider that re-observes past the monitoring
// refresh (the session terminus, or a commit-confirmed class's window resolution): recording them here would
// prematurely reset the consecutive-clean streak a slow-settling heal is about to earn (TG-550), and it costs
// no safety because those deciders fail closed against promotion for an unobservable run — TG-182's
// fail-closed-against-laundering holds there, and this path never credits a clean run. A record
// error is NON-FATAL to the already-executed action: it is recorded to the tamper-evident ledger (INV-19) and
// swallowed, never failing a mutation that already happened and cannot be un-run. On a promotion/demotion the
// transition reason is appended to the ledger so the earned/dropped autonomy is durably attributable. A nil
// recorder is a documented no-op (no regression).
func (i *Interceptor) recordGraduation(ctx context.Context, opClass, actionID string, verdict safety.Verdict, verified bool) {
	if i.grad == nil {
		return
	}
	// THE IMMEDIATE OBSERVATION MAY DEMOTE, BUT NEVER PROMOTE (REQ-1223). This runs ~1s after the effect,
	// against a monitoring surface whose poll cycle is minutes long, with a baseline that subtracts every alert
	// already firing — so the candidate set for a deviation is close to empty by construction and `match` is
	// very nearly guaranteed. That is a fine basis for reacting to the BAD case (a deviation observed this fast
	// is real, and a fast demote+trip is exactly what safety wants) and a worthless basis for the GOOD one: it
	// cannot distinguish a heal that worked from one whose consequences have not surfaced yet.
	//
	// So this path acts ONLY on a DEVIATION and stays silent on everything else. A verified `match`, a
	// `partial`, and an UNVERIFIABLE post-state (verified=false) are all deferred to the decider that
	// re-observes past the refresh — the session terminus (temporal/runner/reconcile.go) or, for a
	// commit-confirmed class, the window resolution (ResolveCommitConfirmActivity). Recording any of those HERE
	// as `unverified` would be worse than silence: it RESETS the consecutive-clean count, so a slow-settling
	// heal — a guest still booting at T+1s most of all — would wipe the streak its own decider is about to
	// credit, and no such class could ever graduate (TG-550: 13 verified-clean start-guest heals stuck
	// oscillating at clean_run_count 2, each heal's premature reset cancelling the credit that followed). The
	// original guard caught only `match`; verified=false falls to OutcomeUnverified, which slipped past it and
	// did exactly this. Dropping the reset costs NO safety: the deciders fail closed against promotion for an
	// unobservable run (terminus clean=false ⇒ reset; commit-confirm confirmed-only ⇒ demote/nothing), so
	// TG-182's "a zero-evidence run must never be laundered clean" holds at its true home — and because this
	// path never CREDITS a clean run, dropping the reset opens no promotion path.
	outcome := policy.OutcomeFromVerdict(verdict, verified)
	if outcome != policy.OutcomeDeviated {
		return
	}
	res, err := i.grad.Record(ctx, opClass, outcome)
	if err != nil {
		i.record("graduation-record-failed:"+outcome.String(), actionID, "executed+verified but the graduation ladder was not advanced: "+err.Error(), false)
		return
	}
	if res.Promoted || res.Demoted {
		i.record("graduation:"+outcome.String(), actionID, res.Reason, false)
	}
}

// record appends one governed decision to the ledger; a nil ledger is impossible past SelfTest. The
// ledger rejects an empty action id, so a refusal that has no bound id (e.g. a nil/unsealed manifest) is
// audited under a sentinel — every refusal must leave a durable record (INV-19), never be dropped.
func (i *Interceptor) record(decision, actionID, reason string, withheld bool) {
	if actionID == "" {
		actionID = "(no-action-id)"
	}
	_, _ = i.ledger.Append(audit.GovDecision{Decision: "actuate:" + decision, Reason: reason, ActionID: actionID, Withheld: withheld})
}
