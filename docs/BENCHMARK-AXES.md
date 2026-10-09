# BENCHMARK-AXES — the objective definition of "relevant work"

**Status: ACTIVE (2026-07-25) — the objective definition of the measured axes. The steering rule
that consumed this file (the anti-drift rule) was superseded on 2026-07-28 and again by the
2026-07-30 recovery sequence — direction now lives in [`docs/BOARD.md`](BOARD.md); this file
remains the axis vocabulary for the benchmark harness. Swap in the chosen neutral suite's
published axes verbatim once a suite is picked; until then this list stands.**

The project's v1.0 gate is a **neutral third-party benchmark** (ITBench-AA / SREGym-class) run on **both**
Territory Grounder and the incumbent, once TG is a ready product. This file is the *only* definition of
"relevant": a unit of agent-selected work is on-focus **iff it moves one of these axes and states a
before→after value**. Prose like "improves security / coverage / observability / posture / readiness" names
no axis and is **drift by definition** — forbidden as agent-selected focus work (owner-*requested* work is
always legitimate regardless; the owner defines relevance for what the owner asks).

## The scored axes (what a neutral SRE-agent benchmark measures)

| # | Axis | Moves the score by… | TG milestones that advance it |
|---|------|---------------------|-------------------------------|
| A1 | **Detection recall** | catching more real incidents | ingest breadth, alert intake, age-gated pull |
| A2 | **Diagnosis correctness** | naming the right root cause more often | healing-core reasoning, the self-improvement flywheel |
| A3 | **Heal success rate** | actually resolving more incidents | actuation, op-class breadth, regime engine |
| A4 | **Autonomy rate** | resolving more *without* a human | graduation ladder, trust/mode, attribution |
| A5 | **Fault-class breadth** | handling more distinct fault types | op-class breadth (restart/start-guest/disk-grow/…) |
| A6a | **Decision steps** | deciding in fewer investigation cycles | loop efficiency, retrieval quality, seed composition |
| A6b | **Wall-clock (MTTR)** | resolving faster in real time | detection latency, decision latency, actuation path |
| A7 | **False-actuation rate** | acting *wrongly* less often | prediction gate, mode chokepoint, safety floor |
| A8 | **Safety-violation count** | never breaching a guardrail | fail-closed gates, breaker, ledger |

## Which axes HAVE a comparator (P6-3) — declared 2026-07-27

