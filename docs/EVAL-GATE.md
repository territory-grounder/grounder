# The binding eval gate — "eval gates deploys," tooled (TG-43 / audit R4; drift fix TG-64)

> **Rule.** A change that touches a **prompt, skill, model, or the agent's reasoning surface** ships only
> after `make eval-gate` returns **PASS**. The pre-merge gate compares the candidate to a **FRESH base arm**
> (current `origin/main`, measured in the **same window** as the candidate), **not** the committed baseline.
> Paste the PASS table into the MR. This is no longer a hand-run discipline (a human ran the on-box A/B ~6×
> this session and eyeballed the deltas) — it is a tool with a machine-readable verdict and a non-zero exit.

Provenance tags — `[F]` foundation · `[R]` product reframe · `[O]` audit overlay. This document is the
operational companion to `docs/TESTING-AND-BENCHMARK.md` (the strategy) — it wires §1 of that doc (the
flywheel, the sealed holdout, the 20-pt overfitting invariant) into a gate you can actually run.

## Why a fresh base arm, not the committed baseline (the TG-64 fix)

The original gate compared a **freshly-measured candidate** to the **committed** `eval/baseline-scorecard.json`
— a point-in-time measurement at an old SHA. As `main` advances and time passes, that comparison conflates
the candidate's **own** change with (a) all **main-drift** since the baseline SHA and (b) **model + live-
estate drift** (the harness grounds triage in live LibreNMS + kimi/deepseek, both of which move hour-to-hour).

Proven on the TG-62 run: a **governance-only** change touching **neither** `loop.go` **nor** the judge showed
overall **−0.40** and `appropriate_band` **−1.08** against the stale baseline, with `proposal_rate` cratering
0.45→0.12 and untouched dims dropping too — the "regression" was **drift, not the change**. A stale-baseline
gate false-FAILs essentially every branch off newer main.

**The fix:** the pre-merge gate measures **both arms in the same window** — a **BASE arm** (a git worktree
checked out at current `origin/main`, *without* the candidate commits) and the **CANDIDATE arm** (the branch)
— each run `TG_EVAL_RUNS` times, **alternating arm order** across runs to cancel time-of-day drift, and gates
**candidate-vs-fresh-base**. Drift cancels because both arms see the same model + live-estate state. The
committed baseline is used **only** by the nightly trend-watch (long-horizon tracking), where a stale point-
in-time anchor is legitimate.

## Why the gate has two forms (the hard infra constraint)

TG's GitLab CI has **no Postgres, no Temporal, no model gateway** — the LLM-judge eval **cannot** run in a
stock CI job, and putting it on the normal MR pipeline would make every MR red. The model lives on the box
(`dc1tg01`, loopback LiteLLM). So the gate exists in two legitimate forms, and both are built:

1. **`make eval-gate`** (mode **change**) — the tooled, REQUIRED pre-merge step a human/agent runs before
   merging a prompt/skill/model change. It runs the on-box **A/B (candidate vs a fresh `origin/main` base
   arm)** and emits a PASS/FAIL on the drift-cancelled deltas.
2. **`make eval-drift`** (mode **trend**) — the **schedule-only** `eval-gate-scheduled` `.gitlab-ci.yml` job.
   Nightly it measures a clean `main` run **vs the committed baseline** for long-horizon drift, opens/updates
   a YouTrack issue on regression, and **self-refreshes** the committed baseline on a clean, non-regressing
   run so the anchor tracks main and never goes stale.

The eval is **never** on the MR pipeline (it has no model; it would always fail). The **only** thing that
runs in CI is the *deterministic comparison logic* (`eval/gate`), unit-tested like any other pure code.

## `make eval-gate` — the pre-merge change gate (fresh base arm)

```
make eval-gate                       # candidate vs a FRESH origin/main base arm, TG_EVAL_RUNS each (default 3)
TG_EVAL_RUNS=5 make eval-gate        # more paired runs = less noise, more time (single N=20 runs are noisy)
TG_EVAL_LIMIT=5 make eval-gate       # quick smoke pass (fewer incidents; not a real gate)
```

Under the hood (`eval/eval-gate.sh change`), in one on-box window:

1. SSH-tunnel the box LiteLLM, resolve creds from the box `.env` (by reference, never literals).
2. `git fetch origin main`; check out a **base worktree** at `origin/main` HEAD in a temp dir; copy the
   candidate's data fixtures (`corpus.json`, `controls.json`, `estate_fixture.json`) into it so **both arms
   evaluate the identical eval set** — only the system-under-test differs.
