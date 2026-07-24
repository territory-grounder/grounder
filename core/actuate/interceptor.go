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
	// so the audited policy_decision joins the decision-tracer walk by external_ref (spec/020 REQ-2005). It NEVER
	// feeds a gate/verdict — it rides only into the policy audit projection + the "runner:<ref>" principal.
	ExternalRef string
	Gated       bool                                         // a committed prediction produced this (from the prediction gate)
	Argv        []string                                     // the FIXED argv vector — no shell, no string-built command (INV-02)
	Stdin       []byte                                       // validated stdin bytes
	Evidence    []Evidence                                   // orchestrator-captured tool-results (INV-11)
	Prediction  verify.Prediction                            // the committed prediction, for the post-execution verdict
	Observe     func(context.Context) []verify.ObservedAlert // captures observed alerts after execution
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
}

var (
	// ErrGateUnwired is returned by SelfTest (and Do) when a collaborator is missing — a control that
	// cannot execute must fail LOUD and safe, never be left dark (S8-5).
	ErrGateUnwired = errors.New("actuate: interception chain is unwired (a governed control is missing) — refusing")
)

// VerdictSink durably records the mechanical verdict for an executed action (the pgx db.VerdictStore
// satisfies it). The deterministic verifier is the only writer; the interceptor persists exactly one verdict
// per action_id after computing it. Optional — nil ⇒ the in-memory path (the verdict rides only the ledger).
type VerdictSink interface {
	Commit(ctx context.Context, actionID, planHash, targetHost, site string, v safety.Verdict) error
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
	breaker    *safety.MutationBreaker // optional armed breaker; a post-execution deviation/chain-gap forces Shadow
	decider    PolicyDecider           // optional policy authorizer (spec/015); nil ⇒ pass-through (mode chokepoint still gates)
	modeNow    func() policy.Mode      // reads the active mode for the policy EvalInput; nil ⇒ ModeShadow (fail closed)
	grad       GraduationRecorder      // optional graduation earn-path recorder (REQ-1217/1514); nil ⇒ the sync path does not advance the ladder
	gateSink   trace.GateVerdictSink   // optional OBSERVE-ONLY per-gate verdict trail (spec/020 T-020-7); nil ⇒ no-op, gate behavior identical
}

