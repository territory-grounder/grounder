<!-- spec/028 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/028 — Earned op-class catalog: candidates, dossiers, the widened ladder

**Owning behavior family:** BEH-14 (narrative row lands via a separate law MR, BEH-10/11 precedent).
**Constitution / invariants:** INV-08 (no model token as control flow), INV-10 (one writer per
meaning), INV-14 (retention), INV-19 (one ledger), INV-22 (no undeclared test-gap).
**Phase:** P2–P3.
**Status:** Draft.
**Epic:** TG-227 / TG-230 (plane 3). Design provenance: workflow wf_3a385a3f-a58, 2026-07-31.
Constitutional decisions in [ADR-0016](../../docs/adr/0016-earned-opclass-overlay.md).

Recurring free-form proposals (spec/026) cluster into op-class CANDIDATES with evidence dossiers;
operators ratify by AUTHORING a template into an empty form; ratified classes climb the EXISTING
graduation ladder widened by one rung (AUTO_NOTICE), with auto-demote; every transition is
hash-chain ledgered. Rung 0 ("proposes only") is registry ABSENCE — fail-closed by construction,
zero new code. The overlay's autonomy CEILING is AUTO_NOTICE; full AUTO requires promoting the
class into the embedded lockstep-hashed `opschema.json` via a generated embed-export MR — the
silent rung always lives in the strongest tamper domain.

## Requirements — data model

- **REQ-2801** — [F] candidate store · migration 0048 · INV-14.
  The system SHALL persist candidate lifecycle state in `opclass_candidate` keyed by
  `candidate_key = SHA-256("v1|" + norm(op_class) + "|" + norm(op) + "|" + sorted-param-names)`
  using opschema's INV-08 normalization (`opschema.go:10-17`), with status CHECK IN
  `observing|candidate|ratify_ready|ratified|dismissed|expired` DEFAULT `observing`,
  `auto_barred BOOLEAN NOT NULL DEFAULT TRUE` until the server-derived safety screen stamps it
  (`core/safety/safety.go:60-109,298`), a dossier snapshot with `dossier_hash` bound into the
  candidacy GovDecision, a 30-day dismiss TTL with read-path expiry
  (`core/governance/demote.go:12-17,92-127` pattern), retention on every row, and one live row per
  key via a partial unique index.

- **REQ-2802** — [F] occurrence journal · append-only · the 4x-credit lesson.
  The system SHALL persist proposal occurrences in `opclass_candidate_occurrence` with
  PRIMARY KEY (candidate_key, external_ref) so one incident counts once regardless of re-proposals,
  append-only with UPDATE and DELETE revoked from the runtime role (migration 0015/0042/0043
  precedent), carrying screened model text, binder-verified evidence ids, band, outcome, and
  retention.

- **REQ-2803** — [F] registry overlay · migration 0049 · separate tamper domain.
  The system SHALL persist ratified classes in `opclass_ratified` as an APPEND-ONLY overlay —
  PK (op_class, seq), the exact opschema entry shape in `spec` jsonb,
  `entry_hash = SHA-256(canonical spec)` embedded in the `opclass:ratify` GovDecision so row content
  is chain-covered, `promote_threshold INT NOT NULL CHECK (promote_threshold >= 5)` set from tier at
  ratify (tier table: low-reversible ⇒ 5, medium ⇒ 10) and only ever raising the compile-time
  default (`core/policy/graduation.go:40`), revocation expressed as a NEW row with
  `revoked = true`, one live row per op_class via a partial unique index, and UPDATE/DELETE revoked.

- **REQ-2804** — [F] ladder widening · migration 0050 · forward-safe.
  The graduation store SHALL widen `policy_graduation.level` to
  CHECK IN (`approve`,`auto_notice`,`auto`) (the 0040 widening precedent; old workers parse unknown
  levels as approve, `core/db/policy_graduation_store.go:70-79`), SHALL add
  `notice_run_count INT NOT NULL DEFAULT 0`, SHALL add last_outcome `ratified` (provenance honesty —
  ratification is not a run), and SHALL add a `graduation_credit` table UNIQUE(op_class,
  external_ref) consulted before any streak increment so credit is exactly-once by key.

## Requirements — ladder state machine

