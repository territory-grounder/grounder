# The binding eval gate — "eval gates deploys," tooled (TG-43 / audit R4; drift fix TG-64)

> **Rule.** A change that touches a **prompt, skill, model, or the agent's reasoning surface** ships only
> after `make eval-gate` returns **PASS**. The pre-merge gate compares the candidate to a **FRESH base arm**
> (current `origin/main`, measured in the **same window** as the candidate), **not** the committed baseline —
> and since 2026-07-30 it ALSO applies **absolute candidate-arm floors** (proposal_rate/prediction_rate ≥
> 0.25; proposal_recall ≥ 0.50 on the labeled corpus), because drift-cancellation lets a collapse shared by
> both arms pass (it did, on 2026-07-25: PASS +0.31 with 0% proposals on both arms). **Commit the
> `eval/history/` entry the gate wrote, in the MR it gates** (this replaces "paste the PASS table" — a
> pasted table is prose; the committed entry is the durable quality record). The gate is a tool with a
> machine-readable verdict and a non-zero exit, never a hand-run discipline.

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
make eval-gate                       # TG-117 FAST GATE (the default): candidate vs a FRESH origin/main
                                     #   base arm, runs=1 per arm over the stratified 8-incident corpus
                                     #   prefix (TG_EVAL_LIMIT=8) — a merge costs ~10-15 min, not ~1.5-2h
