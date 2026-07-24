package regime

import (
	"context"
	"errors"

	"github.com/territory-grounder/grounder/adapters/actuation"
)

// Lane is one regime's effect channel (REQ-1700/1702). A Lane binds a Regime to an effect leaf — the
// actuation.Actuator that ACTUALLY applies a change — but it exposes that leaf ONLY through an UNEXPORTED
// accessor (effectLeaf). This is the composition invariant made structural (REQ-1702): because effectLeaf is
// unexported, no code outside this package can pull a lane's actuator out and call Exec around the chain; the
// ONLY way to reach any lane's effect is to hand its leaf to the spec/013 actuate.Interceptor via LaneEffect
// (effect.go), which runs the full wired chain (admission → never-auto floor → policy authorize → credential
// authenticate → mode chokepoint → execute → verify). A lane adds NO gate and skips NONE — it is a channel,
// not a permission.
//
// The interface deliberately publishes exactly ONE exported method, Regime(); there is no exported EffectLeaf
// / Actuator / Exec method, so grep and the standing composition test (composition_test.go) can prove no
// exported effect path exists.
type Lane interface {
	// Regime is the management regime this lane is the sanctioned effect channel for.
	Regime() Regime
	// effectLeaf is the UNEXPORTED accessor for the lane's actuation.Actuator. Only package regime can call it
	// (Go forbids an outside package from naming an unexported interface method), and only LaneEffect does —
	// handing the leaf to the interceptor's Do. There is deliberately no exported counterpart.
	effectLeaf() actuation.Actuator
}

// ---------------------------------------------------------------------------------------------------------
// native-ssh lane (REQ-1700): the already-built spec/013 SSH effect leaf, re-expressed as ONE Lane among
// several rather than a hardcoded default. Its leaf is exactly the actuation.Actuator cmd/worker already
// builds (sshactuation.NewNativeRunner-backed ssh.Module); this lane wraps it so the existing path routes
// through the SAME LaneEffect seam as every future lane.
// ---------------------------------------------------------------------------------------------------------

type nativeSSHLane struct {
	leaf actuation.Actuator
	// build (optional, REQ-1717) resolves the effect leaf PER ACTION TARGET: instead of one leaf bound to a
	// single configured host, it constructs a leaf bound to THIS action's target host (authenticating with the
	// operator's configured ACTUATION identity — distinct from any read identity, the plane-split). Nil ⇒ the
	// static single-host `leaf` is used (behaviour-preserving). Set only via NewNativeSSHLaneFunc.
	build func(ctx context.Context, target string) (actuation.Actuator, error)
}

// NewNativeSSHLane re-expresses the spec/013 SSH effect leaf as a Lane (REQ-1700). Pass the SAME
// actuation.Actuator cmd/worker wires into actuate.NewInterceptor today (the read-only ssh.Module in Phase
// 0/1; mutation stays OFF regardless — the lane is a channel, the interceptor's mode chokepoint is the key).
// A nil actuator is permitted at construction but the interceptor's SelfTest refuses it loudly at Do time, so
// an unwired native-ssh lane can never silently execute.
func NewNativeSSHLane(leaf actuation.Actuator) Lane { return nativeSSHLane{leaf: leaf} }

// NewNativeSSHLaneFunc re-expresses the SSH lane with a PER-TARGET effect leaf (REQ-1717, P3-B2): rather than
// one leaf pinned to a single configured host, `build` constructs a leaf bound to the ACTION's own target host
// at Apply time. This retires the single-host binding so restart/start-service actuates on the host that
// alerted, fleet-wide. The lane still exposes only the UNEXPORTED accessors — LaneEffect.Apply reaches the
// per-target build through a private in-package interface, so the composition invariant (REQ-1702) holds and
// no exported effect path is introduced. `build` MUST return a leaf whose ActuationHost()==target so the
// spec/013 host-match gate (REQ-1219) passes by construction and stays as defense-in-depth; a build refusal
// (no configured actuation identity, empty target) is surfaced by Apply as a GOVERNED refusal. Mutation stays
// OFF regardless: every per-target leaf is built beneath the SAME mode chokepoint, which refuses at Shadow.
func NewNativeSSHLaneFunc(build func(ctx context.Context, target string) (actuation.Actuator, error)) Lane {
	return nativeSSHLane{build: build}
}

func (l nativeSSHLane) Regime() Regime                 { return RegimeNativeSSH }
func (l nativeSSHLane) effectLeaf() actuation.Actuator { return l.leaf }

// leafForTarget returns the effect leaf for a specific target host: the per-target build when wired (REQ-1717),
// else the static single-host leaf. UNEXPORTED — reached only by LaneEffect.Apply via the private
// perTargetLeafLane interface, so no exported effect path is introduced (REQ-1702).
func (l nativeSSHLane) leafForTarget(ctx context.Context, target string) (actuation.Actuator, error) {
	if l.build != nil {
		return l.build(ctx, target)
	}
	return l.leaf, nil
}

// ---------------------------------------------------------------------------------------------------------
// awx-job lane (REQ-1700): DEFINED here so the regime set is complete and the resolver can bind it. Its REAL
// effect leaf is the native-Go AWX job-template actuator (modules/actuation/awxjob, T-017-3 — an
// actuation.Actuator that launches an allowlisted, policy-authorized template with typed extra_vars under the
// mode chokepoint). Because core must not import modules, the concrete actuator is INJECTED as an
// actuation.Actuator via WithAWXActuator (the worker constructs it and wires it, a later wave) — the interface
// and the resolver binding do not change. With NO leaf injected the lane carries a pendingActuator that
// REFUSES — a well-typed lane whose effect is a documented refusal, never a silent no-op or a bypass. Either
// way mutation stays OFF: the real leaf still routes beneath the mode chokepoint (Shadow) until the
// owner-present flip, and the pending leaf can only refuse.
// ---------------------------------------------------------------------------------------------------------

