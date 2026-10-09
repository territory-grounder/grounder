package regime

import (
	"context"
	"errors"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
)

// LaneEffect is the composition seam (REQ-1702). It does NOT execute and it adds NO gate: it takes a SELECTED
// Lane and hands that lane's UNEXPORTED effect leaf to the spec/013 actuate.Interceptor, whose Do runs the
// full wired chain (admission → never-auto floor → policy authorize → credential authenticate → mode
// chokepoint → execute → verify). This is THE composition invariant: because a lane's leaf is only reachable
// through effectLeaf (unexported, this package only) and LaneEffect routes it straight into Interceptor.Do,
// there is no exported effect path in the whole engine that skips the chain. The standing structural test
// (composition_test.go) fails if any lane ever grows one.
//
// The interceptor is bound to ONE actuator at construction (its actuator field is unexported and has no
// setter — spec/013), but the engine selects a DIFFERENT lane per target, so LaneEffect cannot mutate an
// existing interceptor's leaf. Instead it holds an InterceptorBuilder: given the selected leaf, the builder
// constructs the FULLY-wired interceptor (mode chokepoint + ledger + policy decider + breaker + verdict sink)
// around it. The builder is the composition point a later wave (cmd/worker/main.go, registered in T-017-x)
// fills with the same wiring it already builds for the native-ssh interceptor today — this file never
// constructs the chain itself, so it composes over spec/013 without touching core/actuate internals.
type LaneEffect struct {
	build InterceptorBuilder
}

// InterceptorBuilder constructs a fully-wired spec/013 interceptor around a selected lane's effect leaf. The
// caller (cmd/worker, later wave) owns the wiring — the mode chokepoint (core/safety), the ledger, and the
// optional policy decider / mutation breaker / verdict sink — exactly as it wires the native-ssh interceptor
// today; LaneEffect only parametrizes the leaf. It MUST return a non-nil interceptor; a builder that returns
// an unwired interceptor is caught by the interceptor's own SelfTest at Do time (fail loud).
type InterceptorBuilder func(effectLeaf actuation.Actuator) *actuate.Interceptor

// ErrSeamUnwired is returned when LaneEffect has no interceptor builder or is handed a nil lane — a control
// that cannot route through the chain fails LOUD and safe rather than reaching an effect around it.
var ErrSeamUnwired = errors.New("regime: lane-effect seam is unwired (no interceptor builder / nil lane) — refusing")

// NewLaneEffect builds the composition seam over an InterceptorBuilder. A nil builder yields a seam whose
// Apply fails loud (ErrSeamUnwired) — an unwired seam can never silently reach an effect.
func NewLaneEffect(build InterceptorBuilder) *LaneEffect { return &LaneEffect{build: build} }

// Apply drives a governed actuation for the SELECTED lane through the spec/013 interceptor chain (REQ-1702).
// It obtains the lane's effect leaf via the unexported accessor (reachable only here), asks the builder for
// the fully-wired interceptor around that leaf, and calls Interceptor.Do — the SINGLE chokepoint. Apply
// itself never touches the leaf's Exec: the interceptor owns the effect. A nil seam, nil builder, or nil lane
// fails loud; everything else is the interceptor's outcome (executed / refused / verdict), unchanged.
func (e *LaneEffect) Apply(ctx context.Context, lane Lane, req actuate.Request) (actuate.Outcome, error) {
	if e == nil || e.build == nil || lane == nil {
		return actuate.Outcome{}, ErrSeamUnwired
	}
	// Per-target leaf (REQ-1717): the native-ssh lane implements perTargetLeafLane, so it resolves its leaf via
	// leafForTarget — the FLEET lane (NewNativeSSHLaneFunc) builds a leaf bound to THIS action's target host,
	// while the STATIC lane (NewNativeSSHLane) ignores the target and returns its one fixed leaf (identical to
	// effectLeaf, behaviour-preserving). A fleet build refusal (no configured actuation identity, empty target)
	// is a GOVERNED refusal — Refused + Executed=false with a NIL error — so the runner records a clean,
	// permanent refusal and does NOT retry (retrying a resolution failure would loop). Lanes that do NOT
	// implement perTargetLeafLane (awx-job / proxmox), or a per-target lane reached with an ABSENT manifest,
	// take effectLeaf() and the interceptor's Do owns the outcome unchanged (including its own nil-actuator /
	// nil-manifest fail-loud, which is correct: an absent manifest is a programming error, not a heal to retry).
	var leaf actuation.Actuator
	if pt, ok := lane.(perTargetLeafLane); ok && req.Manifest != nil {
		built, berr := pt.leafForTarget(ctx, req.Manifest.Action.Target)
		if berr != nil {
			return actuate.Outcome{Refused: true, Reason: "regime: no per-target actuation leaf for target " + req.Manifest.Action.Target + " — refused (fail closed): " + berr.Error()}, nil
		}
		leaf = built
	} else {
		leaf = lane.effectLeaf() // unexported — the only in-package way to obtain a lane's actuator
	}
	// An ASYNC lane must never reach the SYNCHRONOUS verify path. Its leaf returns a JOB HANDLE — the estate is
	// untouched at return — so the chain would score the post-state of a mutation that has not happened yet:
	// exit 0 ⇒ `execute: pass`, an unchanged estate ⇒ `match`, and `match` is the only promoting graduation
	// outcome. A mutating op-class would climb toward AUTO on launches that later FAILED, and nothing would ever
	// read the terminal job status. The deferred-verify channel built for exactly this (asyncverify.go) has NO
	// producer wired: Reserve/BindHandle have no non-test callers, and actuate.Outcome carries no handle field,
	// so the job id is discarded at this boundary and could not be bound even by a willing caller.
	//
	// This is inert TODAY only because the AWX lane is unconfigured, so its pendingActuator fails closed on a Go
	// error. That is a config accident, not a control: setting TG_AWXJOB_BASE_URL + TG_AWXJOB_LAUNCH_TOKEN_REF
	// would silently arm the fail-open. This makes the refusal explicit and structural instead, so the missing
	// integration cannot be activated by environment alone — it must be BUILT first. Governed refusal, not an
	// error: Refused + Executed=false with a nil error, so the runner records it permanently and does not retry.
	if returnsHandleNotOutcome(lane.Regime()) {
		return actuate.Outcome{Refused: true, Reason: "regime " + string(lane.Regime()) +
			": an async lane's launch returns a job handle, not an outcome — refusing to adjudicate it on the " +
			"synchronous verify path (a handle is a prediction, not a success). Wire the deferred-verify channel first."}, nil
	}
	ic := e.build(leaf)    // the fully-wired spec/013 chain, built around the selected leaf
	return ic.Do(ctx, req) // the ONE path to any effect: admission → … → mode chokepoint → execute → verify
}

