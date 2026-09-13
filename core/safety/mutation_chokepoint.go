package safety

// The mode-driven actuation chokepoint (spec/015 task T-015-13, REQ-1520/1521). This file is the SAFETY-CORE
// successor to the retired MutationGate: the single mechanical authority answering "may this action actuate
// right now?". The answer is the MODE — TG_MUTATION_ENABLED, the standalone console "Mutation OFF" toggle,
// and the separate core/safety.MutationGate object are all RETIRED and ABSORBED here, so exactly ONE state
// (the active mode) answers the question and no two states meaning the same thing require synchronization.
//
// The absorption preserves every fail-closed property the gate guaranteed (INV-09 / INV-21):
//   - the zero/unknown mode is Shadow (policy.Mode's zero value), so MayActuate is false by construction on
//     any un-initialised, absent, corrupt, or un-bound path — the fail-closed floor the disabled gate held;
//   - a transition INTO an actuating mode (Semi-auto / Full-auto) is gated on the green boot preflight (what
//     EnableMutation did) — enforced by policy.ModeController.Transition consulting this Chokepoint;
//   - "may this action actuate?" is `mode ∈ {Semi-auto, Full-auto} && preflightGreen` (what gate.Enabled() did);
//   - a deviation-breaker trip or a `/halt` kill-switch forces the mode to Shadow via ForceShadow (what
//     gate.Disable() did) — one-directional, idempotent, never gated, never re-enabling.
//
// The genuinely independent defense-in-depth layers are NOT folded into this chokepoint: the never-auto
// deny-floor (IsNeverAuto / IsDestructiveOp, INV-09) and the per-action policy verdict (policy.Engine.Decide)
// remain distinct control layers that run beside it. The chokepoint answers ONLY "is the system in an
// actuating posture?"; it does not authorize any individual action.
//
// core/safety MUST NOT import core/policy (policy imports safety — that would be an import cycle), so the mode
// enters through the narrow ActuationMode / ModeAuthority interfaces defined here; *policy.Mode and
// *policy.ModeController satisfy them.
//
// Provenance: [F] "graded fail-closed autonomy" · [R] paradigm-rule 4/7 (one source of truth) ·
// [O] INV-09 (mutation off by construction, fail closed), INV-21 (no dark control). See
// spec/015-policy-engine requirements.md REQ-1520/1521 and docs/adr/0013-mode-is-the-actuation-chokepoint.md.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// ActuationMode is the narrow read the chokepoint needs from the active global autonomy mode: whether that
// mode permits auto-actuation. policy.Mode satisfies it (MayAutoActuate is true only for Semi-auto / Full-auto;
// the zero value ModeShadow returns false). Defined here so the safety core need not import core/policy.
type ActuationMode interface {
	// MayAutoActuate reports whether an action may auto-execute while this mode is active (Semi-auto / Full-auto).
	MayAutoActuate() bool
}

// ModeAuthority is the mode state machine the chokepoint consults for the CURRENT mode and drives to Shadow
// on a safety kill. *policy.ModeController satisfies it. It is an interface (not the concrete controller) so
// the safety core stays free of a core/policy import and so the chokepoint is testable with a fake.
type ModeAuthority interface {
	// CurrentActuationMode returns the single active mode as the narrow actuation-predicate view.
	CurrentActuationMode() ActuationMode
	// ForceShadow drops the active mode to Shadow (read-only) — the successor to gate.Disable(). It is safe,
	// one-directional, idempotent, and never refused (turning autonomy OFF is never earned).
	ForceShadow(reason string)
}

// ShadowForcer is the one-method kill seam the deviation breaker and the /halt handler hold: force the mode to
// Shadow. *Chokepoint satisfies it (it delegates to its ModeAuthority). Defined so those callers depend only on
// the safety core, never on core/policy.
type ShadowForcer interface {
	ForceShadow(reason string)
}

// MayActuate is the SOLE mechanical authority answering "may an action actuate right now?" (REQ-1520). It is a
// pure function of the two inputs the absorbed gate collapsed into the mode: the active mode and the green boot
// preflight. It is true ONLY when the mode is an actuating mode (Semi-auto / Full-auto) AND the preflight is
// green. A nil mode (an un-bound or read-only chokepoint) is fail-closed false — the zero/unknown-mode floor.
// This is the one place the question is answered; every actuation site routes through it (directly or via the
// Chokepoint methods below).
func MayActuate(mode ActuationMode, preflightGreen bool) bool {
	return mode != nil && mode.MayAutoActuate() && preflightGreen
}