- **REQ-2805** — [F] rung 0 is absence.
  An op-class absent from the composed registry SHALL have no stored graduation level, SHALL seal to
  nil argv (`temporal/runner/activities.go:1012-1022`), SHALL be refused at every effect leaf on
  empty argv, and SHALL be floored never-auto (`core/policy/graduation.go:470-489`) — enforced
  entirely by existing machinery with a defense-in-depth oracle and zero new code.

- **REQ-2806** — [F] ratified enters at approve and never self-promotes from ratification.
  WHEN a candidate is ratified, its class SHALL enter the ladder at LevelApprove with
  last_outcome `ratified` (the OutcomeSeeded analog, `graduation.go:97-104`), and
  UngraduatedClass SHALL open the op-class-not-graduated poll for every action
  (`core/risk/classifier.go:95-104`) so a reachable approval path always exists.

- **REQ-2807** — [R] promotion to AUTO_NOTICE (terminus-only, exactly-once).
  WHEN a ratified class accumulates promote_threshold consecutive terminus-confirmed verified-clean
  runs (Executed AND HasTerminalResult AND NOT PollUnanswered AND verdict match AND ConfirmedClear,
  `temporal/runner/reconcile.go:44-51`), credited ONLY by the terminus GraduationActivity
  (`reconcile.go:14-38`) gated by `graduation_credit`, with AutoEligible(tier) consulted BEFORE the
  earned level (`graduation.go:201-212,455-468`) and promotion durable-or-refused
  (`ErrPromotionNotPersisted`, `graduation.go:343-347`), the class SHALL be promoted to the new
  LevelAutoNotice.

- **REQ-2808** — [R] promotion to AUTO (double bar + the overlay ceiling).
  WHEN a class at LevelAutoNotice accumulates ten additional consecutive verified-clean runs with
  zero operator vetoes and zero incident-recurrence-within-24h-after-heal in the window, the class
  SHALL be promoted to LevelAuto ONLY IF it exists in the embedded lockstep-hashed `opschema.json`;
  overlay-only classes SHALL cap at LevelAutoNotice, and the console SHALL offer a one-click
  embed-export that generates the opschema.json snippet and spec/013 restamp checklist as an MR.

- **REQ-2809** — [F] the band bridge is safe-direction-only.
  `risk.GatedInput` SHALL gain a NoticeFloor input applied by the classifier in the safe direction
  only (a computed AUTO with NoticeFloor becomes AUTO_NOTICE; AUTO_NOTICE and POLL_PAUSE pass
  through), with the level-aware resolver (`cmd/worker/main.go:2700-2705`;
  `temporal/runner/activities.go:913-922`) mapping approve to UngraduatedClass, auto_notice to
  NoticeFloor, and auto to neither, covered by an exhaustive per-rung truth table.

- **REQ-2810** — [F] auto-demote.
  WHEN a verified DEVIATION verdict lands at any rung, the class SHALL drop to LevelApprove with
  both streaks reset via the unchanged immediate hook (`core/actuate/interceptor.go:836-863` — may
  demote, never promote), with the demotion ledger reason carrying the typed SurpriseAlerts
  breakdown; an operator demote verb SHALL drop any rung to approve with mandatory rationale; a
  revoke SHALL remove the class from the composed registry within one refresh; and only
  OutcomeFromVerdict SHALL feed the machine — forecast-lane verdicts SHALL never feed graduation
  (INV-10, `core/falsify/scorer.go:99-108`). The demotion SHALL apply IN-MEMORY even when the save
  fails (fail-safe: a persistence error must never leave a demoted class operating at its old rung),
  and the per-class demote SHALL stay DISTINCT from the estate-wide breaker — one misbehaving class
  never trips the estate breaker by itself, and the breaker never masks a per-class demotion.

## Requirements — candidacy, ratify, composition

- **REQ-2811** — [F] mechanical candidacy thresholds.
  The clustering cron SHALL advance observing to candidate ONLY WHEN a key accumulates three or more
  DISTINCT external_refs AND (two or more distinct hosts OR a seven-day span) AND mean confidence at
  or above 0.6 within a rolling 30-day window, SHALL advance candidate to ratify_ready ONLY WHEN
  five or more distinct refs exist AND family and tier are mechanically assigned from the closed
  sets AND the auto_barred screen is stamped AND blast radius is computed for at least 80% of
  occurrence targets AND no dismiss TTL is active, SHALL treat confidence as a bar and never a
  weight, SHALL exclude occurrences from suppressed or already-remediated sessions, and SHALL
  expire observing and candidate rows after 60 days of silence with a ledger entry.

