# 2026-10-22 — judge-tier A/B on the same live sessions (TG-584)

**Question.** The eval judge and the 2-hourly session judge both ran on `primary` — the agent's own decision
tier — while the recorded ruling (2026-08-31) says the judge is Opus (`judge` alias), chain-free. Does the
tier change the measurement?

**Method.** `tools/rejudge` DB-replay mode over the newest 120 `session_triage` rows of the trailing 7 days,
run twice with the current rubric (`2026-08-04.3`): `-model primary`, then `-model judge` (both through the
box gateway; `-scores-out` pairs the per-session scores by ref). Aggregates are the two `*.scorecard.json`
files here; the paired statistics are `summary.json`. Per-session scores and judge comments are NOT
committed (they name estate hosts); the derivation is `tools/rejudge` + the pairing described in TG-584.

**Result.** `primary` scores bimodally (1-or-5 on every judged dimension; 28/120 sessions identical on all
five; sd 1.6–1.9). `judge` grades (mode 4, 1s for genuine failures; 5/120 identical; sd 0.6–0.8). Paired
agreement on `appropriate_band` is 9/120; Pearson r of the per-session mean is 0.457. The largest
disagreements are cascade dedup stand-downs the `primary` tier scores ~1.0 ("did nothing") and the `judge`
tier scores 3.6–4.2 with the deferral named. Same sessions, same rubric: this is a property of the judge.

**Consequence.** `core/judge/rubric.json` `params.model` is now `judge`, and the rubric version is bumped to
`2026-10-23.1` (TG-194: one rubric version per pool), so every pooled statistic — the flywheel's regression
means, trial fill rates, the nightly trend baseline — re-anchors on new-version samples instead of mixing two
judges. The mean shift (+1.0…+1.4 on the graded dims) is a MEASUREMENT change: no agent improvement may be
read from it, and the committed baseline is not comparable to Opus-judged numbers until the trend-watch
refreshes it.

**Not yet measured.** A judge-calibration record (`cmd/judgecal`, TPR/TNR/κ against the fault-injector truth)
for the new tier needs Opus-judged `session_judgment` rows, which the cron produces only after this deploy;
TG-584 stays open until that record exists.
