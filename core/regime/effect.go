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
	ic := e.build(leaf)    // the fully-wired spec/013 chain, built around the selected leaf
	return ic.Do(ctx, req) // the ONE path to any effect: admission → … → mode chokepoint → execute → verify
}

// perTargetLeafLane is the PRIVATE in-package capability a lane implements when its effect leaf is resolved per
// action target (the native-ssh fleet lane, REQ-1717). Unexported + referenced only here, so it introduces no
// exported effect path — the composition invariant (REQ-1702) and its structural test stay green.
type perTargetLeafLane interface {
	leafForTarget(ctx context.Context, target string) (actuation.Actuator, error)
}
