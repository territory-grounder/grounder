# AGENTS.md — Territory Grounder

Orientation for contributors and AI coding assistants.

## What this is

**Territory Grounder (TG)** — an open-source, self-hosted, **single-organization, multi-user**
(ADR-0010, not multi-tenant SaaS) governed-autonomy SRE platform: it triages alerts, investigates,
and autonomously executes **reversible** remediations under a fail-closed prediction gate, graded
autonomy, and a tamper-resistant audit trail. Humans are the circuit-breaker, not the per-action
approver. The law is [`docs/CONSTITUTION.md`](docs/CONSTITUTION.md); the manifesto is
[`docs/the-map-is-not-the-territory.md`](docs/the-map-is-not-the-territory.md).

## Resume here — two files, nothing else is mandatory

1. **This file** — stable orientation (you are here).
2. **[`docs/BOARD.md`](docs/BOARD.md)** — the one authoritative queue: current posture, the ranked
   queue, owner-blocked items, definition of done. Work it top-down.

**The board is the QUEUE, not the INVENTORY, and its silence is NOT closure.** The complete list of
outstanding work is YouTrack — `project: TG #Unresolved`. The board carries what to do next and why;
it does not carry all 199 open issues and does not pretend to. Its predecessor did pretend to, while
capping itself at ~8 KB, and shed items by deletion until it cited 20 of 199 — losing every
`[P0][safety]` finding and an `[URGENT][SECURITY]` ticket in the process. If you are looking for work
and the board's queue is exhausted, the tracker is where you look next, NOT another document in `docs/`.

Everything else in `docs/` is reference material, indexed in
[`docs/00-README.md`](docs/00-README.md) — read on demand, never as an entry ritual. Live claims
(mutation mode, what is deployed) are verified from the running system (`make check`, worker boot
log, the board), **never from a stamp in any doc, including this one**.

## The operating loop (owner-ruled 2026-08-10, TG-428)

1. **Orient** — CLAUDE.md → this file → `docs/BOARD.md`. Run `make coldstart` (box-local, ~5s):
   it verifies the machine-local parent-directory resume kit, the resume budget, and that the
   loop's tools are executable. A nightly job resets this working tree to fresh `main` at 03:40
   local — never leave uncommitted work or a checked-out branch in the MAIN tree overnight; do
   MR work in worktrees, **claiming the branch first** with `scripts/claim-worktree.sh <branch>
   <path>` so a parallel session sharing this tree yields rather than clobbering it — protocol and
   stale-reclaim rule live in the script header.
2. **Live-verify** — `make check`; the board's Live-posture claims against the running system;
   `scripts/check-merge-gate-setting.sh`; the latest scheduled `delivery-witnesses` verdict.
3. **Select** — the board queue top-down within its Standing scope and Work classes: take
   AFK-tractable items; scope + carve + surface supervised/owner items, never take them
   unattended. Queue exhausted ≠ done — follow the board's queue-exhausted branch.
4. **Prior art** — search the tracker AND open MRs (`glab mr list`) before building anything.
5. **Red→green** — name the oracle that is red; a change lands only by turning it green. New
   gates ship with EXECUTED killing mutations (including the empty-input one); verdicts carry
   denominators; "found nothing" never shares a state with a refusal.
6. **MR** — branch; `make all` locally, plus `go test <pkg> -race` for anything concurrent
   (`make all` does NOT run `-race`; CI does); law-surface paths carry the trailer (below);
   never `[skip ci]` on an MR branch (the merge gate would wedge it).
7. **Merge** — the project setting `only_allow_merge_if_pipeline_succeeds` refuses a merge
   without a green pipeline (re-verify it stays on in step 2). Tool output about auto-merge can
   lie: confirm the arm/merge actually landed via the API or MR page, never the exit banner.
8. **Deploy-verify** — merging to main AUTO-DEPLOYS on green images. After the merge, run
   `scripts/verify-pipeline.sh <merge-commit-sha>` — the merge-result pipeline is the one that
   deploys, and it is NOT the pipeline the MR tested. The scheduled deployed-sha witness
   independently reds when prod ≠ main beyond its grace window.