// WithGateVerdictSink attaches the OBSERVE-ONLY per-interceptor-gate verdict trail (spec/020 T-020-7, REQ-2007)
// and returns the interceptor (chainable). The interceptor emits one ordered row per gate as it runs; the sink
// is a PURE SIDE EFFECT — an Emit error is swallowed and NEVER changes a gate outcome, and nothing reads the
// trail back to make a decision. A nil sink (the default) is a no-op: the chokepoint behaves identically.
func (i *Interceptor) WithGateVerdictSink(s trace.GateVerdictSink) *Interceptor {
	i.gateSink = s
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
func NewInterceptor(chokepoint *safety.Chokepoint, actuator actuation.Actuator, ledger *audit.Ledger) *Interceptor {
	return &Interceptor{chokepoint: chokepoint, actuator: actuator, ledger: ledger}
}

// SelfTest asserts every REQUIRED collaborator is wired — the mode chokepoint, the effect-leaf actuator, and
// the ledger. The boot preflight calls it (and Chokepoint.ProvePreflight only marks the preflight green when it
// passes); a nil collaborator fails loud so a dark control cannot be booted (INV-21/S8-5). The policy decider
// is OPTIONAL (its absence is a documented pass-through, the mode chokepoint still gates), so it is not
// required here.
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
	if safety.IsNeverAuto(r.Manifest.Action.OpClass) || !r.Manifest.Action.Reversible ||
		safety.IsDestructiveOp(r.Manifest.Action.Op, r.Manifest.Action.OpClass) {
		return refuseGate("never-auto-floor", "never-auto floor (adapter) — irreversible, floor-class, or server-derived destructive op")
	}
	emitGate("never-auto-floor", "pass", "")
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
	// 4c. Verifiability gate: a mutating action may execute ONLY if the chain can verify its post-state. The
	//     deterministic verifier diffs the committed prediction against the alerts OBSERVED after execution
	//     (INV-10); with no observer wired, ComputeVerdict would run against a nil observation and return
	//     `match` for EVERY action — the verifier becomes theater and a deviation can never be caught. So a
	//     request with no post-execution observer is refused BEFORE it executes: we do not execute what we
	//     cannot verify. This makes it structural — an executed action ALWAYS carries a real observer, so the
	//     verdict is never computed against a nil observation. (Under mutation OFF the chain refuses at step 1
	//     long before here, so this changes nothing today.)
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
	//     error fails closed. A nil decider is a documented pass-through (no policy engine wired) — the mode
	//     chokepoint still gates. The never-auto floor (step 2) already refused any irreversible/destructive op
	//     BEFORE here, so honoring an approval can never let a floor-class mutation through.
	if i.decider != nil {
		mode := policy.ModeShadow
		if i.modeNow != nil {
			mode = i.modeNow()
		}
		dec, derr := i.decider.Decide(ctx, policy.EvalInput{
			OpClass:    r.Manifest.Action.OpClass,
			Argv:       strings.Join(r.Argv, " "),
			Host:       r.Manifest.Action.Target,
			Reversible: r.Manifest.Action.Reversible,
			Confidence: r.Confidence,
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
		emitGate("policy", "pass", string(dec.Verdict()))
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
		i.chokepoint.ForceShadow("mutation breaker OPEN — cross-process kill (a sibling worker tripped)")
		return refuseGate("breaker", "mutation breaker OPEN — system-wide kill (a sibling worker tripped)")
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
	// TG-148: capture the estate's active alerts NOW, immediately BEFORE the effect fires, as the verify BASELINE.
	// The post-execution Observe (step 6) is estate-WIDE, so without a baseline an UNRELATED alert already firing
	// on a host the prediction never named would be misread as a cascade SURPRISE and false-DEVIATE — demoting the
	// op-class auto→approve AND tripping the breaker on a SUCCESSFUL heal (the flywheel break). Only alerts that
	// APPEAR after this point can be this action's causal effect. Observe is guaranteed non-nil (verifiability gate
	// 4c) and a read error collapses to empty ⇒ an empty baseline = the original estate-wide behavior (fail-safe:
	// widening what counts as a surprise, never hiding a real cascade).
	preObserved := r.Observe(ctx)
	// 5. Execute — the single chokepoint. argv-only, no shell (INV-02).
	if _, err := i.actuator.Exec(ctx, r.Argv, r.Stdin); err != nil {
		return refuseGate("execute", "execute failed: "+err.Error())
	}
	emitGate("execute", "pass", "")
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
	observed := r.Observe(ctx)
	// ComputeVerdictDetail is the sole author; ComputeVerdict is its byte-identical enum projection. We take the
	// DETAIL (surprise hosts = the deviation triggers, rule mismatches = the partial triggers) so a DEVIATION's
	// exact cause is durably recorded in the ledger below (TG-148: action_verdict stores only the enum, leaving a
	// false-deviation — e.g. a pre-existing UNRELATED estate alert misread as a cascade surprise, since Observe is
	// estate-wide — undiagnosable post-hoc). The derived verdict is unchanged; this adds observability only.
	detail := verify.ComputeVerdictDetailWithBaseline(r.Prediction, observed, preObserved)
	verdict := detail.Verdict
	// spec/020 T-020-7: the verify gate's mechanical verdict is the final ordered row of the trail
	// (match/partial/deviation) — the deterministic verifier's outcome, observe-only.
	emitGate("verify", string(verdict), "")
	// 6b. Record the VERIFIED stage, then verify the WHOLE chain binds this one action_id in lifecycle order
	//     (INV-07 — the "one immutable typed chain from evidence to verdict"). A broken chain on an action that
	//     already executed is surfaced (it cannot be un-executed) so the caller reconciles.
	verRecErr := r.Manifest.Record(manifest.StageVerified, verdict)
	if execRecErr != nil || verRecErr != nil || r.Manifest.VerifyChain() != nil {
		i.record("chain-integrity-gap:"+string(verdict), actionID, "executed but the manifest lifecycle chain did not record/verify", false)
		// A chain-integrity gap on an executed action is a trip-worthy safety event: arm the breaker so a
		// repeat (or, at threshold 1, this one) halts mutation in-process (§4.B.3).
		i.tripBreaker(ctx, "chain-integrity gap on "+actionID)
		return Outcome{Executed: true, Verdict: verdict, ActionID: actionID, Reason: "manifest lifecycle chain gap"}, nil
	}
	// 7. Persist the mechanical verdict durably (INV-10) — the deterministic verifier is the only writer, so
	//    the verdict store records it exactly once per action_id. A persist failure is a control gap on an
	//    action that ALREADY executed: it is recorded as a refusal-shaped audit entry but the execution stands
	//    (we cannot un-execute), so the caller learns the verdict was not durably written and can reconcile.
	if i.verdicts != nil {
		if err := i.verdicts.Commit(ctx, actionID, r.Prediction.PlanHash, r.Prediction.TargetHost, r.Prediction.Site, verdict); err != nil {
			i.record("verdict-persist-failed:"+string(verdict), actionID, "executed but verdict not durably written: "+err.Error(), false)
			if verdict == safety.VerdictDeviation {
				i.tripBreaker(ctx, "deviation verdict (verdict persist failed) on "+actionID)
			}
			return Outcome{Executed: true, Verdict: verdict, ActionID: actionID, Reason: "verdict not persisted"}, nil
		}
	}
	// 8. Audit — append the governed decision to the tamper-evident ledger (INV-19). On a DEVIATION, enrich the
	//    reason with the structured breakdown (TG-148 diagnostic): the surprise hosts are the deviation triggers,
	//    so a false-deviation is traceable to the exact unpredicted host(s) — e.g. a pre-existing unrelated estate
	//    alert (Observe is estate-wide) misread as a cascade — instead of an opaque "deviation".
	execReason := "governed actuation executed"
	if verdict == safety.VerdictDeviation {
		execReason = fmt.Sprintf("governed actuation executed; DEVIATION %s; observed=%d target_excluded=%q",
			detail.Summary(), len(observed), r.Prediction.TargetHost)
	}
	i.record("execute:"+string(verdict), actionID, execReason, false)
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
	//     chokepoint (mutation OFF) still gated the execute above, so nothing accrues until an operator escalates.
	i.recordGraduation(ctx, r.Manifest.Action.OpClass, actionID, verdict)
	return Outcome{Executed: true, Verdict: verdict, ActionID: actionID}, nil
}

// recordGraduation feeds ONE executed+verified run outcome to the graduation ladder (spec/013 REQ-1217,
// spec/015 REQ-1514), if a recorder is wired. It runs STRICTLY on the post-verify tail — after execute, verify,
// and the audit record — so it can only ADVANCE ladder state; it never authorizes or gates an action, and a
// caller reaches it only for an action that actually executed and was verified (a refusal returns before it).
// The verify verdict maps to a RunOutcome via policy.OutcomeFromVerdict with verified=true — the verifiability
// gate guarantees a real post-state observation, so the run IS verified: `match` → verified-clean (the only
// promoting outcome), `deviation` → demote+reset, `partial`/other → streak-break (never a promotion). A record
// error is NON-FATAL to the already-executed action: it is recorded to the tamper-evident ledger (INV-19) and
// swallowed, never failing a mutation that already happened and cannot be un-run. On a promotion/demotion the
// transition reason is appended to the ledger so the earned/dropped autonomy is durably attributable. A nil
// recorder is a documented no-op (no regression).
func (i *Interceptor) recordGraduation(ctx context.Context, opClass, actionID string, verdict safety.Verdict) {
	if i.grad == nil {
		return
	}
	outcome := policy.OutcomeFromVerdict(verdict, true)
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
