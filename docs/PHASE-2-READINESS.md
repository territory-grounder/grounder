<!-- docs/PHASE-2-READINESS.md — the autonomous half of #23 (the readiness REVIEW). The flip itself is
owner-present-only; this document is the go/no-go decision aid, not an authorization to flip. -->

# Phase-2 readiness review — mutation-ON go/no-go

> **⟶ SUPERSEDED (2026-07-20) — actuation has since been enabled.** The flip was executed owner-present:
> actuation is now governed by the **mode chokepoint** (spec/015 absorbed the former `mutation_enabled` gate),
> the live mode owner-set to Full-auto; an absent/zero/corrupt mode still fails closed to Shadow (no actuate),
> and a breaker trip or `/halt` still forces Shadow. This document is retained as the **point-in-time**
> readiness review (dated below): §2's safety-machinery inventory still describes **live** controls, but the
> NOT-READY verdict and the "mutation stays OFF" framing are **historical**. The reconciled current state is
> [`docs/BACKLOG.md`](BACKLOG.md) § Verified state; the §3 evidence gates are still being earned.

**Status:** **NOT-READY** — 4/5 gates RED (TG-79, 2026-07-19; G4 fixed + verified). · **Scope:** the
*readiness review* half of #23. Turning `mutation_enabled` true is an **owner-present-only** action; nothing
in this document authorizes it. **Posture:** conservative — this review surfaces every open gap (§3) and
declares the *narrowest* first step (§3b), not autonomous mutation.

## 1. What the flip actually is

Phase 0/1 built TG as a system that **investigates and proposes but never acts**: every effect is deferred
to the deterministic orchestrator and gated. The flip enables the *execute* path for a bounded set of
actions. It is reversible (a single gate) and, by design, cannot happen by accident (§2).

## 2. The safety machinery gating the flip (built; the controls below are live)

| Control | Where | State |
|---|---|---|
| Mutation gate defaults **false** | `core/safety/safety.go:213` (`enabled atomic.Bool`) | ✅ fail-closed |
| **Proof-gated** enable — sole exported path, refuses unless preflight green | `safety.go:263` `EnableMutation`; `tryEnableMutation` fails closed `:242` | ✅ nothing else can flip it |
| Boot preflight never enables mutation, only proves the base is safe; `grounder --check` | `cmd/grounder/main.go:285` | ✅ |
| **Double** `GuardMutation` on the execute path (activity + interceptor) | `temporal/runner/activities.go` ExecuteActivity + `core/actuate/interceptor.go` Do step 1; gate `safety.go:285` | ✅ refuses twice under OFF |
| Single pre-execution **chokepoint** every command traverses | `core/actuate/interceptor.go` | ✅ |
| **Never-auto floor** — destructive op-classes can never auto-resolve | `core/safety/safety.go:68` `neverAutoFloor` | ✅ |
| Argv-only mutating actuator, **reversible-op allowlist** + allowed-units at the effect leaf | `modules/actuation/ssh/{ssh.go:70,mutate.go}` (`1c22eac`) | ✅ inert (registered only when gate enabled) |
| **Canary pin** — deployment-declared (host,op) → forced POLL_PAUSE (human vote every time) | `core/risk/policy.go:21` `CanaryPins`; zero-value matches nothing | ✅ inert until configured (`6124a88`) |
| Mutation **breaker** (armed → refuses) | `core/actuate/interceptor.go` `WithMutationBreaker`; `cmd/worker/main.go:992` | ✅ |
| Append-only **governance ledger**, hash-chained + verify + DSN-gated round-trip guards | `core/audit/ledger.go`, `core/db/ledger.go`, migration `0015` | ✅ tamper-**resistant** (G4: `tg_runtime` UPDATE/DELETE REVOKEd — verified on box: `UPDATE governance_ledger` → `permission denied`) |

**Conclusion for §2:** the *mechanism* is sound — fail-closed, proof-gated, reversible-scoped, tamper-evident,
and it cannot flip without an explicit proven `EnableMutation`. This is the strongest part of the readiness case.

## 3. Readiness criteria — the honest gate verdict (TG-79)

