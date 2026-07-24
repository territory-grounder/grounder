<!-- spec/019 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/019 — Design: Scheduling awareness & the maintenance-window seam

How the requirements in `requirements.md` are realized on the Go stack. Where this design and the code
disagree, the code is the bug and this document is the intent. The capability INTEGRATES the estate's own
scheduler (Cronicle) — it is a read-only sensor plus an actuation-facing seam, not a second scheduler
(Temporal already owns TG's own orchestration).

## Components

- **`core/schedule` (vendor-neutral model + seam).**
  - `Recurrence` (`recurrence.go`) — a scheduler-agnostic recurrence over calendar fields
    (years/months/days-of-month/weekdays/hours/minutes), each an EMPTY-means-every whitelist. `WindowContains`
    answers "is `now` inside some `[start, start+duration)` occurrence?" by a bounded backward minute scan;
    `Next` finds the soonest occurrence within a horizon by a bounded forward scan. Both evaluate in an
    explicit `*time.Location` so a window declared in one timezone is DST-correct (the embedded `time/tzdata`
    makes `LoadLocation` resolve on distroless). Weekday numbering is `time.Weekday` (Sunday=0), which matches
    Cronicle's `weekdays` convention exactly — no off-by-one remap (REQ-1901).
  - `Directive` (`directive.go`) — the operator's window intent parsed from an event's free text:
    `tg-window=maintenance|freeze`, `tg-duration=<godur>`, `tg-target=<glob>`. TG requires no native
    "maintenance window" type in the scheduler; the operator tags an ordinary event and TG derives the window
    from the event's own recurrence. An unrecognised `tg-window` value is `KindUnspecified` (the zero value,
    which sanctions nothing — fail closed).
  - `Calendar` + `WindowRule` + `ScheduledJob` (`schedule.go`) — the derived snapshot and the
    `MaintenanceWindow(target, now) (inWindow bool, reason string)` query. Semantics are deny-overrides
    (REQ-1904/1905): unreadable ⇒ outside (REQ-1903); an active covering freeze ⇒ outside; an active covering
    maintenance window with no freeze ⇒ inside; otherwise outside. `ImminentJob` surfaces the soonest
    already-scheduled job on a target (REQ-1902). `EvaluateBand`/`DeferBand` map "not-in-window" to
    `safety.BandPollPause` (REQ-1906) — the defer signal — without this package importing the interceptor.
  - `WindowGuard` — the interface the actuation path consults: `MaintenanceWindow(ctx, target, now)`. A live
    connector implements it by re-reading the schedule (INV-05); a nil guard is inert (window-gating is opt-in).

- **`modules/schedule/cronicle` (the read-only connector).**
  - `Client` (`cronicle.go`) — a native `net/http` Cronicle REST client (no subprocess, INV-02, REQ-1908),
    grounded in `docs/APIReference.md` and verified live: `POST /api/app/{get_schedule,get_event}/v1`, auth via
    the `X-API-Key` header from a `config.SecretRef` (env:/file:/store:) resolved at request time and never
    logged (REQ-1907, INV-13). The response `code` is `0` (a number) on success and a STRING code on error, so
    it is decoded as a raw token and only the literal `0` is success. `Schedule` paginates `offset/limit` to
    completion; `Event` re-reads one event by id (REQ-1900). Every non-2xx / non-zero-code / decode / transport
    error fails closed.
  - `Provider` + `derive` (`derive.go`) — maps Cronicle events onto `core/schedule`: an operator-tagged
    enabled event → a `WindowRule` (timezone via `LoadLocation`, duration from `tg-duration` or the configured
    default, capped at 24h; a tagged-but-on-demand or over-long or unrecognised-kind event is
    skipped-with-record, never a guessed always-open window); every enabled recurring event → a `ScheduledJob`.
    `Provider` implements `schedule.WindowGuard`: `Snapshot` re-reads live, and `MaintenanceWindow` converts a
    read error into a fail-closed-safe unreadable `Calendar` (REQ-1903).
  - `ParseDeployments` (`derive.go`) — config-not-code `id|baseurl|keyref[|defaultduration][|cacert]` rows
    from `TG_CRONICLE_DEPLOYMENTS`; a partial row is skipped (fail closed), and the key stays a reference.

## The actuation seam (how "defer → POLL_PAUSE" composes)

The interceptor (`core/actuate`, spec/013) already realises "defer" as a recorded refusal at a numbered
admission gate. The clean insertion point is a sibling of the existing band/territory gates: consult a
`schedule.WindowGuard` for `r.Manifest.Action.Target` at `now`, and if `!inWindow`, `refuse("outside a
sanctioned maintenance window: " + reason)` — which records the deferral to the ledger and holds the action
(equivalent to the POLL_PAUSE band `DeferBand` returns). For THIS spec the seam, the derivation, and the band
mapping are built and oracle-proven; wiring the guard into the live interceptor chain is a Phase-2 follow-on
gated with the rest of the mutation-enablement work (mutation stays OFF regardless), so the keystone chain is
untouched here. A nil guard keeps the gate inert, so the wiring is a pure addition with no default behaviour
change.

## Grounding & the live demo

The Cronicle API shapes are grounded in the official reference and verified against a temporary demo Cronicle
(`soulteary/cronicle`, `dc1tg01:3012`, loopback-only, labeled `tg.purpose=demo-temporary`) seeded with a
read-only API key and four events (a nightly maintenance window, a nightly moratorium freeze, a business-hours
freeze, and a plain hourly job). The acceptance oracle re-serves those exact wire shapes from an in-process
fake so the derivation + fail-safe are proven deterministically in CI, and the same shapes were confirmed live.

## What this does NOT do

It does not schedule, create, or run any job (Temporal owns TG's orchestration; the connector is read-only).
It does not lift the mechanical never-auto floor or enable mutation. It does not cache schedule state across
evaluations (every answer is a fresh re-read, INV-05).
