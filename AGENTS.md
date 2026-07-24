# AGENTS.md — Territory Grounder

Orientation for contributors and AI coding assistants. (Vendor-neutral: Claude Code reads
`CLAUDE.md`, which points here; other tools read this file directly.)

## What this is
**Territory Grounder (TG)** — an open-source, self-hosted, **single-organization, multi-user**
(ADR-0010, **not** multi-tenant SaaS) governed-autonomy SRE platform: it triages alerts, investigates,
and autonomously executes **reversible** remediations under a fail-closed prediction gate, graded
autonomy, and a tamper-evident audit trail. Humans are the circuit-breaker, not the per-action approver.

The law of the project is [`docs/CONSTITUTION.md`](docs/CONSTITUTION.md). Start at
[`docs/00-README.md`](docs/00-README.md) for the full doc set. The manifesto is
[`docs/the-map-is-not-the-territory.md`](docs/the-map-is-not-the-territory.md).

## Project status — resume here

> **The authoritative, reconciled backlog + execution sequence is [`docs/BACKLOG.md`](docs/BACKLOG.md)** — the
> single source of truth, verified against `git log`. Read its top block FIRST for the in-flight state, the
> owner grant, and what to work on next and in what order. This file carries only STABLE orientation; live
> status is NOT duplicated here (it churns and goes stale — that is BACKLOG.md’s job). YouTrack is a reference
> index only (its state field never transitions).
>
> **Live mutation posture:** the mode is owner-set (currently **Semi-auto, MUTATION ON**); graduated-AUTO
> op-classes self-heal, others POLL_PAUSE; an absent/zero/corrupt mode fails closed to Shadow. Verify the live
> mode + the graduated op-class set from the board / the running worker, never a stamp here.

- **What’s built:** the full Phase 0/1 spine + the Phase-2 governed-autonomy behavior lattice — every
  governed-behavior spec is implemented with green acceptance oracles, `make all`-green, `-race` clean. For
  the per-spec inventory read [`spec/00-INDEX.md`](spec/00-INDEX.md) — it is not duplicated here.
- **The grounding loop every session:** read [`docs/SDD-WORKFLOW.md`](docs/SDD-WORKFLOW.md) → pick a
  `pending` task off a `spec/NNN/tasks.json` whose deps are done → implement its `files_owned` → make
  its godog scenarios pass → `make all` green. Use `go run ./tools/specvalidate spec-index <file>` to
  learn which spec/REQ governs any file before you touch it.

## Stack
Go control-plane · **Temporal** (durable orchestration; replaces n8n + Cronicle) · **PostgreSQL +
pgvector** (one DB, single-org; the DML-only runtime role is the privilege boundary — ADR-0010, no
`tenant_id`/RLS) · a native Go agent loop over a **bundled LiteLLM model-gateway** (there is
**no Claude Code / `claude -p` subprocess**) · loadable integration **modules** · a TypeScript
`frontend/` console. Decisions: [`docs/adr/`](docs/adr/). Architecture: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Repo layout
`core/` (safety · auth · config · manifest · db) · `adapters/` (module **interfaces**) · `modules/`
(loadable **implementations**) · `agent/` · `eval/` · `temporal/` · `frontend/` (**not** `web/`) ·
`deploy/` (the compose stack) · `sdk/` · `cmd/grounder/` · `spec/` (the **executable spec lattice**) ·
`tools/specvalidate/` (the spec gate) · `docs/`.

## How the project is built — read this first
TG is built under a **spec-driven-development lattice**: every governed behavior has an EARS
requirement, a dependency-ordered `tasks.json`, and a runnable godog acceptance oracle, all
CI-enforced. **Before writing code or specs, read [`docs/SDD-WORKFLOW.md`](docs/SDD-WORKFLOW.md).**
The exemplar spec is `spec/001-risk-classification`; the map is [`spec/00-INDEX.md`](spec/00-INDEX.md);
the decision is [ADR-0009](docs/adr/0009-spec-driven-development-lattice.md). "Done" is what CI's
oracles say, never what you assert.

