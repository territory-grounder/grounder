<!-- Canonical map of the executable spec lattice. Provenance: [F]/[R]/[O] as in the docs. -->

# spec/ — Territory Grounder executable specification index

This is the canonical map of the spec lattice. Each `spec/NNN-slug/` directory is the machine-checkable
home of one governed-behavior family: EARS requirements → design → a `tasks.json` execution DAG →
executable godog acceptance oracles → a per-feature STRIDE threat model. The narrative counterpart of
each spec is its `BEH-N` family in [`docs/GOVERNED-BEHAVIORS.md`](../docs/GOVERNED-BEHAVIORS.md); the
authoring rules and CI gates are in [`docs/SDD-WORKFLOW.md`](../docs/SDD-WORKFLOW.md).

**Every push runs `go run ./tools/specvalidate`** (EARS shape · REQ uniqueness · weasel-word ban ·
tasks DAG acyclicity · no file-ownership collision · requirement↔task↔scenario traceability) and
`go run ./tools/specvalidate lockstep --check` (governed source files stay hash-bound to their spec).

| Spec | Title | Behavior | Constitution / INV | Phase | Status |
|---|---|---|---|---|---|
| [spec/001](001-risk-classification/) | Three-band risk classification | BEH-1 | INV-06/07/09/10/11 | P2 (safety core P0) | **Ratified** |
| [spec/002](002-prediction-gate/) | Fail-closed prediction gate + mechanical verdict | BEH-2 | INV-07/10 | P2 | **Ratified** |
| [spec/003](003-auto-resolve/) | Per-incident auto-resolve + escalation requeue | BEH-3 | INV-01/11/12 | P2 | **Ratified** |
| [spec/004](004-governance-demote/) | Governance auto-demote + judge-death detection | BEH-4 | INV-15/22 | P2–P3 | **Ratified** |
| [spec/005](005-tier1-suppression/) | Tier-1 known-transient & scheduled-reboot suppression | BEH-5 | INV-04/20 | P2 | **Ratified** |
| [spec/006](006-interface-contracts/) | Interface contracts (auth, generated wire contracts) | BEH-6 | INV-01/15/16/19 | P0–P3 | **Ratified** |
| [spec/007](007-spec-code-lockstep/) | Content-aware spec↔code lockstep | BEH-7 | INV-22 | P4 | **Ratified** |
| [spec/008](008-connectors/) | Day-1 connector fleet (loadable integration modules) | — | INV-05/17/18 | P1–P2 | **Ratified** |
| [spec/009](009-kubernetes-deploy/) | Kubernetes / Helm as a first-class deploy target | — | INV-13/16 | P1+ | **Ratified** |
| [spec/010](010-ux-console/) | Operator console (UX pillar) | — | INV-01/15 | Track A | **Approved** |
| [spec/011](011-agent-loop/) | Native Go agent loop (ReAct over LiteLLM) | — | INV-06/08 | P1 | **Ratified** |
| [spec/012](012-runner-workflow/) | Read-only Runner Temporal workflow (stops at propose) | — | INV-07/09/21 | P1 | **Ratified** |
| [spec/013](013-actuation-interceptor/) | Wired-by-construction actuation interceptor + mutation gate | — | INV-09/10/11/21 | P2 | **Ratified** |
| [spec/014](014-skill-store/) | Versioned skill store + graduation flywheel (console-editable, eval-gated) | ADR-0012 | INV-08/11/19/22 | P1 | **Ratified** |
| [spec/015](015-policy-engine/) | Operator-managed policy engine (graduated autonomy access control) | BEH-8 | INV-06/07/09/10/11/12/13/19/21 | P2 | **Draft** |
| [spec/016](016-credential-engine/) | Credential / Identity Engine (per-target credential resolution + unified sync) | BEH-9 | INV-01/05/09/13/16/17/19 | P2 | **Draft** |
| [spec/017](017-actuation-regime-engine/) | Actuation Regime Engine (regime-aware effect lanes, AWX-job first) | — | INV-02/05/07/09/10/11/13/17/19/21 | P2 | **Draft** |
| [spec/018](018-recency-decay/) | Shared recency/decay & periodic reconciliation of the learned stores | — | INV-08/09/21/22 | P1 | **Ratified** |
| [spec/019](019-maintenance-window/) | Scheduling awareness & maintenance-window seam (estate scheduler integration) | — | INV-02/05/09/13/17 | P1–P2 | **Ratified** |
| [spec/020](020-decision-tracer/) | Governed decision tracer (per-workflow packet-tracer + trace archive) | — | INV-01/08/13/19/22 | P1 | **Ratified** |
| [spec/021](021-groundnet/) | The groundnet contract (federation envelope + adapter seam + invariants) | — | INV-01/08/09/10/13/14/19/21/22 | P-network (far-future) | **Draft** |
| [spec/022](022-credential-delivery/) | Credential delivery & secret substrate (master key off the worker, JIT resolution, blast-radius split) | — | INV-01/05/09/13/16/17/19 | P2 | **Draft** |
| [spec/023](023-actor-attribution/) | Actor-attribution grounding (who is the actor behind the observed change?) | BEH-10 | INV-04/05/08/09/11/13/14/17/18/19/20/21/22 | P2 | **Draft** |
| [spec/024](024-secret-plane/) | Secret plane — force a real backend, eliminate secret-zero (no plaintext at rest; OpenBao/Vault + Vaultwarden + Passbolt) | BEH-11 | INV-01/05/09/13/16/17/19/21/22 | P2 | **Draft** |
| [spec/025](025-benchmark-harness/) | Benchmark harness — the measurement plane is governed evidence | — | INV-19/21/22 | P2 (proof plane) | **Ratified** |
| [spec/026](026-open-proposal-plane/) | Open proposal plane — day-zero free-form proposals, never executable (epic TG-227) | BEH-12 | INV-06/07/08/11/15/19 | P2 | **Ratified** |
| [spec/027](027-world-model-discovery/) | Auto-drafted world model — the admin reviews, never authors (epic TG-227) | BEH-13 | INV-13/14/15/17/19 | P2 | **Ratified** |
| [spec/028](028-earned-op-class-catalog/) | Earned op-class catalog — candidates, dossiers, the widened ladder (epic TG-227, ADR-0016) | BEH-14 | INV-08/10/14/19/22 | P2–P3 | **Draft** |
| [spec/029](029-commit-confirmed-actuation/) | Commit-confirmed actuation — auto-reverting mutations, the fail-safe-by-construction G2 shape (TG-82; owner sign-off 2026-08-14, TG-488 B5 — T-029-1..5 workable) | — | INV-02/07/09/19 | P2 | **Approved** |
| [spec/030](030-transaction-plan/) | Transaction plans — multi-step repairs, approved once, all-or-nothing with auto-revert (TG-58; owner ruling 2026-08-22 — T-030-1..5 done, T-030-6 blocked on the drill deferral) | — | INV-07/08/11/12/17/19/21 | P2 | **Approved** |