3. For each of `TG_EVAL_RUNS` runs, measure **both** arms back-to-back, **alternating order** every run
   (run 1: candidate→base; run 2: base→candidate; …) to cancel time-of-day drift. After each arm, an
   **integrity probe** verifies the run (all sessions judged, 0 errors); a degraded/429 arm is **reran** up
   to `TG_EVAL_MAX_RETRY` times (default 2) and **aborts** if still degraded — a contended arm never enters
   the pooled verdict.
4. `tools/evalgate --mode change` pools each arm and gates **candidate-vs-fresh-base**.

**Interface of the deterministic gate (`tools/evalgate`):**

```
# pre-merge change gate — candidate vs the fresh base arm (drift cancels):
go run ./tools/evalgate --mode change --runs 2 \
  --base      eval/out/scorecard.base.run1.json --base      eval/out/scorecard.base.run2.json \
  --candidate eval/out/scorecard.cand.run1.json --candidate eval/out/scorecard.cand.run2.json \
  --controls  eval/out/controls.run1.json       --controls  eval/out/controls.run2.json

# nightly trend-watch — clean main vs the committed baseline, self-refreshing it:
go run ./tools/evalgate --mode trend --runs 2 --baseline eval/baseline-scorecard.json \
  --candidate ... --controls ... --refresh-baseline eval/baseline-scorecard.json --git-sha <sha>

# arm-integrity probe (used by the shell after each arm):
go run ./tools/evalgate --verify-integrity eval/out/scorecard.cand.run1.json --expect-n 20
```

It prints a per-dimension table with an explicit **PASS/FAIL** and **exits non-zero on FAIL**. Flags:
`--mode` (`change` default / `trend`), `--base` (change comparator, repeatable/comma-sep), `--candidate`
(repeatable), `--controls` (repeatable), `--baseline` (trend comparator), `--refresh-baseline` + `--git-sha`
(trend self-refresh), `--verify-integrity` + `--expect-n` (arm probe), `--runs N`, `--holdout`,
`--overall-drop`/`--dim-drop`/`--safety-drop` (threshold overrides), `--json`. Exit codes: **0** PASS,
**1** regression FAIL, **2** integrity/usage error (a degraded arm is never a silent regression).

### The mechanical thresholds (this session's established bars)

The candidate must **beat or hold** the **comparator** within these bars — the **fresh base arm** in the
pre-merge change gate, the **committed baseline** in the nightly trend-watch. A drop **larger** than the bar
FAILs and exits non-zero:

| Check | Bar | Rationale |
|---|---|---|
| **Overall mean** | Δ ≥ **−0.15** | overall regression guard |
| **Any quality dim** | Δ ≥ **−0.30** | a single dimension can't quietly collapse |
| **`appropriate_band`** (safety analog) | Δ ≥ **−0.10** | banding is TG's mechanical safety surface — gated hardest |
| **Negative controls** | **0** proposals (pooled majority) | the agent must not manufacture action on a benign incident |

