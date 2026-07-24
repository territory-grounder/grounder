<!-- Territory Grounder — the operating manual for the spec-driven-development lattice.
     Provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# SDD-WORKFLOW.md — how to build Territory Grounder under the spec lattice

**Read this before writing code or specs.** Territory Grounder (TG) is built largely by autonomous
coding agents, and the thing that lets an agent *finish* rather than *flail* is not prose — it is a
machine-checkable lattice where every requirement has a unique id, a dependency-ordered task, a
runnable acceptance oracle, and a CI gate that rejects drift. This document is that lattice's operating
manual. It is deliberately short; the enforcement lives in `tools/specvalidate` and CI, not in your
discipline.

This adopts the industry-convergent **spec-driven development (SDD)** pattern (GitHub Spec Kit, AWS
Kiro, EARS, BDD/godog) — verified as best practice across Google/Anthropic/GitHub/AWS agentic-SDLC
guidance — tuned to TG's Go/Temporal/Postgres stack and its inviolable safety core. The single most
load-bearing rule the research produced:

> **Detailed documentation does not drive agents. Runnable oracles, a dispatchable task DAG, and
> enforced gates do.** "Done" is decided by CI running a test the agent did not author — never by the
> agent asserting success.

---

## 1. The document hierarchy (what governs what)

```
docs/CONSTITUTION.md ....... inviolable law: the mechanical safety core + invariants INV-01..22
docs/adr/NNNN-*.md ......... one architectural decision per file, immutable once Accepted
docs/*.md .................. the narrative layer (ARCHITECTURE, DATA-MODEL, THREAT-MODEL, GOVERNED-BEHAVIORS, …)
spec/00-INDEX.md ........... the canonical map of the executable spec lattice
spec/NNN-slug/ ............. one executable spec per governed-behavior family (the machine-checkable layer)
spec/.lockstep.lock ........ governed source files hash-bound to their owning spec
AGENTS.md / CLAUDE.md ...... agent orientation (where things are, how to run the gates)
```

The narrative `docs/GOVERNED-BEHAVIORS.md` (`BEH-1..7`) and the executable `spec/NNN-*` tree are two
planes of the same thing: each `BEH-N` family maps 1:1 to a `spec/NNN-*` directory, and every `REQ-NNN`
keeps its stable id across both. Where they disagree, the spec is intent and the code is the bug.

---

## 2. The spec directory shape (fixed 5-file contract)

Every `spec/NNN-slug/` directory has exactly this shape. `tools/specvalidate` fails CI if a file is
missing, mis-shaped, or untraceable.

```
spec/001-risk-classification/
  requirements.md                 # EARS requirements, one `- **REQ-NNN**` block each
  design.md                       # how the requirements are realized in Go/Temporal
  tasks.json                      # the machine-readable execution DAG (see §4)
  acceptance/
    <slug>.feature                # godog/Gherkin scenarios, each @REQ-NNN tagged
    _test_mapping.json            # scenario -> {req, status, test}; the honest coverage frontier
  security/
    threat-model.md               # per-feature STRIDE slice referencing INV-NN
```

**spec/001-risk-classification is the frozen exemplar.** Copy its shape for every new spec.

**Conventions** (validator-enforced): zero-padded `NNN-kebab` dir names; each spec owns a 100-block of
`REQ-NNN` ids (see `spec/00-INDEX.md`); status vocabularies are fixed per layer — specs
`Draft`→`Approved`→`Ratified`; tasks `pending`/`in_progress`/`completed`; test-mapping
`present`/`pending`/`retrospective_gap`.

---

## 3. Requirements: EARS, and nothing vague

Every requirement is one Markdown block:

```
- **REQ-006** — [F] spec/001 · [O] INV-06/INV-09.
  IF the model output is unparseable ..., the classifier SHALL fail closed to POLL_PAUSE ...
```

Rules the validator enforces:

