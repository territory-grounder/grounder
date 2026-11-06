<!-- spec/027 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/027 — Design: auto-drafted world model

Design provenance: TG-227 design workflow `wf_3a385a3f-a58`, 2026-07-31. Re-verify cited line
numbers at implementation.

## Shape

Discovery is an extension of the EXISTING estate substrate, not a new system. The estate graph
already has: per-source-isolated Build (a failing source degrades loudly and contributes nothing —
`core/estate/build.go:8-56`), a fixed confidence table (pve 0.95 > netbox/librenms 0.90 > declared
0.85 > learned ≤0.75 hard-capped — `core/estate/estate.go:63-110`), MAX-ratchet with winning
provenance (`:187-216`), and closed vocabularies that loud-reject unknown entity/relation types
(`core/estate/declared.go:41-56`). The ONLY greenfield discovery code is two EdgeSources — systemd
units and docker containers — compiled-in per INV-17 (`modules/registry.go:19-40`) and
transport-injectable like `netbox.go` so oracles drive fakes.

The manifest is the reviewable projection of discovery: `manifest_entry` (migration 0047), a
latest-wins operational row whose history lives in the governance ledger (the policy_graduation
split precedent, `policy_graduation_store.go:13-96`). All transitions flow through one audited
Transition func cloned from `core/skillstore/transition.go:15-104` — allowedTransitions map,
mandatory rationale, ledger-append-BEFORE-row, no resurrection. Zero state (`draft`) renders
nothing actionable.

## Review, adopt, materialize

The console `manifest` view renders draft-vs-approved DIFFS with skDiffPanel (no new diff engine)
and authored-vs-derived provenance labels. Adopt/reject/retire are closed-verb POSTs through the
hardened write lane (AuthSession step-up, mandatory rationale, worker Temporal workflow, vote.go
hardening kit). Materialization: approved rows become the allowlist SOURCE through injected
AllowlistProvider seams at the three constructor sites (`cmd/worker/main.go:3830-3846`,
`:1515-1516`, `:3912`) — the leaf default-deny gates are byte-untouched, so the enforcement point
never moves; only where the operator's grant is AUTHORED moves (from hand-typed env to reviewed
adoption). Composition with boot-frozen env vars is UNION with provenance labels (open question 2 —
recommendation adopted v1; DB-replaces-env deferred, silent-narrowing hazard).

`tools/guardallow` accepts adopted entries as subject input so host-guard authorization flows from
the same adopt click; the host guard remains final authority (exit 42 DENY).

## Drift

A singleton Temporal cron in the finalizer shape re-discovers, diffs, marks disappeared entries
`stale` (never auto-retires — the safe direction), drafts new/changed entries, appends
`manifest:drift`, and surfaces one-click adopts. A failing source contributes nothing, loudly.

## Oracles and RED controls

- O-2701 discovery drafts rows with correct provenance+confidence; unknown entity_type from a
  corrupted source is loud-rejected. RED: widen the vocabulary check → red.
- O-2702 adopt: ledger row precedes row update; reject with empty rationale → 4xx. RED: skip the
  ledger append → red.
- O-2703 materialization end-to-end (the flagship oracle): an ssh effect for a non-adopted unit is
  refused at the leaf; after one-click adopt the SAME effect passes the leaf gate (mode still gating
  above it). RED: stub the AllowlistProvider to return the universe → the refusal assertion goes red.
- O-2704 drift: remove a unit + add a container in the fake source → disappeared becomes `stale`
  (NOT retired), new becomes `draft` + `manifest:drift` ledger row + console diff visible. RED:
  break the diff comparator to report no-change → red.
- O-2705 failing source: transport error → loud per-source error, zero changes to that source's
  entries, other sources proceed. RED: swallow the source error → red.
- O-2706 console e2e: 3-state rendering, diff panel, data-keyed adopt honoring server
  caller_can_act=false. RED per approvals-reachable.mjs — drive the populated branch first.

## Open questions

OQ-2 (allowlist composition UNION vs replace — UNION adopted for v1) lives in ADR-0016
§Open-questions with the full rationale.
