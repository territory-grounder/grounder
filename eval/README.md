# `eval/` — evaluation & benchmark harness

## Implemented (task #26, iteration 1): the on-box measurement harness

Runs a corpus of realistic NL incidents through the REAL Runner (read-only, mutation OFF) over the real
359-node estate on the live model gateway, and scores each triage session with an LLM-as-judge on five
dimensions: `correct_diagnosis`, `evidence_grounded`, `sensible_proposal`, `appropriate_band`, and TG's
differentiator `falsifiable_prediction` (was a committed, mechanically-verifiable prediction produced — the
predecessor's LLM-judge does not measure this). Files: `corpus.json` (real hosts + the 19 real LibreNMS
rules), `estate_fixture.json` (a captured `/v1/estate` snapshot), `eval.go` (pure logic, unit-tested in CI),
`eval_integration_test.go` (`TestEvalCorpusOnBox`, skipped in CI, writes `scorecard.json`/`sessions.json`/
`REPORT.md`), `run-on-box.sh` (SSH-tunnels the loopback gateway + runs). Run: `eval/run-on-box.sh`
(or `TG_EVAL_LIMIT=5 eval/run-on-box.sh` for a quick pass). NEXT: the auto-patching A/B flywheel below.

**A sixth axis rides along the scorecard: `diagnosis_grounded` (TG-201).** It is scored DETERMINISTICALLY in
Go (`core/judge.ScoreDiagnosis`) from the session's typed diagnosis, not by the judge model — every input is
a fact the orchestrator bound (`cited` means "this id matched a ToolResult we actually captured", INV-11), so
there is nothing for a model to author, and asking one would re-open a checkable property to free text. It
appears in `dim_means`/`dim_samples` and deliberately **does not** enter `overall` or the gate's per-dimension
bars: `overall` is a fixed-denominator mean over the five LLM axes and every committed card, the trend
baseline included, was computed that way — widening the denominator would move every historical number for a
reason unrelated to agent quality. Sessions the axis does not apply to (a record with no diagnosis field, or
a stand-down that claimed no cause) are OMITTED, never floored. The scale, and the rule that honest
uncertainty scores WELL, live in `core/judge/rubric.json` (`diagnosis_rule`).

## Implemented (TG-43): the BINDING eval gate — "eval gates deploys," tooled

The measurement above became a **gate**. `make eval-gate` (mode **change**, TG-64) measures the candidate
against a **FRESH `origin/main` base arm** in the **same on-box window** — pooling `TG_EVAL_RUNS` runs per
arm, alternating arm order to cancel drift — plus the negative controls, then gates **candidate-vs-fresh-base**
with mechanical thresholds (overall Δ≥−0.15, any dim Δ≥−0.30, the safety-analog `appropriate_band` Δ≥−0.10,
0 proposals on controls), printing a PASS/FAIL table and exiting non-zero on FAIL. Comparing to a *committed*
baseline conflated the change with model/estate/main drift (a stale-baseline false-FAIL) — the committed
`baseline-scorecard.json` is now the **trend anchor** used only by `make eval-drift` (mode **trend**), the
nightly drift-watch that compares clean `main` to the committed baseline and self-refreshes it on a clean,
non-regressing run. Files: `baseline-scorecard.json` (the trend anchor: 5-dim means + N + SHA + date +
provenance), `controls.json` (5 negative controls — benign, no-action-warranted), `holdout-corpus.json` (the
sealed holdout the system may never tune to), `gate/gate.go` (the pure comparison + pooling + fresh-base
comparator + integrity checks, unit-tested in CI at `gate/gate_test.go`), `eval-gate.sh` (on-box
orchestration: base worktree, arm alternation, per-arm integrity rerun), `gate_integration_test.go`
(`TestEvalControlsOnBox` / `TestEvalHoldoutOnBox` — skipped in CI), and `../tools/evalgate` (the deterministic
CLI). `make eval-holdout` reports the regression-vs-holdout gap (the >20pt overfitting signal). The nightly
`eval-gate-scheduled` CI job runs the trend-watch, files a YouTrack issue on drift, and pushes the refreshed
anchor. Full docs: `../docs/EVAL-GATE.md`. **Required before merging any prompt/skill/model change.** (The
original 20 in `corpus.json` are untouched, so the trend baseline stays comparable.)

## The fixture arm (B4a) — why expected-propose incidents are deterministic

The 2026-07-30 trend run (`eval/history/2026-07-30-trend-e22fc14b7ac5/`) is the motivating failure: every
expected-propose incident in `corpus.json` had gone stale vs the LIVE estate (the down devices had been
re-enabled or healed since capture), so the freshness pass — correctly — excluded all of them from recall,
proposal capability was UNMEASURED, `falsifiable_prediction` floored at 1.00 every run, and the baseline
could never refresh. That is structural, not bad luck: a live-armed propose corpus decays by nature,
because a healthy estate heals its faults.

So the six expected-propose incidents are **fixture-armed** (`Incident.ToolFixtures`, `fixtures.go`): each
carries the CAPTURED outputs of the real investigation tools (LibreNMS device status / active alerts /
eventlog, hostdiag `check-host-services` with the target unit in the real *down services* form, memory /
disk / load reads), served verbatim by `NewFixtureToolSet` — same tool names as the production-parity live
set, **zero live network calls** (guarded by a tripwire test), no env-gating, and the freshness pass skips
them (stale-proof by construction — that is the point). Fixture-armed sessions COUNT in `proposal_recall`
(they are the measurable propose supply); the scorecard publishes `fixture_armed` alongside
`stale_excluded` so recall always discloses how much of it came from the deterministic arm. Stand-down /
escalate incidents stay live-armed on purpose: their correctness IS live-groundedness. Shape faithfulness
is pinned in CI — `TestFixtureShapeFaithfulHostdiag` replays each fixture's captured step outputs through
the REAL hostdiag renderer and requires the corpus text byte-for-byte; after a deliberate tool-dialect
change, re-capture with `TG_EVAL_REGEN_FIXTURES=1 go test ./eval/... -run TestRegenerateToolFixtures`.

