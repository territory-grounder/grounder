<!-- spec/019 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/019 — Scheduling awareness & maintenance-window seam (estate scheduler integration)

**Owning behavior family:** — (an infrastructure sensor + actuation seam; no `BEH-N`, like spec/008/009/010).
**Constitution / invariants:** INV-02 (no subprocess — native client, fixed effect surface), INV-05
(re-read the system-of-record by id, trust no cached copy), INV-09 (fail closed — the never-auto floor and
the conservative default), INV-13 (secrets are references, never literals), INV-17 (a disabled/unconfigured
connector has no path). **Phase:** Phase-1-safe read-only sensor; the actuation-defer consumption is Phase-2.
**Status:** Approved.

TG's scheduling / maintenance-window capability INTEGRATES the estate's existing scheduler (Cronicle) rather
than rebuilding one: a read-only connector reads the scheduler's events over its REST API and derives a
vendor-neutral model of (a) sanctioned **maintenance windows** and change-**freeze** windows and (b) already
**scheduled jobs**, so the actuation path can DEFER a change that falls outside a sanctioned window (or inside
a freeze) and avoid colliding with a change that is already coming. The connector actuates nothing. This
document is the requirement source of record; the design is in `design.md`, the runnable acceptance oracles
are in `acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-1900** — [F] spec/019 · [O] INV-05.
  The Scheduling connector SHALL read the estate scheduler's events over its REST API read-only, and on every
  evaluation it SHALL re-read the schedule from the scheduler's system-of-record by id rather than trust a
  cached copy, so an upstream retiming or disablement is reflected without a stale window surviving.

- **REQ-1901** — [F] spec/019 · [R] "integrate the estate scheduler, do not rebuild one".
  WHEN a scheduler event carries an operator maintenance-window or change-freeze directive, the system SHALL
  derive a time window by projecting the event's recurrence in the event's declared timezone to concrete
  `[start, start+duration)` occurrence ranges.

- **REQ-1902** — [F] spec/019.
  The system SHALL derive the set of already-scheduled jobs from every enabled recurring scheduler event, so
  an actuation can be deferred away from a change that is already scheduled on the same target.

- **REQ-1903** — [F] spec/019 · [O] INV-09.
  IF the schedule cannot be read, THEN the maintenance-window seam SHALL report the current time as OUTSIDE
  any sanctioned maintenance window, so an unreadable scheduler makes actuation MORE conservative and the
  system never assumes it is safe to actuate.

- **REQ-1904** — [F] spec/019 · [O] INV-09.
  WHILE the current time falls inside a change-freeze window whose scope covers the target, the
  maintenance-window seam SHALL report NOT-in-window even when a maintenance window would otherwise cover the
  same time — a freeze denies over an overlapping maintenance window (deny-overrides).

- **REQ-1905** — [F] spec/019.
  The maintenance-window seam SHALL report in-window only WHILE the current time falls inside a sanctioned
  maintenance window whose scope covers the target and no covering change-freeze is active; the absence of any
  covering sanctioned window SHALL be reported as not-in-window.

- **REQ-1906** — [F] spec/019 · [O] INV-09.
  WHEN the maintenance-window seam reports the current time is not inside a sanctioned maintenance window for a
  target, the system SHALL map that actuation to the POLL_PAUSE band as the defer signal, so the change is
  held for a human and never auto-executed on the strength of an out-of-window evaluation.

- **REQ-1907** — [F] spec/019 · [O] INV-13.
  The connector SHALL authenticate to the scheduler with an API key supplied as a sealed SecretRef reference,
  resolved at request time, and SHALL place no literal secret in code, configuration, logs, or any exportable
  artifact; an unresolvable or empty key SHALL fail closed rather than read with a blank credential.

- **REQ-1908** — [F] spec/019 · [O] INV-02/INV-09.
  The Scheduling connector SHALL actuate nothing and SHALL spawn no subprocess: it reads over a native Go HTTP
  client only, and it exposes no execution path a mutation could reach.

## Persistence contract

This spec adds no governed table. The derived schedule is an ephemeral, re-read snapshot
(`core/schedule.Calendar`); no maintenance-window state is cached across evaluations (REQ-1900 / INV-05). The
scheduler's own store remains the system-of-record; TG reads it read-only.

## Fail-safe invariant

A standing check SHALL FAIL if the maintenance-window seam reports in-window for any target while the schedule
is unreadable, or while a change-freeze whose scope covers the target is active — the conservative default
(not-in-window) holds under every unreadable, ambiguous, or frozen condition (INV-09).
