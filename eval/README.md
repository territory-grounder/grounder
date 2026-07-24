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