- **REQ-2812** — [F] cron liveness dead-man.
  The clustering cron SHALL refuse its whole pass loudly WHEN the newest occurrence is older than 48
  hours while session volume is nonzero, computed from tables the cron does not write.

- **REQ-2813** — [R] ratify is operator authorship into an empty form.
  The ratify lane SHALL expose POST /v1/opclass/candidates/{key}/{verb} with verb drawn from the
  closed table {ratify, dismiss, demote, revoke, export-embed}, AuthSession step-up, mandatory
  rationale, worker-workflow ledgering, and the vote-lane hardening kit; the form SHALL be
  structurally empty with no prefill code path; model verb, rationale, and undo sketches SHALL
  render only as screened read-only exhibits visually separated from the form.

- **REQ-2814** — [F] admission validation (error-returning, laundering tripwire).
  Ratification SHALL validate the operator-authored template through an error-returning refactor of
  the registry's admission checks (`core/actuate/opschema/opschema.go:244-365`: closed family and
  tier; literal argv[0]; whole-element slots; slots reference declared required params; validator
  tolerance equals renderer tolerance) PLUS: an overlay slug SHALL never shadow an embedded slug; a
  claimed tier SHALL be refused when IsDestructiveOp or IsNeverAuto contradicts it; and the template
  SHALL be refused WHEN any element byte-matches any occurrence's model text.

  **Scope of "model text" (amended 2026-07-31, owner ruling; found by the aliveness oracle, not by
  planning).** The comparison set EXCLUDES the op-class's own declared verb. The comparison is
  whole-element and exact, so admitting the bare verb would refuse every template containing it —
  feeding `"reload"` makes the entire `service-lifecycle` family unratifiable, and a gate that
  refuses every legitimate grant is indistinguishable from a broken lane (the first live run failed
  422 on an honest operator template). Excluding it costs nothing the requirement was defending:
  the verb is dictated by the op-class registry, not suggested by the model, and it is independently
  validated against the closed family/tier checks in this same requirement. Everything a model
  actually contributes — hosts, paths, flags, values, free prose — remains in the comparison set.

- **REQ-2815** — [F] composed registry with hash re-verification.
  `opschema.Lookup`, `Specs`, and `Catalog` SHALL consult the embedded base THEN an injected
  OverlayProvider snapshot (atomically swapped, refreshed within 60 seconds and on ratify signal),
  SHALL re-verify each overlay row's entry_hash against the ledgered hash at load and DROP a
  mismatching row loudly (fail closed to fewer capabilities) with a page, and SHALL flow ratified
  classes into the agent preamble only through the single Catalog render (`agent/loop.go:29-57`).

- **REQ-2816** — [R] the dossier answers five operator questions in order.
  The candidate dossier SHALL render: (1) what keeps happening (occurrence counts, hosts, span,
  exemplar refs deep-linking to session detail); (2) what TG wants to do (screened quoted model
  text, labeled untrusted); (3) what it could break (estate blast-radius walk with per-edge
  provenance and confidence); (4) how good its predictions are (prediction confusion matrix,
  display only, never feeding graduation); (5) what the operator must type (the empty form beside
  the tier ceiling badge, the nearest registered class's rollback template for reference,
  recurrence and poll-answer rates).

- **REQ-2817** — [F] one chain, one console ladder.
  Every candidate and ladder transition SHALL append its decision string
  (opclass:candidate, opclass:ratify-ready, opclass:ratify, opclass:dismiss, opclass:expire,
  opclass:auto-notice, opclass:auto, opclass:demote, opclass:revoke, opclass:export-embed) to the
  ONE org-global chain, and the console SHALL extend the existing /v1/policy/graduation ladder view
  with the widened vocabulary and captions rather than adding a second ladder surface. ActionID rule
  for `opclass:*` rows: the GovDecision `action_id` SHALL be the `candidate_key` (candidacy-phase
  rows) or the `entry_hash` (ratified-phase rows) — the ledger writer rejects an empty `action_id`,
  so every row is joinable to the exact artifact it governs.

- **REQ-2818** — [O] opcover honesty.
  Every embedded AUTO-capable class SHALL carry a faultinjector pairing or a ledgered exemption in
  the opcover declaration; overlay classes SHALL carry a declared exemption noting that the
  embed-export path closes it.
