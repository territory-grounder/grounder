<!-- spec/025 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/025 — Design: governing the measurement plane

How the requirements map onto the existing harness. Everything here is read-only and additive: no governed
behavior, decision path or safety gate changes. What changes is that the code producing the v1.0 claim
becomes bound, tested and CI-covered like the code it measures.

## 1. Binding the surfaces (REQ-2500) — `spec/.lockstep.lock`

The proof plane is bound to this spec exactly as the product plane is bound to its own:

| surface | what it computes |
|---|---|
| `core/db/axis_read.go` | every axis's SQL — the single largest measurement surface |

**A5 capability breadth counts BOTH autonomous rungs.** `GraduatedOpClasses` answers "what can TG heal
without a human right now", and that is `policy_graduation.level IN ('auto', 'auto_notice')` — not `'auto'`
alone. `core/policy.Level.Verdict` grants both rungs the `auto` verdict; the notice is applied downstream
as a band floor, so a class at `auto_notice` acts without a vote exactly as one at `auto` does.

This is stated here because the narrower query was the original, and its error was systematic rather than
occasional: `auto_notice` is a mandatory intermediate rung (spec/028 REQ-2808) every class holds before it
may reach silent `auto`, so filtering it out understated capability precisely for newly-autonomous classes
— the ones a capability axis is read about. The reverse error is worse: counting `approve` (or any rung
that permits at most `approve`) would claim TG can act without a human where it cannot.
| `cmd/axisscore/` | the scorecard: axis rendering, denominators, bounds |
| `tools/shadowbench/judge.py` | the blind head-to-head cards, budgets, winner rule |
| `tools/shadowbench/_driver.py` | pair alignment and the rolling aggregate; stamps each PAIR record with the supply-join fields (§7b), the ladder-tier tag (§7d); campaign scheduling knobs (§7c) |
| `tools/faultinjector/` | the evidence generator that mutates production |
| `tools/evalgate/` | the quality gate |

Binding is the mechanism REQ-2500 asks for: an axis cannot be redefined without the hash moving, and the
lockstep gate already fails a change whose owning spec was not updated. This costs a spec amendment per
measurement change — deliberately, because a measurement change IS a change to the claim.

## 2. Golden fixtures over a real Postgres (REQ-2501) — `core/db/axis_read_test.go`

The measurement SQL is tested against a real database with every migration applied, not a fake. A pgx fake
has already hidden a field-drop in this repository once; measurement SQL is exactly where a stub is
worthless, because the defects are in JOIN semantics, `DISTINCT ON` ordering and NULL handling — none of
which a fake reproduces.

Each fixture is small and hand-computed: a handful of rows whose expected axis value a reader can verify by
eye. Expected values are written from the fixture, never captured from the implementation, so a wrong
implementation cannot bless itself.

**The mutation control is the load-bearing half.** For each covered axis the test suite perturbs one
predicate of that axis's query and asserts the golden test goes RED. A test that stays green under a
deliberately broken query proves nothing, and the project has already shipped tests that passed vacuously —
the protected-paths gate ran on every main commit for weeks while examining nothing.

Gated on a DSN: `TG_TEST_DSN` present ⇒ the suite runs; absent ⇒ it SKIPS with a clear message. CI supplies
a `postgres:16` service so the skip is a local convenience, never the CI behaviour (REQ-2507).

## 3. Denominators, exclusions and bounds (REQ-2502/2503) — `cmd/axisscore`

The scorer already prints most denominators. This spec makes the remaining honesty properties structural:

