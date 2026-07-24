<!-- Territory Grounder — how to port logic from the predecessor. [F]/[R]/[O] provenance as elsewhere. -->

# PORTING-GUIDE.md — porting the predecessor's logic into TG

Territory Grounder (TG) re-implements the battle-tested governed-autonomy logic of its predecessor —
the **claude-gateway** system — as a clean, multi-tenant Go/Temporal/Postgres product. This document
tells a fresh session **where that logic lives, how to port it, and how the external audit is already
applied**. Read it with [`SDD-WORKFLOW.md`](SDD-WORKFLOW.md) (the *how to build* manual) and
[`../spec/00-INDEX.md`](../spec/00-INDEX.md) (the *what to build* map).

## The predecessor repository

```
/home/tg/gitlab/n8n/claude-gateway/
```

It is a **Python + bash + n8n** system (SQLite, n8n workflows, Cronicle, Matrix/YouTrack). It is the
**reference logic and the battle-test record**, not code to copy. Its own `CLAUDE.md`,
`README.extensive.md`, `docs/`, and `memory/` carry the operational history behind every behavior.

### The one cardinal rule: re-implement, never vendor

> **Port the *logic* and the *hardening*, re-expressed through TG's typed, authenticated, fail-closed
> Go spine. Never copy Python / bash / n8n code into TG.**

The predecessor's controls were, in its own words, "real in intent but false in binding" — a prediction
gate walkable via a second grammar, unauthenticated ingress, strings interpolated into shell and SQL.
TG's whole point is to make that class *structurally uncompilable*. So a port is a **translation under
the constitution**, not a transcription: the predecessor tells you *what the behavior is and why*; TG's
[`CONSTITUTION.md`](CONSTITUTION.md) + [`ARCHITECTURE.md`](ARCHITECTURE.md) dictate *how it is allowed to
be expressed* (one grammar, one `action_id`, `argv`-only actuation, RLS, fail-closed enums).

## The predecessor already has an EARS spec/ tree — use it

The predecessor carries its **own** `spec/NNN-*/{requirements.md, tasks.json}` plus `spec/steps/` (its
gherkin step layer) and a `spec/.lockstep.lock`. These are the **`[F]` foundation** that TG's
[`GOVERNED-BEHAVIORS.md`](GOVERNED-BEHAVIORS.md) was reframed (`[R]`) and hardened (`[O]`) from. When
migrating a behavior into TG's lattice, read **all three** sources:

1. **TG `docs/GOVERNED-BEHAVIORS.md` BEH-N** — the primary source; already multi-tenant-reframed +
   audit-hardened, with stable `REQ-NNN` ids. Start here.
2. **predecessor `spec/NNN-*/requirements.md` + `tasks.json`** — the raw `[F]` EARS + task detail.
3. **predecessor source files** (below) — the actual logic, edge cases, and battle-tested guards.

## The map: TG spec ↔ predecessor spec ↔ predecessor source

