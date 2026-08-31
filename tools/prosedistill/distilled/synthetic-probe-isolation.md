## Goal
A synthetic end-to-end probe (a canary that exercises a whole pipeline on a schedule) earns its trust
from two properties: it exercises the REAL code path stage by stage, and it is STRUCTURALLY isolated
from production state — with the isolation itself instrumented, because an assumed isolation is the
next leak.

## Required evidence
- The probe's per-stage results: each pipeline stage asserts its own artifact (the thing the next
  stage consumes), plus a coherence check that the artifacts chain (identifiers match across stages).
  A single pass/fail hides WHICH stage broke.
- The isolation design, stated: throwaway state (a temp store seeded from schema, deleted on exit), no
  production writers, no real actuation targets, read-only plans.
- The leak metric: every run counts probe-tagged rows in the LIVE store; the count must be zero and a
  nonzero count pages at the highest severity — isolation broke, disable the probe before fixing.
- The probe's own freshness metric with an absent() guard, so a probe that stops running is itself an
  alert, not a silence.

## Decision rules
- Structural isolation beats behavioral promises: "the canary is careful" is not a property; "the
  canary physically writes to a store that is deleted on exit" is. Prefer designs where pollution is
  impossible over designs where it is avoided.
- The three risks any pipeline canary must eliminate by construction: polluting production records,
  satisfying a real in-flight gate with synthetic artifacts (a synthetic prediction must never be
  able to answer a real session's check), and triggering real remediation. Name how each is
  eliminated.
- A canary failure is graded by stage: the earliest failed stage owns the defect; later stages'
  failures are cascade until proven independent.
- The canary's cadence and alerting are sized to what it defends against — the months-long-silent-dark
  class needs daily proof and a staleness alert, not perfect uptime.
- Isolation regression is treated as a top-severity defect even when nothing was harmed: the leak
  metric firing means a code change rewired the probe at production state; the probe stays disabled
  until leak-zero is re-proven.

## Verification
- Each run's record shows per-stage pass/fail, the chain-coherence check, and leak-count zero.
- Killing a stage in a controlled test turns exactly that stage red and pages per policy — the probe's
  own failure modes have been exercised, not assumed.
- The staleness alert has been proven to fire by pausing the schedule once in a controlled window.