- an axis that EXCLUDES unmeasurable rows prints the excluded count and why (A6b already does: "n=50 of 57
  mutated incidents", plus its correlation method inline);
- a zero-numerator axis prints its rule-of-three upper bound alongside the zero — at n=12, "0 false
  actuations" honestly reads "≤22% at 95%", and publishing the bare 0 overstates the evidence;
- an axis whose join is a correlation rather than a key states the method and the bound in the output.

### 3.1 A6b correlates by RULE FAMILY, not by rule string (2026-08-01)

The time-to-recovery correlation matched `x.alert_rule = t.alert_rule`. That is exact string equality, and
it silently excluded the commonest incident class in this estate: `modules/ingest/pveliveness` raises under
TG's own label `Device-Down`, while captured recovery transitions carry LibreNMS spellings
(`Devices-up/down`, `Device-Down-SNMP-unreachable`, `Device-Down-Due-to-no-ICMP-response.`). The two
vocabularies never intersect, so every incident of that class correlated to nothing.

The failure mode is the one this whole spec exists to prevent, in its most flattering direction: those
incidents were not counted as SLOW, they were **absent from the denominator**. A6b therefore looked better
the more of this class occurred, and its printed "n=50 of 57" honestly reported an exclusion whose cause
was a vocabulary mismatch rather than a missing recovery.

The recovery belt (`core/db.TransitionLogStore.RecoveredSince`) was fixed for exactly this on 2026-07-30;
this query was not. The two then answered different questions about the same pair of rows — which is the
drift `knowledge.CanonicalRule` was made the single authority to prevent.

The fix folds both sides through that one authority (`rulefamily.json`) via a `fam(alias, canon)` CTE built
from the new `knowledge.RuleFamilyPairs()`, and compares `COALESCE(fx.canon, lower(btrim(x.alert_rule)))`
against the same expression for the triage side. `COALESCE` preserves the previous behaviour **exactly** for
any rule in no family — it falls back to its own lower-cased identity — so this narrows nothing and widens
only within a deliberately reviewed family that excludes `TargetDown` and `Device-rebooted`.

The predicate stays the interpolated constant `healCorrelationMatch`, so the REQ-2520 mutation control
continues to perturb the shipped text rather than a paraphrase.

### 3.2 A6 is SPLIT: A6a = steps (gate-side), A6b = wall-clock (reported) — REQ-2525, TG-205 (2026-08-04)

`docs/BENCHMARK-AXES.md` defined A6 as MTTR while every implementation measured decision STEPS. The JSON keys
had already been renamed `a6a_*`/`a6b_*` on 2026-08-01, but A6b held only TIME-TO-RECOVERY (§3.1) — the leg
dominated by the monitoring system's recovery poll and by the provider. **TG's own reasoning time was still
measured nowhere**, so the axis published how many cycles a decision cost and nothing about how long it took.

The number already existed in process: `temporal/runner/activities.go` times the agent loop as `loopDur` and
hands it to `observe.RecordAgentLoop`, which accumulates into the single counter `tg_agent_run_seconds_total`
— a running sum with no distribution, no per-incident attribution, and a reset on restart. A total is not a
measurement of a decision. It was dropped at the DB boundary, exactly as `step_count` was before migration
0037.

So the value is now persisted per incident (`session_triage.decision_ms`, migration 0058) and carried by the
same path as every other terminal-record fact: activity result → `RunnerResult` → `judge.TriageRow`. The copy
sits **before** the propose/stop branch, so a grounded stop — the commonest terminus — records it too;
instrumenting only the propose path would bias the published median toward the branch that happened to be
edited (the same failure TG-198 and TG-201 each had to place their copies above the branch to avoid).

`AxisAgg` gains `DecisionN` / `DecisionMedianMs` / `DecisionP95Ms`: percentiles (never a mean — one gateway
stall drags an average arbitrarily) over `decision_ms > 0`. That filter is load-bearing rather than tidy:
every pre-0058 row carries the column default 0, and pooling them puts the median among the zeros, i.e.
publishes "TG decides instantly" for a population that recorded no time at all. The exclusion is printed as
the gap between `n` and the window's incident count (REQ-2502), and a window with nothing timed is a NAMED
coverage gap rather than a 0.0s median.

Both surfaces carry it, from the SAME aggregate (REQ-2524): the `axisscore` scorecard
(`a6b_time_to_decision_median_ms` / `_p95_ms` / `_n`, plus a seconds-rendered text block) and `/metrics`
through the already-armed axis sampler (`tg_axis_decision_latency_p50_seconds`, `_p95_seconds`,
`tg_axis_decision_measured_total`, emitted only when something was timed). The scorecard's existing
`a6b_n` key is left meaning the time-to-RECOVERY denominator, so an artifact written before this change still
reads correctly; the decision leg carries its own explicit `_n`.

A6a is untouched as the change-gate figure. The rejection of wall-clock as a merge bar stands — this leg is
REPORTED, and what it does not cover (the ingest→workflow-start leg) is stated wherever it is published, so
it reads as a lower bound on alert→decision rather than an end-to-end MTTR.

## 4. Judge symmetry and rubric identity (REQ-2504)

Already largely implemented in `tools/shadowbench/judge.py`: one rubric source (`core/judge/rubric.json`)
read by both the Go and Python surfaces, one shared trajectory budget applied to both cards, comparable-only
aggregation, and a like-for-like winner rule. What this spec adds is the REQUIREMENT, so those properties
cannot be quietly removed, plus `rubric_version` stamping so historical verdicts remain attributable to the
rubric that produced them.

**Delivered 2026-08-03 (TG-194, migration 0052).** `core/judge/rubric.json` declares a top-level
`version`; a bump-enforcement test pins version→content-hash so an un-bumped rubric edit is a red build.
Every `session_judgment` row carries `rubric_version` (empty = judged before versioning — never
backfilled with a guess); the pooled MEANS (`axis_read` A2 dimension means, the flywheel's
`DimensionMeans`, trial `armScoresForDim`) combine rows from exactly one version — the running binary's —
while judged COUNTS stay version-blind, since "was it judged" is rubric-independent. Scorecards carry
`rubric_version` and `gate.VerifyComparable` refuses arms judged under different rubrics.

### 4.1 The judge survives a primary-model outage (TG-72, 2026-08-15)

The judge runs behind a LiteLLM gateway that fails over when the primary reasoning model (kimi-k3) is
down. The two tiers place their chain-of-thought differently: kimi-k3 emits it in `reasoning_content`
(the verdict JSON is the whole `content`), while the failover model (deepseek-v4-pro) emits
chain-of-thought IN `content` before the JSON. A tight completion budget therefore truncated the
failover reply mid-reasoning, before the verdict closed, and EVERY judged pair silently degraded to
`judge_unavailable` for the whole outage — a measurement gap that read like a code fault, not a
provider one. So `judge.py:_completion_content` gives the reasoning tier enough `max_tokens` headroom
for chain-of-thought AND the verdict, and DIAGNOSES a `finish_reason=length` truncation as truncation —
naming the model actually served (§5 / REQ-2505's manifest rule) — rather than surfacing it as an
opaque "no JSON object". A reply that closed its verdict before the cap is still parsed normally: the
truncation branch defers to the real parser (`parse_verdict`), so it never discards a recoverable
verdict. This keeps REQ-2504's "one judge, one meaning" honest under failover — an outage shows as a
NAMED unavailable window, never as a phantom clean verdict.

## 5. Evidence provenance and independent n (REQ-2505/2506)

The committed evidence artifact carries its manifest (code revision, rubric hash, model actually served,
window, hosts). Clustered observations report independent n: the head-to-head's own analysis is the worked
example — 21 rows, six independent incidents, one contributing seven.

## 6. Gates that can fail (REQ-2507) — `.gitlab-ci.yml`, `scripts/lint-forbidden.sh`

A `harness` job runs the proof plane's tests on every change to it. The forbidden-pattern lint extends over
`tools/` (today explicitly excluded), so the harness cannot carry a shell-built command or an embedded
secret that the runtime is forbidden. A gate whose inputs are missing FAILS rather than exiting 0.

## 7. The injector's obligations (REQ-2508) — `tools/faultinjector`

Already implemented (roadmap P0-5, migration 0041) and restated here as a requirement so the properties are
governed rather than incidental: durable-record-before-effect, reconcile on start/cycle/shutdown, refuse on
an unobservable estate, refuse a target the system under test cannot act on, never stack a fault on a target
that owes a restore, and quarantine rather than assume on an unverified discharge.

**Refusing on an unobservable estate retries a transient read first (TG-544).** The per-tick cluster
snapshot (`pvesh get /cluster/resources`, the estate's observability) is read through a bounded retry:
the PVE API returns a transient non-zero often enough that a single-shot read skipped ~40% of ticks
during campaign #3 (2026-08-26 soak.log), halving the inject rate for no safety benefit. The read now
retries a TRANSIENT transport failure — an SSH/pvesh error or a non-zero exit — a bounded number of
times with a linear backoff before it refuses; a malformed-JSON parse is NOT retried (a deterministic
condition, surfaced immediately, never masked). The refusal is unchanged once the retries are exhausted:
the tick still skips rather than fault blind, so the "refuse on an unobservable estate" obligation holds
— it is only made resilient to a flapping API, not weakened.

### 7a. The rotation cursor is the campaign's liveness, and it must not be the campaign's success counter

**Incident, 2026-07-29 02:19Z-09:34Z.** The estate campaign selected ONE class — `log-fill` — on 148
consecutive cycles and injected nothing at all. Six classes were configured. Alert volume reaching TG fell
from ~25/hour to ~1/hour, and every axis kept being computed over that window as though the campaign were
healthy.

One conflated counter caused it. The planner chose the class with `rotation[Injected % len(rotation)]`, and
the engine advanced `injected` only when an injection actually LANDED. A class that could not act therefore
left the cursor exactly where it was, and the next cycle chose it again — permanently. Nothing about this is
specific to `log-fill`: it is a property of the cursor, so ANY class that stops being satisfiable acquires the
whole campaign. `log-fill` merely happened to be the first class the pool could not satisfy.

So the two counters are now distinct and each answers exactly one question:

- **`Cycle`** counts TICKS, landed or not. It drives the class rotation and the pool sweep. It is the
  campaign's liveness.
- **`Injected`** counts faults that LANDED. It answers only "has the campaign reached its target".

Fixing the class-eligibility check alone would have made this WORSE, not better: the planner would have found
no eligible guest, returned `Act=false`, left the cursor frozen for exactly the same reason, and gone
completely SILENT rather than logging a loud abort every three minutes.

**A barren campaign must announce itself.** Each of those 148 cycles logged a correct and reassuring sentence
("provably nothing was broken"). The SEQUENCE was the finding and nothing named it, so the engine now emits an
escalating `CAMPAIGN BARREN` line at powers of two, stating that any axis measured over the window is
unpopulated rather than a result. Per-cycle reasons remain — they are how the cause is diagnosed.

**The suite was green throughout**, because every planner oracle called `PlanNext` once. A planner defect that
only exists ACROSS cycles cannot be found by a test that never takes a second cycle.

### 7b. Confirmatory supply plumbing — joining the injector's ground truth to the harvest

The continuous evidence path is: the injector engine soaks 24/7 (§7) → both systems triage the provoked
alerts → the nightly `run.sh` harvest judges and appends scorecard PAIR records. That path produced pair
records but NO campaign-manifest entries joining them to the injector's ground truth — only the retired
`campaign.sh` orchestrator ever wrote PAIRED manifest entries — so `accrual.py` reported 0/30 forever while
the estate looked busy. A supply meter that cannot see the supply is the same defect shape as §7a's barren
campaign: every component healthy, the composition producing nothing, and nothing saying so.

`tools/shadowbench/reconcile-supply.py` closes the gap. It reads the injector's durable ledger
(`injected_fault`, the same table the A1 scorer joins on — read-only, over `extract_tg.sh`'s SSH
conventions) and joins each post-freeze fault to a scorecard PAIR record on (host, fault
injection→restore+slack window, §3a coarse fault class), nearest-in-time, one record per fault. The class
mapping is a COMPOSITION of two existing declarations — `core/db/axis_read.go`'s `detectRuleMatch` (injector
vocabulary → monitoring rule family) and `_driver.fault_class` (rule family → §3a coarse class) — so the
reconciler declares no vocabulary of its own. `_driver.py` stamps each pair record with the join fields
(`fault_class`, `tg_created_at`, `pred_first_ts`) at harvest time, because the judge verdict alone carries
neither a class nor an incident time.

Matches are appended to the committed `tools/shadowbench/confirmatory/manifest.jsonl` (append-only,
idempotent on (fault id, scorecard key)); `accrual.py` counts BOTH that manifest and the legacy campaign
manifest. Unmatched post-freeze faults and unmatched pair records are printed one-line-each with reasons —
the pre-registration's no-silent-exclusion rule. Both tools remain structurally verdict-free
(`test_accrual.py`, `test_reconcile_supply.py`): supply plumbing may improve mid-campaign precisely because
it can never reach a conclusion; the conclusion comes only from the frozen `analyze.py`.

### 7c. Campaign scheduling knobs (2026-07-31) — throughput without touching the pairing rule or scoring

Three compounding scheduling defects starved confirmatory pairs while both systems triaged perfectly:
pairs judged oldest-first (fresh bankable pairs missed their fault window by a full cycle), the serial
judge burned most of each cycle on single-sided records that §3 excludes from the bar by definition, and
one judge subprocess at a time capped throughput. `_driver.py` therefore carries three env knobs, all
harness-side SCHEDULING only — `align()`'s deterministic pairing rule and `judge.py`'s scoring are
byte-identical whatever they are set to:

- **newest-first**: pairs sort by TG triage time (else predecessor first-ts) descending before judging,
  so the pairs that can still bank against a live fault window are judged first.
- **`SB_PAIRS_ONLY=1`** (campaign cycles): singles (`pred_only`/`tg_only`) are DEFERRED, not lost — the
  scorecard is append-only and dedup-idempotent, so the next unflagged run judges them.
- **`SB_JUDGE_WORKERS=N`** (default 1 = legacy serial): up to N concurrent `judge.py` subprocesses;
  scorecard appends stay serialized in the main thread so the ledger remains a clean line stream.

Verdict CONTENT per pair is unchanged by construction; only wall-clock ordering moves. `run-campaign.sh`
sets `SB_PAIRS_ONLY=1 SB_JUDGE_WORKERS=3` for campaign cycles and threads `ACCRUE_FROM` (the §6
comparator-change boundary) into reconcile and accrual.

### 7d. Per-tier record tag (TG-72) — additive selection metadata, never analysis input

The Benchmark Ladder (docs/BENCHMARK-LADDER.md) reports results per rung — "at Tier N, TG vs
predecessor = X" — but scorecard records carried no tier identity, so a Tier-2 pair (ambiguous
multi-signal, `tools/shadowbench/tier2-run.sh`) would have been indistinguishable from the Tier-1/campaign
population it must never pool with. `_driver.py` therefore stamps every appended record with a `tier`
field, read from `SB_TIER` at harvest time (default `"1"`).

Three properties keep this additive rather than analysis-affecting:

- **Absent means "1"**: every record written before the field existed is a tier-1-era record, so old
  scorecards stay valid unmodified and no backfill ever rewrites the append-only ledger.
- **The frozen analysis never sees it**: `analyze.py` (sha-frozen, PRE-REGISTRATION.md) ignores unknown
  fields; the tag steers tier-scoped SELECTION only. Promoting it into the confirmatory analysis would be
  a governed `Law-Change-Approved-By` change to the frozen file, exactly like any other.
- **An explicit tier is never overwritten**: the stamp uses setdefault, so a harness that computes its
  records' tier itself (as the tier-2 scorecard does) keeps its own value.

The Tier-2 harness itself (`tier2-run.sh`) follows the tier-1 shape — inject, sanctioned detection, strict
same-class pair select via `_driver.fault_class`, blind judge — with the two-fault ledger discipline
(durable-record-before-effect into `injected_fault`, both obligations recorded before either effect) and a
refuse-to-score half-injection exit, and emits its own `tier: "2"` scorecard documents under
`tools/shadowbench/out/`; those are run RECORDS, not scorecard-ledger pair records, so they carry the tag
directly.

## What this design does NOT do

It does not change any axis's value. Every requirement is about making the existing computation attributable,
tested and honest about its own population. Where a number changes as a consequence of coverage (as A1 and A7
will when their definitions are corrected), that is a restatement with a published reason — not a silent
edit, which is precisely what this spec exists to prevent.

## 8. Coverage of the unmeasured (G7, TG-180, 2026-08-22) — `core/axis`, `core/db/axis_read.go`

The axis aggregate gains one report-only input: the latest observation-census snapshot (migration 0106,
`observation_coverage`, appended by the worker's census job each refresh — total / observed /
healthy-quiet / unobservable / probe-confirmed / probe-armed). `AxisAgg.Coverage` is read FAIL-SOFT in
`Aggregate` — nil when no snapshot has ever been recorded, and a read error never fails the eight scored
axes. The scorecard renders it as G7 with three honest states: no snapshot ⇒ a named gap; a snapshot with
an unarmed probe ⇒ the denominator is measured and the numerator is rendered as the rule-of-three bound,
never as 0 % coverage; an armed, partially-confirmed probe ⇒ the ratio. G7 lives in `core/axis`, which is
structurally outside `gate.Dimensions`, so it can never bar a merge — it informs. Census = hypothesis,
probe = test (TG-180's own null-test), and the denominator now counts every host-like entity the estate
graph knows (`FreshObservableNames`), not only `TypeHost`.