9. **Record on-event** — board updates at merge/deploy/live-verify time, never batched; dated
   entries go to `docs/history/BOARD-JOURNAL-YYYY-MM.md`; tracker issues close with evidence
   per the delivery bar below.

### The per-issue delivery bar (Definition of done v1.1, owner-ruled 2026-08-10)

An issue is DONE when its code is **delivered** (MR merged) → **deployed** (prod runs a sha at
or past the merge; witness green) → **e2e-tested** (a named oracle went red→green and the live
evidence is on the ticket) → **evaluated** (an eval-gate record wherever the change touches
behavior surfaces) → **QA-passed** (a fresh-eyes review verdict at ≥0.90 stated confidence,
recorded on the ticket). `tools/tgledger` (TG-428) publishes the generated count of issues at
this bar; hand-written tallies are rejected on sight.

### Standing trailer authority (owner grant; merging this section is its ratification)

The owner (`@ncpjfuzl`, CODEOWNERS) grants autonomous sessions standing authority to stamp
`Law-Change-Approved-By: @ncpjfuzl` on law-surface MRs that IMPLEMENT an owner ruling recorded
on a tracker issue, or MECHANIZE an already-recorded rule (gates, drills, CI wiring). It does
NOT cover: weakening or removing a safety control, changing `core/safety` semantics,
constitutional amendments (CONSTITUTION §6 requires the owner as ratifying authority), or
anything the board's Work classes mark supervised/owner-only. Cite the ruling's issue id in the
MR description. Trailers are one contiguous block — a blank line inside it defeats the gate.

## The autonomy boundary (owner-ruled 2026-08-14, TG-488)

Default rule: a session DECIDES, DOES, and RECORDS (ticket + journal) every decision NOT reserved
below; the owner vetoes after the fact. Surfacing a non-reserved decision to the owner list is a
DEFECT — resolve it by deciding it. The reserved classes, complete:

- **R1** Constitution amendments; anything weakening or removing a safety control.
- **R2** Autonomy-mode changes (Shadow/Semi/Full); arming NEW actuation-capable lanes.
- **R3** Money, external publishing, positioning (TARGET-*).
- **R4** Credential-plane provisioning; multi-host / network-wide estate changes.
- **R5** Scope cuts — won't-do/deferred/superseded closes (they change the 100% denominator).
- **R6** Ratifying governed-behavior specs.
- **R7** Estate knowledge only the owner holds (information asks, not permission asks).

