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
| `core/axis/benchmark.go` | the G4 benchmark-window report: per-site n against the bar, rubric/harness uniformity (TG-632) |
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
aggregation, and a like-for-like winner rule. The judge TIER is part of that identity: every judging surface
— the Go eval harness, the session-judge cron, `tools/rejudge`, `judge.py` and the shadowbench drivers'
`MODEL` default — takes `params.model` from the rubric (the `judge` alias since TG-584, 2026-10-23; before
that the agent's own `primary` tier, which scored bimodally — see `eval/history/2026-10-22-judgecal-tier-ab`).
A tier change is a measurement change and moves `rubric_version` with it. The one deliberate exception is
`judge-calibrate.py`'s FROZEN anchor, which names the historical judge of the campaign records it calibrates
against and must not follow the current tier. What this spec adds is the REQUIREMENT, so those properties
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

## 9. Device-down on VMs (TG-624 G3, 2026-11-06) — `tools/faultinjector`

The injector could only stop containers (`pct`), so TG's start-guest heal had a qemu path that no live run
had exercised. A guest's KIND is now resolved on the owning node at name-assert time and never declared in
the pool file: `AssertGuest` reads `pct config` (a container names itself with `hostname:`); when that exits
non-zero it reads `qm config` (a VM names itself with `name:`); a vmid neither CLI knows is refused. The same
name-assert safety holds — the fallback widens what can be asserted, never what can be acted on blind, and a
container's call sequence is unchanged. The kind drives every CLI call of the fault (`pct|qm stop`, the timer's
`pct|qm start`, the reconcile repair and the `pct|qm status` verify) and rides in the ledger's device-down
`fault_ref` (`qemu:<vmid>` for a VM; a bare vmid stays a container), because a reconcile after a restart
restores from the row alone. Every row written before this change keeps restoring with `pct start`.

## 10. Hang faults for the restart verbs (TG-624 G3, 2026-11-06) — `tools/faultinjector`

