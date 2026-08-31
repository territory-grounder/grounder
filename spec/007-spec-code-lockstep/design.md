<!-- spec/007 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/007 — Design: content-aware spec↔code lockstep

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent.

## Component

The lockstep gate is `tools/specvalidate` — a pure-stdlib Go command that runs in the same golang CI
image as the build, adding no runtime dependency. It exposes the subcommands the lattice needs:

```
go run ./tools/specvalidate lockstep --check     # recompute governed-file hashes, fail on drift
go run ./tools/specvalidate lockstep --restamp   # rewrite the manifest (authorized re-stamp)
go run ./tools/specvalidate spec-index <path>    # print which spec/REQ own a source file
```

The manifest `spec/.lockstep.lock` is the persisted binding: a JSON `{note, files[]}` document where
each entry is `{path, spec, sha256}`. `path` names a governed safety-critical file, `spec` names the
owning `spec/NNN-*` directory, and `sha256` is the stamped hash. `lockstep --check` loads the manifest,
recomputes each hash, and fails on the first mismatch — realizing REQ-701 (record) and REQ-702 (drift
fails CI). The command also asserts that each entry's owning spec directory exists, so a binding to a
deleted spec is a hard failure.

## Comment-insensitive semantic hash (REQ-704)

`hashSemantic(path, src)` is the decision procedure for "did the governed content change". For a `.go`
file it first runs `stripGoComments`, which is string- and rune-literal aware: it removes `//` line and
`/* */` block comments, preserves the text inside string, raw-string, and rune literals verbatim, then
collapses whitespace runs. The SHA-256 is taken over that normalized byte stream. A cosmetic comment
edit or a `gofmt`-only reflow therefore yields the identical hash and does not read as drift, while any
change to executable tokens changes the hash and fails the check. Non-Go governed files (the SQL
schema/migration set) are hashed byte-for-byte, because their whole content is semantic.

## Coverage: no governed file excluded (REQ-702 / INV-22)

The predecessor's manifest excluded 11 of its 12 governed files, so its hash gate certified almost
nothing. TG inverts that: the manifest is the closed set of governed safety-critical files, and the
coverage invariant fails if a governed file — classifier, prediction gate, verifier, suppression chain,
actuation interceptor, ledger, or schema/migration set — is missing from it. As specs 002–006 land,
their governed Go files (`core/risk`, `core/manifest`, `core/verify`, `core/suppression`,
`adapters/actuation`, `core/ledger`, `core/db`) join the manifest bound to their owning spec.

## Authorized, audited re-stamp (REQ-703)

`lockstep --restamp` recomputes and rewrites the stamped hashes. On its own that is a mechanical write;
the governed act is the authorization around it. A re-stamp is accepted only inside an authorized,
RBAC-gated approval: it runs in a protected merge request whose approver holds the `spec-owner` role,
in the same change that updates the owning spec, and the acceptance is recorded as an immutable entry on
the tamper-evident governance ledger (INV-19) carrying the actor role, the changed paths, and the owning
spec. A re-stamp that lacks that authorization is rejected — there is no host-local edit path that can
silence drift, because a manifest edited outside the approval flow still fails `lockstep --check` in CI
and produces no ledger record. This authorization binding is the greenfield reframe of the predecessor's
host-local "operator re-stamp" ([R] paradigm-rule 4).

**The tool enforces the same-diff half mechanically (2026-07-30).** For 15 days the "in the same change
that updates the owning spec" clause was honor-system: `--restamp` silently overwrote every mismatched
hash, so a session could (and 431 times did) re-stamp as a reflex without touching any spec — the gate
was a per-change tax that protected nothing. `--restamp` now derives the set of spec directories changed
in the current diff (merge-base with `origin/main` when available, else `HEAD`, unioned with staged and
uncommitted changes) and **refuses** to move a hash whose owning spec is not in that set, exiting
non-zero and naming each refused file. The escape hatch `--allow-unchanged-spec` exists for exceptional
cases and is deliberately loud: it must appear in the command line, where the MR and shell history
record it. When git itself is unavailable the guard fails closed (no spec counts as changed).

**Governed-set scope (narrowed 2026-07-30, ADR-0013).** The manifest governs the safety spine, the
effect channel, the measurement plane, and this gate itself — files where drift from spec is dangerous
and change is rare — and deliberately not high-churn orchestration/plumbing, where binding produced
reflex re-stamps (24.8% of all commits) and side-effect spec edits rather than protection.

## Composition over existing primitives