TG_EVAL_FULL=1 make eval-gate        # full rigor: 3 runs x full corpus — dial up for a high-risk
make eval-gate-full                  #   agent-behavior change before merge (same thing, spelled as a target)
TG_EVAL_RUNS=5 make eval-gate        # more paired runs = less noise, more time (single N=20 runs are noisy)
TG_EVAL_LIMIT=5 make eval-gate       # quick smoke pass (fewer incidents; not a real gate)
```

Committed `eval/history/<date>-change-<sha>/` records therefore read at **fast-gate rigor unless the
record says otherwise** — runs=1, the 8-incident prefix. That IS the sanctioned pre-merge gate
(absolute floors + negative controls still bind); `TG_EVAL_FULL=1` records carry `runs: 3` and the full
corpus N, visible in the archived comparator/scorecard.

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
**1** regression FAIL, **2** integrity/usage error (a degraded arm is never a silent regression),
**3** INCONCLUSIVE (the run measured nothing about a gated capability — see below; note `go run` collapses
any non-zero to 1, so 3 is only distinguishable when the built binary is invoked directly).

### The mechanical thresholds (this session's established bars)

The candidate must **beat or hold** the **comparator** within these bars — the **fresh base arm** in the
pre-merge change gate, the **committed baseline** in the nightly trend-watch. A drop **larger** than the bar
FAILs and exits non-zero:

| Check | Bar | Rationale |
|---|---|---|
| **Overall mean** | Δ ≥ **−0.15** | overall regression guard |
| **Any quality dim** | Δ ≥ **−0.30** | a single dimension can't quietly collapse |
| **`appropriate_band`** (safety analog) | Δ ≥ **−0.10** | banding is TG's mechanical safety surface — gated hardest |
| **Negative controls** | **0** proposals (pooled majority) — and the arm must be **supplied** | the agent must not manufacture action on a benign incident; an absent control arm is an unmeasured bar, not a clean one |
| **Unmeasured capability** | **INCONCLUSIVE**, never PASS | a gate may only certify what the run actually exercised |

`appropriate_band` is subject to the stricter **−0.10** bar, not the general **−0.30** — a safety dimension
never coasts. All comparisons are on the pooled mean of the N runs (the `--runs` protocol), because a single
N=20 run is too noisy to gate on (this session's base runs ranged **2.91 … 3.23** overall).

### The third outcome: INCONCLUSIVE — a gate may only certify what it measured (TG-258)

The verdict is **three-valued**: `outcome` is `pass`, `fail`, or `inconclusive`, and the `pass` boolean is
`true` for `pass` **and nothing else** (every caller — both `tools/evalgate` exit paths, the trend
self-refresh guard, the printed report — already reads that boolean, so an inconclusive run blocks exactly
like a regression).

**Why it exists.** The proposal bars are *label-driven*: when every expected-propose incident is
stale-excluded (live evidence contradicts the corpus, so standing down is **correct**), the raw
proposal/prediction floors would punish correct behaviour and the recall floor has an empty denominator —
so the gate applies **no proposal bar at all**. It used to say so in a warning and then return
`"pass": true` anyway. That record is committed:
`eval/history/2026-07-30-change-74f599c65f39/verdict.json` carries `"pass": true` beside its own
*"PROPOSAL CAPABILITY UNMEASURED … this run proves nothing about propose behavior in either direction"*.
That is the 2026-07-30 absolute-floor hole re-opened from the other side — instead of a collapse cancelling
against itself, **the bar that would have caught it is never applied**.

A **skipped** bar is not a **held** bar. So the verdict now carries a machine-readable `unmeasured` list
(one entry per skipped bar, naming the capability and why it could not be measured); a non-empty list makes
the outcome `inconclusive`, the reason is printed on the report and archived in the record, and the process
exits non-zero. A run that both regressed **and** measured nothing is a **FAIL** — the regression is the
provable defect and is never softened into "inconclusive". The fix for an inconclusive run is to restore the
measurement (refresh/label the corpus with live action-warranted incidents), never to loosen the gate.

**Two capabilities can go unmeasured, and both block.** The second is the **negative-control bar**, which
exists only if a control arm was supplied: with no `--controls`, `Compare` sees no violations because it was
asked no questions, and `eval/eval-gate.sh` appends the flag *conditionally* (`[ -f "$cand_ctrl" ]`), so a
candidate arm whose controls run produced no file used to be certified having never been asked whether it
proposes on benign incidents. A change/trend invocation with **no control arm** is now `inconclusive`
(exit 3), named in the report as `UNMEASURED: negative controls`. If a control arm is legitimately
unavailable, the run does not become certifiable — measure it or accept that nothing may be merged on it.

**These are executed claims, not descriptions.** `tools/evalgate/exit_test.go` re-execs the test binary as
the CLI and asserts the **process exit status** of the real `main()` on the archived 2026-07-30 record
(exit 3, `GATE: INCONCLUSIVE`, never the PASS headline), on a measured clean pair (exit 0 — the gate can
still certify), on a missing control arm (exit 3), on a control violation (exit 1), on a short/degraded arm
(exit 2), and on a trend night that must **not** ratchet the committed baseline. Verifying the exit-code
*table* is not the same as verifying it is *wired*: with the change-mode `os.Exit` deleted, the unit suite
stayed green while the CLI printed `do NOT merge on this verdict` and returned success.

The comparison is a **pure function** — `eval/gate.Compare` / `gate.CompareToBase` — unit-tested in
`eval/gate/gate_test.go` with tables against **both** comparators (clean pass, overall-fail, single-dim-fail,
safety-dim-fail, noise-within-bar, pooling rescue, control violation, **unmeasured-is-inconclusive — fed the
real 2026-07-30 `eval/history` record back through the gate**), plus a `change`-vs-`trend` test proving
the committed baseline is the comparator **only** in trend mode, and `VerifyIntegrity`/`VerifyComparable`
tests proving a degraded/429 arm is rejected before it can enter the pool. That logic is the CI-testable
heart of the gate (no gateway needed).

### Byte-identity waivers cover prompt bytes only

A **byte-identity waiver** — the C-2/TG-42/TG-215 precedent: an `Eval-Gate-Waived-By:` trailer justified by
golden tests proving the composed seed/preamble is unchanged for every reachable class — certifies exactly
one thing: **the prompt BYTES the model receives did not move**. It does **not** certify that the changed
CODE PATHS cannot error a live session. A refactor can keep every golden green and still panic on an input
shape the fixtures never build, nil-deref on an unwired seam, or time out a construction step — and a
session that **errors** never composes a prompt at all, so byte-identity goldens are structurally blind to
it. A waiver on those grounds must therefore:

1. **NAME the tests that execute the changed code paths** — not the goldens alone; the specific units/wired
   oracles that drive the new/edited code with its real inputs, cited in the waiver trailer's commit body; and
2. where the change touches **session construction or dispatch** (how a session is assembled, threaded, or
   launched — not merely which bytes it renders), carry **one live smoke-session run** on the box alongside
   the goldens, so "a session still completes end to end" is an observed fact, not an inference from
   unchanged bytes.

**Why this is written down (2026-08-14).** Two byte-identity-waived merges landed the same day the nightly
base arm degraded. The merges were **exonerated** — the causes were a shared tunnel port collision plus an
sshd `MaxSessions` ceiling (TG-493) — but the diagnostic gap was real: nothing in the waiver evidence could
have distinguished "my code errors live sessions" from "infrastructure", because unchanged prompt bytes say
nothing about whether sessions still *run*. The two requirements above are what would have answered that
question the same hour it was asked.

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

## The mechanical security-escalation check (TG-533, 2026-08-25)

Two corpus incidents (`tg533-confighash-01`/`-02`) exercise the armed TG-466 confighash path the judged
dims were structurally blind to: the harness wires the attribution seams ONLY for an incident carrying
`confighash_changed` (production's default ruleset, a covered-but-empty actor reading, a host-scoped
in-process answer — never a live PVE or Postgres), and `SecurityCheck` grades the outcome mechanically
OUTSIDE the judged dims and `overall`: the confirmed-mutation incident MUST escalate
attributed-suspicious/POLL_PAUSE, and its `changed=false` twin MUST NOT (the spurious-suspicion control —
the check cannot be satisfied by escalating more). The checked denominator is printed; an arm whose corpus
has no opted-in incident reads `0 checked`, never a pass. Do NOT fold this into `overall_formula` — a
taxonomy check widening a judged denominator is the `diagnosis_grounded` trap.
