<!-- spec/005 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/005 — Tier-1 known-transient & scheduled-reboot suppression

**Owning behavior family:** BEH-5 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-04 (envelope boundary validation), INV-20 (temporally-bounded,
live-verified suppression registry), INV-12 (session isolation under RBAC), INV-19 (decision ledger).
**Phase:** the deterministic suppression chain lands in Phase 2 (`core/suppression`); the temporally-bounded
registry rows and the grammar-validated envelope boundary land in Phase 0/1 (`core/safety`, the Postgres
schema). **Status:** Draft.

The suppression chain is a deterministic, pre-model admission filter run **in code before any model is
spent**: dedup → blast-radius fold → scheduled-reboot phase SR → host-agnostic known-pattern →
active-memory. Every phase fails **OPEN** to standard escalation; a critical-severity or unknown-severity
alert **always** escalates and is never suppressed. Suppression knowledge is assembled at runtime from a
temporally-bounded, org-global registry and is never hardcoded into a prompt. This document is the
requirement source of record; the design is in `design.md`, the runnable acceptance oracles are in
`acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-401** — [F] spec/005 · [R] paradigm-rule 1.
  The system SHALL match a known-transient pattern by host-agnostic rule — the pattern is keyed on the
  `alert_rule` and scoped to the estate, not to one hostname.

- **REQ-402** — [F] spec/005 · [R] paradigm-rules 1/3 · [O] INV-20.
  WHILE an org-global blast-radius suppression-policy record is currently valid — meaning `now()`
  falls between its `valid_from` and `valid_until` and its `last_verified_at` is fresh — the system
  SHALL activate the fold for a child alert that falls within the record's declared host and rule scope.

- **REQ-403** — [F] spec/005 · [R] paradigm-rule 3.
  WHILE a blast-radius fold is active for a matched child alert, the system SHALL post that alert as a
  notice and SHALL NOT spawn a remediation session for it.

- **REQ-404** — [F] spec/005 · [O] INV-20.
  WHEN an alert is an on-schedule reboot on a host carrying a live, un-expired, un-killed registered
  schedule whose strict DST-correct window contains the alert time, the system SHALL suppress it in
  phase SR, WHERE a schedule reaches the `live` state only after at least two observed in-window boots
  confirm the observing row (observe-before-live).

- **REQ-405** — [F] spec/005 · [O] INV-20.
  IF a suppression match is unconfirmed, expired, unverified, or contradicted, THEN the system SHALL
  fail open to standard escalation so the incident is investigated.

- **REQ-406** — [F] spec/005.
  AFTER a suppressed scheduled reboot, WHEN the recorded boot is not a clean `systemd-reboot`, the
  system SHALL reopen the incident and page the approver graph.

- **REQ-407** — [F] spec/005.
  The system SHALL never suppress a critical-severity reboot, nor any critical-severity or
  unknown-severity alert, regardless of any matching schedule or pattern.

- **REQ-408** — [F] spec/005 · [O] INV-04.
  WHEN the dedup stage evaluates a prior triage-log entry, IF the entry is future-dated or carries a
  negative age relative to the current time, THEN the system SHALL reject it at the envelope boundary
  and fail open to escalation rather than treat it as a duplicate.

## Persistence contract

Two temporally-bounded registries and one decision record back this behavior (INV-20, INV-19):

- **`discovered_scheduled_reboots`** — the bi-temporal schedule registry:
  `hostname`, `kind`, `cron`, host `timezone`, `status` (`observing` / `live` / `disabled`),
  `observed_count`, `kill_switch`, `valid_from`, `valid_until`, `last_verified_at`. The matcher reads
  `status = 'live'` rows only. A discovery or classify writer registers `observing` rows; the promoter
  flips a row to `live` only after `observed_count` reaches the promotion threshold, and drives a row to
  `disabled` on schedule drift, expiry, or kill-switch.
- **`suppression_policy`** — the org-global blast-radius record (BEH-5 REQ-402/403) with
  `valid_from`, `valid_until`, `last_verified_at`, and the declared host / rule scope. Managed via the
  ticket module; a record applies only while currently valid and live-verified.
- **Suppression decision row** — every chain run appends one immutable decision
  record (`outcome`, `phase`, `reason`, `action_id`, `signals`) to the tamper-evident governance ledger
  (INV-19). Omitting a field is a Go type error. See [`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Fail-open invariant

A standing check SHALL FAIL if any suppression decision is emitted for a critical-severity or
unknown-severity alert, or if a suppression outcome is produced from a registry row that is expired, past
its `last_verified_at` freshness bound, kill-switched, or in the `observing` state (INV-20).