| TG spec / BEH | predecessor spec dir | predecessor source (logic to port) |
|---|---|---|
| **spec/001-risk-classification** (BEH-1) | `spec/001-risk-classification/` | `scripts/classify-session-risk.py` (bands AUTO/AUTO_NOTICE/POLL_PAUSE, never-auto floor, fail-closed); `scripts/lib/` risk signals |
| **spec/002-prediction-gate** (BEH-2) | `spec/002-prediction-gate/` | `scripts/lib/infragraph.py`, `scripts/infragraph-verify.py` (mechanical match/partial/deviation verdict), `scripts/infragraph-propose-blast-radius.py` |
| **spec/003-auto-resolve** (BEH-3) | `spec/003-auto-resolve/` | `scripts/reconcile-completed-sessions.py` (band-aware close-out, per-incident best-outcome, escalation requeue) |
| **spec/004-governance-demote** (BEH-4) | `spec/004-governance/` | `scripts/write-governance-metrics.py` (auto-demote repeat-offender), `scripts/llm-judge.sh` (judge-death detection) |
| **spec/005-tier1-suppression** (BEH-5) | `spec/005-tier1-suppression/` | `scripts/lib/tier1_suppression.py` (dedup→blast-radius→SR→known-pattern), `scripts/discover-scheduled-reboots.py`, `scripts/classify-reboot-alert.py`, `scripts/promote-scheduled-reboots.py` |
| **spec/006-interface-contracts** (BEH-6) | `spec/006-interfaces/` | the n8n Runner/Bridge/receiver workflows (→ Temporal activities), `scripts/lib/schema_version.py` (schema-version stamping) |
| **spec/007-spec-code-lockstep** (BEH-7) | `spec/007-spec-governance/` | predecessor `spec/.lockstep.lock` + `spec/steps/`, its spec-index tooling (→ TG's `tools/specvalidate lockstep`) |

**Substrate translation** (the predecessor runtimes TG deletes — see `ROADMAP.md` §subtraction): n8n
Runner/Bridge/Poller/receivers → **Temporal workflows/activities**; Cronicle → **Temporal Schedules**;
`gateway-watchdog` + `platform-controller` → **Temporal-native health** + a lean residual; host-local
`~/gateway.*` sentinels → **tenant-scoped policy rows**. Never port a sentinel file or a `pkill`.

## How the external audit is already applied — you do not need the report files

The two external audit reports (a **security** audit → 29 findings and a **quality** audit → 7.3/10
B+) are **already distilled** into the docs and do not exist as files in this repo. You apply the audit
by satisfying each spec's **`[O]`-tagged requirements** — nothing more to fetch:

- The 22 invariants **INV-01..22** in [`CONSTITUTION.md`](CONSTITUTION.md) are the security audit's
  findings turned into non-negotiable clauses.
- [`THREAT-MODEL.md`](THREAT-MODEL.md) maps the 15 threat classes → their controlling invariant.
- Every `[O] INV-NN` / `[O] M-NN` / `[O] H-NN` tag in a requirement **is** an audit finding applied at
  that point. The quality audit's `ActionManifest`, execution classes, and whole-trajectory benchmark
  live in [`ARCHITECTURE.md`](ARCHITECTURE.md) + [`TESTING-AND-BENCHMARK.md`](TESTING-AND-BENCHMARK.md).

So "apply the audit improvements to TG" = **implement each spec's `[O]` requirements and pass its
adversarial acceptance oracles**. If a governed behavior lacks an `[O]` clause where the predecessor had
a bug, that is a porting miss — add the clause, do not skip it.

## The per-spec porting procedure

For each of spec/002–007 (spec/001 is the frozen exemplar — copy its shape):

1. **Read** the three sources (TG BEH-N, predecessor spec/NNN, predecessor source files).
2. **Author** `spec/NNN-slug/` in the fixed 5-file shape (SDD-WORKFLOW §2): EARS `requirements.md`
   (carry the stable `REQ-NNN` ids), `design.md` (Go/Temporal realization), `tasks.json`
   (files_owned + DAG + budgets + req back-links), `acceptance/*.feature` (godog, `@REQ-NNN` tagged) +
   `_test_mapping.json`, `security/threat-model.md` (STRIDE).
3. **Bind GREEN** any scenario whose Go implementation already exists (e.g. spec/006 ↔ `core/auth`,
   spec/002 ↔ `core/manifest`); mark the rest `@pending`. Never fake a `present`.
4. **Re-implement** the logic in Go under `core/` / `temporal/` — through the safety core, one grammar,
   the ActionManifest, RLS — satisfying every `[O]` requirement.
5. **Add** the governed Go files to `spec/.lockstep.lock` (bound to their spec) and re-stamp.
6. **Gate:** `make all` green (`vet · lint · spec · test · build`) before the MR.

## Do not

- Copy Python/bash/n8n into TG, or reproduce a host-local sentinel / `pkill` / mode string.
- Import the predecessor's `CLAUDE.md` scale — it is a **364-line, historically 1000-line** file the
  research flagged as the anti-pattern. TG's orientation stays short (AGENTS.md ~60 lines).
- Treat a predecessor behavior as safe because it "worked" — port it *with* its `[O]` hardening, since
  several predecessor controls were bypassable in binding.