TG answers a STOPPED container or unit with its literal inverse (start-container / start-service), so the
restart verbs had no fault TG would heal with them, and G3 requires 15 verified runs of each. Two classes freeze
a workload instead of stopping it: `container-hang` (`docker pause` on the declared container) and
`service-hang` (SIGSTOP to every process of the declared unit via `systemctl kill`). Both stay "up" (Paused /
active) with a port that accepts connections nothing answers, so the service probe alerts and only a restart
heals them. Each provokes ONLY its restart verb (opcover), owes a restore on the guest (`docker unpause` /
SIGCONT), and keeps the declaration rule (no declared container or unit, not eligible). A service-hang repair
is verified by the main process's state, not by `systemctl is-active`, which reads active for a frozen unit; a
unit with no main PID does not verify (fail closed).
For A1, both hang classes map to the Service rule in `core/db` `detectRuleMatch`, as their -down twins do;
`TestEverySchedulableFaultClassHasADetectionMapping` fails a class that ships without one.
SIGCONT cannot start a unit, so the reconciler runs an idempotent follow-up `systemctl start` for a
service-hang (a unit can go inactive while frozen); and a unit with no running main process is refused before
any effect, since it could never be verified thawed. Declared unit and container names, and the fault_ref read
back into an undo, must be plain tokens (the actuation leaf's unit shape): they reach a remote shell.

## 11. Per-site, fixed-window scoring and the G4 benchmark-window report (TG-632, 2026-11-06) — `core/db/axis_read.go`, `core/axis/benchmark.go`, `cmd/axisscore`

The G4 bar ("TG v1 graduated", owner-ruled 2026-11-05) is: every axis A1–A8 has **n ≥ 30 per site** (`dc1`,
`dc2`) in **one window of at least 14 days**, under **one judge rubric version** and **one harness
fingerprint**, and `cmd/axisscore` reports PASS. The scorer could only score the whole estate over a trailing
window, so it could not say whether the bar was met.

**One aggregate, scoped.** `AxisReadStore.AggregateScoped(ctx, AxisScope{Since, Until, Site, Rubric})` runs every
axis query over a `[Since, Until)` window (zero `Until` = open) and, optionally, one declared site.
`Aggregate(ctx, since)` is now exactly `AggregateScoped(ctx, AxisScope{Since: since})`: with `Until` NULL and the
site arm off, every query selects the rows it always did, so the worker sampler, the console and plain
`axisscore` are unchanged. The scope is four bound parameters in a fixed order (`$1` since, `$2` until, `$3`
scoped, `$4` the site's raw spellings or fault ids) inside named constants (`triageInScope`, `latestInScope`,
`faultInScope`) — no scope value ever becomes query text, and the mutation controls perturb those shipped
constants (REQ-2512).

**How each axis is attributed to a site — a stated join, never an inference:**

| population | axes | site comes from |
|---|---|---|
| `session_triage`, `session_risk_audit` | A2 judge, A3, A4 (+ composition), A5 exercised, A6a, A6b both legs, A7 | the incident's `ingest_alert` row by `external_ref` (unique there) — the ingest-boundary stamp |
| `injected_fault` | A1 recall + detection latency | the injector's OWN record: the Proxmox `node` that owns the guest, else the `host` name |
| `action_verdict`, `infragraph_prediction`, `action_execution` | A2 verified outcome + blast radius, G6 | their own `site` column |

Every raw site value is folded through `core/ingest.CanonicalizeSite`, the one vocabulary authority (pre-TG-456
spellings such as `NL` still belong to their site), and a value outside `ingest.DeclaredSites()` — an Alertmanager
label like `no` or `tx` — attributes the row to NO site. Unattributed rows are counted and printed
(`WindowCensus`, including `action_verdict` / `action_execution` / `infragraph_prediction` rows whose own site
column is empty or undeclared), never folded into a site, and stay in the estate-wide numbers.

A1 deliberately does NOT use `ingest_alert` for its site. A missed fault on a host that never alerted has no
alert, so an alert-derived site would drop exactly the misses from the denominator and inflate per-site recall;
and a source that mis-stamps a site would credit one fault to two sites. The injector's node/host record exists
for every fault, detected or not. If node and host name DIFFERENT declared sites the fault is unattributed
(refuse to guess). The prefix rules stay in Go (`faultSite` calls `CanonicalizeSite`), not copied into SQL.

**What cannot be attributed, and is therefore estate-wide in every scope:** A8 — a breach is a GAP in the
gap-free ledger `seq` (a deleted row carries nothing to attribute) and a breaker trip is the deployment-wide kill
(`action_id = 'mutation-breaker-trip'`); A5 capability breadth (`policy_graduation` is per op-class); G5 (one
graph) and G7 (one census). A site-scoped scorecard says so in its header. The benchmark report gives A8 **no
per-site n** (`n: null`, `NOT-SITE-ATTRIBUTABLE`) and gates it once, estate-wide, on the window's **actuated
mutations** — the occasions on which a guardrail could have been breached. (Ledger rows were the first choice
and were wrong: every decision writes one, so that n could never fail.) The breach census and the window's
breaker trips and demotions ride in the cell; a breaker trip is printed first but is a value, not missing
evidence, so it does not fail the bar. A per-site denominator over an estate-only violation count would be
fabricated evidence.

**The report** (`axisscore -benchmark-window -since … [-until …]`, text or `-json`, exit 3 when the bar is not
met) prints, per declared site × axis, the n the axis's value is computed over and what one unit of n is (A1
injected faults; A2 incidents judged under the scored rubric; A3/A7 actuated mutations; A4 proposals, the
actionable population; A5 proposals carrying an op_class; A6a looped triages; A6b timed triages and correlated
recoveries, as two cells because the legs are never pooled).

**A3, A4 and A7 are gated on the AGENT-LOOP n** — the pooled n minus the deterministic heal lane — while their
value stays the pooled one, labelled, with both populations beside it (`side_n`). Those axes pool every heal, and a
device-down-heavy soak would clear n ≥ 30 on deterministic heals alone; the bar is a claim about the agent's
evidence. A2 stays gated on judged incidents, with the verified-outcome and blast-scored n beside it so "judged but
never verified" is visible.

