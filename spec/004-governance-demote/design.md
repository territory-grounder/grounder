<!-- spec/004 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/004 — Design: governance auto-demote + judge-death detection

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent.

## Components

Two Temporal Schedules under `temporal/governance` drive pure
Go decision functions in `core/governance`. `CreateSchedules` registers both and is genuinely IDEMPOTENT —
an already-existing schedule (`ErrScheduleAlreadyRunning`) is skipped, never fatal — so a
call-on-every-startup reconcile never crash-loops AND never aborts before ensuring the LATER (judge-liveness
dead-man) schedule: a "return on the first error" loop silently dropped judge-liveness whenever
governance-metrics already existed, leaving the judge-death control uninstalled.

- **`RepeatOffenderActivity` → `Demote`** (`core/governance/repeat_offender.go`,
  `core/governance/demote.go`) realizes REQ-301..304. It replaces the predecessor
  `scripts/write-governance-metrics.py` — re-expressed, never copied — as a typed activity that reads
  the per-incident, best-outcome close-out rows produced by the BEH-3 reconciler (spec/003) and the
  org policy store, and writes demotion policy rows plus generated metrics. The `MemDemotionStore` oracle is
  concurrency-safe (a mutex guards its map) — the demote and read activities share it.
- **`JudgeLivenessActivity`** (`core/governance/judge_liveness.go`) realizes REQ-305/306. It replaces
  the judge-death signal that lived inside `scripts/llm-judge.sh` and `judge_scored_fraction()`,
  re-expressed as a monitor that reads only judge-independent session-outcome tables.

## Decision procedure — repeat-offender demotion (REQ-301..304)

1. Group the recent close-out rows by `(host, alert_rule)` over a rolling 30-day
   window, counting per incident rather than per event so an alert storm cannot inflate the count
   (the count-incidents-not-events discipline carried from BEH-3). A tuple with three or more incidents
   is a **demote candidate** (REQ-302); two or fewer is not.
2. Drop any candidate that is an **intentional known-transient** — a tuple tagged expected or
   known-benign for the organization in the policy store (REQ-303). Its recurrence is by design, so demoting
   it would re-introduce the very noise the organization chose to suppress.
3. For each surviving candidate not already carrying a live demotion, write one demotion policy row:
   `analysis-only`, `demotion_reason = pattern_repeat_3plus`, `valid_from = now`,
   `valid_until = now + 30d`. Tier-1 suppression (spec/005) reads this row and **escalates** the tuple
   instead of suppressing or auto-resolving it (REQ-301).
4. A demotion carries its own expiry: once `valid_until` passes, the tuple is eligible again without any
   human action (REQ-304). The circuit-breaker is the metric, the audit record, and the expiry — the
   read path treats an expired demotion as absent.

The activity emits generated metrics (candidate count, demoted-pattern count, an `autodemote_enabled`
gauge, and a last-run freshness stamp) from one typed source (INV-15); a staleness alert fires if the
schedule stops writing.

## Decision procedure — judge-liveness (REQ-305/306)

The monitor counts the sessions that **ended** inside the evaluation window — a two-sided bound: recent
enough (`< Window` old) to still matter, AND lagged enough (`>= Lag` old) for the judge's cadence to have
run. A session that ended within `Lag` is NOT yet judgeable and is excluded, so a burst of just-ended
sessions cannot depress the fraction and page a healthy judge as dead. Synthetic rows are excluded. For each it asks whether a real judgment (a non-negative `overall_score`) exists — reading the
judgment existence from a table the judge writes, but taking the **denominator population** from the
session-outcome tables the judge holds no write grant on, so the judge cannot enlarge or shrink its own
eligibility set. The judged fraction is that ratio. When the eligible population exceeds three and the
fraction is below 50%, the monitor raises a judge-death warning through the escalation module
(REQ-306); at three or fewer eligible sessions the sample is too thin to page and the monitor stays
quiet.

## Frontier cross-check — the no-human eval anchor (REQ-305)

Judge-liveness catches a judge that STOPS writing real scores (the judged fraction drops). It cannot catch
two failure modes the **frontier cross-check** (`core/governance/frontier_crosscheck.go`, ported from the
predecessor `judge-frontier-crosscheck.py`) exists for: **DRIFT** — the local judge keeps scoring, so
liveness reads healthy, but its verdicts have silently gone wrong — and **confirmed DEATH** — the local
judge returns unscored (`-1`) rows for sessions a frontier model, re-judging over the SAME rubric, scores as
genuinely judgeable (a dead judge that still writes `-1` rows never looks "dark" to any purely-local metric;
this is the exact 3-week dead-judge class). `FrontierCrossCheckMonitor.Evaluate` is a pure decision over
local↔frontier `CrossCheckPair`s. The two branches mirror the predecessor's `JudgeFrontierDrift` rule
(`… agreement_rate >= 0 and < 0.6 and pairs > 5) or local_unscored_rate > 0.5`) exactly: **DEATH** when the
frontier-scored-but-locally-unscored fraction STRICTLY exceeds `CrossCheckDeathFraction` — with **no sample
gate**, so a dead judge pages even on a thin window (a min-sample gate here produced a false negative on the
exact 3-week dead-judge class, and `>=` fired one step early at the 0.5 boundary); **DRIFT** when the TOTAL
sample exceeds `CrossCheckMinSample` (the predecessor's `pairs > 5`, so `CrossCheckMinSample = 5`), at least
one pair is comparable (both-scored — the `agreement_rate >= 0` guard), and that agreement rate falls below
`CrossCheckAgreementFloor`. DRIFT does not fire on a sub-minimum sample; DEATH is ungated. The frontier
re-judgment I/O is confined behind the injected `PairSource`, so the
anchor's decision is deterministic and oracle-tested; a warning routes through the same escalation module and
fails open (a broken alerting path must not mask a dead judge).

## Persistence & audit

Demotion decisions and judge-liveness facts are appended to the immutable audit spine and hash-chained
into the governance ledger (INV-19); the raw judged transcripts and scores live in the purgeable
operational store under a retention TTL and right-to-erasure. The two stores are drawn separately: a
transcript purge cannot erase a governance decision, and the audit spine cannot be edited to un-demote a
tuple. Every row is authority-checked against the acting user/role under RBAC (INV-12) and `schema_version`-stamped (INV-15).

## Out of scope

The reconciler close-out rows that feed the recurrence count are owned by spec/003 (BEH-3). The Tier-1
suppression chain that consumes a demotion policy row is spec/005 (BEH-5). The judge itself — the model
call that writes a judgment — is an evaluation concern outside this family; this spec measures only
whether that judgment is present, never its content.
