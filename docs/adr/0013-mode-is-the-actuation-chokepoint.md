# ADR 0013 — The mode is the sole actuation chokepoint (absorb MutationGate + TG_MUTATION_ENABLED)

## Status
Accepted (spec/015-policy-engine task T-015-13, REQ-1520/1521). This is a deliberate, audited safety-core
refactor — **never a silent deletion**. It re-expresses the INV-09 / INV-21 mutation-gate obligations in
mode-chokepoint terms across the constitution, the deviation breaker, the `/halt` kill-switch, the boot
preflight, spec/013, and `.lockstep.lock`.

## Context
Before this change, "may an action actuate?" was answered by **three** things that all meant the same thing
and had to be kept in sync:

1. the `TG_MUTATION_ENABLED` environment knob (a boot-time flag the worker read to arm mutation),
2. the standalone console "Mutation OFF / read-only" toggle, and
3. the process-global `core/safety.MutationGate` object (`enabled` + `preflight` atomic bits), whose
   `Enabled()` gated every mutating side effect, whose `Disable()` was the breaker/`/halt` kill, and whose
   `EnableMutation` was the proof-gated flip.

Meanwhile spec/015 introduced the **four-mode autonomy ladder** (`policy.Mode`: Shadow / HITL / Semi-auto /
Full-auto, zero value Shadow), whose `MayAutoActuate()` already answered exactly the same question — "is the
system in an actuating posture?" — for Semi-auto / Full-auto. Two states meaning "can actuate" is a
**synchronization-bug surface**: a console toggle, an env flag, a gate bit, and a mode can drift, and the
safest-looking one can be the stale one. On the single most safety-critical control, that is unacceptable.

## Decision
**The active mode is the SOLE mechanical actuation chokepoint.** `TG_MUTATION_ENABLED`, the standalone
console toggle, and the `MutationGate` object are all RETIRED and ABSORBED into the mode state machine, so
exactly ONE state answers "may this action actuate?".

Everything the gate did is now expressed on the mode, with the fail-closed properties preserved one-for-one:

| Retired gate mechanism | Absorbed into the mode chokepoint |
|---|---|
| `NewMutationGate()` default (disabled) | the mode's zero value is `ModeShadow` (read-only) — `MayActuate == false` by construction on any un-initialised / absent / corrupt / un-bound path |
| `gate.Enabled()` (runtime "may actuate?") | `safety.MayActuate(mode, preflightGreen) = (mode ∈ {Semi-auto, Full-auto}) && preflightGreen` — the ONE authority; `Chokepoint.MayActuate()` / `GuardMutation()` call it |
| `EnableMutation(gate, prover)` (proof-gated flip) | split into two: `Chokepoint.ProvePreflight(prover)` marks the boot preflight green (proves the chain wired) WITHOUT actuating; **enabling** is an operator-authorized, audited `policy.ModeController` transition into Semi-auto/Full-auto, gated on that same green preflight |
| `gate.Disable()` (breaker / `/halt` kill) | `Chokepoint.ForceShadow(reason)` → `ModeController.ForceShadow` drops the mode to Shadow — one-directional, idempotent, never refused, never touches the preflight bit |
| `TG_MUTATION_ENABLED=true` boot arm | **removed** — there is no env-armed switch; the worker boots read-only (mode Shadow, the durable default) and stays read-only until an operator escalates the mode |

The actuation interceptor's `Do` chain is now, in order: admission (poll-approval → never-auto floor →
structure → evidence → territory → verifiability) → **policy-authorize** (`policy.Engine.Decide` via the
`AuditedEngine`, refuse unless `auto`) → credential-authenticate (downstream in the effect leaf) →
**mode-chokepoint** (`MayActuate`, refuse unless actuating) → execute → verify → audit.

### What stays DISTINCT (not folded into the chokepoint)
Two genuinely independent defense-in-depth layers remain separate control layers, exactly as before:

- the **constitutional never-auto deny-floor** (INV-09) — `safety.IsNeverAuto` / `safety.IsDestructiveOp`,
  a non-configurable mechanical clamp beneath the engine; and
- the **per-action policy verdict** (REQ-1506) — `Engine.Decide`'s deny-overrides authorization of the
  individual action.

The mode chokepoint answers only "is the system in an actuating posture?"; it authorizes no individual
action. A policy `auto` verdict cannot execute while the mode is not actuating, and the never-auto floor
clamps even in Full-auto — each layer alone fails closed (the negative control: **no code path actuates at
Shadow**).

Host-side hardening (a dedicated actuation user + a sudoers verb-allowlist) MAY be applied by an operator as
an OPTIONAL independent backstop, but is NOT required by TG and SHALL NOT take the unscalable form of a
per-command forced-command SSH key: the actuation path uses one ordinary scoped credential, and the
authoritative control is the policy engine + the mode chokepoint.

## Consequences
- **One source of truth.** There is no second "enabled" state to synchronize; the console reads the mode,
  the worker gates on the mode, the breaker/`/halt` force the mode. A stale toggle can no longer advertise a
  posture the runtime does not hold.
- **Fail-closed is stronger, not weaker.** The zero-value floor moved from an atomic bool to the mode enum's
  zero value (Shadow); a `ForceShadow` now also persists Shadow so a restart after a kill stays read-only.
- **Enabling is now governed, not env-flipped.** Arming actuation requires an authenticated, authority-checked,
  ledger-audited mode transition through the policy engine — a real improvement over an out-of-band env edit.
- **Import hygiene.** `core/safety` must not import `core/policy` (policy imports safety), so the mode enters
  the chokepoint through the narrow `safety.ActuationMode` / `safety.ModeAuthority` interfaces; `policy.Mode`
  and `*policy.ModeController` satisfy them.
- **Mutation stays OFF.** This refactor changes the plumbing, not the posture: the deployed default is Shadow,
  `MayActuate == false`, and this leaf does not enable actuation anywhere.

## References
- spec/015-policy-engine `requirements.md` REQ-1520 / REQ-1521, `design.md` (`policy.Mode` component).
- `core/safety/mutation_chokepoint.go` (the authority), `core/policy/mode.go` (`ForceShadow` /
  `CurrentActuationMode`), `core/actuate/interceptor.go` (the wired chain).
- `docs/CONSTITUTION.md` (mutation keystone / INV-09 / INV-21), spec/013-actuation-interceptor.