**The window check has two halves.** The nominal length (until − since ≥ 14 days) says nothing about when the
evidence landed, so each site's first and last attributed incident must also be ≥ 13 days apart, else
"`<site>`: data spans X days". It checks the window is ≥ 14 days (and, in report mode, refuses an
`-until` still in the future: a window that has not elapsed has a nominal length its evidence does not have) and
counts the distinct `session_judgment.rubric_version` and `session_triage.harness_fp` values in the window:
exactly one RECORDED value of each is required — `''` (judged before versioning / harness unrecorded) is its own
value, because an unrecorded identity cannot be shown to equal the recorded one. The harness count covers the
incidents whose session COMPOSED a harness: `temporal/runner` stamps `harness_fp` on every investigate result, so
`harness_fp = ''` with `step_count = 0` means the investigate step never returned (suppressed before the loop,
or failed) — such sessions are reported as "never composed a harness" beside the check, not against it, while
`''` with steps counts as unrecorded. The **deterministic heal lane** (`decision_model_tier = 'deterministic-heal'`,
the liveness guest-down heal) is likewise outside the check on its own line: it never runs the agent loop, so it
carries `step_count = 0`, `decision_ms = 0` and an empty `harness_fp` (measured live 2026-11-06: 11 of the 28
proposals in the preceding 14 days; stamping that lane is TG-634). Because A6a and A6b's decision leg exclude it
by construction, each site also prints a REPORTED, ungated "deterministic lane" line with its own n, so the
exclusion is stated rather than read as fewer timed sessions. The tier spelling is pinned to
`temporal/runner/deterministic_heal.go` by `TestDeterministicHealTierMatchesTheRunner`. Without that split, any estate with a suppressed session would fail
uniformity forever, and a line that always reads FAIL teaches its reader to skip it. A seq gap anywhere in the
ledger fails the window: A8 is a census, and a census over a ledger with deleted rows is not a complete count. A2 is scored under the window's one
recorded rubric when there is one (`WindowRubric`), so a binary built after a rubric bump still scores a window
judged before it. PASS is an evidence verdict, not a quality verdict: it says the values may be published, not
that they are good.

Coverage limits stated rather than hidden: the golden fixture (`core/db/axis_scope_test.go`) and its mutation
controls cover the scope constants, the fault attribution, legacy-spelling folding, the census, the inline site
arms of the A2 verdict, blast-radius and A4-composition queries, and — through decoy rows AT `until` and at no
declared site — the window bounds of the verdict, execution, risk-audit and prediction reads. The report seam
(`axis.BenchmarkWindow`, what the CLI calls) is exercised end to end against the database by
`acceptance/benchmark_window_db_test.go`, in this spec's package because the CI `harness` job runs it with
`TG_TEST_DSN` set. The ledger window bound behind A8's n is not
DB-tested: `governance_ledger` is one hash chain whose fixture owns the whole table (`ledger_read_test.go`), so
seeding rows here would collide with it.

Known input caveat: `pve-liveness` stamped GR guests' down alerts `dc1` until TG-631 (fixed on main the same
day, each guest-down now carries its owning node's site). Incidents ingested before that fix was DEPLOYED are
attributed to NL by this report; it reads the stamp and never second-guesses it, so a G4 window should open
after the fix is live.

## 12. Every class sweeps the whole pool (TG-624 G3, 2026-11-06) — `tools/faultinjector` PlanNext

The start offset into the VMID-sorted pool advances once per full ROTATION (`(Cycle / len(rotation)) % n`), not
once per cycle. With `Cycle % n`, a pool whose size shares a factor with the rotation length correlated class and
start: a 10-guest pool under a 5-class rotation started device-down only at guests 0 and 5, so two soak VMs were
never chosen for it in 39 live faults. `TestEveryClassSweepsTheWholePool` drives two full sweeps and requires
every class to reach every guest.

## 13. A6 per triage RUN in the benchmark window (TG-633, 2026-11-06) — `core/db/axis_read.go`, `core/axis/benchmark.go`

`session_triage` is one row per alert and first-write-wins, so A6a (steps) and the A6b decision leg described first
runs only: measured 2026-11-06, about 2,618 runs over 769 alerts in 30 days. Migration 0121's `triage_run` (one row
per Temporal workflow run, live from 2026-11-06 20:37 NL) carries the same two inputs per run.

- `AggregateScoped` adds the run-level pair, `RunStepsN` / `RunMeanSteps` (step_count > 0) and `RunDecisionN` /
  median / p95 (decision_ms > 0). It uses the session queries' filters and the same window and site scope
  (`runInScope`, the site test through `ingest_alert`). `RunsSince` is the table's earliest row.
- The query is read fail-soft. An unreadable table leaves the run fields zero and logs, and the scored axes never
  fail on it.
- The benchmark report reads A6a and the A6b decision leg per run when `RunsCover(window.Since)`, that is, when
  the table's earliest row is at or before the window's start. Otherwise it reads the session numbers and says
  "first runs only" in the cell note. The A6b recovery leg and every other axis are unchanged.
