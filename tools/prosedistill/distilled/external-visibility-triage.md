## Goal
Triage a claim made by an OUTSIDE observer — a routing-visibility service, a third-party monitor, an
external probe — that the estate's reachability or redundancy has shrunk. The observer, the
instrument, and the estate can each be the faulty party; severity is graded by the redundancy that
REMAINS, not by which component vanished. NOTE — tool-gated content: TG has no external-routing or
internet-visibility tools wired; this runbook is knowledge-library material.

## Required evidence
- Confirmation from at least one INDEPENDENT vantage: any single visibility source has transient gaps
  and rebalances; two unrelated observers agreeing is signal, one observer alone is a hypothesis.
- The instrument's own health: is the exporter/collector producing fresh data (a stale-metrics
  meta-alert is the honest companion to any external-data alert), and is the upstream data service
  itself answering sanely? If the data source is down, your alerts are noise — say so and stand down.
- The self-inflicted check: is the estate still announcing/serving what the observer says vanished?
  An empty answer here means WE caused it, and the fix is internal.
- The redundancy inventory: which independent paths/providers exist for the affected function, and
  which of them remain healthy right now.

## Decision rules
- Grade severity by residual redundancy, not by the failed component's name: all paths visible with
  one observer disagreeing = likely observer blip (wait out its absorption window); one path of N
  lost = SERVICE INTACT but single-homed — critical, because the next failure is an outage; last path
  lost = full outage, page immediately.
- A surviving fallback DOWNGRADES severity and must be stated with its measured cost: "degraded via
  fallback, quality/latency cost X" is a different incident from "down" — and a fallback whose cost
  was never measured cannot support that downgrade.
- Known false-positive absorption windows (rule hold durations sized to observer maintenance and
  reshuffles) are respected: an alert still inside its window is watched, not escalated.
- Escalation to a provider is asymmetric evidence-gathering: for functions terminating on THEIR
  equipment, your visibility ends at the handoff — record what you verified on your side, then
  escalate with it.
- First-boot/fresh-deploy staleness is expected once: an instrument that has never run yet is not a
  dead instrument. Wait one collection cycle before paging on staleness.

## Verification
- The record names each vantage consulted and the instrument-health check's result before any verdict.
- The severity grade cites the remaining-redundancy count and, where a fallback holds, its measured
  cost.
- If the verdict was "observer blip", a later re-check confirming auto-resolution is in the record —
  a blip that never resolved was not a blip.