// ---------------------------------------------------------------------------------------------------------
// Chokepoint — the stateful runtime object the interceptor, the effect leaves, the breaker, the /halt handler,
// and the posture reporters hold in place of the retired *MutationGate. It carries NO "enabled" bit — the
// actuating state lives in the bound mode (one source of truth). It carries only the boot-preflight-green bit
// (the proof, discharged once, that the interception chain is wired), exactly as the gate did.
// ---------------------------------------------------------------------------------------------------------

// Chokepoint is the mode-driven actuation chokepoint. Its "may actuate?" answer is delegated to the bound
// ModeAuthority via MayActuate — there is no second enabled state to synchronize. A nil mode makes it
// read-only by construction (MayActuate always false): that is the grounder's posture (which never actuates)
// and a worker's posture before the mode authority is bound at boot (fail closed throughout the window).
type Chokepoint struct {
	mode      ModeAuthority // the single source of "may actuate?"; nil ⇒ read-only (fail closed).
	preflight atomic.Bool   // false by default — the boot preflight has not proven the chain wired.
}

// NewChokepoint builds the runtime chokepoint over a mode authority (the worker binds *policy.ModeController).
// A nil mode is permitted and safe: the chokepoint is then read-only (MayActuate always false) until a mode is
// bound with BindMode — so a construction-before-the-controller-exists window fails closed.
func NewChokepoint(mode ModeAuthority) *Chokepoint {
	return &Chokepoint{mode: mode}
}

// NewReadOnlyChokepoint builds a chokepoint with NO mode authority: it can NEVER actuate (MayActuate is always
// false) and never becomes preflight-green. This is the control plane's (grounder's) posture — read-only by
// construction — replacing the old always-disabled gate it held only to report posture.
func NewReadOnlyChokepoint() *Chokepoint { return &Chokepoint{} }

// BindMode binds the mode authority once, after the worker has constructed the durable ModeController. Binding
// only ever ADDS the ability to consult a real mode; before it (mode nil) the chokepoint is read-only, so the
// window between construction and binding is fail-closed. It is idempotent-safe: a re-bind is refused so the
// authority cannot be swapped out from under the running chain.
func (c *Chokepoint) BindMode(mode ModeAuthority) error {
	if c == nil {
		return errors.New("safety: nil chokepoint — cannot bind a mode authority")
	}
	if c.mode != nil {
		return errors.New("safety: chokepoint mode authority already bound — refusing to rebind (one source of truth)")
	}
	c.mode = mode
	return nil
}

// MayActuate reports whether an action may actuate right now — the mode-chokepoint answer the interceptor and
// the effect leaves consult (the successor to gate.Enabled()). It is MayActuate(current mode, preflight-green);
// a nil chokepoint or an un-bound mode is fail-closed false.
func (c *Chokepoint) MayActuate() bool {
	if c == nil || c.mode == nil {
		return false
	}
	return MayActuate(c.mode.CurrentActuationMode(), c.preflight.Load())
}

// GuardMutation returns ErrMutationDisabled unless the chokepoint currently permits actuation. Every mutating
// adapter/activity calls it before any side effect — the identical obligation the gate's GuardMutation held,
// now answered by the mode chokepoint. In Shadow/HITL (or an un-bound mode, or a red preflight) it always
// returns the error.
func (c *Chokepoint) GuardMutation() error {
	if !c.MayActuate() {
		return ErrMutationDisabled
	}
	return nil
}

// ForceShadow drops the active mode to Shadow — the mode-chokepoint successor to gate.Disable(): the runtime
// kill the deviation breaker and the /halt kill-switch call. It is SAFE (can only make the posture MORE
// restrictive), IDEMPOTENT, and NEVER refused — turning autonomy OFF is never earned. It never touches the
// preflight bit, so it cannot re-enable actuation. A read-only chokepoint (nil mode) is already off ⇒ no-op.
func (c *Chokepoint) ForceShadow(reason string) {
	if c == nil || c.mode == nil {
		return
	}
	c.mode.ForceShadow(reason)
}

