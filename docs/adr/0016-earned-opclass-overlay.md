# ADR-0016 — The earned op-class overlay: two tamper domains, an AUTO_NOTICE ceiling, and embed-export as the road to AUTO

- **Status:** Accepted (owner approval 2026-07-31 — the owner delegated review-and-merge of the
  Stage-0 Draft MR ("you review it and merge it"), which is the approval act this ADR was drafted
  to await; the approving commit carries `Law-Change-Approved-By` per docs/CONSTITUTION.md §6.
  Sequencing note for the record: the delegated adversarial review — workflow `wf_495c3a80-aa8`,
  4 lenses + per-finding adversarial verification; 20 raw findings, 18 confirmed and dispositioned
  in the follow-up commit on this branch, 2 refuted — COMPLETED BEFORE MERGE, so approval-follows-
  review holds even though the Accepted flip was pushed while the review was in flight)
- **Date:** 2026-07-31
- **Relates to:** spec/026, spec/027, spec/028 (epic TG-227); ADR-0013 (mode is the chokepoint);
  spec/007 (lockstep); spec/013 (actuation interceptor); spec/015 (policy engine)
- **Provenance:** TG-227 design workflow `wf_3a385a3f-a58` — six ground maps, three independent
  architectures (risk-first / operator-first / reuse-first), one synthesis. Owner problem statement:
  the predecessor EARNED autonomy under supervision; TG demands hand-authored catalogs. The port
  mandate ("clone it and make it better") requires porting the earning, not the authoring.

## Context

TG's op-class registry is `go:embed`-compiled and lockstep-hashed: capability admission is a code
release, reviewed and hash-bound (spec/007). That gives the strongest possible tamper story — and a
zero-config story of "the administrator authors everything before TG can act." The epic requires
runtime-admitted op-classes (operator-ratified from evidence dossiers) without weakening what makes
the embedded registry trustworthy, and without ever letting model text become an executed vector.
`opschema.go:277-286` already anticipates operator-authored argv TEMPLATES as a legitimate actuation source (the T1 template branch — "until a template branch ships, a missing builder is unactuatable"); the composed embedded+overlay LOOKUP is introduced by this ADR. `docs/ARCHITECTURE.md:107-162` already
refuses the "model writes its own tools" lane (T3). This ADR settles how those two facts coexist.

## Decision

1. **Two tamper domains, explicitly unequal.**
   The EMBEDDED registry stays `go:embed` + lockstep-hashed — reviewed code, the strongest domain.
   The OVERLAY (`opclass_ratified`, spec/028 REQ-2803) is append-only, UPDATE/DELETE-revoked, and
   every row's `entry_hash` is bound into its `opclass:ratify` GovDecision on the ONE hash-chained
   ledger; rows are re-verified at every load and a mismatch DROPS the row loudly — fail closed to
   FEWER capabilities. The two domains compose at exactly one seam (`opschema.Lookup/Specs/Catalog`
   consult embedded THEN overlay; embedded always wins a slug collision), so spec/007's scope
   statement stays honest: the lockstep hash governs the embedded registry; the ledger hash governs
   the overlay.

2. **The overlay's autonomy ceiling is AUTO_NOTICE.**
   An overlay class may climb propose-only → approve → AUTO_NOTICE on verified evidence, and no
   further. The silent rung (AUTO) is reserved for classes present in the embedded registry — i.e.
   **the last rung requires a code release**. This dissolves the tamper-domain-vs-runtime-mutability
   tension legibly instead of papering over it: the rung where no human watches always lives in the
   domain with the strongest guarantees, and the promotion path is a one-click **embed-export** that
   generates the `opschema.json` snippet + spec/013 restamp checklist as a reviewable MR (which also
   discharges per-class opcover in CI).

3. **Ratification is operator authorship, never model admission.**
   The ratify form is structurally empty (no prefill code path exists); model verb/rationale/undo
   text renders only as screened, visually-separated read-only exhibits. Admission runs an
   error-returning refactor of the registry's own validation (`mustBuildRegistry`'s panic checks
   become `opschema.ValidateSpec` — a live worker cannot panic), plus overlay-never-shadows-embedded,
   tier-vs-destructiveness contradiction refusal, and a laundering tripwire that refuses any template
   element byte-matching an occurrence's model text. T3 stays refused: TG never writes its own tools;
   it writes evidence that a human converts.

