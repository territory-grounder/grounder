# ADR-0014 — Gate scope matches risk: narrow lockstep, evaluate protected-paths locally, spec content off the law surface

- **Status:** Accepted (2026-07-30)
- **Deciders:** owner (via the approved 2026-07-30 environment-recovery plan)
- **Supersedes:** the governed-set scope implied by ADR-0009's original lockstep rollout; the
  `spec/` clause of the protected-path gate (TG-185)

## Context

Fifteen days of history (1,815 commits) measured what the widest possible gate scope actually
buys. Three findings:

1. `spec/.lockstep.lock` bound 202 files including the repo's hottest orchestration code
   (`cmd/worker/main.go`, 167 changes; `temporal/runner/*`; `core/httpapi/*`, 23 entries). Every
   routine change to those files forced a spec-prose edit plus a full lock rewrite: **431 lock
   commits — 24.8% of all commits** — and `spec/012/design.md` was edited 80 times as a side
   effect of unrelated fixes. Meanwhile `lockstep --restamp` silently overwrote every mismatched
   hash without checking that any spec changed, so the binding protected nothing. Maximum tax,
   zero protection.
2. The protected-path gate skipped locally and hard-failed in CI, so a green `make all` said
   nothing about the law surface; sessions discovered the trailer requirement only after pushing
   (135 commits carry the trailer, many as reflex).
3. Protecting `spec/` made the gate trip twice on every governed change (the decision-core code
   hit already requires the trailer) and turned routine oracle/task edits into governance events.

## Decision

1. **The lockstep lock governs files where drift from spec is dangerous AND change is rare:**
   the safety spine (`core/{safety,actuate,risk,predict,verify,audit,screen,proposal,manifest,auth}`),
   the effect channel (`modules/actuation`), the measurement plane (the spec/025 surfaces,
   `tools/shadowbench`, `tools/faultinjector`), and the gate itself (`tools/specvalidate`).
   202 → 43 entries. Orchestration and plumbing are deliberately un-governed: their correctness
   is owned by their tests, not by a hash binding that history shows produces reflex re-stamps.
2. **`lockstep --restamp` refuses to move a hash whose owning spec did not change in the same
   diff** (merge-base with `origin/main` ∪ staged ∪ uncommitted; fails closed without git). The
   escape hatch `--allow-unchanged-spec` is deliberately loud in the command line. This converts
   the "authorized, audited re-stamp" (REQ-703) from honor system to mechanism.
3. **The protected-path gate evaluates locally** against the merge-base whenever `origin/main`
   is reachable — the same range CI judges — instead of skipping. Local green now predicts CI
   green.
4. **`spec/` content is off the law surface.** Spec correctness is enforced by `specvalidate`
   (shape, traceability, ratify, lockstep); the law lives in the decision-core code paths and the
   named docs, which stay protected.

## Consequences

- A change to `cmd/worker/main.go` is a normal change again: no spec edit, no lock rewrite, no
  trailer. A change to `core/verify/verdict.go` still requires all three — and the restamp is now
  mechanically bound to a real spec edit.
- The lock file churn signal becomes meaningful: a lock diff now always accompanies a real
  governed-spine change.
- Files can be re-added to the lock when they become safety-critical; additions are one-line
  entries reviewed under the normal MR flow (and the lock lives in `spec/`, validated by the
  spec-lattice job).
