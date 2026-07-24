<!-- spec/014 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/014 — Versioned skill store + graduation flywheel

**Owning behavior family:** the versioned, console-editable skill store; the candidate → trial →
graduation flywheel; the compose-from-store seed path (`core/skillstore`, `agent/skills/store.go`,
`temporal/runner` ComposeSeed, `frontend/src/panels/skills/`).
**Constitution / invariants:** INV-08, INV-11, INV-19, INV-22.
**Phase:** Phase 1 (read-only competence plane; no estate mutation anywhere in this family).
**Status:** Approved. **ADR:** 0012 (skills: adopt the format, re-author the content).

The compiled `agent/skills` registry made competence explicit and testable, but editing it means a code
deploy, and improving it means a human noticing a weakness. This spec moves skills to a versioned,
Postgres-backed store with a console surface and an automated improve → evaluate → graduate loop, porting
the predecessor's prompt-patch-trial machinery (deterministic arm assignment, judged scoring, Welch
finalizer) while designing out its five verified production failures: online-only trials that never
completed (traffic starvation), a 2.5-month dark pipeline from unnormalized assignment keys, a silently
disabled finalizer (scheduler split-brain), out-of-enum statuses written by raw SQL, and a JSON-file/DB
split-brain with an unexercised rollback. Prompt content changes agent COMPETENCE only — enforcement
(bands, the never-auto floor, the interceptor) is machine-checked elsewhere and is not reachable from
this family.

## Requirements

- **REQ-1301** — [F] prompt-patch trial state machine · [O] INV-19.
  The store SHALL persist each skill version as a row carrying body, declarative applies-when predicate,
  semver, status, author, source, content hash, and a rationale; a status SHALL be one of
  draft / trial / production / retired / rejected, enforced by a database CHECK constraint; every status
  transition SHALL pass through the single `skillstore.Transition` function, which SHALL record a
  non-empty rationale and append a governance-ledger entry for the transition.

- **REQ-1302** — [F] patch supersede logic, made structural.
  The store SHALL enforce at most one production version per skill name through a partial unique index;
  WHEN a version graduates to production, the prior production version SHALL be transitioned to retired
  in the same transaction.

- **REQ-1303** — [O] INV-08 · [F] deterministic seed composition.
  Seed composition SHALL read one production-set snapshot per session and SHALL select applicable skills
  by evaluating each version's declarative applies-when predicate (phase and execution-class membership)
  as a pure function of typed signals; the predicate vocabulary SHALL be validated at write time against
  the known phases and execution classes; no model-produced token SHALL participate in selection,
  ordering, or composition; the composed skill list (name, version id, content hash, trial arm) SHALL be
  recorded per session so the seed is reconstructable.

- **REQ-1304** — [O] fail-safe totality.
  IF the store is unreachable, a row fails validation, or the snapshot is empty, THEN seed composition
  SHALL fall back to the compiled registry in full and SHALL record the fallback reason in the
  per-session skill record; the compiled registry SHALL also be imported at boot as production rows
  (idempotently, keyed by name + version) so the console renders the real library from first start.

- **REQ-1305** — [O] defense in depth for the floor.
  A skill marked pinned SHALL never be overridden by a store row: the write path SHALL reject a draft
  targeting a pinned skill, and composition SHALL use the compiled body for pinned skills regardless of
  store content.

- **REQ-1306** — [F] deterministic A/B assignment (ported verbatim) · [O] INV-08.
  WHEN a session composes a skill under an active trial, the arm SHALL be chosen as
  `blake2b(external_ref | trial_id) mod (candidates + 1)` with the final bucket meaning control; the
  assignment SHALL be persisted idempotently and re-read after insert so a later hash change can never
  flip a recorded arm; an empty or whitespace external_ref SHALL be rejected before hashing and counted
  in a malformed-reference metric; at most one active trial per skill SHALL be enforced by a partial
  unique index.

- **REQ-1307** — [R] the offline admission gate (the starvation fix).
  A draft SHALL enter an online trial only after an offline evaluation run in which the regression set
  does not regress and the trial's target dimension improves on the discovery set; the sealed holdout
  set SHALL NOT be read by the admission run; the offline scores SHALL be stored on the version row.