4. **Rung 0 is registry absence.**
   "Propose-only" is not a stored level — it is the absence of the class from the composed registry,
   enforced by the existing chain (nil sealedArgv → empty-argv leaf refusal → never-auto floor →
   mode chokepoint). Zero new safety code; the Level zero-value law and old-worker forward-safety
   (`parseLevel` unknown→approve) hold untouched.

## Consequences

- Day-zero TG (empty catalog) is a full-capability shadow adviser and can execute nothing — the
  predecessor's shadow posture with zero configuration (spec/026).
- The administrator's job collapses to reviewing dossiers and clicking ratify/adopt/veto; authoring
  survives only inside the embed-export MR, where code review belongs anyway.
- Autonomy is earned per op-class, finer-grained than the predecessor's four coarse bands, and every
  rung transition is on one auditable chain with auto-demote (deviation ⇒ full drop to approve).
- Cost: two registry sources to reason about. Contained by the single compose seam, embedded-wins
  collision law, and hash-verified overlay loads.

## Benchmark interaction (owner-ratified 2026-07-31)

Op-class correctness and band correctness are SEPARATE axes in every future head-to-head: the
per-fault-type `accept` list in `core/diagcorpus/expectations.json` names the correct op-class(es)
(a list, because more than one answer can be right); band
correctness (including actor-evidence floors, spec/026 REQ-2609..2611) is scored via
`appropriate_band`. Provenance: live fault 1406 — naming the fix and being authorized to apply it
are different questions, and conflating them made a correct restraint score as a miss.

## Open questions (OWNER-DECISION-PENDING)

Recorded verbatim-condensed from the design synthesis; each carries its recommendation. Deciding
any of these amends this ADR or its specs through the normal governed path.

1. **SeedDefaults vs day-zero empty catalog** (`core/policy/defaults.go:36-105` seeds 4 classes to
   LevelAuto on fresh deploys — in tension with day-zero-empty). REC: make seeding an explicit
   opt-in deployment profile through the audited config-write lane; fresh TG-227-posture deploys
   default empty; existing deploys keep their earned/seeded state. (spec/026; core/policy ⇒ trailer.)
2. **Allowlist composition — DB vs boot-frozen env.** REC: UNION with provenance labels on the
   console (both are operator acts); deprecate env as a v2 migration. DB-replaces-env-when-nonempty
   creates a silent-narrowing hazard on first adopt. (spec/027 REQ-2704 ships UNION.)
3. **Deviation demote severity: full drop vs drop-one-rung.** REC: full drop in v1 — matches the
   shipped applyOutcome semantic and is the simplest to explain ("one deviation drops it to
   Asks-first"); revisit with data in v2.
4. **AUTO for overlay-only classes without a code release.** REC: no in v1 (the ceiling is the
   design's central safety trade); v2 at most behind a double key — operator arming AND a fresh
   (≤14d) spec/025 scorecard with staleness failing closed.
5. **Ladder key: slug vs family** (`opschema.go:134-139` names family as the intended ladder unit).
   REC: slug-keyed v1 with family as console grouping; family-keyed graduation re-attributes earned
   autonomy across verbs without per-verb evidence — it deserves its own v2 evidence argument and
   migration.
6. **Dismiss vs permanent reject for candidates.** REC: v1 ships only dismiss with a 30d TTL
   (read-path expiry, the proven DemotionRow shape); permanent reject with lineage is v2 if
   operators ask.
7. **Judge surface enrichment (undo_sketch to the judged card).** REC: no in v1 — the judge prompt
   is byte-pinned by a golden; a re-pin is a scoped deliberate change and P1's value does not
   depend on it.
8. **Board scheduling of Stage 1 vs measurement campaigns.** RESOLVED BY EVENTS: the Phase-D
   verdict landed 2026-07-31 (campaign #1 record in `tools/shadowbench/confirmatory/`), so Stage 1
   no longer risks confounding a live head-to-head; it is queued on the BOARD after Stage 0 review.