Explicitly NOT reserved: design-option picks (the a/b/c menus); arming read-only ingest sources
shadow-first with an alarm and auto-disarm; single-host reversible config with a written rollback;
eval-gated behavior changes taken WITH the mandatory on-box gate; protected-path MRs that implement
a recorded ruling (the trailer grant above). Every board owner-list entry cites its [R#] clause —
`scripts/lint-autonomy-boundary.sh` (in `make all`) enforces the tag, and an EMPTY owner list is the
goal state.

## Parallel sessions (standing mode since 2026-08-14; protocol of record: TG-488 + its partition comments)

Multiple autonomous sessions may work this repo concurrently. The rules that keep them from
colliding — mechanized or server-enforced wherever possible:

1. **Partition on the record** — the ticket split lives as TG-488 comments. Read it before selecting
   work; never take another session's item; renegotiate edges over session messages and record the
   outcome back on TG-488.
2. **Claim-before-touch (TG-81a)** — a branch is worked only under a claim
   (`.claude/worktrees/.claims/<url-encoded-branch>.claim/meta`); `make all` refuses an unclaimed
   worktree branch (`scripts/lint-claim-before-touch.sh`). Release the claim on merge.
3. **Worktrees only** — the main checkout stays clean on `main`; the nightly reset owns it.
4. **The server referees** — MR-only merges behind the forced-green setting; deploys serialize via
   the CI resource_group; merges are verified via the API, never a tool banner.
5. **Shared-resource mutexes** — `/tmp/tg-gateway.lock` is the box heavy-load mutex (no batteries,
   deploys, or estate churn while any session's eval arm measures); eval tunnel ports are
   session-pinned via `TG_EVAL_PORT` (TG-493 records why). **The lock is RESERVED 03:30–06:00
   local for the owner-scheduled nightly** (eval-drift trend-watch): no session takes it, or holds
   it into, that window — daytime session usage lock-starved the nightly for six consecutive
   nights before this rule existed (TG-493, Error-75 every night, no measurement since 08-08).
6. **Nothing durable lives in the message channel** — every agreement, handoff, or finding lands on
   a tracker ticket or the board in the same session.

## Stack & layout

Go control-plane · **Temporal** (durable orchestration) · **PostgreSQL + pgvector** (one DB,
single-org; DML-only runtime role is the privilege boundary) · native Go agent loop over a bundled
**LiteLLM** gateway (no Claude Code / `claude -p` subprocess) · loadable integration modules.

| path | role |
|---|---|
| `core/` | safety spine: risk, predict, verify, audit ledger, actuation interceptor, estate graph, knowledge |
| `agent/` | the native Go ReAct loop + skills registry |
| `adapters/` · `modules/` | module interfaces · their implementations (~30 connectors) |
| `temporal/` | the Runner workflow, flywheel, crons |
| **`deploy/console/v2/`** | **the operator console that is actually served** — `assemble.py` builds `index.html`; verified by `make console-verify` + `deploy/served_console_test.go`. (The React `frontend/` was unreachable by construction and is being removed — see BOARD/Phase E; do not build on it.) |
| `eval/` | the binding eval gate + corpus |
| `tools/shadowbench/` | the predecessor head-to-head harness |
| `spec/` + `tools/specvalidate/` | the executable spec lattice + its gate |

## How it is built

Spec-driven where it matters: governed behaviors carry EARS requirements, a `tasks.json` DAG, and
godog acceptance oracles ([`docs/SDD-WORKFLOW.md`](docs/SDD-WORKFLOW.md), ADR-0009). Which spec
governs a file: `go run ./tools/specvalidate spec-index <file>`. Task status vocabulary is CLOSED:
`pending` / `blocked` / `completed` — "in progress" is the open MR, never a tasks.json value.
Lattice state is honest, not aspirational: some specs carry `pending`/`blocked` tasks; the board
and `spec/00-INDEX.md` say which. "Done" is what CI's oracles say, never what you assert — and an
oracle that cannot fail is a defect, not coverage (this project's most expensive lesson).

## A push is not finished until its pipeline is (2026-07-31)

`make all` green locally is necessary, never sufficient — main runs jobs no local gate can
(image build/sign/scan, deploy). **After every push, run
`scripts/verify-pipeline.sh`** (blocks on that commit's pipeline, prints failing jobs, exits
non-zero). Work is not done while it is red: fix it, or state in the same breath why the red is
expected and what clears it. This rule exists because MR pipelines were being watched while
pushes to main were fire-and-forget, and a wall of unread red on main hid the one job that was
genuinely reporting a problem. (Steps 7–8 of the operating loop operationalize this; the merge
gate and the scheduled witness now enforce its two halves mechanically.)

## Build / test / lint — before every commit

```
make all       # vet · lint · protected-paths · spec · contracts-check · console-verify · test · build (~1 min)
make check     # boot preflight (no infra): fail-closed enums, mutation posture as BUILT
make up/down   # the single-node compose stack (deploy/.env from deploy/.env.example)
```

### Mutate toward EMPTINESS, not only toward wrongness (TG-365)

Before shipping anything that gates, verifies or counts, ask: **if the thing this check is about were
entirely absent, what would it print?** If the answer is its healthy output — or an exit code it shares
with a real refusal — it will certify rather than catch. Four unrelated controls failed exactly this way
on 2026-08-06, and the fourth is the proof: `grounder --check`'s SSH preflight reported
`Configured() == 0 → skipping → pass` while compose was forwarding one of five key-ref sources, so the
gate that existed to catch that omission could not catch its own blinding.

- **Publish the denominator beside the verdict, always, including at zero.** `0 of 3,383` is evidence;
  `0` alone is what a broken query, an unwired store and a healthy system all produce identically.
- **Give "found nothing" its own state**, never the refusal's — a shared exit code or string reaches the
  agent as a grounded estate fact.
- **Run a killing mutation that EMPTIES the input** — delete it, blank the config, point at a
  nonexistent path — not only one that corrupts it. Corrupt-data mutations pass on this class; several
  guards written on 2026-08-06 passed theirs and failed the empty-data one. Confirm the mutation applied
  (grep the marker) before reading the result.

- **Ask what the gate does when you BREAK the thing it claims to watch** — its subject list is usually
  narrower than its prose. Six gates failed this on 2026-08-23 alone, each reporting PASS: the dead-code
  gate rooted at 9 of the module's 26 mains (43 pieces of LIVE code reported dead, advising "delete it");
  contract-drift compared 1 of 18 artifacts; eval-evidence omitted the `//go:embed`ed op-class catalog that
  renders into every preamble; `lint-protected-paths` promised "CI gates" and covered only `lint-*.sh`, so
  the actuation-coverage check and the deploy witness could be weakened with no trailer; and a peer's
  pairing check passed on the exact state it was written to catch. None was found by reading a PASS — every
  one by weakening a real subject file and demanding red. Widening to the scope a gate ALREADY CLAIMS is a
  defect fix, not a policy change; keep its bounded-ness pinned by a specimen that genuinely belongs outside.

Sibling shape: *present, not reaching* — a control that never runs at all. This one is the control
running and having nothing to say.

## Non-negotiable guardrails (CI-enforced; the constitution mandates them)

1. **Never spawn a shell** (`sh -c`) — actuation is a fixed argv vector (INV-02).
2. **Never build SQL from strings** — bound parameters only (INV-03).
3. **Never put a literal secret anywhere** — `env:` / `file:` / `bao:` references (INV-13).
4. **Every HTTP route is authenticated** — `auth=none` panics at registration (INV-01).
5. **Actuation only through the mode chokepoint** — the sole gate answering "may this actuate?";
   owner-set live mode; absent/zero/corrupt fails closed to Shadow; breaker trip or `/halt`
   forces Shadow; never wire an execute path skipping the interceptor chain (INV-09).
6. **`core/safety` is inviolable** — every safety enum's zero value is its most restrictive.
7. **The model never authors an executed command** — op-classes are compiled builders or
   operator-authored argv templates; no model token reaches the vector. There is no
   operator-arbitrary-argv lane (designed three ways and refuted 2026-07-28 — see
   `docs/ARCHITECTURE.md` §4.1).
8. **Eval gates behavior changes** — any change to a prompt, skill, model, or the agent's
   reasoning surface ships only after the eval gate passes on-box ([`docs/EVAL-GATE.md`](docs/EVAL-GATE.md));
   use the `tg-eval-runner` agent for procedure and scorecard-reading traps.

## Conventions

- ADRs: one decision per file in `docs/adr/`, immutable once Accepted (a reversal is a new ADR).
- Identical terminology everywhere: `ActionManifest` · execution classes · `AUTO / AUTO_NOTICE /
  POLL_PAUSE` · single-org / `external_ref` (no `tenant_id`).
- Provenance tags in docs (`[F]`/`[R]`/`[O]`) are load-bearing — preserve them.
- Protected paths need a `Law-Change-Approved-By:` trailer — the current list lives in
  `scripts/lint-protected-paths.sh`, run locally by `make all`.
- [`CONTRIBUTING.md`](CONTRIBUTING.md): honesty over marketing; verify agent-generated claims
  against the live system before recording them.
