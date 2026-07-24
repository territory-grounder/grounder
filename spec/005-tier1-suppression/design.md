<!-- spec/005 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/005 — Design: tier-1 known-transient & scheduled-reboot suppression

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent.

## Component

`core/suppression.Chain` (Phase 2) is the deterministic admission filter that runs before a triage
workflow ever spends a model. It is invoked from the ingest Temporal activity on a typed, already
validated `AlertEnvelope` (spec/006) carrying `hostname`, `alert_rule`, `severity`, and a
grammar-checked `observed_at` timestamp.

```
Chain.Decide(ctx, AlertEnvelope) -> Decision
```

`Decision` is a required-field struct (`Outcome`, `Phase`, `Reason`, `ActionID`, `Signals`). Its zero
value is `OutcomeEscalate` — the fail-open default — so any panic, unmatched branch, or dropped error
path yields escalation by construction rather than accidental silence. This mirrors the predecessor's
"any exception fails OPEN" rail (`scripts/lib/tier1_suppression.py`) but makes fail-open the *type*
default instead of an exception handler an author has to remember.

## Composition over the safety core

- **Fail-open zero value.** `OutcomeEscalate` is the zero value of the `Outcome` enum (composes with the
  `core/safety` fail-closed philosophy: for the suppression lane, "closed" *is* escalation — investigate
  rather than swallow). This realizes REQ-405 and the whole-chain error handling.
- **Critical/unknown floor.** `Chain.Decide` reads `severity` first; a `critical` or an unrecognized
  severity short-circuits every phase and returns `OutcomeEscalate` before any registry is consulted
  (REQ-407). An unrecognized severity is treated as never-suppress, never as low by omission.
- **One grammar, one envelope.** The `observed_at` field is parsed once by the ingest grammar (INV-04);
  the dedup stage compares it against `now()` and rejects a future-dated or negative-age entry at the
  boundary (REQ-408) rather than trusting a malformed timestamp.

## Decision procedure (ordered most-specific-first)

`Chain.Decide` runs the phases in order and returns the first non-escalate decision. Ordering is
load-bearing: a cascade parent outranks a per-host schedule, and a deterministic schedule outranks a
knowledge-base transient guess.

0. **Declared freeze (maintenance / chaos)** — consulted BEFORE the severity floor. `Chain.Freeze` holds
   operator-declared `FreezeWindow`s (scope host / alert_rule / whole-estate, with a start+end). An alert
   inside an active, in-scope window is suppressed as an EXPECTED effect of the declared maintenance or
   chaos drill — even at `critical` severity, because the operator already knows it is coming (a planned
   reboot's HostDown must not spawn remediation). Deliberately narrow: only in-scope, active-window alerts
   freeze; an unexpected or out-of-scope alert falls through to the severity floor and escalates normally.
1. **Severity floor** — `critical` or unknown severity → `OutcomeEscalate` (REQ-407).
2. **Phase 1 — dedup boundary.** Scan the recent triage log for the same
   `(hostname, alert_rule)` within the window `[now-window, now)`. An entry timestamped after `now()`
   (future-dated / clock-skew / negative age) is rejected and the alert fails open (REQ-408). Dedup collapses
   a re-fire only against a still-OPEN prior INCIDENT: a prior entry that was itself suppressed is not a valid
   anchor, and — when an `OpenIssue` checker is wired — a re-fire whose parent incident has since CLOSED is a
   genuine new incident that escalates, never silently suppressed as a duplicate.
3. **Phase 1b — blast-radius fold.** Read the `suppression_policy` records. A record folds the
   child alert only WHILE it is currently valid — `now()` between `valid_from` and `valid_until` and
   `last_verified_at` within its freshness bound (REQ-402). A match posts the child as a notice with no
   session (REQ-403). An expired, stale-verified, or scope-mismatched record fails open (REQ-405).
4. **Phase SR — scheduled reboot.** For a reboot-class alert, match against `discovered_scheduled_reboots`
   rows in `status = 'live'`, `kill_switch = false`, and un-expired, whose DST-correct cron window (a
   re-implemented, timezone-aware window evaluator, never a shelled-out `croniter`) contains the alert
   time (REQ-404). The evaluator parses the full 5-field crontab grammar — `*`, single values, ranges `a-b`,
   steps `*/s` and `a-b/s`, comma-lists, day-of-month and month, and cron's DOM-or-DOW day semantics (Sunday
   as 0 or 7) — so a real reboot schedule (a weekday range `0 3 * * 1-5`, a monthly `0 3 1 * *`) matches, not
   only single-value forms. The window is evaluated against every fire on the alert's day AND its adjacent
   days, so a just-after-midnight boot correctly matches a late-night cron from the previous day (a
   `59 23 * * *` fire at 23:59 matches a 00:03 boot), with day matching checked on the fire's day. The window
   around each fire is **asymmetric** — `[fire − PreBuffer, fire + PostWindow]`, default `[−5m, +10m]` — because
   a reboot alert normally arrives AFTER the fire (detection lag + the reboot itself), so the post-window is
   wider than the pre-buffer (the predecessor's `DEFAULT_PRE_BUFFER_MINUTES=5` / `DEFAULT_WINDOW_MINUTES=10`).
   A symmetric ±tolerance was the port defect: it escalated an on-schedule boot observed at `fire+8m`, and could
   not be widened to catch it without also wrongly suppressing a `fire−8m` boot. No match, or a row in
   `observing`, fails open (REQ-405).
5. **Phase 2 — host-agnostic known-pattern.** Match a transient pattern keyed on `alert_rule` across the
   estate (REQ-401), gated by THREE floors — a `confidence >= 0.7` floor, a transient-nature keyword in the
   rule (flap/blip/recover/…, so a standing fault like "DiskFull" is never auto-suppressed), and, for a
   learned pattern carrying a `LastSeen`, a 7-day recency window. Any gate failing, or no match, fails open.
6. **Phase 3 — active-memory operator rule.** The LAST, most-permissive phase: an explicit operator
   `SuppressRule` — a `(HostPattern, RulePattern)` glob pair (`path.Match` syntax, either side `*` for any) with
   an operator-supplied `Reason` — suppresses an alert whose host AND rule both match, recording the reason on
   the decision. It ports the predecessor's `openclaw_memory` `triage-rule` rows (key `<hostpat>:<rulepat>`,
   value `suppress:<reason>`) as typed, operator-curated config injected into the stage — the ONLY suppression
   path a human explicitly authorizes. Two fail-safe properties hold by construction: a critical/unknown
   severity is NEVER suppressed by an operator rule even if one matches (defense-in-depth beneath the chain's
   severity floor), and a malformed glob matches nothing so a broken rule fails OPEN rather than silencing
   every alert. No match fails open (REQ-401/405).