**The incumbent runs in SHADOW with mutation OFF** (`tools/shadowbench/campaign.sh`: "Both run shadow /
MUTATIONS=OFF; this harness only TRIAGES"). It receives the same LibreNMS alerts on transport 4 that TG
receives on transport 7, reasons about them, and records a band — and then does nothing to the estate.

That single fact decides which axes can carry a head-to-head number at all:

| # | Axis | Comparator? | Why |
|---|------|-------------|-----|
| A1 | Detection recall | **SHARED, not comparative** | both systems receive the SAME alert stream, so this measures LibreNMS coverage, not either agent. Publish once, attributed to the monitoring, never as a win. |
| A2 | Diagnosis correctness | **YES — the head-to-head** | both produce a conclusion for the same incident. This is the ONLY scored axis where a win or loss is meaningful, and it is what the exceed-proof's primary endpoint measures. |
| A3 | Heal success rate | **NO — unilateral TG** | the incumbent never actuates, so it has no heals to succeed or fail at. |
| A4 | Autonomy rate | **NO — unilateral TG** | a shadow system resolves nothing without a human because it resolves nothing at all. |
| A5 | Fault-class breadth | **NO — unilateral TG** | breadth is counted in op-classes actuated; the incumbent has none. |
| A6a | Decision steps | **NO — not comparable** | a step is a TG ReAct cycle; the incumbent's loop has no commensurable unit, so a step count compares two different things wearing one name. |
| A6b | Wall-clock (MTTR) | **PARTIAL** | time-to-DECISION is comparable (both reason). Time-to-RESOLUTION is not — only one system resolves. Publish the two separately and never pool them. |
| A7 | False-actuation rate | **NO — and TG can only lose** | a system that never acts can never act wrongly. The incumbent's structural 0% is not a better score; it is an absent one. |
| A8 | Safety-violation count | **NO — and TG can only lose** | same shape: no actuation, no guardrail to breach. |

**THE RULE THIS EXISTS TO ENFORCE.** A unilateral axis is published as a **TG property with that stated**,
never as a comparison, a win, or a delta. Two failure modes are being prevented, and they point in opposite
directions:

1. **Inflated victory** — reporting "TG 34 heals vs incumbent 0" on A3/A4/A5 reads as a crushing win and is
   in fact a description of the experimental setup. The incumbent was not playing.
2. **Manufactured defeat** — reporting A7/A8 comparatively makes TG look strictly worse forever, because the
   only way to score a perfect false-actuation rate is to never actuate. Holding that against the system that
   does the work would be the same error with the sign flipped.

Only **A2** carries a defensible head-to-head, which is why the pre-registration
(`tools/shadowbench/PRE-REGISTRATION.md`) makes diagnosis-against-ground-truth the PRIMARY endpoint and treats
everything else as secondary or unilateral.

## Why A6 is SPLIT — the vocabulary and every implementation had drifted apart (TG-205, 2026-08-04)

A6 was defined here as **MTTR** ("resolving faster … detection latency, decision latency, actuation path")
while **every implementation of it measured decision STEPS**: `cmd/axisscore` computes
`a6a_mean_decision_steps`, `eval/gate` scores `MeanDecisionSteps`, and `session_triage.step_count`
(migration 0037) is what the live scorer reads. No scored surface measured time at all — the only clock in the
system was `tg_agent_run_seconds_total`, a cumulative sum of every agent loop's seconds that has no
distribution, cannot be attributed to an incident, and resets on restart. The name and the code had
silently drifted apart, so a reader of this file and a reader of the scorecard disagreed about what the axis
even was — and TG could not report time-to-decision or time-to-heal for anything, **including its own
measured ~39s-vs-~11min detection result**, which was unpublishable as a latency number.

The rejection of wall-clock **on the merge gate** stands and is not being reopened (`eval/eval.go`:
"wall-clock latency is model-gateway-dominated and noisy … the cycle count is the deterministic,
agent-controlled signal"). What was wrong was leaving one axis NAME over two different measurements. So:

- **A6a — decision steps. THE GATE-SIDE HALF.** Fewer investigation cycles per triage. It is the figure the
  merge gate carries (`eval/gate.Scorecard.MeanDecisionSteps`, printed as a base→candidate Δ; today it is
  reported rather than barred, and if a bar is ever set on A6 it belongs here, because this half is
  deterministic and agent-controlled). It is a token/efficiency measure, **not** a latency proxy: the same
  two-cycle decision costs seconds on the fast tier and minutes on the reasoning tier.
- **A6b — wall-clock. REPORTED, NEVER GATED.** Two legs, published separately and never pooled:
  - *time to decision* — composed seed → the terminal proposal or grounded stop
    (`session_triage.decision_ms`, migration 0058; percentiles on the axisscore scorecard and on `/metrics`
    as `tg_axis_decision_latency_p50_seconds`). This is TG's own reasoning time and it is the manipulated
    variable in the model-tier A/B.
  - *time to recovery* — triage → the estate observed healthy again (`core/db/axis_read.go`, correlated by
    host + rule-family). Dominated by the monitoring system's recovery poll and by the provider, not by TG.

**What A6b does NOT yet cover, stated so nobody reads it as end-to-end:** the ingest→workflow-start leg is
unmeasured, so time-to-decision is a LOWER bound on alert→decision. Detection latency is published
separately per ingest source under A1. Untimed sessions (recorded before migration 0058, or suppressed
before the loop ran) are EXCLUDED from the denominator — never counted as an instant decision.

## Benchmark-readiness axes (needed to RUN the proof at all)

| # | Axis | Moves it by… |
|---|------|--------------|
| R1 | **Benchmark harness/adapter** | letting the neutral suite drive TG (and the incumbent) over its scenario set |
| R2 | **Scenario coverage** | supporting the fault types the suite injects |

## How this is used

- Direction and work-selection live in [`docs/BOARD.md`](BOARD.md) (the anti-drift contract that
  once lived here is archived in `history/BACKLOG-2026-07.md`).
- These axis NAMES remain the frozen vocabulary for benchmark reporting: a benchmark claim is
  meaningful only if it cites an axis above with a before→after value.
- Swap in the chosen suite's published axes verbatim when a suite is picked; until then this list is the
  standing anchor.