// ProvePreflight discharges the boot proof obligation, marking the preflight GREEN only when the prover's
// SelfTest passes (the interception chain is fully wired). It is the successor to the proof half of the retired
// EnableMutation — but it does NOT enable actuation: enabling is now a mode transition into Semi-auto/Full-auto
// (gated on this same green preflight). So after ProvePreflight the chokepoint is preflight-green yet still
// read-only while the mode is Shadow (MayActuate stays false). A failed proof leaves the preflight untouched
// (fail closed). This is what makes a LATER, operator-authorized mode escalation possible — it never actuates
// on its own.
func (c *Chokepoint) ProvePreflight(p PreflightProver) error {
	if c == nil {
		return errors.New("safety: nil chokepoint — refusing to prove preflight")
	}
	if p == nil {
		return errors.New("safety: nil preflight prover — refusing to mark the preflight green")
	}
	if err := p.SelfTest(); err != nil {
		return fmt.Errorf("safety: refusing to mark preflight green — the interception chain is not wired: %w", err)
	}
	c.preflight.Store(true)
	return nil
}

// IsPreflightGreen reports whether the boot preflight has proven the interception chain wired (the read the
// console governance surface renders). It is a POSTURE read only — a green preflight does not mean actuation is
// on (the mode governs that); it means an escalation is admissible.
func (c *Chokepoint) IsPreflightGreen() bool {
	return c != nil && c.preflight.Load()
}

// PreflightGreen satisfies policy.PreflightChecker: it gates a mode transition INTO Semi-auto/Full-auto on the
// green boot preflight (REQ-1520). It returns nil when green, else ErrPreflightNotGreen — the escalation fails
// closed and the mode is left unchanged. This is the seam by which the mode's "enable" transition inherits the
// exact preflight obligation the gate's EnableMutation enforced.
func (c *Chokepoint) PreflightGreen(_ context.Context) error {
	if c.IsPreflightGreen() {
		return nil
	}
	return ErrPreflightNotGreen
}

// ---------------------------------------------------------------------------------------------------------
// Test / adapter helpers. A FixedModeAuthority is a mode authority whose actuating state is a single flag — the
// concise way oracles across packages construct a "mutation ON / OFF" chokepoint now that MutationGate is
// retired, without pulling in the full policy.ModeController.
// ---------------------------------------------------------------------------------------------------------

// FixedModeAuthority is a minimal ModeAuthority whose current mode is a fixed actuating/non-actuating flag.
// ForceShadow flips it non-actuating (the kill). It satisfies both ModeAuthority and (implicitly) the mode's
// contract enough for the chokepoint. Used by oracles and by any caller that needs a deterministic posture.
type FixedModeAuthority struct{ actuates atomic.Bool }

// NewFixedModeAuthority returns a fixed authority whose mode does (true) or does not (false) auto-actuate.
func NewFixedModeAuthority(actuates bool) *FixedModeAuthority {
	f := &FixedModeAuthority{}
	f.actuates.Store(actuates)
	return f
}

// CurrentActuationMode returns the fixed mode as an ActuationMode.
func (f *FixedModeAuthority) CurrentActuationMode() ActuationMode { return staticMode(f.actuates.Load()) }

// ForceShadow flips the fixed authority to non-actuating (Shadow) — the kill, preserved for the breaker/halt
// oracles.
func (f *FixedModeAuthority) ForceShadow(string) { f.actuates.Store(false) }

// staticMode is a fixed ActuationMode used by FixedModeAuthority.
type staticMode bool

func (s staticMode) MayAutoActuate() bool { return bool(s) }

// NewActuatingChokepoint builds a chokepoint whose bound mode auto-actuates AND whose preflight is green — the
// concise oracle replacement for the retired "NewMutationGate() + EnableMutation()" pair. It is a TEST/adapter
// helper: production actuation posture always comes from the real ModeController, never this. ForceShadow on
// the returned chokepoint flips it read-only (for the breaker/halt oracles).
func NewActuatingChokepoint() *Chokepoint {
	c := NewChokepoint(NewFixedModeAuthority(true))
	c.preflight.Store(true)
	return c
}
