<!-- spec/003 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/003 — Per-incident auto-resolve and escalation requeue

**Owning behavior family:** BEH-3 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-01, INV-11, INV-12.
**Phase:** close-out and requeue behavior lands in Phase 2, composing over the band from spec/001, the
mechanical verdict from spec/002, and orchestrator-captured evidence (INV-11). **Status:** Approved.

The band-aware reconciler is the close-out lane: it drives a genuinely-finished session to a terminal
decision, transitions its ticket through the ticket module, records a per-incident best-outcome
row, and — when an approval poll went unanswered — schedules a delayed re-check that re-enters the
gated pipeline as an authenticated internal Temporal signal. It closes an incident only on a confirmed
clear, never on the acting agent's asserted success. This document is the requirement source of record;
the design is in `design.md`, the runnable acceptance oracles are in `acceptance/`, and the engineering
tasks are in `tasks.json`.

## Requirements

- **REQ-201** — [F] spec/003 · [O] INV-11.
  WHEN the reconciler evaluates a finished session for close-out, the system SHALL close the incident
  only after an orchestrator-captured `ToolResult` or an independent post-condition check confirms the
  alert condition actually cleared — never on the acting agent's asserted success.

- **REQ-202** — [F] spec/003.
  WHEN a band-**AUTO** session drives a host back to health, the system SHALL reconcile the
  recovered host and transition its ticket to **Done** through the ticket module.

- **REQ-203** — [F] spec/003.
  WHEN a session reaches close-out, the system SHALL record a `resolution_type`
  (`auto_resolved`, `human_resolved`, `escalated`, or `deferred`) on the append-only close-out record.

- **REQ-204** — [F] spec/003.
  IF a session ends with no terminal result (crash, timeout, or indeterminate output), THEN the system
  SHALL leave the incident open by transitioning it to **To Verify** rather than closing it silently.

- **REQ-205** — [F] spec/003 · [R] paradigm-rule 1.
  The system SHALL record every outcome as a per-incident best-outcome row, so an
  alert storm cannot inflate the auto-resolve denominator.

- **REQ-206** — [F] spec/003.
  WHEN an approval poll goes unanswered and its session archives, the system SHALL schedule a delayed
  re-check row in `escalation_queue` carrying `attempts`, `status`, and `eligible_at`.

- **REQ-207** — [F] spec/003 · [O] INV-01/INV-12.
  WHEN a queued re-check fires, IF the alert condition is still active THEN the system SHALL re-escalate
  and page the approver graph, and otherwise SHALL defer closure to the autocloser — re-entering
  the gated pipeline as an authenticated internal Temporal signal keyed by `session_id`,
  never a bare re-trigger.

- **REQ-208** — [F] spec/003 · [R] paradigm-rule 2.
  AFTER the per-incident unanswered-poll cap is reached, the system SHALL stand down to a human by
  escalating to the fallback approver or next on-call tier rather than retrying autonomously.

## Persistence contract

Every close-out appends exactly one immutable close-out record, stamped `schema_version`,
carrying `resolution_type`, the terminal `outcome`, and the `session_id`, chained into the tamper-evident
governance ledger (INV-19). Outcomes are rolled up as per-incident best-outcome rows (count incidents,
not events — REQ-205). Unanswered-poll re-checks are appended to the
`escalation_queue` registry (`attempts`, `status`, `eligible_at`, `kind`); a re-check
re-enters the pipeline only as an authenticated internal Temporal signal (REQ-207). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Best-outcome accounting invariant

A standing check SHALL FAIL if the auto-resolve rollup counts alert events instead of incidents. A
re-check row SHALL re-enter execution only through the
authenticated Temporal signal path (INV-01); a re-check delivered by any unauthenticated re-trigger
SHALL be rejected before it reaches the gated pipeline.