// perTargetLeafLane is the PRIVATE in-package capability a lane implements when its effect leaf is resolved per
// action target (the native-ssh fleet lane, REQ-1717). Unexported + referenced only here, so it introduces no
// exported effect path — the composition invariant (REQ-1702) and its structural test stay green.
type perTargetLeafLane interface {
	leafForTarget(ctx context.Context, target string) (actuation.Actuator, error)
}

// ApplyReserved drives an ASYNC lane's LAUNCH through the same spec/013 chain Apply uses — the deferred-verify
// PRODUCER's dispatch (TG-122 slice 0, REQ-1709/1712). It exists because Apply's structural refusal of
// handle-returning lanes is a guard against an UNWIRED deferred verify; once the producer Reserved the
// pending-verification record for this action, the launch is governed end-to-end and the refusal would be the
// only thing standing between a reserved launch and its lane.
//
// THE CONTRACT (pinned by the runner's oracle, not enforceable here): the caller MUST have successfully
// Reserved the action on the AsyncVerify channel BEFORE calling this, and MUST BindHandle the returned
// Outcome.AsyncHandle after an executed launch — a caller that skips Reserve leaves the launch with no
// pending record, which the channel then cannot verify (BindHandle fails ErrNoPending, loudly). The runner's
// ExecuteActivity is the ONE caller and also withholds the synchronous verdict (req.Observe=nil ⇒ the
// interceptor records the launch UNVERIFIABLE, TG-182) so no inline `match`/deviation is ever minted for an
// estate the launch has not touched — the deferred channel stays the sole verdict author (INV-10).
//
// A SYNCHRONOUS lane is refused here (governed, not an error): routing a sync effect around Apply would skip
// nothing today, but the two entry points MUST stay disjoint or the launch-shape contract blurs.
func (e *LaneEffect) ApplyReserved(ctx context.Context, lane Lane, req actuate.Request) (actuate.Outcome, error) {
	if e == nil || e.build == nil || lane == nil {
		return actuate.Outcome{}, ErrSeamUnwired
	}
	if !returnsHandleNotOutcome(lane.Regime()) {
		return actuate.Outcome{Refused: true, Reason: "regime " + string(lane.Regime()) +
			": ApplyReserved is the ASYNC launch dispatch — a synchronous lane routes through Apply"}, nil
	}
	// effectLeaf(), NOT Apply's perTargetLeafLane resolution: neither async lane implements it today (only
	// native-ssh does). If a future async lane grows per-target leaves, this dispatch must gain the same
	// resolution — until then the guard refuses rather than silently launching the wrong (non-per-target)
	// leaf while Apply would have resolved the right one.
	if _, perTarget := lane.(perTargetLeafLane); perTarget {
		return actuate.Outcome{Refused: true, Reason: "regime " + string(lane.Regime()) +
			": an async lane with per-target leaf resolution is not supported by ApplyReserved yet — refusing (wire the resolution first)"}, nil
	}
	// actuation.NewStdoutCapture (not an in-package wrapper): the REQ-1702 composition guard forbids any
	// direct Exec call in this package — the interceptor must own the one call to the effect — so the
	// delegation lives beside the Actuator contract itself. The leaf is still reachable solely through this
	// package (effectLeaf stays unexported); the capture adds observation, never a second effect path.
	capture := actuation.NewStdoutCapture(lane.effectLeaf())
	ic := e.build(capture)
	out, err := ic.Do(ctx, req)
	if err == nil && out.Executed {
		// The leaf's Result carried the job handle (awxjob returns its job id in Stdout; gitopsmr its MR ref).
		// Post-fill the Outcome so the producer can BindHandle it — the interceptor itself stays launch-shape
		// agnostic and never touches the field.
		out.AsyncHandle = capture.Captured()
	}
	return out, err
}