## Build / test / lint — run before every commit
```
make check     # boot preflight (no infra needed): asserts mutation OFF, fail-closed enums
make spec      # the spec-lattice gate: EARS/traceability/tasks-DAG + spec<->code lockstep drift
go build ./... && go vet ./... && go test ./...   # test runs the godog acceptance oracles
make lint      # the security gate: bans sh -c / string-built SQL / checks migration down-pairs
make all       # vet · lint · spec · test · build (the full local gate)
make up / make down   # the single-node compose stack (needs deploy/.env — copy deploy/.env.example)
```

## Non-negotiable guardrails (CI enforces these; the constitution mandates them)
1. **Never spawn a shell** (`sh -c`, `bash -c`) — actuation is a fixed `argv` vector (INV-02).
2. **Never build SQL from strings** — bound parameters / sqlc only (INV-03).
3. **Never put a literal secret** in code/config/logs — use `env:` / `file:` references (INV-13).
4. **Every HTTP route is authenticated** — a route declared `auth=none` *panics at registration* (INV-01).
5. **Actuation only through the mode chokepoint** — the sole gate that answers "may this actuate?",
   never a bypass. Enabling/changing the live mode is an authenticated, authority-checked,
   ledger-audited transition (owner-set, currently Semi-auto); an absent/zero/corrupt mode fails
   **closed** to Shadow (no actuate), and a breaker trip or `/halt` forces Shadow. Never wire an
   execute path that skips the interceptor chain (INV-09).
6. **The mechanical safety core** (`core/safety`) is inviolable; every safety enum's zero value is
   its most-restrictive option, so errors fail **closed**.
7. **Eval gates deploys** — a change to a **prompt, skill, model, or the agent's reasoning surface** ships
   only after **`make eval-gate` returns PASS**. The pre-merge gate compares the candidate to a **FRESH base
   arm** (current `origin/main`, measured in the **same on-box window**, arms alternated), **not** the
   committed baseline — so model/estate/main drift cancels instead of false-FAILing the change (TG-64). Paste
   the PASS table into the MR. The eval needs the on-box model gateway, so it is **not** on the MR pipeline
   (it would always fail); it is a required pre-merge step + a nightly `eval-gate-scheduled` trend-watch
   (`make eval-drift`, the committed-baseline drift anchor that self-refreshes). See
   [`docs/EVAL-GATE.md`](docs/EVAL-GATE.md). (Pure test/CI-infra changes are exempt — they don't move agent
   behavior; the gate logic in `eval/gate` is itself CI-unit-tested.)

## Conventions
- **Provenance tags:** docs tag claims `[F]` foundation / `[R]` product reframe / `[O]` audit overlay
  with source ids (`INV-NN`, `spec/00x`, paradigm-rule N). Preserve them — the layering is auditable
  and must not be re-inverted.
- **ADRs:** one decision per file in `docs/adr/`, immutable once *Accepted* (a reversal is a new ADR).
- **Identical terminology everywhere:** `ActionManifest` · the five execution classes · the three
  bands `AUTO / AUTO_NOTICE / POLL_PAUSE` · single-org / `external_ref` correlation key (ADR-0010, no
  `tenant_id`) · the LiteLLM model-gateway.
- **Contributing / build-culture:** [`CONTRIBUTING.md`](CONTRIBUTING.md) — honesty over marketing,
  verify agent-generated claims against live source, spec↔code lockstep as a build gate.

## Roadmap & tracking
[`docs/ROADMAP.md`](docs/ROADMAP.md) (5 phases) · [`docs/EXECUTION-PLAN.md`](docs/EXECUTION-PLAN.md)
(P0 = done, P1 = current backlog). Issues: the **`TG`** YouTrack project.
