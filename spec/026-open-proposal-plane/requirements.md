<!-- spec/026 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/026 — Open proposal plane: day-zero free-form proposals, never executable

**Owning behavior family:** BEH-12 (narrative row lands in `docs/GOVERNED-BEHAVIORS.md` via a separate
law MR after this Draft, per the BEH-10/11 precedent).
**Constitution / invariants:** INV-06 (one grammar), INV-07 (action identity), INV-08 (no model token as
control flow), INV-11 (evidence binding), INV-15 (no fabricated console rows), INV-19 (one ledger).
**Phase:** P2 (proposal plane; actuation untouched).
**Status:** Draft.
**Epic:** TG-227 / TG-228 (plane 1). Design provenance: workflow wf_3a385a3f-a58, 2026-07-31.

With an EMPTY op-class catalog, TG triages fully and emits ledgered, console-rendered,
never-executable free-form remediation proposals. Day-zero TG behaves like the predecessor in shadow
mode with zero configuration. The controlling fact (verified in the ground maps): the fail-closed
machinery already exists — an unregistered op_class already parses (`core/proposal/parse.go:61`),
already seals to nil argv (`temporal/runner/activities.go:1012-1022`), is already refused by every
effect leaf, and is already floored never-auto (`core/policy/graduation.go:470-489`). This spec
converts the "stand-down generator" into a proposer and gives the result a record, a ledger entry,
and a console surface — it builds no new actuation machinery.

## Requirements

- **REQ-2601** — [R] TG-228 · [O] eval-gate (AGENTS.md:85-87).
  WHEN triage attributes an action-warranted cause and no registered op-class addresses it, the agent
  SHALL emit a proposal through the one proposal grammar with a free-form `op_class` slug instead of
  standing down. Realized as (a) the conservative-remediation skill v1.3.0 rewording
  (`agent/skills/skills.go:174-199`) and (b) the protocolPreamble empty-catalog text
  (`agent/loop.go:36-39`). This is a reasoning-surface change and SHALL pass the on-box eval gate in
  addition to this spec's oracles.

- **REQ-2602** — [F] INV-06/INV-07 · spec/002 lockstep pairing.
  The proposal grammar SHALL gain exactly one additive optional field `undo_sketch` (string) on
  proposalJSON (`core/proposal/parse.go:15-82`, `core/proposal/proposal.go:24-38`), with `op_class`
  remaining required and free-form-legal, with no second grammar and no fallback parser, and with the
  field living on the proposal record and never inside `manifest.Action`
  (`core/manifest/manifest.go:40-48`). The same MR SHALL carry the spec/002 prose amendment and
  lockstep restamp.

- **REQ-2603** — [R] TG-228 · [O] replay determinism.
  WHEN a parsed proposal's `op_class` fails `opschema.Lookup` (exact slug, `opschema.go:486-489`),
  the runner workflow SHALL divert to a shadow branch BEFORE NotifyActivity
  (`temporal/runner/workflow.go:367-378`), before the pending-decision projection
  (`workflow.go:388-401`), and before the vote wait (`workflow.go:411-621`), guarded by
  `workflow.GetVersion` for replay determinism; the shadow branch SHALL seal nothing executable,
  record the triage row, append the ledger entry, call the proposal-occurrence seam, and terminate
  with outcome `proposed:shadow`.

- **REQ-2604** — [F] durable record · migration 0046.
  The system SHALL persist shadow proposals additively: `session_triage` gains
  `undo_sketch text NOT NULL DEFAULT ''` and the outcome vocabulary gains `proposed:shadow`, with
  the write path unchanged (first-wins ON CONFLICT DO NOTHING, `core/db/triage_judgment.go:27-60`)
  and the byte-pinned judge prompt (`core/judge/judge.go:79-82`) not rendering `undo_sketch` in v1.
  Migration 0046 adds NO actor-evidence column: structured actor-evidence REUSES the existing
  `session_triage.actor_evidence` jsonb from migration 0035 (`0035_actor_attribution.up.sql`),
  already persisted on every triage row by `core/db/triage_judgment.go:49-54` (see REQ-2610).

- **REQ-2605** — [F] INV-19.
  Every shadow proposal SHALL append exactly one GovDecision
  `{decision:"propose:open", withheld:true}` carrying the manifest action id to the one org-global
  hash chain (`core/audit/ledger.go:28-33,146-177`) and never to a new chain.