The gate composes over the Phase 0 spec-lattice validator (the same `tools/specvalidate` binary that
enforces EARS shape, REQ uniqueness, the weasel-word ban, the tasks DAG, and requirement↔task↔scenario
traceability) and over the governance ledger (spec/006 / INV-19) that carries the re-stamp audit record.
The validator's `REQ-NNN` id grammar accepts three or four digits (`REQ-\d{3,4}[a-z]?`): the ten
three-digit 100-blocks (`0xx`..`9xx`) are assigned to spec/001–010, so spec/011 onward own four-digit
blocks (`REQ-10xx` for spec/011). The lockstep binding is spec-id-width agnostic.
It writes no operational state and reads no operational data; the manifest holds file paths and one-way hashes
only.

## Amendment 2026-08-07 — the traceability ratchet (TG-416)

The same `tools/specvalidate` binary that carries this gate also runs the default spec-lattice pass,
whose `files_owned` traceability check emits a WARN when a task marked `completed` owns a path that does
not exist. A warning made the debt measurable but never enforceable: 38 such entries existed on
2026-08-07 (spec/010's `frontend/**`, orphaned when the console shipped as `deploy/console/v2`, plus a
handful in 020/023/028), and nothing stopped a 39th — so the spec-task completion count could inflate
indefinitely against files that verify nothing. The validator now RATCHETS that count against a pinned
ceiling (`phantomOwnedCeiling`): a total ABOVE it fails the lattice, so a newly-completed task naming a
file that is not there goes red on the commit that adds it. The ceiling is lowered as tasks are repointed
at the file that actually exists (content-verified — a name-match is a trap) or have `completed` dropped,
and is never raised — a ratchet, not a waiver, the same discipline as `ratify --max-findings`. This binds
the completion metric to the filesystem: `completed` may no longer outrun the code it claims.

**Cleared to 0 on 2026-08-08 (TG-416) — two honest causes, treated differently.** A phantom path means either
the code moved (repoint) or the work was never delivered (drop `completed`); conflating them is the trap, so
each entry was adjudicated by CONTENT, not name. The 020/023/028 Go paths were REPOINTED at the differently-named
file that carries the concept — content-verified per entry (the confidence/attribution persistence in
`triage_judgment.go`, the runner wiring in `activities.go`, the security-escalation routing in
`risk/classifier.go`, the per-rung ladder truth-table in `ladderRungFor`); a name-match would have been a trap
(T-023-6 named migration 0034, which is `ingest_transition` — the real one is 0035). But the spec/010 console
tasks were NOT delivered as specified: ADR-0015 removed the React frontend the tasks were written against, and
the served `deploy/console/v2` is a partial preview — per this project's own acceptance audit
(`spec/010-ux-console/acceptance/_test_mapping.json`) every T-010-1..8 defining REQ is `pending`: no oracle, or
no feature at all (REQ-607 manifest replay, REQ-610/611 kill-switch policy-API writes). Repointing those
`completed` tasks at a real console file would have traded a visible WARN for a SILENT false green — the exact
failure this amendment warns against — so they were flipped to `pending` (files_owned left tracing the real
partial artifact for reference). `phantomOwnedCeiling` is now `0`: zero-tolerance, any future `completed` task
naming an absent file fails on its commit.

## Amendment 2026-08-10 — the mechanized lattice tally (TG-416 successor)

The same `tools/specvalidate` binary gains a `tally` subcommand (`tools/specvalidate/tally`). The
"Honest lattice state" block in `spec/00-INDEX.md` was HAND-WRITTEN, dated 2026-07-31, and wrong on
every count ten days later: it said "No spec has reached `Ratified`" under a table marking 18
`Ratified`, and carried task totals (250/61/9) the tree had left behind (275/48/8). Nothing failed,
because nothing compared the prose to the tree — the same claim-outruns-evidence shape as the
phantom-owned ratchet above, one level up. The block is now GENERATED between exact HTML-comment
markers: `tally --write` recomputes it from the tree (spec-dir count, task totals by closed-vocabulary
status, the index's own Status column, acceptance-scenario totals, and per-spec pending concentration —
deterministic, no timestamp), and `tally --check` byte-compares and fails on any hand edit or stale
number, with distinct failures for a missing marker pair and for a blind walk (0 `tasks.json` found
refuses to certify rather than certifying zeros). Whether a `Ratified` row is EARNED stays with
`ratify --check`; tally only counts, so the two verdicts cannot blur.

## Out of scope

The mechanics of the governance ledger and its hash-chaining belong to spec/006. The acceptance-oracle
binding of the specs this gate governs (spec/001–006) belongs to those specs. This spec owns the manifest,
the drift decision procedure, the coverage invariant, and the authorized re-stamp.
