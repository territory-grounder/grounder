---
name: amber-zone-discovery
class: skill
version: 0.1.0-distilled
source: distill:.claude/skills/proactive-discovery/SKILL.md
description: Find conditions degrading but not yet alerting, surface each once, and route findings by reversibility
---

## Goal
Find conditions that are degrading but have not yet crossed an alert threshold — the amber zone — and
route each finding by reversibility. Un-actioned degradation is a certain slow march to an outage; a
"review post" nobody reads is un-actioned work plus false comfort. Surface each condition exactly once,
and never let discovery authorize an action.

## Required evidence
- Trend reads against the amber bands, per host class:
  - `check-host-disk <host>` — filesystems 80–93% full (the band BEFORE the ~95% critical threshold).
  - `check-host-memory <host>` — 10–18% memory available (pressure building, not yet critical).
  - `get-active-alerts` — anything already in a pending/about-to-fire state on its own threshold.
  - Certificate expiries within 21–30 days, via your monitoring system (before the final-week alert).
  - Work items stuck in progress beyond 7 days (staleness is degradation of the process, not the host).
- `get-estate-context <host>` for every finding — a finding must bind to a known, identifiable target.

## Decision rules
- DEDUP: surface each (host, condition) pair once, not daily. Re-surface only when the condition
  escalates to a worse band. A repeated identical finding is noise that trains readers to ignore you.
- ROUTE BY REVERSIBILITY, never by convenience:
  - Reversible, bounded conditions (reclaimable disk, a restartable degraded unit) become PROPOSALS
    through the governed proposal gate — the gate decides what may act, this procedure never does.
  - Irreversible or high-blast remedies (volume resize, data deletion, host reboot) are SURFACED to the
    operator with the evidence. Do not propose them as automatic work.
- A finding on a target the inventory cannot identify is surfaced only — dispatching investigation at
  an unidentifiable host produces thin, un-resolvable work.
- A trend query returning nothing is a safe no-op, not an error. Absence of amber findings is a valid,
  reportable outcome.

## Verification
- A remediated finding's metric leaves the amber band — name the exact post-fix reading
  (`check-host-disk` shows the filesystem under 80%, the certificate's new expiry date, the unit active).
- The dedup holds: the next discovery cycle does not re-surface the same (host, condition) pair.
- Every dispatched finding carries the identifier of the proposal or session it became — a finding with
  no downstream record was dropped, not routed.