**Status vocabulary** (fixed): `Draft` → `Approved` → `Ratified`. A spec is `Approved` once its
requirements + acceptance oracles exist and the validator passes; `Ratified` once every non-`@pending`
requirement has a `present` acceptance oracle and its governed files are in `.lockstep.lock`.

**Requirement id scheme.** Each spec owns a 100-block of `REQ-NNN` ids preserved from the predecessor
for stable cross-referencing: spec/001 → REQ-0xx, spec/002 → REQ-1xx, spec/003 → REQ-2xx,
spec/004 → REQ-3xx, spec/005 → REQ-4xx, spec/006 → REQ-5xx, spec/007 → REQ-7xx, spec/008 → REQ-8xx,
spec/009 → REQ-9xx, spec/010 → REQ-6xx, spec/011 → REQ-10xx, spec/012 → REQ-11xx, spec/013 → REQ-12xx,
spec/014 → REQ-13xx, spec/015 → REQ-15xx, spec/016 → REQ-16xx, spec/017 → REQ-17xx, spec/018 → REQ-18xx,
spec/019 → REQ-19xx, spec/020 → REQ-20xx, spec/021 → REQ-21xx, spec/022 → REQ-22xx,
spec/023 → REQ-23xx, spec/024 → REQ-24xx, spec/025 → REQ-25xx, spec/026 → REQ-26xx,
spec/027 → REQ-27xx, spec/028 → REQ-28xx, spec/029 → REQ-29xx, spec/030 → REQ-30xx.

<!-- BEGIN GENERATED: specvalidate tally -->
> **Lattice tally (generated).** 30 specs in the lattice shape under `spec/`.
> Index status column (as claimed): 19 Ratified / 3 Approved / 8 Draft of 30 rows.
> Tasks: 334 completed / 16 pending / 1 blocked of 351 across 30 specs.
> Acceptance scenarios: 539 present / 12 pending / 0 retrospective_gap of 551 across 30 mappings.
> Pending-task concentration: 010-ux-console 2 · 016-credential-engine 1 · 021-groundnet 7 · 022-credential-delivery 2 · 024-secret-plane 3 · 025-benchmark-harness 1.
> Generated by `specvalidate tally --write`; hand edits are rejected by `tally --check`.
<!-- END GENERATED: specvalidate tally -->

Operational truth lives in [`docs/BOARD.md`](../docs/BOARD.md), never here. spec/001 is the frozen
exemplar of the shape.
