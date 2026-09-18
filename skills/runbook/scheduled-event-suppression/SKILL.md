---
name: scheduled-event-suppression
class: runbook
version: 0.1.0-distilled
source: distill:docs/runbooks/scheduled-reboot-suppression.md
description: Suppress intentional scheduled events with a conjunctive safety floor — never critical, behavioral confirmation, strict windows, fail-open, suppress-then-verify
---

## Goal
Suppress alerts for INTENTIONAL scheduled events (nightly reboots, planned maintenance cycles) without
ever suppressing a real fault that happens to look like one. The design is suppress-fast-then-verify:
the suppression is a CLAIM ("this was the scheduled event"), and every claim gets checked after the
fact, with reopen-and-page as the price of being wrong.

## Required evidence
- A registry of declared schedules: which host, which event class, which cadence, with an expiry —
  discovered from the estate's own schedule sources, not hand-asserted.
- BEHAVIORAL confirmation before a schedule may suppress anything: at least two observed occurrences
  landing inside the declared window. A declared cron that has never been seen firing is a hypothesis
  in an observe-only state, not an authority.
- The event's actual post-hoc signature: for a reboot, the boot reason read from the host itself —
  clean scheduled restart vs panic/OOM/watchdog/self-heal.
- The alert's class, severity, and precise timestamp, evaluated in the HOST'S timezone with DST
  handled correctly — window math in the wrong timezone suppresses real faults twice a year.

## Decision rules
- The safety floor is conjunctive — every guard must hold, and every guard failure fails OPEN to
  escalate: critical severity never suppresses; only the event's own alert class matches (a disk
  alert during a reboot window is not the reboot); only confirmed-live registry rows suppress; the
  kill switch and expiry are checked in the read path (instant deactivation, no cache); the timestamp
  must fall inside the strict window.
- An off-schedule instance of a scheduled event class is a REAL incident — the window is the whole
  point. A 13:09 reboot on a host whose cron says 07:00 is investigated, never absorbed.
- Suppress-then-verify closes the irreducible residual: a coincidental real fault landing in-window is
  caught by the post-hoc signature check, which force-reopens and pages. Suppression without the
  verify limb is silent risk accumulation.
- Any matcher error — malformed schedule, unresolvable timezone, store failure — escalates. A
  suppression mechanism that fails closed (suppressing on error) has inverted its own safety.
- Drift maintenance is part of the mechanism: schedules that disappear from the source are disabled;
  registry rows expire; misclassification and unreachable-verify counters are exported and alarmed,
  so the mechanism's error rate is observable, not assumed.
- The mechanism ships dark and is enabled by an explicit operator control with an instant global
  kill — suppression authority is owner-granted, never self-granted.

## Verification
- Every suppression in the record carries its registry row, window math, and the post-hoc
  verification result; reopened ones show the page.
- Sampled suppressed events show clean signatures; the misclassification counter is zero or each
  count maps to a reopened, investigated incident.
- A deliberately off-window test event escalates — proving the window guard can actually refuse.
