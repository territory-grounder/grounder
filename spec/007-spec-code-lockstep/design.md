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

## Composition over existing primitives

The gate composes over the Phase 0 spec-lattice validator (the same `tools/specvalidate` binary that
enforces EARS shape, REQ uniqueness, the weasel-word ban, the tasks DAG, and requirement↔task↔scenario
traceability) and over the governance ledger (spec/006 / INV-19) that carries the re-stamp audit record.
The validator's `REQ-NNN` id grammar accepts three or four digits (`REQ-\d{3,4}[a-z]?`): the ten
three-digit 100-blocks (`0xx`..`9xx`) are assigned to spec/001–010, so spec/011 onward own four-digit
blocks (`REQ-10xx` for spec/011). The lockstep binding is spec-id-width agnostic.
It writes no operational state and reads no operational data; the manifest holds file paths and one-way hashes
only.

## Out of scope

The mechanics of the governance ledger and its hash-chaining belong to spec/006. The acceptance-oracle
binding of the specs this gate governs (spec/001–006) belongs to those specs. This spec owns the manifest,
the drift decision procedure, the coverage invariant, and the authorized re-stamp.