- **REQ-1308** — [F] Welch finalizer (ported) · [O] INV-19.
  A daily finalizer SHALL first transition expired active trials to aborted-timeout, then for each
  remaining active trial with every arm at or above the minimum sample count SHALL graduate the best
  candidate only if its mean exceeds the concurrent control mean by at least the configured lift AND a
  one-sided Welch test (Student-t tail, Welch–Satterthwaite degrees of freedom) rejects at the configured
  threshold AND the safety-analog dimension of the candidate arm is not below the control arm; losing
  candidates SHALL be transitioned to rejected; every per-arm outcome SHALL be ledger-recorded.

- **REQ-1309** — [R] traffic-aware start refusal.
  WHEN a trial is requested, the store SHALL project time-to-completion from the trailing judged-session
  rate; IF the projection exceeds the trial's end date, THEN the trial SHALL be refused with a stored
  reason instead of started.

- **REQ-1310** — [R] graduation rollback breaker.
  WHEN a version graduates, a named breaker SHALL open a bounded regression watch in which each judged
  session that loaded the version scores success or failure against the trial's control mean; IF the
  breaker trips, THEN the version SHALL be auto-transitioned to retired, the prior production version
  SHALL be restored, and the demotion SHALL be ledger-recorded and escalated to the human queue.

- **REQ-1311** — [O] every route authenticated (spec/006).
  Read routes for the skill library, version history, and trial state SHALL require read-only
  authentication; write routes (create draft, admit to trial, promote, retire, abort) SHALL require an
  operator session principal; a machine principal SHALL have no write route.

- **REQ-1312** — [F] GEPA generate-only invariant · [O] INV-08.
  The candidate generator SHALL only create draft rows (from eval-failure and resolved-incident
  signals), each carrying its generation rationale and source; a generated draft SHALL have no effect on
  composition until it passes the REQ-1307 gate and a REQ-1308 graduation; no generator output SHALL
  become control flow.

- **REQ-1313** — [R] visibility with rationale.
  The read surface SHALL expose, for every skill: its version history with per-version rationale log,
  author, source, offline and online eval scores, ledger sequence references, and diffable bodies; for
  every trial: per-arm sample counts, means, the test statistic, projected completion, and assignment
  staleness; and for every composed session: the skill/version/arm list with any fallback reason.

- **REQ-1314** — [R] the flywheel creation-half cron (generate -> offline-admit -> trial-start) · [O] INV-08.
  A daily generator cron SHALL, for every non-pinned production skill, read the trailing-window rolling
  judged mean per dimension; WHEN a dimension's mean falls below the configured threshold with at least
  the configured sample count, THEN it SHALL fire candidate generation for that dimension, and a skill
  that already carries an open flywheel candidate SHALL NOT be regenerated. HOWEVER, the targeted
  dimension SHALL be the WORST regressed dimension whose OBSERVED scored-sample rate can still fill a
  REQ-1309 trial before its window closes (TG-67): a dimension only PROPOSING sessions score
  (falsifiable_prediction, made proposer-only by seq C) can floor the judged mean yet be scored by too few
  sessions on mostly-stand-down traffic to ever reach a trial's per-arm minimum, so arming it burns the
  skill's single trial slot on a trial that can only timeout-abort — the predecessor's zero-completed-trials
  starvation, reintroduced through a proposer-only trigger dimension. WHEN the worst regressed dimension
  cannot fill, the cron SHALL target the worst regressed dimension that CAN, and WHEN none can it SHALL
  defer the skill to a later run (never arming an unfillable trial). The fill projection is a pure function
  of the measured scored-sample counts; no model token participates (INV-08). Each open flywheel draft
  SHALL be run through the REQ-1307 offline gate and, on a pass, transitioned draft->trial, and on a
  refusal left a draft with the refusal stored. For a skill without an active trial, the offline-passed
  candidates SHALL be started as one REQ-1309 traffic-aware trial. The cron SHALL only create draft rows
  and drive the audited state machine; no step SHALL enable or perform estate mutation.

## Out of scope

Judge scoring itself (the eval harness and its dimensions) is task #26 / `eval/` infrastructure. The
browser-session principal and its future admin tier are spec/006 / #27. Skill content (what the bodies
say) is governed by ADR-0012, not this spec. Estate mutation is untouched by this family: nothing in
this spec grants or alters actuation capability (spec/013 owns the interceptor).