Any step's error is caught and recorded as a pass-through, never as a suppression — the accumulated
journey is carried on the final escalate decision for observability.

## Observe-before-live promotion (REQ-404 self-learning)

The `ScheduleRegistry` is CONCURRENCY-SAFE — the discovery and promotion writers below run as SEPARATE
scheduled activities that share it, so every method holds a mutex; `Promote` is the sole mutator of a row's
promotion state and holds the lock across its whole read-modify sequence, so a concurrent promote can neither
lose an observed boot nor half-apply a lifecycle transition.

Three writers, each re-expressed as a Temporal-scheduled activity (never a Cronicle job or a
host sentinel):

- **Discovery** registers a found cron/timer/unattended reboot schedule as an `observing` row (with a zero
  boot count) on FIRST sight. An `observing` row never suppresses. Re-discovery of an existing schedule
  refreshes its descriptive fields (window, `valid_until`) but PRESERVES its promotion state — status,
  observed count, kill switch — so a periodic re-scan can never demote a promoted-to-live schedule back to
  observing (an `ON CONFLICT` that preserves, not a force-reset).
- **Classify** registers an `observing` row reactively when a clean-boot reboot alert traces to a
  deterministic schedule.
- **Promotion** reads each host's boot history and accumulates the DISTINCT in-window boots across runs —
  deduped by exact timestamp (a boot seen in overlapping `journalctl` lookbacks counts once, so a single
  boot can never promote) and capped — flipping a row to `live` once `observed_count` (the size of that
  accumulated set) reaches the promotion threshold (two in-window boots), driving a row to `disabled` on
  cron drift, and expiring a row past `valid_until`. Promotion is the only transition
  that lets a row suppress; a wrong attribution never accumulates two in-window boots and stays
  `observing`.

## Two-phase verify (REQ-406)

A suppressed scheduled reboot enqueues a durable verify activity. After the host returns, it reads the
boot reason. Only a genuinely CLEAN boot (`IsCleanBoot` — `reached target reboot.target` / `systemd-reboot`
/ `syncing filesystems`) confirms; a REACTIVE boot (`IsReactiveBoot` — OOM-kill / kernel panic / watchdog /
hung_task / emergency / self-heal / thermal) or an UNKNOWN reason reopens the incident and pages the approver
graph. A reactive reboot is a symptom, never a schedule — it is never confirmed and never learned as one.
The suppression is provisional until the verify confirms the reboot was clean.

## Persistence & audit

Each `Chain.Decide` appends one immutable decision record to the governance ledger
(INV-19) inside the same activity. Registry reads and writes are authority-checked against the acting
user/role under RBAC (INV-12); the estate is a filtering label, not an isolation boundary.

## Out of scope

The risk-classifier bands an alert that survives suppression (spec/001). The prediction gate and verdict
mechanics are spec/002. Ingest signature-verification and the canonical entity re-read are spec/006. The
generated interface contract for the registry rows is spec/006 (INV-15).
