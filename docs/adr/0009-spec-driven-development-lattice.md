# ADR 0009 — Spec-driven development: an executable spec lattice with godog acceptance

## Status
Accepted.

## Context
TG is built largely by autonomous coding agents. Research into how the major players (Google,
Anthropic, GitHub, AWS) drive coding agents on large projects — and into whether an ISO-style
documentation/spec standard exists — produced two findings that shape this decision:

1. **There is no single ISO doc-schema standard for agent-driven development.** The real standards
   (ISO/IEC/IEEE 29148 requirements engineering, 42010 architecture description, arc42, C4) are prose /
   process standards for humans; none is machine-parseable or agent-executable. "Google's standard" is
   a *design-doc culture*, not a schema. AI governance standards (ISO/IEC 42001, NIST AI RMF) are a
   governance posture, not a build method.
2. **What actually drives agents, ranked by leverage, is convergent across vendors:** (i) an
   **execution-based definition-of-done** — "done" decided by running strong tests/evals in CI, never
   the agent asserting success (SWE-bench Verified is the template); (ii) **deterministic guardrails
   the agent cannot bypass** (required CI checks / hooks, not prose); (iii) a **spec-driven artifact
   chain with requirement↔task traceability + a stable constitution** (GitHub Spec Kit, AWS Kiro
   requirements/design/tasks); (iv) **requirements in a constrained grammar (EARS)** so each clause
   maps 1:1 to an acceptance-testable oracle; (v) **context engineering**; (vi) **independent
   adversarial verification** (the doer is not the checker).

A parallel study of a sibling project (Omoikane) confirmed the same shape in practice: a two-plane
lattice where human-narrative docs map 1:1 onto a mechanically-checkable spec tree
(constitution → ADRs → `spec/NNN/{requirements,design,tasks,acceptance,threat-model}` → a validator
that gates every push at N/N). TG already had the constitution, ADRs, EARS-*ish* prose
(`GOVERNED-BEHAVIORS.md`), and real code-level CI gates — but its requirements lived as prose, with no
executable acceptance criteria, no dependency-ordered task DAG, and no spec↔code lockstep. Those are
exactly the load-bearing (i)/(ii)/(iii) items above.

## Decision
Adopt **spec-driven development** as TG's build method, realized as an **executable spec lattice** under
`spec/`, enforced by a pure-stdlib Go validator, with **godog** as the executable-acceptance mechanism.

- **`spec/NNN-slug/` fixed 5-file shape** — `requirements.md` (EARS, unique `REQ-NNN`, provenance) ·
  `design.md` · `tasks.json` (files_owned + dependency DAG + req back-links + hard budgets) ·
  `acceptance/<slug>.feature` (godog/Gherkin, `@REQ-NNN` tagged) + `acceptance/_test_mapping.json`
  (present/pending/retrospective_gap) · `security/threat-model.md` (STRIDE). `spec/00-INDEX.md` maps
  them; `spec/001-risk-classification` is the frozen exemplar. The operating manual is
  `docs/SDD-WORKFLOW.md`.
- **`tools/specvalidate`** (pure-stdlib Go, runs in the existing golang CI image — no runtime
  dependency) gates every push: EARS shape, REQ uniqueness, weasel-word ban, tasks-DAG acyclicity, no
  file-ownership collision, requirement↔task↔scenario traceability, and — via
  `lockstep --check` — spec↔code content-hash drift (BEH-7 / REQ-701..704).
- **godog** (test-only dependency) makes acceptance criteria *executable*: `.feature` scenarios drive
  the real code path; `@pending` scenarios are skipped until implemented and tracked as declared debt.
  This is the objective, author-independent definition-of-done (finding (i)).
- **CODEOWNERS** puts the law files (constitution, invariants, `core/safety`, the spec lattice,
  AGENTS.md) under ownership so an agent cannot silently rewrite its own guardrails (finding (ii)).

The narrative `docs/GOVERNED-BEHAVIORS.md` (`BEH-1..7`) is retained as the human plane; each `BEH-N`
maps 1:1 to a `spec/NNN-*` dir and shares stable `REQ-NNN` ids. Specs 002–007 are migrated into the
lattice from their existing prose.

## Consequences
- "Done" is decided by CI running a test the agent did not author — the single biggest finish-vs-flail
  differentiator — and the lattice makes safe parallel agent fan-out possible (`files_owned` prevents
  collisions, the DAG orders work, budgets bound runaways).
- One new **test-only** dependency (godog + its transitive test deps). It never ships in the binary.
  The validator itself stays dependency-free.
- Governed code can no longer silently drift from its spec; a governed-file change without a spec
  update fails CI.
- Small ongoing authoring cost per governed change (write the spec block + oracle first). This is the
  intended cost: it front-loads the clarification that otherwise fails an agent mid-build.

## Alternatives
- **Prose specs + plain Go table tests only** — rejected: loses the traceable requirement↔oracle
  binding and the honest present/pending/gap coverage frontier; nothing stops spec drift.
- **A heavier spec framework / full ISO 29148 SRS documents** — rejected as audit theatre for an
  internal platform; borrow 29148's well-formed-requirement *quality bar* and realize it in EARS +
  godog instead.
- **Python validator (like the sibling project)** — rejected: would add a Python runtime to a Go CI
  image; a stdlib Go tool dogfoods the stack and adds no dependency.
- **godog deferred / bespoke Gherkin runner** — rejected: godog is the cross-vendor-standard executable
  acceptance mechanism (Cucumber for Go) and is test-only; reinventing it adds risk for no benefit.