A grounded, adversarial readiness review (2026-07-19, YT **TG-79**) scored five gates against **current**
evidence — repo, box DB, scorecards — each defaulting to NOT-ready unless the evidence proved otherwise.
**Verdict: NOT-READY — 4 of 5 gates RED. Do not flip.** (This supersedes any earlier optimistic reading;
§2's *machinery* remains the strong part — these gates are about *evidence*, not mechanism.)

| Gate | Status | Honest evidence |
|---|---|---|
| **G1 · Triage quality** | 🔴 RED | On the ONLY real incidents it faced, the agent produced **zero** correct completed proposals: 13/13 live proposals were **hollow** (empty conclusion), all denied/stood-down, judge-scored **~1.15/5**. The only demonstrated competence is deciding to do NOTHING (stop = 4.2/5). Corpus aggregates (3.86) are **stop-dominated** — the *proposed*-subset scores 1.15–1.6. crit-3/TG-69 (falsifiable_prediction +0.70) helped but did **not** clear the hollow-proposal failure mode. ~36 real triages ever (mostly self-monitoring/replay/synthetic). |
| **G2 · Prediction-check loop** | 🔴 RED | The verify-time writer runs, but the loop has **never run on a real action** (0 executions). Predictions are degenerate: **~100× over-prediction** (`sum_tp=1, sum_fp=595`), 20/25 empty, a 131–140-host hub blast radius, and the INV-22 falsifiability control **empty on 24/25 rows** — so the control that is supposed to make auto-apply safe cannot mechanically fail a bad prediction. `ComputeVerdict` isn't site-scoped (deviation = cross-site noise). No auto-rollback. TG-61 (#36) is the long pole; see also TG-83 (learned edge-weight calibration). |
| **G3 · Head-to-head vs predecessor** | 🔴 RED | **Zero** validly-scored pairs; the "5.0 tie" does not exist as a record (both attempts judge_unavailable). No spread; the judge saturates flat 5.0 on the easy Tier-0 case. The Tier-1 instrument is now built (`tools/shadowbench/tier1-run.sh`, safety-hardened) but no *discriminating* pair has been produced. |
| **G4 · Audit-spine tamper-resistance** | 🟢 **GREEN** | **Was RED, now fixed + verified.** `tg_runtime` could UPDATE/DELETE the append-only spine; migration **0015** REVOKEs it — verified on the box (`UPDATE governance_ledger` as tg_runtime → `permission denied`). |
| **G5 · Canary op-readiness** | 🔴 RED | No concrete canary configured (the `TG_CANARY_*`/`TG_ACTUATION_*` knobs are empty); no rollback/breaker **drill** ever run; the alertmanager receiver is an empty sink (no out-of-band paging); a plaintext `TG_ADMIN_TOKEN` literal sits in the container env. **§3b below declares the concrete scope** (closes the "no scope" half). |

Full evidence + the ordered unblock sequence: **TG-79**.

## 3b. The concrete first canary (declares G5's scope)

The narrowest possible first canary, ready to configure once the gates above are green **and** the owner is present:

- **Host:** `dc1librespeed01` — a non-critical guinea-pig LXC (speed-test box; no dependents; already the
  Tier-1 benchmark target; reversible; isolated).
- **Op:** `restart-service` of **exactly one** allowlisted **stateless** unit. Everything else self-protected.

```yaml
# TG_CANARY_POLL_POLICY_FILE (file: ref, config-not-code) — forces this (host,op) to POLL_PAUSE so a human
# votes on EVERY action even with mutation ON. Malformed = hard boot error (fail-closed).
- host: dc1librespeed01
  op:   restart-service
  band: POLL_PAUSE               # human vote every time; never AUTO
# effect leaf, scoped to that ONE host + unit:
TG_ACTUATION_SSH_HOST=dc1librespeed01
TG_ACTUATION_SSH_KEY=file:/run/secrets/canary_ssh_key           # secret ref, never a literal
TG_ACTUATION_SSH_KNOWN_HOSTS=file:/run/secrets/canary_known_hosts  # pinned host key
TG_ACTUATION_SSH_IDENTITY=tg-canary
TG_ACTUATION_ALLOWED_UNITS=<one-stateless-unit>.service          # allowlist of exactly one
```

**Auto-revert (TG-82, recommended for the canary):** wrap the action in commit-confirmed semantics — apply →
arm a Temporal rollback timer → run the mechanical verify → *confirm* (cancel timer) only if the prediction
held, else **auto-revert**. Worst case for the narrow reversible op becomes "auto-healed in T minutes."

**Still required before this runs (TG-79, in order):** G1 (eliminate hollow proposals, clear Tier-1), G2
(bound the predictions + non-empty control + close #36), G5 (rollback+breaker **drill** on a lab host, real
out-of-band paging, remove the plaintext admin token). The canary is **owner-present** regardless.

## 4. Recommendation (conservative)

**The safety machinery is READY; the operating evidence supports a HUMAN-IN-THE-LOOP canary, not autonomous
mutation.** Because live actuation has never been exercised and absolute triage quality is mid, the first
step should keep a human on **every** action:

**Proposed first canary (owner-present) — concrete host/unit/config in §3b:**
1. Configure **one** `CanaryPin` (§3b): `restart-service` of one allowlisted stateless unit on
   `dc1librespeed01` → forces **POLL_PAUSE** (human vote on every action; no AUTO band).
2. Register the SSH mutating module for that host+op only, `allowedUnits` = that one unit.
3. Arm the mutation **breaker**; bind rollback to `action_id`.
4. `EnableMutation` (proven preflight) — then drive **one** real reversible action end-to-end with a human
   approving the POLL_PAUSE, watching the ledger + verify verdict + breaker.
5. Only after N clean human-approved canary actions consider widening scope or an AUTO_NOTICE band.

**Do NOT** flip straight to autonomous AUTO-band mutation. **Do NOT** widen beyond reversible/non-critical
until the canary has proven live actuation + rollback + verify on real hardware.

**Blocking before even the canary:** none in the *machinery*. Recommended-first: (a) R2 input screen — **done**
(merged, eval-positive); the tool-result screen follow-up is desirable but not blocking; (b) exercise the SSH
mutating path against a *lab* host in a dry-run/record mode to validate argv + rollback before the live canary.
Nice-to-have: TG-66 lifecycle capture for full auditability of the first canary action.

## 5. Rollback

The flip is one gate: `EnableMutation` → (to disable) the breaker trips or the gate is reset; the actuator
falls back to `ReadOnly()=true` and the interceptor refuses. Every attempted/blocked action is ledger-recorded.
No estate change is autonomous under the canary (POLL_PAUSE pin = human vote every time).