## Implemented (TG-204): the three-arm model-tier A/B — and why it currently refuses to report a number

`make tier-ab` (`eval/tier-ab.sh` + `eval/tierab` + `tools/tierab`) runs the same corpus through three arms in
one window — **ARM-CONTROL** (fast investigate / primary decide, production's routing), **ARM-STRONG**
(primary throughout), **ARM-CHEAP** (fast throughout) — and reports Δ`correct_diagnosis` (the gated axis),
Δ`diagnosis_grounded` (TG-201's deterministic companion), Δ decision steps (A6a), Δ wall-clock (A6b) and
Δ USD. Arms are selected through `TG_EVAL_ARM{,_INVESTIGATE,_DECIDE}`, which are inert unless a process
declares itself an experiment (`temporal/runner/activities.go` — the MECH-402 floor cannot be lowered by one
env var in production).

**It reports nothing today, on purpose. Measured 2026-08-04 against the deployed litellm config and the
proxy's own telemetry: `fast`, `primary` and `opus-cc` all resolve to `openai/opus-cc`, and all three arms
were served `claude-opus-5`.** TG-204's three arms are *one arm measured three times*, so the harness exits
COLLAPSED (status 1) and publishes **no deltas** — a Δ of ~0.00 between an arm and itself reads exactly like
"the expensive tier buys nothing", which is the decision the ticket exists to inform. The 53-second reasoning
tier the ticket was written about no longer exists: the 2026-07-31 single-brain decision pointed every
agent-facing alias at the same Claude Opus 5 proxy. Committed evidence:
`eval/history/2026-08-04-tierab-preflight-fdcca37d-*/`.

Three properties are worth knowing before trusting any number it *does* print:

- **Arm identity comes from the proxy, never the gateway.** LiteLLM echoes the requested *alias* back in the
  response's `model` field (alias `fast` answers `{"model":"fast"}`), so a distinctness check reading the
  gateway passes vacuously on aliases that are the same brain. The harness requires the tg-claude-proxy's
  `served_model` telemetry and fails closed (status 3) for any arm it cannot account for.
- **A preflight settles distinctness for ~4 completions**, before three corpus passes are spent on arms that
  cannot differ (`make tier-ab-preflight`). It is a positive-control-carrying check, not one that always
  refuses: the default arms collapse (exit 1) and `arm-haiku`/`arm-opus` are distinct (exit 0) —
  `eval/history/2026-08-04-tierab-preflight-ba01f540-*/`.
- **The arm window is the SESSION phase only** (`eval/phase.json`), because the judge runs on `primary` in
  every arm; including it would stamp the judge's brain onto every arm's signature and flatten ΔUSD/Δms. The
  obvious alternative — filtering on the `user` field TG sends — does **not** work: LiteLLM drops `user`
  before an `openai/`-provider upstream, so the proxy logs `caller=""`.

**To actually answer TG-204** one of these must change: (a) point `fast` at a genuinely cheaper upstream so
the production aliases differ again, or (b) accept that the literal question is closed and run the
`arm-haiku` vs `arm-opus` experiment instead — a real question about whether a cheaper brain loses diagnosis
quality, but *not* the production routing TG uses. Say which one was run; the archived verdict records the
tiers so the two can never be confused.

---

The 3-set flywheel (regression / discovery / **sealed holdout** the system may never tune to), the
LLM-as-a-judge, judge calibration + frontier cross-check + RAGAS, prompt-patch A/B trials, and the
whole-trajectory benchmark (Verified Incident Success Rate, the `Agentic Utility` composite).

**Teacher / lessons loop (built):** `core/lessons` closes the outcome-labelled memory loop — observe →
resolve → learn → retrieve. `lessons.Lesson`/`Distill` distill a RESOLVED incident into a `knowledge.Incident`
ONLY from a confirmed-clean outcome (a mechanical `match` verdict AND an orchestrator-confirmed clear), so a
deviation / partial / unconfirmed session never becomes precedent — the corpus is never poisoned with advice
from a session where reality diverged or the fix was unverified. The survivors feed `core/knowledge` (the
retrieval plane), so the agent is seeded next time with its own verified successes. The write-side hop is
`knowledge.MergeCorpus`/`WriteCorpus` (dedup by external_ref, newer record wins, round-trippable), which
appends distilled lessons into the corpus file the retriever reloads at runtime — so the learn → retrieve
loop closes without a restart. (The FEED — which resolved incidents to distill — arrives from the reconcile
close-out / tracker resolutions in Phase 2.)

**Frontier cross-check (built):** the no-human eval anchor lives in `core/governance/frontier_crosscheck.go`
(`FrontierCrossCheckMonitor`) — it re-judges a sample of locally-judged sessions with a frontier model and
raises **DRIFT** (local↔frontier verdict disagreement while liveness reads healthy) and confirmed **DEATH**
(the local judge left unscored what the frontier scores real). The decision (`Evaluate`) is pure; the
frontier I/O is behind the `PairSource` seam.

See `docs/TESTING-AND-BENCHMARK.md`. **Status:** phased in from P1 onward; not part of the P0
read-only foundation.
