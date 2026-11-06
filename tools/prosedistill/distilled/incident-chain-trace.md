## Goal
Verify a multi-stage incident pipeline the only way that means anything: follow ONE incident through
EVERY stage by a shared key. Per-stage green is how a pipeline stays dark for months — each stage
passes its own test while the handoffs between them are broken — and the cure is a chain trace that
makes the handoffs observable.

## Required evidence
- Every stage logs a structured line keyed by the incident identifier, so one query over the logs
  returns the incident's whole journey in order.
- The EXPECTED signature: the ordered list of stage events a healthy incident produces (received →
  triaged → escalation decision → handoff acknowledged → classified → recorded → reconciled/closed).
  The expectation is written down; without it, a partial trace looks complete.
- A symptom table mapping trace shapes to owning stages: an empty payload at stage N means stage N-1
  emitted nothing; a missing acknowledgment means the handoff transport, not either endpoint; a
  present-but-never-reconciled tail means the closing sweep is dead.
- Freshness reads on the chain's terminal stores (last-written timestamps), because the tail of the
  chain failing is invisible from the head.

## Decision rules
- Prove fixes end-to-end, never at their own stage: a repaired stage that emits correctly into a
  broken next stage has not repaired the pipeline. The acceptance for any pipeline fix is a full
  trace of a real (or synthetic — see synthetic-probe-isolation) incident.
- When a trace stops, the defect is AT or immediately BEFORE the silence: read the last present line's
  payload before blaming the next stage — an empty input recorded downstream indicts the upstream
  emitter.
- Chain health checks are freshness checks: "the table has rows" is history; "the newest row is
  recent, and the closing sweep's last run is recent" is health.
- Every stage boundary that crosses a process or host gets its own logged handoff result (status
  code, length) — the boundaries are where chains die, and an unlogged boundary is undiagnosable.
- Keep the red-flag table current: each newly diagnosed chain failure adds its trace shape and owning
  stage, so the next diagnosis is a lookup, not an investigation.

## Verification
- A randomly chosen recent incident's key, queried across the logs, produces the full expected
  signature in order.
- Each terminal store's freshness is within its cadence.
- The symptom table's entries each name a real diagnosed occurrence — a red-flag row nobody ever hit
  is a guess, marked as such.