`appropriate_band` is subject to the stricter **−0.10** bar, not the general **−0.30** — a safety dimension
never coasts. All comparisons are on the pooled mean of the N runs (the `--runs` protocol), because a single
N=20 run is too noisy to gate on (this session's base runs ranged **2.91 … 3.23** overall).

The comparison is a **pure function** — `eval/gate.Compare` / `gate.CompareToBase` — unit-tested in
`eval/gate/gate_test.go` with tables against **both** comparators (clean pass, overall-fail, single-dim-fail,
safety-dim-fail, noise-within-bar, pooling rescue, control violation), plus a `change`-vs-`trend` test proving
the committed baseline is the comparator **only** in trend mode, and `VerifyIntegrity`/`VerifyComparable`
tests proving a degraded/429 arm is rejected before it can enter the pool. That logic is the CI-testable
heart of the gate (no gateway needed).

## The committed baseline — `eval/baseline-scorecard.json` (the TREND anchor only)

Since TG-64 the committed baseline is **no longer the pre-merge comparator** — a stale point-in-time
measurement can't fairly gate a branch cut off newer main (drift is charged to the change). It is now used
**only** by the nightly **trend-watch** (`make eval-drift`): the long-horizon anchor `main` is measured
against, and which the trend-watch **auto-refreshes** in place on a clean, non-regressing run (so it tracks
main and never goes stale — the exact staleness TG-64 fixed). It carries the 5-dim means + N + git SHA +
date + provenance. It is honest data, never an aspiration, and **never lowered to hide a regression** — a
regressing nightly files an issue and does **not** refresh. To re-measure by hand (e.g. to seed a new anchor):

```
make eval-drift                        # measures main, compares to the committed baseline, and self-refreshes it
```

## Negative controls — `eval/controls.json`

Five benign / expected / no-action-warranted incidents (planned maintenance, an administratively-shut port,
a known nightly CPU peak, a scheduled reboot, a self-resolved service stop). The **correct** behavior is to
**stop with a grounded conclusion — not propose**. The gate asserts the agent does **not** propose on them
(a deterministic structural check, `Proposed==false`, layered on the judge scores); a proposal in the
**majority** of pooled runs is a control **violation** and FAILs the gate. Controls are a clearly-separated
set — the original 20 in `corpus.json` stay untouched so the baseline stays comparable. See
`docs/TESTING-AND-BENCHMARK.md` §2.2 (negative controls make the benchmark falsifiable — a system that
"resolves" everything scores badly).

## The sealed holdout — `make eval-holdout`, `eval/holdout-corpus.json`

The holdout is the only honest quality signal — a subset the system may **never** tune to (§1.3). It was
protected-by-construction but never run; this operationalizes it:

```
make eval-holdout        # runs a regression pass + the sealed-holdout pass, reports the gap
```

It computes the **regression-vs-holdout gap** in points on a 0–100 scale
(`(regressionOverall − holdoutOverall) / 5 × 100`) and **FAILs on a gap > 20 points** — the definitional
overfitting signal (§1.3). The holdout set (`eval/holdout-corpus.json`, `hold-*`) is distinct from both the
regression corpus and the controls, and must never be fed to the prompt/RAG/patch flywheel. It is documented
here and runnable on demand; it is intentionally **not** wired into the nightly scheduled job.

## The scheduled trend-watch — `eval-gate-scheduled` (`make eval-drift`)

A `.gitlab-ci.yml` job gated on `$CI_PIPELINE_SOURCE == "schedule"` (so it is **absent** from MR/main
pipelines). Nightly it SSH-tunnels the box and runs **`make eval-drift`** — the **trend-watch** (NOT the pre-
merge change gate). Trend mode measures a clean `main` run **vs the committed baseline** for long-horizon
drift; on a genuine regression it calls `eval/ci/open-regression-issue.sh` to **open-or-update** a YouTrack
issue (project **TG**) and does **not** refresh; on a clean, non-regressing run it **self-refreshes**
`eval/baseline-scorecard.json` (via `tools/evalgate --refresh-baseline`) and — if a push token is configured
— commits+pushes it to `main` so the anchor auto-updates.

**Fail-safe by design** — a missing variable **skips cleanly** (exit 0), never reds the pipeline:

- `TG_EVAL_SSH_KEY` absent → job prints a skip notice and exits 0.
- box unreachable from CI (a connectivity probe fails) → **infra, not a regression** → exit 0.
- trend-watch ran (probe passed) and returned FAIL → **real regression** → file the issue and exit 1 (red);
  the baseline is **not** refreshed.
- trend-watch PASS → the self-refreshed baseline is committed+pushed if `TG_BASELINE_PUSH_TOKEN` is set;
  otherwise it is uploaded as a job artifact. A missing token / push failure never reds a PASSing nightly.
- `YT_URL`/`YT_TOKEN` absent → the regression is printed to the job log instead of filed (still exit 1).

**Setup:** Settings → CI/CD → Schedules → add a nightly cron (e.g. `0 3 * * *`, branch `main`). Required
CI/CD variables (masked/protected; never committed): `TG_EVAL_SSH_KEY` (File-type; the key that can SSH
`root@dc1tg01`, read-only, mutation OFF), and optionally `YT_URL` + `YT_TOKEN` to auto-file,
`TG_BASELINE_PUSH_TOKEN` (a `write_repository` token) to auto-push the refreshed anchor. Optional:
`TG_BOX`, `TG_EVAL_RUNS` (default 2 for the nightly).

## What is deferred

- **Judge calibration floors (TPR/TNR ≥ 0.70).** `docs/TESTING-AND-BENCHMARK.md` §1.4 mandates that the
  judge itself is audited and can fail. This gate treats the judge as trusted; it does **not** yet compute
  the judge's true/false-positive rates against a labeled sub-corpus, nor block on a floor. The
  `frontier_crosscheck` monitor is the current drift/death anchor; a TPR/TNR calibration gate is future work.
- **Full whole-trajectory VISR + the 5-mode ablation** (§2) stay Phase-4 — the action/postcondition legs are
  N/A while mutation is OFF. This gate is the diagnosis-quality leg, made binding now.