- **REQ-2606** — [F] screening · INV-11.
  The fields `op` (the verb), `op_class`, rationale, and `undo_sketch` of a free-form proposal SHALL
  pass `screen.Scrub` before persist, ledger, and console render, and the citation gate
  (`agent/loop.go:433-437`) with evidence binders (`activities.go:944-1000`) SHALL remain the
  unconditional precondition for any shadow record.

- **REQ-2607** — [R] console honesty · INV-15.
  The console SHALL render shadow proposals in a new standalone `proposals` view following the
  nine-touchpoint module recipe (module dir, `assemble.py` list entries, static rail anchor, guarded
  liveGet contract, real-count badge, AuthReadOnly `GET /v1/proposals` handler with nil-reader 503),
  live-only with no fixtures and with no actuation control of any kind rendered.

- **REQ-2608** — [F] fail-closed inheritance (cited, not built).
  The spec SHALL cite the structural never-executable chain verbatim — nil sealedArgv
  (`activities.go:1012-1022`), nil sealEffect (`1039-1075`), effect-kind refusal (`1086-1099`),
  empty-argv refusal at every leaf (`adapters/actuation/actuation.go:58-69`,
  `modules/actuation/ssh/ssh.go:135-145`), never-auto floor for unregistered slugs
  (`core/policy/graduation.go:470-489`), and the mode chokepoint
  (`core/safety/mutation_chokepoint.go:64-72`) — and SHALL carry a defense-in-depth oracle asserting
  the chain refuses a force-routed free-form action, claiming no new machinery.

## Actor-evidence banding policy (owner-ratified 2026-07-31)

Live fault 1406 (2026-07-31): the agent read the PVE task trail (`root@pam` vzstop), named the fault
correctly, and required human confirmation before reversing an operator-authored state — operationally
right, yet a primary-endpoint miss under a rubric that conflates naming the fix with authorization to
apply it. The policy below separates op-class correctness (comparable across models) from band
correctness (scored via the existing `appropriate_band` dimension), keeping every future head-to-head
well-defined regardless of model temperament.

- **REQ-2609** — [R] owner ruling 2026-07-31 · propose duty is band-independent.
  WHEN observations confirm an action-warranted fault, the agent SHALL name the addressing op-class
  (free-form or registered) in its proposal regardless of any actor-evidence about who caused the
  state; actor-evidence SHALL never suppress the proposal itself.

- **REQ-2610** — [F] structured evidence field.
  The proposal record SHALL capture actor-evidence as a first-class structured field — examples: an
  authored stop by a named actor in the PVE task log, a declared maintenance sentinel, a declared
  chaos or benchmark window — screened like every other model-derived text field. This REUSES the
  existing `session_triage.actor_evidence` column (migration 0035; element shape
  `[]attribution.Evidence`: actor, verb, timestamp, ref) rather than adding a new column; the
  evidence-class member REQ-2611's floor mapping keys on is an additive extension of that element
  shape, not a schema change.

- **REQ-2611** — [F] evidence raises the bar, never lowers it.
  WHERE an operator-declared policy maps an evidence class to a band floor (v1 rule:
  authored-action evidence maps to a POLL_PAUSE floor), the banding path SHALL compose that floor
  with the computed band in the safe direction only — a floor SHALL only raise the approval bar
  (computed AUTO or AUTO_NOTICE + authored-action floor ⇒ POLL_PAUSE; a computed POLL_PAUSE passes
  through unchanged) and SHALL never lower a computed band. The composition seam itself — the
  `core/risk` GatedInput floor field and the classifier's safe-direction clamp — is DEFERRED to and
  delivered by spec/028 T-028-4 (the NoticeFloor seam, TRAILER + lockstep restamp there); this
  spec's T-026-8 supplies only the evidence-class→floor mapping table that seam consumes, so the
  band is composed inside the one typed deterministic gate and nowhere else.

## Benchmark interaction

The rematch rubric (campaign #2 and later) SHALL score op-class correctness and band correctness as
separate axes: the per-fault-type `accept` list in `core/diagcorpus/expectations.json` names the
correct op-class(es) (a list, because more than one answer can be right); band correctness is scored
independently through `appropriate_band`. Recorded here and in ADR-0016 so no future campaign
re-conflates them.