- **EARS shape.** Each requirement is an obligation containing **SHALL** (or is explicitly `RETIRED`).
  Use the five EARS templates: *Ubiquitous* ("The system SHALL …"), *Event* ("WHEN <trigger> the
  system SHALL …"), *State* ("WHILE <state> …"), *Unwanted* ("IF <condition> THEN the system SHALL
  …"), *Optional* ("WHERE <feature> …"). One requirement = one obligation (split compound "and" rules).
- **Unique ids.** `REQ-NNN` is unique within its spec and stable forever (never renumber; retire in
  place, as REQ-005 shows).
- **No weasel words.** `TODO`, `TBD`, `might`, `should be`, `robust`, `scalable`, `simple`,
  `user-friendly`, `and/or`, `etc.`, `some`, `several`, … are banned — they defeat a machine-verifiable
  oracle. If you cannot phrase it as a testable SHALL, the requirement is not ready.
- **Provenance.** Tag each block `[F]`/`[R]`/`[O]` with source ids; the layering is auditable and must
  not be re-inverted.

---

## 4. tasks.json: the dispatchable execution DAG

`tasks.json` is what makes safe parallel agent fan-out possible. It is the *plan*, not prose.

```json
{
  "spec": "001-risk-classification",
  "tasks": [
    {
      "id": "T-001-2",
      "title": "core/risk.Classifier typed admission gate …",
      "files_owned": ["core/risk/classifier.go", "core/risk/input.go"],
      "deps": ["T-001-1"],
      "req_ids": ["REQ-001", "REQ-002", "REQ-003", "REQ-007"],
      "acceptance": { "feature": "risk-classification.feature", "scenarios": ["A low-risk reversible action is classified AUTO", "…"] },
      "budget": { "max_loc_delta": 400, "max_wall_clock_minutes": 90 },
      "status": "pending"
    }
  ]
}
```

Validator-enforced properties, and why each matters to an agent:

- **`files_owned`** — exact paths; **no two tasks may own the same file** (so parallel agents never
  collide on a write).
- **`deps`** — a task id DAG; the validator rejects cycles, so the ordering is always executable.
- **`req_ids`** — every task back-links to ≥1 real requirement (no orphan work; no untraced code).
- **`acceptance`** — the scenarios whose passing *is* this task's definition-of-done.
- **`budget`** — `max_loc_delta` + `max_wall_clock_minutes` bound a runaway agent (both must be > 0).

**To pick up work:** find a `pending` task whose `deps` are all `completed`, implement only its
`files_owned`, make its `acceptance` scenarios pass (remove their `@pending` tag and bind step
definitions to the real code), and set its `status` to `completed`.

---

## 5. Acceptance: the executable oracle (godog)

The `.feature` files are Gherkin behavior specs **and** runnable tests via
[godog](https://github.com/cucumber/godog). Each scenario is tagged `@REQ-NNN`. Scenarios whose
implementation does not exist yet are additionally tagged `@pending` and are **skipped by the runner**
(`Tags: "~@pending"`) — they are spec-ahead-of-code, tracked as `pending` in `_test_mapping.json`.

`_test_mapping.json` is the **honest coverage frontier**:

- `present` — a runnable step drives the real code and passes (a green oracle). Must name a `test`.
- `pending` — specified behavior whose implementation does not exist yet.
- `retrospective_gap` — shipped **without** a driving test. For governed safety-critical behavior this
  must be **zero** (INV-22); it exists so debt is *declared*, never silent.

Implementing a requirement means: write the real code, bind the scenario's steps to it, drop `@pending`,
and flip the mapping entry to `present`. Never mark something `present` that a fabricated or no-op step
"passes" — that is the exact test-theatre failure class TG is founded against (INV-22).

---

## 6. spec↔code lockstep: governed files can't drift

`spec/.lockstep.lock` binds each governed safety-critical Go file (the classifier, prediction gate,
verifier, suppression chain, actuation interceptor, ledger, schema/migrations) to its owning spec by a
comment-insensitive content hash (REQ-701..704, INV-22).

- `go run ./tools/specvalidate lockstep --check` (CI) fails if a governed file changed but its owning
  spec did not — i.e. code drifted from spec.
- `go run ./tools/specvalidate spec-index <path>` tells you which spec/REQ govern a file **before** you
  touch it.
- Re-stamping is an **authorized, audited** act: `go run ./tools/specvalidate lockstep --restamp` in the
  same MR that updates the owning spec. Never re-stamp to silence drift without a spec change.

---

## 7. The authoring flow (Spec-Kit-style phase gates)

For a new or changed governed behavior, in order — do not skip forward:

1. **Constitution check.** Confirm the change respects `docs/CONSTITUTION.md` (the mechanical safety
   core is never negotiable). A conflicting change needs a new ADR, not a spec edit.
2. **Specify.** Write/extend `requirements.md` (EARS, ids, provenance). Add `security/threat-model.md`.
3. **Clarify.** Resolve ambiguity *now*: no weasel words survive. If a decision is architectural, write
   an ADR and link it from `design.md`.
4. **Plan.** Write `design.md` — how the requirements map onto Go/Temporal/Postgres, and which existing
   safety primitives (`core/safety`, the ActionManifest, the prediction gate) they compose over.
5. **Tasks.** Write `tasks.json` — the file-owned, budgeted, dependency-ordered DAG.
6. **Analyze.** Run `go run ./tools/specvalidate` — cross-validates requirements ↔ tasks ↔ scenarios
   and rejects drift. Green before code.
7. **Implement.** Pick tasks off the DAG, write code + bind acceptance oracles, `make all` green.

`make all` runs `vet · lint · spec · test · build`. A change ships only when all are green **and** the
release bars in `docs/TESTING-AND-BENCHMARK.md` §5 hold.

---

## 8. Definition of done (per change)

- [ ] `make all` is green (`vet · lint · spec · test · build`).
- [ ] Every new/changed requirement is EARS-shaped, uniquely-ided, weasel-free, provenance-tagged.
- [ ] Every requirement has a `tasks.json` task and ≥1 acceptance scenario; the honest status is
      recorded in `_test_mapping.json` (no undeclared `retrospective_gap` on governed behavior).
- [ ] Governed source changes updated their owning spec and re-stamped `.lockstep.lock` in the same MR.
- [ ] The mechanical safety core, fail-closed gate, and never-auto floor remain untunable by the change
      (asserted under adversarial fuzz — TESTING-AND-BENCHMARK §3.1).

---

*The point of this lattice is not prose volume — it is that every requirement, task, and criterion is
uniquely ided, cross-linked, and backed by a runnable ground-truth check an agent cannot fake. That is
what turns "the agent thinks it's done" into "CI says it's done."*