type awxJobLane struct {
	leaf actuation.Actuator
}

// NewAWXJobLane builds the awx-job lane. With no leaf wired it carries the pendingActuator (fail-closed
// refuse). The worker constructs the real modules/actuation/awxjob actuator and injects it via
// WithAWXActuator (a later wave); the interface and the resolver binding do not change when that lands.
func NewAWXJobLane(opts ...AWXJobLaneOption) Lane {
	l := awxJobLane{leaf: pendingActuator{regime: RegimeAWXJob}}
	for _, o := range opts {
		o(&l)
	}
	return l
}

// AWXJobLaneOption configures the awx-job lane. It exists so the real AWX actuator (modules/actuation/awxjob,
// T-017-3) can be injected without changing this file's exported surface.
type AWXJobLaneOption func(*awxJobLane)

// WithAWXActuator injects the real AWX job-template effect leaf (the modules/actuation/awxjob actuator,
// T-017-3), passed as an actuation.Actuator so core does not import modules. A nil actuator is ignored so the
// lane keeps its refusing placeholder rather than an unwired leaf.
func WithAWXActuator(a actuation.Actuator) AWXJobLaneOption {
	return func(l *awxJobLane) {
		if a != nil {
			l.leaf = a
		}
	}
}

func (l awxJobLane) Regime() Regime                 { return RegimeAWXJob }
func (l awxJobLane) effectLeaf() actuation.Actuator { return l.leaf }

// proxmox lane (REQ-1700): the Proxmox VE hypervisor channel for guest-LIFECYCLE ops (start / stop). Its real
// effect leaf is the native-Go PVE actuator (modules/actuation/proxmox — an actuation.Actuator that POSTs an
// allowlisted, floor-clamped guest lifecycle op to the PVE REST API under the mode chokepoint; reboot/shutdown/
// reset/destroy are on the never-auto floor there). Because core must not import modules, the concrete actuator
// is INJECTED as an actuation.Actuator via WithProxmoxActuator (the worker constructs + wires it). With NO leaf
// injected the lane carries the fail-closed pendingActuator. This lane is selected by the op-class's effect KIND
// (proxmox-lifecycle) rather than the target host's management regime — the same guest is native-ssh for a
// service restart but proxmox-mediated for start/stop.
type proxmoxLane struct {
	leaf actuation.Actuator
}

// NewProxmoxLane builds the proxmox lane. With no leaf wired it carries the pendingActuator (fail-closed refuse).
// The worker constructs the real modules/actuation/proxmox actuator and injects it via WithProxmoxActuator; the
// interface + resolver binding do not change when that lands.
func NewProxmoxLane(opts ...ProxmoxLaneOption) Lane {
	l := proxmoxLane{leaf: pendingActuator{regime: RegimeProxmox}}
	for _, o := range opts {
		o(&l)
	}
	return l
}

// ProxmoxLaneOption configures the proxmox lane so the real actuator can be injected without changing this
// file's exported surface.
type ProxmoxLaneOption func(*proxmoxLane)

// WithProxmoxActuator injects the real PVE effect leaf (modules/actuation/proxmox), passed as an
// actuation.Actuator so core does not import modules. A nil actuator is ignored so the lane keeps its refusing
// placeholder rather than an unwired leaf.
func WithProxmoxActuator(a actuation.Actuator) ProxmoxLaneOption {
	return func(l *proxmoxLane) {
		if a != nil {
			l.leaf = a
		}
	}
}

func (l proxmoxLane) Regime() Regime                 { return RegimeProxmox }
func (l proxmoxLane) effectLeaf() actuation.Actuator { return l.leaf }

// ---------------------------------------------------------------------------------------------------------
// pendingActuator is the refuse-until-wired placeholder effect leaf for a lane whose real actuator is a later
// task. It is an actuation.Actuator that reports ReadOnly()==true (it can only refuse — mutation stays OFF)
// and whose Exec always fails closed. It is reachable only through the interceptor's Do like any leaf, and
// under mode Shadow the interceptor refuses long before Exec, so today it is never even reached.
// ---------------------------------------------------------------------------------------------------------

// ErrLaneNotWired is the fail-closed refusal a not-yet-built lane returns from its effect leaf. It is a hard
// refusal, never a silent success — a lane whose actuator is a later task cannot actuate.
var ErrLaneNotWired = errors.New("regime: lane effect leaf is not yet wired — refusing (fail closed)")

type pendingActuator struct{ regime Regime }

func (p pendingActuator) Capability() string { return string(p.regime) + ".pending" }
func (p pendingActuator) ReadOnly() bool     { return true }
func (p pendingActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	return actuation.Result{}, ErrLaneNotWired
}

// Compile-time proofs: both concrete lanes satisfy Lane, and the placeholder satisfies the stable actuation
// interface (so it drops into the interceptor exactly like a real leaf).
var (
	_ Lane               = nativeSSHLane{}
	_ Lane               = awxJobLane{}
	_ Lane               = proxmoxLane{}
	_ actuation.Actuator = pendingActuator{}
)
