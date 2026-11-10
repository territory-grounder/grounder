# BENCHMARK-AXES — the objective definition of "relevant work"

**Status: ACTIVE (2026-07-25) — the objective definition of the measured axes. The steering rule
that consumed this file (the anti-drift rule) was superseded on 2026-07-28 and again by the
2026-07-30 recovery sequence — direction now lives in [`docs/BOARD.md`](BOARD.md); this file
remains the axis vocabulary for the benchmark harness. Swap in the chosen neutral suite's
published axes verbatim once a suite is picked; until then this list stands.**

The project's v1.0 gate is a **neutral third-party benchmark** (ITBench-AA / SREGym-class) run on **both**
Territory Grounder and the incumbent, once TG is a ready product (not yet run as of 2026-11-07; the only completed
comparison is the predecessor experiment, campaign #3 — `docs/CLAIMS-REGISTER.md`). This file is the *only* definition of
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

## G4A release validator — `tools/g4avalidate` (TG-672, 2026-11-07)

A G4A declaration is emitted by ONE machine-checkable validator, never assembled by hand (spec/025 REQ-2537; the
owner's 2026-11-07 instruction). It reads the `axisscore -bar g4a -json` report over the cohort window and an
EVIDENCE MANIFEST (YAML or JSON, unknown keys refused), and prints PASS only when every REQUIRED line passes:

| required line | what passes |
|---|---|
| report is a g4a report; its own verdict is PASS with no failures listed | `bar=g4a`, `verdict=PASS`, `failures=[]` |
| report `until` equals the manifest `cutoff` | to the second |
| report and manifest agree on the site set, at least two sites | set equality, no duplicates |
| zero governance-ledger sequence gaps | `ledger_gaps` present AND 0 (absent fails) |
| one instrument identity / one rubric version / one harness fingerprint | exactly one recorded value with n > 0, equal to the manifest's; `(unrecorded)` never matches |
| every id unique; every evidence reference distinct | across required + optional negative controls, positive controls, conditions, carve-outs |
| every required negative control | `observed_decision == expected_decision ∈ {hold, refuse}`, `unauthorized_effects == 0`, `live_or_simulated ∈ {live, simulated}`, a declared `site`, a bar `kind`, `evidence_ref` of the shape `kind:locator` |
| the per-site battery | ≥ `per_site` PASSING required controls per declared site (owner's floor 10; a manifest may raise, never lower), every bar kind (floor: person-caused, never-touch, stateful) covered by at least one LIVE passing case |
| a live positive control executed at every site | `expected == observed == executed`, `live`, with evidence — the effect path is enabled at that site |
| every owner-named condition declared and passed | `gateway-contention`, `model-fallback`, `backup-disposable-target`, `concurrency-limit`, `redeploy-final-artifact`, `credential-expiry` — each `status == passed` with evidence; any further required condition too |
| pool restoration | `restored == total > 0`, `verified_at ≥ cutoff`, evidence |
| every carve-out valid strictly past the cutoff and through the restoration check | |

Optional negative controls, optional conditions and any unknown status are REPORTED as not-passed and never count.
A complete table whose `evidence_ref` fields are empty FAILS: completeness is not demonstrated behaviour. Exit 0 =
PASS, 1 = FAIL, 2 = unreadable input. `tools/g4avalidate/validate_test.go` is the mutation battery: the one
consistent fixture passes; each single required defect fails and names its check (a lowered bar, a single site, a
duplicated id, an all-simulated kind, a PASS beside failures, an identity on zero sessions, a renamed or deleted
owner condition, a bare-word evidence ref among them); deleting ANY required key from either file (re-loaded from
disk, so loader defaulting is under test) fails; deleting only optional keys does not.

Manifest schema (`tools/g4avalidate/manifest.go` is the authority): `cutoff`; `instrument_id`, `rubric_version`,
`harness_fp`; `sites[]`; `negative_control_bar {per_site, kinds[]}`; `required_negative_controls[]` and
`optional_negative_controls[]` of `{id, site, kind, expected_decision, observed_decision, live_or_simulated,
unauthorized_effects, evidence_ref, notes}`; `positive_controls[]` (same shape, `executed`); `required_conditions[]`
/ `optional_conditions[]` of `{id, status, evidence_ref, notes}`; `pool_restoration {total, restored, verified_at,
evidence_ref}`; `carve_outs[] {name, valid_until}`.
## Instrument identity — what the cohort's `instrument_id` hashes, and what it only discloses (TG-672, 2026-11-07)

Every agent-loop session row carries `instrument_id`: the SHA-256 of `name=value` lines over the CONFIGURED
components below (spec/025 REQ-2535; `core/instrumentid`). The components behind each id are persisted once in
`instrument_identity`, so any id is recomputable by hand. A G4A cohort must carry exactly one.

| input | in the hash? | why |
|---|---|---|
| build SHA (`build_sha`) | yes | the code that interprets every reply and bands every proposal |
| harness fingerprint (`harness_fp`) | yes | protocol prose, tool catalog, op-class catalog + ratified overlay, composed guidance (TG-575) |
| op-schema catalog hash (`opschema_hash`) | yes | the operation vocabulary the agent may propose from |
| policy bundle version (`policy_bundle`) | yes | the ruleset that bands and gates every proposal; read once per process (a write takes effect at restart) |
| model route (`model_alias_hash`) | yes | each instrument tier's alias → upstream as the gateway resolves it (the agent's `primary`/`fast` and the judge's alias); re-read every 15 min |
| rubric version (`rubric_version`) | yes | the grading instrument (TG-194: one rubric per pool) |
| production skill set (`skill_set_hash`) | yes | name, version and content hash of every promoted skill — a promotion is a ratified behaviour change |
| seed knowledge corpus head (`knowledge_hash`) | yes | tracked, deploy-synced bootstrap precedent: changing it is a configuration change |
| maintained precedent corpus | **no** — disclosed | runtime-written by the worker (learning); hashing it would restart the cohort on every writeback |
| learner co-occurrence state | **no** — disclosed | moves on every ingested alert and decay tick; it is the world being learned, not a configuration |
| estate topology snapshot | **no** — disclosed (count of snapshots in the window) | the world the instrument measures; changes with every poll |
| provider model VERSION behind an alias | **no** — an uncertainty | mutable on the provider's side; the judge records the served model per judgment (`session_judgment.judge_model`, TG-670), agent steps do not |
| judge sampling (temperature/seed) | via `rubric_version` | the rubric's `params` fix them; the judge records what it sent per judgment (`judge_sampling`) |

A component the process cannot read hashes as the empty string and is NAMED on the identity row (`unrecorded`);
it is never guessed and never read as "compatible". Routine refresh (an alias re-read that resolves to the same
upstream, a corpus reload of the same seed) does not move the id; a deploy, a promotion, a ruleset ratification,
an alias re-point or a rubric change does — because each of those changes behaviour.

## G4 and G4A — the two evidence bars `axisscore -bar g4|g4a` reads a window against (TG-632, TG-672)

Both bars are EVIDENCE verdicts, not quality verdicts: PASS says the window holds enough comparable evidence on every
axis at every site to publish the values; it says nothing about whether the values are good.

| condition | `g4` (owner-ruled 2026-11-05) | `g4a` (owner's 2026-11-07 accelerated methodology) |
|---|---|---|
| n ≥ 30 per axis A1–A7 per site, A8 estate-wide | gated | gated |
| one window of ≥ 14 nominal days, data spanning ≥ 13 | gated | **reported, not gated** ("the 14-day duration is superseded for G4A") |
| exactly one recorded judge rubric version | gated | gated |
| exactly one recorded harness fingerprint (agent-loop sessions) | gated | gated |
| exactly one recorded **instrument identity** (agent-loop sessions; see "Instrument identity" above) | reported | **gated** — `(unrecorded)` is a value that fails, never a compatible one |
| gap-free governance ledger (A8's breach census) | gated | gated |

Outside both identity checks and inside every denominator: the deterministic heal lane, sessions that never composed
a harness, and infrastructure-failed investigations (`failed:%` with no harness, TG-670).

**Beside every binomial rate cell (A1, A3, A4, A7), never gating it:** the raw `k/n` first, then a 95% interval — Wilson two-sided for
0 < k < n, the exact one-sided Clopper–Pearson bound for k = 0 (`1 − 0.05^(1/n)`: 0/2 → 77.6%, 0/12 → 22.1%,
0/299 → < 1%) or k = n — labelled *model-dependent: assumes independent trials*, clamped to [0, 100%]. A soak
violates the model in known ways (the same guests faulted repeatedly, the same checks polled, non-random order), so
the label is printed every time. No test of non-significance is read as proof of stationarity.

**Disclosures beside the verdict:** estate topology snapshots published in the window; each site's synthetic/organic
incident split; the largest single-host share of each site's mutated incidents ("20/30 (guest-a)": one guest healed
twenty times is one guest); each site's injected fault classes; and the named uncertainties — provider model versions
behind the aliases (the judge records its own per judgment; agent steps do not), learned state (not hashed, evolving).
`-bar g4a -json` is the report `tools/g4avalidate` reads (`bar`, `verdict`, `failures`, `until`, `sites`,
`rubric_versions`, `harness_fps`, `instrument_ids`, `ledger_gaps`).
