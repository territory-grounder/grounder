<!-- spec/007 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/007 — Content-aware spec↔code lockstep

**Owning behavior family:** BEH-7 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-22 (an executable, hash-verified governed-file set that excludes no
governed file), INV-19 (tamper-evident governance ledger for the audited re-stamp).
**Phase:** the lockstep gate lands in Phase 4 (release hardening); it composes over the Phase 0 spec
lattice (`tools/specvalidate`) and the governance ledger. **Status:** Approved.

The lockstep manifest binds each governed safety-critical Go file — the risk classifier, the prediction
gate, the mechanical verifier, the suppression chain, the actuation interceptor, the ledger, and the
schema/migration set — to its owning EARS spec by a comment-insensitive content hash. It is a product
safety control ([R] paradigm-rule 10), not build hygiene: it turns "governed code silently drifted from
its owning spec" into a continuous-integration failure rather than a latent hazard. This document is the
requirement source of record; the design is in `design.md`, the runnable acceptance oracles are in
`acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-701** — [F] spec/007 · [R] paradigm-rule 10.
  The system SHALL record a content hash for every governed safety-critical file, bound to its owning
  spec (BEH-1 through BEH-6) in the lockstep manifest.

- **REQ-702** — [F] spec/007 · [O] INV-22.
  WHEN a governed safety-critical file changes without its owning specification changing, the lockstep
  check SHALL report spec drift and fail continuous integration; the manifest SHALL NOT exclude any
  governed safety-critical file from the hash-verified set.

- **REQ-703** — [F] spec/007 · [R] paradigm-rule 4.
  WHEN the manifest is re-stamped, the system SHALL accept the recorded content hashes only through an
  authorized, RBAC-gated, audited approval action that is appended to the governance ledger, never
  through a host-local edit.
  *Wired 2026-08-25 (TG-536):* `specvalidate lockstep --restamp` now authorizes every hash move through
  `core/governance.AuthorizeRestamp` before the manifest is written — actor from `TG_RESTAMP_ACTOR`
  (compiled authority: `owner` / `autonomous-session`), record appended to the IN-REPO chained ledger
  `spec/.restamp-ledger.jsonl` (committed in the same MR as the moved hashes; a forked or hand-edited
  tail refuses, never restarts). A denial leaves neither ledger entries nor a rewritten manifest.
  The dev-box ledger is deliberately in-repo rather than the production DB chain: the record must be
  reviewable in the same diff that moves the hash, and a CLI must not need DB credentials to be governed.

- **REQ-704** — [F] spec/007.
  WHILE detecting drift, the lockstep check SHALL compare only the comment-insensitive semantic content
  of a governed file, so that a cosmetic comment or formatting-only edit cannot clear genuine drift.

## Persistence contract

The manifest is `spec/.lockstep.lock` — a JSON list of `{path, spec, sha256}` entries, one per governed
safety-critical file, each bound to an existing `spec/NNN-*` directory. Every accepted re-stamp
(REQ-703) is an immutable, RBAC-attributed record appended to the tamper-evident governance ledger
(INV-19), carrying the actor role, the changed paths, and the owning spec that was updated in the same
change. See [`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Coverage invariant

A standing check SHALL FAIL if any governed safety-critical file — the classifier, prediction gate,
verifier, suppression chain, actuation interceptor, ledger, or schema/migration set — is absent from the
manifest (INV-22: the predecessor excluded 11 of 12 governed files; the hash-verified set admits no
exclusion).

- **REQ-705** — [O] INV-22 · [R] law with no route to an oracle is law nobody runs.
  The lattice SHALL be checkable for RATIFICATION: for every spec, each declared requirement SHALL be
  referenced by at least one task OR carry a declared retrospective gap WITH a stated reason, and every
  acceptance scenario a task names SHALL exist in the feature file it names. A gap declared without a reason
  SHALL NOT satisfy this.
  *Rationale:* lockstep binds CODE to specs and the prose validator checks wording; neither notices a
  requirement no task references, nor a task pointing at a scenario that was renamed or never written. Both
  are silent, and both leave governance that READS as exercised. Measured when the checker first ran: 43 of
  370 requirements had no route to an oracle, and 5 tasks named acceptance scenarios their feature files did
  not contain — the task still read as covered and the godog suite still passed, because it never knew the
  scenario was expected. A DECLARED gap is a legitimate engineering position that can be reviewed; silence is
  not, which is why an empty reason is rejected rather than accepted as a formality.
  The check MAY be run against a pinned CEILING on the finding count instead of demanding zero, PROVIDED the
  ceiling can only ever be LOWERED. *Rationale:* a gate that fails on all pre-existing debt from the day it
  ships gets disabled within a week, at which point it protects nothing — the failure mode is a gate that
  exists and is switched off, which is worse than one that exists and is honest about its scope. A ceiling
  that cannot rise still reds the build the instant anything NEW goes unratified, which is the property that
  matters; it is the one form of tolerated failure that is not a fail-open, because it can never admit a new
  finding. The unceilinged form remains available and is what the phase exits on.

