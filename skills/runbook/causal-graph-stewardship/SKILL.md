---
name: causal-graph-stewardship
class: runbook
version: 0.1.0-distilled
source: distill:docs/runbooks/infragraph.md
description: Operate a learned causal dependency graph — visible staleness, capped mined confidence, fail-open advisory vs fail-closed actuation, mechanical adjudication
---

## Goal
Steward a learned causal dependency graph of the estate — topology seeded from authoritative sources,
dynamics mined from incidents — without letting learned structure acquire authority it has not earned.
The graph's value is prediction; its danger is confident staleness.

## Required evidence
- Per-source seed freshness: the graph is seeded from several authoritative sources (cluster APIs,
  inventory, monitor dependency data, reviewed operator declarations), each with its own timestamp
  and its own failure isolation — one source failing must not silently thin the others' seed.
- Edge provenance on every edge: seeded-from-which-source vs mined-from-incidents, with confidence.
- A staleness mechanism that degrades VISIBLY: automatically-derived edges carry expiry and self-
  expire; the stale-edge count is exported and alarmed. A graph that silently keeps dead edges
  predicts yesterday's estate.
- Backtest results against replayed incidents, as the graph's precision instrument.

## Decision rules
- Mined confidence is CAPPED below authority thresholds by design: an edge learned from co-occurrence
  may inform advisory context, but its cap keeps it structurally unable to qualify for suppression or
  actuation eligibility. Earned authority requires reviewed, declared knowledge.
- The advisory lane fails OPEN (a graph outage must not block triage); any actuation-adjacent gate
  fails CLOSED (no prediction, no action). Never blur the lanes — and the model-based invariant
  binds: remediation without predictions is not a degraded mode, it is disabled.
- Verdicts on the graph's predictions are written ONLY by mechanical adjudication against observed
  outcomes — the model that made the prediction never judges it.
- Writers resolve entities through the graph's own resolver before writing: a wrong-typed twin node
  (the same host as both "device" and "vm") is invisible to traversal, which is worse than absent —
  it looks covered.
- Coverage grows through the AUTHORITATIVE sources, in ROI order (monitor dependency declarations
  first — they also improve the monitor's own suppression; inventory cabling next; reviewed manual
  declarations last) — never by hand-inserting unreviewed edges directly.
- Precision drops HOLD graduation: a backtest below the bar freezes any expansion of the graph's
  authority until the cause (topology change without reseed, stale dynamics) is found.

## Verification
- Per-source seed timestamps are within cadence; a deliberately failed source shows isolated failure
  and its own alert, not a silently smaller graph.
- Sampled mined edges show capped confidence; no suppression- or actuation-eligible decision cites a
  mined edge as its authority.
- The prediction ledger shows verdicts only from the mechanical adjudicator, and the backtest
  precision meets the bar current authority claims require.
