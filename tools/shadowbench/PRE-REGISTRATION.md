# PRE-REGISTRATION — TG vs predecessor exceed-proof

**Status: FROZEN 2026-07-27, before the first confirmatory pair was accrued.**

`analyze.py` SHA-256: `d382f04831de9bc7407de76eae168f3deb6cac3babbcd089d1c6c306f5faa3b7`

`test_analyze.py` asserts that hash against the file on every CI run. Editing the analysis therefore reds the
build, and updating the hash here is a governed change carrying `Law-Change-Approved-By`. Someone can still
change the analysis; they cannot change it quietly, or after seeing which way the numbers went.

This document exists because of a specific failure mode, not as ceremony. TG's team writes the harness, the
rubric, the judge prompt and the fault injector, and TG's own LiteLLM hosts the judge. Every degree of freedom
in the analysis is therefore a degree of freedom for the party being measured. Freezing the analysis first is
the one mitigation that costs nothing and cannot be argued with afterwards.

---

## 1. Primary endpoint — deliberately judge-free

**Does each system propose the op-class that actually addresses the INJECTED fault?**
Ground truth is the fault injector's own record (`injected_fault.fault_type`), scored by `core/diagcorpus`
against the operator-declared expectations in `core/diagcorpus/expectations.json`. No LLM judge is involved.

- **Unit:** one injected FAULT, not one triage session. On this estate a single stopped guest trips FOUR
  LibreNMS rules and so raises four sessions, of which a system need act on one. Scoring sessions counts the
  same event up to four times and penalises the correct stand-downs on the duplicates — measured, that error
  reported device-down at 73.7% when the per-fault truth was 89.7%.
- **Test:** McNemar exact on the discordant pairs. Correct for a paired binary outcome; the chi-square
  approximation is unreliable at the discordant counts a 30-pair campaign produces, and it errs toward
  significance.
- **Direction:** two-sided. The primary question is whether the systems differ, not whether TG wins.

**Why this is primary and the judge is not.** TG's internal session judge is measured **uncalibrated**:
TPR 0.883, **TNR 0.000**, Cohen's kappa **-0.141** over 94 ground-truth-labelled items, and 58.3% of sessions
receive one identical score across all five rubric dimensions. The *shadowbench* judge is a different
instrument and measurably healthier on the same checks (23.3% dimension-collapse, 80.3% of scores >= 4), so
the judged pairs are **not void** — but they carry a positivity skew and a dimension correlation that a
primary endpoint must not rest on. A ground-truth endpoint has neither problem, and P5's corpus made it
available. Do not silently promote a judged dimension to primary later; that is the exact move this document
exists to prevent.

## 2. Secondary endpoints — the judged rubric

The five dimensions in `analyze.DIMENSIONS`, ordinal 1-5, paired per incident: one-sided **Wilcoxon
signed-rank** (H1: TG > predecessor), **Cliff's delta** as effect size, and **Holm** correction across the
family of five at alpha = 0.05.

Every p-value is published with its effect size. Significance without an effect size states only that n was
large enough.

## 3. Population — fixed in advance

| Rule | Value | Why |
|---|---|---|
| Minimum pairs | **30** | below this the interval is too wide to separate the systems |
| Minimum hosts | **12** (amended from 15 — §6 2026-07-31) | guards against a result carried by one host's quirks; 15 proved arithmetically unreachable — the predecessor's alert intake covers only 12 of the 15 pool hosts |
| Max pairs per host | **3** | in the 21-pair pilot **7 of 15 shared one host**, so effective independent n was ~6-8 |
| Tie-break when capping | **earliest by `judged_at`** | a rule fixed in advance cannot be steered by outcome |
| Excluded | judge unavailable; single-sided pairs | a missing side is missing data, not a loss |

Every exclusion is COUNTED AND PRINTED. A silent exclusion reads as "we covered everything" when it did not.

If the population rule is not met the run prints **NOT A CONFIRMATORY RESULT** and its numbers are descriptive
only. `test_analyze.py` asserts that the committed 15-pair pilot corpus **fails** this bar — if that test ever
passes, the minimums have been weakened.

## 3a. What counts as a PAIR — added 2026-07-27, **0 confirmatory pairs accrued**

A pair requires **all three**: the same subject host, the **same coarse fault class on both sides**
(`disk` / `memory` / `service` / `device`, derived from each side's own alert rule or category), and
|Δt| within the alignment window. Among qualifying candidates the **nearest in time** is taken, and a TG
session may serve at most one predecessor incident.

This clause was added after the plan was first frozen and **before any confirmatory pair existed** — which is
the only point at which adding it is legitimate, and why it is dated and labelled rather than folded in
silently. It is recorded here, not corrected away, because §6 requires exactly that.

The rule it replaces was: same host, |Δt| ≤ 12h, **first match wins**. Two defects, both disqualifying:

1. **No fault-class check.** On this estate one stopped guest raises FOUR distinct LibreNMS rules, so
   concurrent unrelated alerts on a host are the normal case. The old rule could pair a TG *disk-fill* triage
   against a predecessor *device-down* triage and score it as one head-to-head comparison. Comparing two
   systems on *different incidents* is not a comparison, and no downstream statistical care recovers it.
2. **First match wins.** Pairing depended on the order rows came back from the database, so two runs over
   identical data could pair differently — disqualifying for a campaign that must reproduce to the digit.

An **unclassifiable** side yields NO pair rather than a loose one: a wrong pair is worse than a missing one,
because a wrong pair enters the analysis as evidence. `test_align.py` asserts the classifier over the CLOSED
set of alert rules observed live, because an unclassified rule does not fail loudly — it silently removes that
entire fault stratum while the report still reads as complete. (That is not hypothetical: the first
implementation classified every disk alert as unknown, since the patterns were written spaced and the real
rule names are hyphenated. The test caught it; reading the code did not.)

## 4. Confidence intervals — clustered by host

Cluster bootstrap resampling **hosts**, not pairs (n = 10000, seed **20260727**, pinned so the interval
reproduces to the digit). Pairs from one host are correlated; resampling pairs would treat them as independent
draws and return an interval that is too narrow — the direction that manufactures a finding.

## 5. What is NOT claimed

- **No causal claim** about *why* either system performs as it does.
- **Unilateral properties are labelled, not compared.** Where the predecessor has no comparable instrument
  (A4/A5/A6), the metric is published as a TG property with that stated, never as a win.
- **Organic incidents are outside the primary endpoint** — they carry no ground truth. They form the
  contamination-control arm and are reported separately.
- **Meta-contamination is not fixed by this harness.** The predecessor recognises some guinea-pig hosts by
  name; those pairs are marked and excluded from the primary.

## 6. Deviations

**2026-08-25 — CAMPAIGN #3 DECLARATION: boundary, manifest-membership population (TG-526), as-deployed
arms, judge disclosure, ground-truth accrual term. Confirmatory pairs accrued at the time of this change:
ZERO by every instrument — the fault injector has been `systemctl disable`d since 2026-08-15, the newest
judged pair on record is 2026-07-31T23:02:43Z, and both fall before the new boundary. Every element below
is therefore declared OUTCOME-INDEPENDENTLY, before any campaign-#3 observation exists. Owner-approved
(graduation-plan approval + AskUserQuestion, 2026-08-25); `Law-Change-Approved-By` on the MR.**

- **Boundary.** `ACCRUE_FROM = 2026-08-26T00:00:00Z` (analyze.py + run-campaign.sh). Campaigns #1/#2 remain
  non-confirmatory (2026-08-19 ruling below); none of their pairs may be re-used, re-judged, or pooled.
- **Population = manifest membership (TG-526).** The 2026-08-19 item-10 fix filtered on the pair's
  INCIDENT time, but the boundary is FAULT time and the pair record carries no fault-time field — so
  ORGANIC post-boundary pairs could still occupy per-host cap slots and evict ground-truth-carrying pairs
  before the `_gt_for` join. `enforce_population` now keeps only pairs `reconcile-supply.py` joined to a
  post-boundary INJECTED fault (`confirmatory/manifest.jsonl` scorecard keys); a run without a readable
  manifest can never be `powered`. `snapshot_pairs.py` applies the same membership, so the committed
  snapshot IS the confirmatory set. Organic pairs remain the §5 contamination-control arm, reported
  separately — this narrows the primary population's WHO, never its scoring.
- **Ground-truth accrual term (the campaign-#1 stop-on-unpowered-primary failure).** `accrual.py`'s
  done-condition — which `run-campaign.sh` uses as its stop rule — now additionally requires
  MIN_PAIRS/MIN_HOSTS of ground-truth-CAPABLE supply, derived from `core/diagcorpus/expectations.json`
  (classes without an expectation, or marked unhealable, cannot produce a primary item; campaign #1 met
  the judged bar with only 18 of 24 counted pairs carrying ground truth). `accrual.py` is not part of the
  frozen analysis; this is a stop-rule correction, disclosed here because it changes when the campaign
  ENDS, never what it CONCLUDES.
- **Arms: AS-DEPLOYED STACKS (supersedes campaign #2's model-parity framing for #3; owner-ruled
  2026-08-25).** TG runs its production rail (Azure `gpt-4.1` primary / `gpt-5.4-mini` fast, per the
  2026-08-22 rail ruling); the predecessor runs `claude-opus-5` as deployed. Campaign #3 therefore
  answers the SYSTEM question — can TG-as-offered replace the predecessor-as-run — not the equal-brains
  question. Model parity is NOT asserted, and no parity claim may be derived from the verdict. If TG
  loses on the secondary family alone, the named diagnostic follow-up is a parity-on-opus rerun
  (non-confirmatory unless separately declared under a fresh §6 boundary).
- **Judge disclosure (direction of bias).** The shadowbench judge remains `claude-cc-opus` — the same
  underlying model family as the PREDECESSOR arm. The PRIMARY endpoint is judge-free and unaffected. For
  the judged secondary family this shares a brain with the comparator, and the bias direction is stated
  in advance: a judge sharing the predecessor's model makes a TG win on secondaries CONSERVATIVE (harder,
  not easier, for TG); symmetrically, a predecessor win on secondaries under this judge carries a
  discount, and the third-vendor-judged rerun is the named diagnostic before any such win is relied on.
- **Host floor mechanics unchanged**: MIN_PAIRS 30 / MIN_HOSTS 12 / cap 3 stand. The guinea-pig pool is
  restored to the 15-guest roster (12 predecessor-covered) before arming; the injector rotation for
  campaign #3 is restricted to ground-truth-scorable classes (operator config, outside this freeze —
  disclosed for completeness).

**2026-08-19 — FROZEN-ANALYSIS CORRECTIONS (TG-249 items 10/11/12) + CAMPAIGN-INTEGRITY RESET. Confirmatory
pairs accrued into the campaign at the time of this change: by the accrual instrument (`accrual.py` over
`confirmatory/manifest.jsonl`, `ts >= ACCRUE_FROM`) ZERO records — BUT `scorecard.jsonl` already carries 11
fully-judged two-sided post-boundary pairs (issues 2199/2200/2208, judged 2026-07-31 22:05–23:02Z) that were
NEVER reconciled into the manifest, with outcomes visible and leaning predecessor (pred 6 / tg 2 / tie 2 / 1
uncounted). Disclosed in full: "0 records" is the letter; the substance — visible post-boundary outcomes — is
exactly what the outcome-independence rule protects, so it is stated here rather than hidden behind the
instrument's count. No confirmatory claim is drawn from campaign #2 (integrity ruling below). Owner-approved
(AskUserQuestion, 2026-08-19).**

*The three corrections — each an outcome-independent error-correction, orderable by any referee from the
frozen prose + code with zero outcome data, on the same basis as the 2026-07-31 keying correction.*
- **ITEM 11** — the non-inferiority leg (the primary-rate-difference cluster-bootstrap CI) joined ground truth
  by the bare `incident_key`, while the primary McNemar leg was already corrected (2026-07-31) to key per-fault
  via `_gt_for`. §1 declares the unit as ONE INJECTED FAULT, so the bare-issue join cannot express it, and
  `TG MATCHES` was structurally unreachable for every §6-compliant (per-fault-keyed) GT file. The leg now uses
  `_gt_for`. Changes no accrued verdict's direction; it repairs a leg that could only ever fail closed.
- **ITEM 12** — `MAX_BLINDING_GUESS_ACCURACY = 0.60` was a registered constant with NO producer and NO
  consumer: the judge emits no which-system guess (blinding is enforced by construction — `judge.py`), and
  nothing scores a guess accuracy. §5 documents blinding as a residual limit, not a measured bar. Removed as
  dead; wiring it would be NEW instrumentation (itself a governed change), not a correction.
- **ITEM 10** — `enforce_population` capped ≤3 pairs/host keeping the EARLIEST by `judged_at`, with NO accrual
  boundary, so a disowned pre-freeze PILOT pair (injected earlier, hence sorting first) was admitted and
  evicted a real confirmatory pair out of a host's cap slot. The accrual boundary is now enforced IN the
  analysis: pairs are filtered on their incident time (`tg_created_at` or `pred_first_ts`, per
  `reconcile-supply.py`) `>= ACCRUE_FROM` BEFORE the cap; a pair with no establishable incident time is
  excluded (fail-closed). `ACCRUE_FROM` is a governed constant (like `MIN_HOSTS`), declared before any pair
  accrues; a later campaign sets its own boundary under its own §6 entry. The mechanism is outcome-independent:
  the registered accrual boundary already gated the SUPPLY side (`accrual.py` / `reconcile-supply.py`); this
  makes the analysis honor the SAME boundary. Its verdict effect is reserved — see below.

*Reproducibility (item-5 residual).* The published `confirmatory/final-campaign1-verdict.txt` (21 analyzed /
17 primary / p=0.0020) is NOT reproducible from the committed tree: the exact judged-pairs snapshot was never
committed, `scorecard.jsonl` is git-ignored + mutable + since contaminated with post-campaign-1 rows, and
re-running the frozen analysis over the committed inputs yields 51/0/p=1.0. `run-campaign.sh` now materialises
and commits a frozen `confirmatory/pairs-campaign<N>.jsonl` snapshot at bar-met, so future verdicts reproduce
from committed data. Campaign #1's verdict stands unamended (it already declares itself non-confirmatory); its
embedded sha references the PRIOR analysis hash and is deliberately NOT re-run under this one.

*Integrity ruling (owner-approved 2026-08-19): CAMPAIGNS #1 AND #2 ARE NON-CONFIRMATORY.* Campaign #1 is
underpowered (17 GT items < 30, 9 hosts < 12) and non-reproducible (above). Campaign #2's accrual is
contaminated: the 11 post-boundary verdicts above were judged with outcomes visible and never reconciled, so
amending the analysis now cannot be certified outcome-independent WITH RESPECT TO a campaign-#2 verdict. Those
11 pairs are DISCLOSED and are NOT reconciled into the manifest. Any confirmatory EXCEEDS / MATCHES / HOLDS
verdict is reserved for a fresh CAMPAIGN #3 opened at a new `ACCRUE_FROM` boundary with zero accrued pairs, the
corrected analysis, and a committed pairs snapshot — the same abandon-and-restart discipline campaign #2
applied to campaign #1's model-parity confound.

*Direction-of-effect (fairness).* Items 11 and 12 change no accrued verdict's direction (11 only lets a
fail-closed leg populate; 12 removes an unread constant). Item 10 EXCLUDES pre-boundary pilots; on the
currently-visible campaign-#2 data it would surface a predecessor-leaning population — i.e. it does not favour
TG — and regardless, no verdict is drawn from that data per the ruling above.

*Hash.* `analyze.py` SHA-256 moves `e8c3adfc…` → `089eb4f6…` in this same change (recorded at the top of this
file), carrying `Law-Change-Approved-By: @ncpjfuzl`. Every §1–§5 quantity, the §7 decision rule, `MIN_PAIRS`=30,
`MIN_HOSTS`=12, and `NONINFERIORITY_MARGIN` are UNCHANGED.

**2026-07-31T21:55:57Z — CAMPAIGN #2 OPENED (Opus-vs-Opus), MODEL PARITY DECLARED. Confirmatory
pairs accrued into campaign #2 at the moment of writing: ZERO. Campaign #1 is CLOSED and its
verdict stands unamended (composite: PREDECESSOR HOLDS, unpowered at 21/30 pairs; record in
`confirmatory/final-campaign1-verdict.txt`). Campaign #2 accrues only from the boundary above
(`ACCRUE_FROM=2026-07-31T21:55:57Z`); no campaign-#1 pair may be re-used, re-judged, or pooled.**

*Why a second campaign rather than an extension.* Campaign #1 compared unequal brains: TG ran a
third-party gateway model while the predecessor ran Claude on the owner's Max subscription. That
confound is unfixable within a campaign — the exposure differs for every already-banked pair — so
the honest move is a new campaign at a fresh boundary, not a bigger #1.

*Model parity, verified rather than asserted (this is the load-bearing claim of campaign #2).*
Both arms now run **`claude-opus-5`** on the same Max subscription:
- PREDECESSOR: dispatched-session envelopes in `/tmp/claude-run-<issue>.jsonl` report
  `modelUsage` keys `claude-opus-5` (+ the CLI's internal `claude-haiku-4-5` housekeeping helper);
  `scripts/claude-provider.sh status` = `anthropic`, with an EMPTY `env` block in
  `~/.claude/settings.json` (no Z.ai/GLM override in force).
- TG: reaches the same CLI through `deploy/claude-proxy` (`OPUS_MODEL=claude-opus-5`) behind
  LiteLLM, whose `primary` and `fast` routes both resolve to it with fallbacks emptied.

*Two traps recorded so a later reader does not have to re-derive them.* (1) A model's SELF-REPORT
of its own identity is not evidence — asked directly, the sidecar path answered
`claude-opus-4-5-20251101`, which the envelope evidence above contradicts; self-reports were
therefore excluded from this determination. (2) The sidecar's `served_model` log line falls back
to the CONFIGURED model name when `modelUsage` is absent, so on its own it could be a mere echo of
its own config. It is treated as evidence here only because the predecessor's envelopes prove the
CLI's `modelUsage` key is literally `claude-opus-5` (not a dated id), and because TG's calls return
non-zero token counts — the success path on which `modelUsage` is populated. Hardening that line to
report the raw envelope (and to fail loudly when it is absent) is tracked as follow-up TG-235; it
does not gate this campaign, but an unhardened instrument is why the reasoning is written out here
in full rather than compressed to "both run Opus 5".

*Unchanged by this deviation:* every §1–§5 quantity, the §7 decision rule, and the frozen
`analyze.py` hash. The population bar (30 pairs / 12 hosts / ≤3 per host) is re-armed from zero.

**2026-07-31 ~06:15Z — GROUND-TRUTH KEYING ERROR-CORRECTION + LOG-FILL RULING (owner-directed:
"answer these two the scientific way"). Banked pairs at the time: 21. FULL SEQUENCE DISCLOSURE:
the per-fault outcome table (TG 13 / PRSR 3 over 16 scorable faults) was computed and recorded
BEFORE this fix was proposed — the deviation is disclosed with outcomes visible, and its
legitimacy rests on the decision criterion being outcome-independent, per below.**

*Keying correction (analyze.py; hash moved above in this same change).* §1 has always declared
the primary's unit as ONE INJECTED FAULT, but the ground-truth join used the bare `incident_key`
(the predecessor ISSUE id). Recurrence dedup makes one issue back up to 5 distinct faults with
different outcomes, so 7 of 9 issue keys were UNREPRESENTABLE — and which faults survived
depended on alert-collision patterns, i.e. informative missingness, which invalidates the
analysis outright. Under the standard treatment of analysis-program errors (ICH E9 / estimand
framework; CONSORT disclosure), correcting the program to implement the REGISTERED estimand is
error-correction, not a protocol change: the decision criterion ("does the code implement §1's
declared unit?") is answerable from the frozen prose + code alone, with zero outcome data — any
referee orders the same fix without seeing a single score. The fix keys ground truth by the pair
record's unique `key` (bijective with the manifest's `fault_id`), with bare-incident_key files
still joining as a legacy fallback. Sensitivity note: the pre-fix join could have represented
only 2 of 9 keys (both TG-correct/PRSR-wrong) — there was no valid alternative analysis to
displace; the choice was "no primary at all" vs "the registered one."

*Log-fill ruling (expectations.json UNTOUCHED for this campaign).* `log-fill` has no entry in
the registered rubric, so its 5 banked faults are EXCLUDED from the primary as-registered.
Scoring them under a rule written after outcomes exist would be a post-hoc rubric addition — the
precise degree of freedom pre-registration exists to remove. Per adjudication-charter practice
they are instead reported as a SENSITIVITY analysis under the artifact-derived rule (stand-down
is the only correct response: no reclaim verb exists in core/actuate/opschema/opschema.json;
tools/faultinjector declares log-fill detection-only with an enforcing test — all frozen before
the campaign). The completed rubric (log-fill: stand_down_is_correct) is REGISTERED HERE for
campaign #2, before that campaign begins. If primary and sensitivity disagree, only campaign #2
may decide the contested stratum.

*Ground-truth provenance.* `confirmatory/ground-truth-campaign1.json` (committed): 16 per-fault
entries keyed by scorecard key, from a 3-extractor adversarially cross-checked build (expected
response per expectations.json; TG proposals from session_triage across each fault's full window
per §1's any-correct/all-decline rules; PRSR proposals from full run-log triages). Flagged, not
hidden: fault 1361's PRSR miss traces to Anthropic session-limit throttling (the disclosed
shared-pool risk), fault 1336's TG miss to unparseable model output; two TG container misses are
the strict `restart-container`-only vocabulary of the registered rubric (the injector declares
TG's proposed `start-container` as addressing — counted AGAINST TG as registered, not
reconciled mid-campaign).

**2026-07-31 ~04:30Z — JUDGE/CONTESTANT MODEL ISOLATION (owner-directed). Banked pairs at the time
of this change: 18 — all retained, because the judge's SERVED model does not change.**

*The risk closed.* The shadowbench judge requested the `primary` LiteLLM alias (kimi-k3) and reached
deepseek-v4-pro only via a deterministic fail-over (kimi-k3 rejects the judge's fixed
`temperature=0` with HTTP 400 → first fallback rung). So the judging instrument was pinned by
ACCIDENT, and worse: `deepseek-v4-pro` sat in the fallback chains of TG's own agent aliases
(`primary` and `fast`), so a single Mistral/Kimi outage could have served a TG triage from the same
brain that judges it. Measured exposure to date: ZERO — all 80 post-boundary TG triages ran tier
`fast` (mistral-large-latest) and every judged pair records `judge_model_served: deepseek-v4-pro` —
but the isolation was luck, not law.

*The change (harness + TG-side, comparator untouched).* A dedicated `judge` alias pinned to
deepseek-v4-pro with NO fallback (a judge outage is loud `judge_unavailable`, excluded per §3 —
never a silent model drift); deepseek REMOVED from every agent-facing fallback chain
(`primary: [mistral, zai]`, `fast: [zai]`); the campaign harvest now passes `MODEL=judge`. The
served judge model is BYTE-IDENTICAL before and after (deepseek-v4-pro on every pair), so the 18
banked pairs and all future pairs are scored by the same instrument; what changed is only that the
guarantee is now structural. Direction of effect on TG: strictly negative-or-neutral (it LOSES a
fallback rung); on the predecessor: none (it runs Claude Opus 5 outside LiteLLM entirely).

*Model identity for the record (who is actually playing):* predecessor = claude-opus-5 (Claude Code,
Anthropic Max); TG agent = mistral-large-latest (`fast` tier — TG's own production routing for the
agent loop; kimi-k3 `primary` serves its synthesis/judgment callers); judge = deepseek-v4-pro,
anonymized A/B. The head-to-head therefore compares SYSTEM STACKS (architecture + model choice),
not a single model — TG's model choices are its own design decisions under the operator cost
policy, measured as-is, exactly like the predecessor's.

**2026-07-31 — COMPARATOR SCHEDULING CHANGE: the predecessor's intra-territory triage parallelism
raised from 1 to 4 concurrent sessions (owner-directed). Accrual boundary moves to
2026-07-31T01:30:00Z. Confirmatory pairs accrued under the NEW boundary at the time of this entry:
ZERO (all prior pairs are serial-era and excluded mechanically).**

*What changed and why.* The predecessor serialized ALL infra-nl alerts through one slot lock
(~2-3 triages/hour under storm — an agent-built emergent behavior the owner never ratified, and,
measured against TG's 64-triages/15-hosts parallel throughput, the binding constraint that made
the campaign a multi-day operation). The owner directed parity: per-issue locks with a cap of 4
concurrent sessions for infra-nl (claude-gateway commit 72cb739; Runner + Matrix Bridge patched
live via the n8n API with the repo's validator sequence; rollback snapshots retained).
**SCHEDULING ONLY:** Build Prompt (including the ORIGIN-NEUTRAL DIAGNOSIS clause), model, tools,
hooks, memory content and every reasoning surface are byte-identical — asserted programmatically
at patch time. E2E-verified live: 3-4 REAL concurrent triage sessions admitted within minutes of
the change, cap enforced under contention, per-issue cancel (the pkill-all defect is gone),
scoped cleanup, box healthy (12/32G with 4 sessions).

*Fairness.* Direction of effect: strengthens the comparator's THROUGHPUT only; per-session
reasoning is unchanged. One disclosed residual risk: both systems draw the same Anthropic Max
OAuth pool, so heavy parallel bursts could throttle either side; the campaign window is short
(2-3h) and both sides run in the same window, so any throttling is shared, not one-sided.
`analyze.py` and its hash pin are untouched.

**2026-07-31 — POPULATION AMENDMENT: minimum hosts 15 → 12. Discovered infeasibility, decided
BEFORE any verdict was examined, on an OUTCOME-INDEPENDENT basis. Confirmatory pairs accrued at the
time of this change: 2 (cloudbeaver01/device, ghostfolio01/service) — NOT zero, and disclosed as
such; see the integrity argument below for why the amendment is still legitimate. Owner-approved
2026-07-31.**

*What changed.* `MIN_HOSTS` 15 → 12 in `analyze.py` and `accrual.py` (kept equal;
`test_accrual.py` asserts the equality). `MIN_PAIRS` is UNCHANGED at 30 — the pair floor governs
statistical power and has nothing to do with the coverage ceiling; only the host-breadth criterion
moved, and only to the value that is actually attainable. The `analyze.py` SHA-256 pin moved in
this same change (recorded above), carrying `Law-Change-Approved-By`, exactly as the freeze rule
requires. Nothing else in the composite verdict (§7) was touched.

*Why (the discovered infeasibility).* The 15-host bar was set assuming the 15-host injector pool
maps to 15 attainable predecessor pairs. It does not. The predecessor's alert intake structurally
covers only **12 of the 15 pool hosts**: `openarchiver01`, `whiteboard01`, and `imaginary01` have
**zero predecessor triage sessions, ever**, despite being injected post-boundary with real faults
(openarchiver01 2× device-down restored, whiteboard01 1× disk-fill restored, imaginary01 1×
device-down) that **TG received and triaged 131 / 3 / 3 times** respectively. So the alerts exist
and reach TG; they simply never reach the predecessor. Under the owner rule that the predecessor is
measured AS-IS (its config/prompt/gate/alert-scope untouched), 15 distinct predecessor-paired hosts
is arithmetically impossible — the ceiling is 12. (Those 3 hosts were added to the pool on
2026-07-31 specifically to reach 15; the padding was hollow. The predecessor's narrower coverage is
itself a recorded finding, in TG's favor, not something this amendment hides.)

*Why it is still legitimate at 2 accrued pairs (the integrity argument).* The pre-registration's
purpose is to forbid changes made BECAUSE of how the numbers are turning out. This amendment is not
that, on three independent grounds: (1) it is driven ENTIRELY by host-COVERAGE counts (0 predecessor
sessions on 3 hosts) — a structural property of the predecessor's alert intake that is invariant to
which system wins any pair; (2) the verdicts of the 2 accrued pairs were **NOT examined** before
deciding to amend — the decision rests on coverage, not on the 2 pairs' scores, and this is recorded
here so the claim can be audited against the commit history; (3) the change can only make the bar
EASIER to *satisfy* (12 ≤ 15), but it does NOT change the DIRECTION or the scoring of any comparison
— the primary McNemar endpoint, the Wilcoxon secondaries, the Holm correction, and the §7 composite
all operate identically on whatever pairs accrue; a smaller host floor cannot manufacture a TG win,
it only makes a powered result reachable at all. The honest alternative — leaving an unreachable
15-host floor in place — would guarantee "PREDECESSOR HOLDS (unpowered)" by construction regardless
of the evidence, which is itself an outcome-predetermining choice.

*Scope guard.* This is the ONLY criterion amended. If the campaign still fails to reach 30 pairs on
12 hosts, that is reported as a non-confirmatory result, not patched away with further amendments.
The neutral third-party suite (owner directive 2026-07-25) remains the decider for any public v1.0
claim; this internal campaign informs.

**2026-07-30 — EXTRACTION FIX: the predecessor is scored on its FULL triage, not a ~1500-char
truncated preview. Confirmatory pairs accrued at the time of this change: ZERO.**

`extract_predecessor.py` read the predecessor's conclusion from `sessions.last_response_b64`, which
is a hard-capped ~1500-char PREVIEW (verified: every session 1503–1512 chars regardless of a
13–49-turn triage). The predecessor writes its merits-based conclusion (`## Synthesis` →
`## Proposed Actions` → `CONFIDENCE`) at the END of a multi-thousand-char triage, so the preview cut
lands BEFORE the verdict — the judge saw "investigated, never concluded" and scored a compliant,
fully-diagnosed triage *worse* than a one-line skip (whose verdict sits in the opening line and thus
survived the cut). This is a pure measurement artifact that penalizes exactly the behavior the
comparator-instrument fix above unlocked. The extractor now reads the FULL triage from the
predecessor's Claude run log (`/tmp/claude-run-<issue>.jsonl`, the complete transcript), falling back
to the preview only when the log is absent, and `_extract_conclusion` anchors on the real conclusion
markers. **Symmetry preserved:** both sides remain conclusion-first with the judge's shared
`_TRAJ_CAP=3000` governing final length (the predecessor's rationale cap is unchanged at 1200); the
change only ensures the predecessor's conclusion EXISTS in what the judge reads, as TG's always did.
Direction of effect: strictly a fairness fix for the comparator. `analyze.py` + its hash untouched.

**2026-07-30 — COMPARATOR-INSTRUMENT FIX: the predecessor's triage prompt now diagnoses every
alert on its merits, including recognized-synthetic ones. Confirmatory pairs accrued at the time
of this change: ZERO (tools/shadowbench/confirmatory/manifest.jsonl: 0 records; no confirmatory
entry on the runner). Disclosed here because it modifies the comparator, and the only legitimate
moment to do that is before the first pair.**

*The defect it fixes.* During the pre-campaign shakedown the predecessor (n8n workflow
`qadF2WcaBsIR7SWG`, "NL - Claude Gateway Runner") was observed recalling an operational memory
(`feedback_tg_actuate_restart_falsepos`) that recognizes the fault-injector's own synthetic
~20-minute vzstop/vzstart sweep and instructs it to "append one line, do not re-diagnose." On
every injected fault it recognized as ours it therefore short-circuited — recording a recognition
note instead of a diagnosis. A comparator that declines to play cannot be measured: those pairs
carry no predecessor SYNTHESIS/EVIDENCE/action to score against TG's, so the endpoint is not
"predecessor vs TG" but "predecessor-skip vs TG", which answers nothing.

*The change.* One diagnostic-**neutral** clause ("ORIGIN-NEUTRAL DIAGNOSIS — MANDATORY
COMPLIANCE") was inserted into the Build Prompt reasoning framework, immediately after its
existing "Investigate both before committing to one" line. It requires a full diagnosis
(root-cause synthesis + observation-backed evidence + a recommended action) on **every** alert
regardless of recognized origin; it explicitly preserves memory as a **speed** aid ("Memory may
make the diagnosis FASTER; it must never replace it") and leaves declared-maintenance suppression
(the `gateway.maintenance` sentinel, ASA EEM reboot windows, declared chaos windows) unchanged.
It does **not** tell the predecessor what to conclude, does not bias it toward propose vs
stand-down, and touches no safety, actuation, or memory-write path. Memory rows are left intact.

*Why this is not a thumb on the scale — direction of effect.* The clause can only make the
**predecessor** produce more/better output; it removes the predecessor's ability to win-by-not-
answering, restoring the very capability (recurring-incident recognition) that is its documented
advantage over TG — now expressed as faster diagnosis rather than as a skip. TG is given **no**
reciprocal hint and no knowledge of the clause. A change whose only mechanical effect is to
strengthen the *comparator* cannot be construed as favoring TG; if anything it raises the bar TG
must clear. The scoring rule (`analyze.py`, its frozen §7 composite, and its SHA-256 pin) is
untouched — HOW pairs are judged did not move.

*Accrual boundary moves to the change instant.* Because a pre-patch predecessor triage is not a
valid comparison, the confirmatory accrual boundary for this campaign is set to the change instant
**2026-07-30T21:31:00Z** (later than the plan's 2026-07-27 freeze, never earlier). `run-campaign.sh`
threads it as `ACCRUE_FROM` into both `reconcile-supply.py` (which fetches only faults
`injected_at >= boundary`) and `accrual.py` (which counts only manifest records whose fault-ts is
`>= boundary`), so the 1283 pre-patch ledger faults — max `injected_at` 2026-07-30T20:57Z, all
before the boundary — are excluded mechanically at both stages, not by trust. Confirmatory pairs at
the time of this entry remain ZERO.

*Reversibility.* Pre-change live snapshot saved (`runner.rollback.json`, 53 nodes); the git mirror
`workflows/claude-gateway-runner.json` carries the identical logical edit on the predecessor repo;
revert = PUT the snapshot (or `git revert` the mirror commit). Applied live via the n8n API and
verified (workflow still 53 nodes, clause present in the live Build Prompt).

**2026-07-31 — INITIATION GATE SATISFIED. Confirmatory pairs accrued at the time of this entry:
ZERO (tools/shadowbench/confirmatory/manifest.jsonl: 0 records).**

The port-fidelity ledger (docs/PORT-FIDELITY-AUDIT.md) is CLOSED: all 26 findings adjudicated —
**25 fixed, 1 owner-waived N/A-by-design (#21, compound-plan detection, with an auto-reopen
tripwire), 0 open** — each re-measured against HEAD in the adversarially-verified 2026-07-31
re-score. The four gate work items (TG-219 suppression-learning chain incl. the #5 unlearning
escape; TG-220 learned falsifiability window; TG-221 model-path breaker; TG-222 governance
monitors) and the two owner-BUILD rulings (TG-223 prior-verdict banding, TG-224 destructive-verb
coverage) have all landed with real-path oracles and executed-RED mutation controls.

Per the 2026-07-31 owner initiation gate (recorded below), **confirmatory accrual is now
authorized to begin.** Records dated on or after this entry count as supply; anything earlier —
including the pre-freeze shakedown and every labeled smoke run — remains excluded by
accrual.py's freeze-date logic. The frozen analyze.py decision rule is unchanged (its hash pin
holds); nothing about HOW the comparison is scored was touched by closing the gate.


**2026-07-31 — OWNER DIRECTIVE: campaign initiation gate. Confirmatory pairs accrued at the time
of the change: ZERO (tools/shadowbench/confirmatory/manifest.jsonl exists with 0 records).**

The plan froze *how* the comparison is scored but left *when accrual begins* as a free choice —
an ungoverned degree of freedom equivalent to optional stopping (run when conditions favor you,
or be accused of it). The owner closed it. **Confirmatory accrual may not begin until the
initiation criteria are satisfied:**

1. **The port-fidelity ledger is CLOSED**: all 26 findings in `docs/PORT-FIDELITY-AUDIT.md` are
   re-measured against current code, and every one ends as **fixed**, **N/A-by-design**, or
   **explicitly owner-waived** (waiver recorded in the audit doc with rationale). The ledger is
   closed-ended: no findings may be added mid-course without owner sign-off — "ready" is a
   checkbox count, never a feeling (the predecessor's unsatisfiable 0.95 gate is the named
   anti-pattern).
2. **Gate-satisfied date recorded here** (a dated §6 entry naming the ledger state), and only
   confirmatory records dated ON OR AFTER that entry count as supply. Anything earlier —
   including engineering smoke runs of the harvest→reconcile chain, which are permitted and
   encouraged to keep the pipes proven — is excluded by date, mechanically, in `accrual.py`'s
   freeze-date logic.
3. The formative instrument during port completion is the **eval plane** (nightly trend-watch,
   per-change gate), never this campaign: the exam is sat once, with authority, or not at all.

**2026-07-27 — analysis amended. Confirmatory pairs accrued at the time of the change: ZERO.**

`powered` was computed from the JUDGED pair set alone. The primary endpoint is scored from ground truth, so
its usable population is the SUBSET of judged pairs carrying a ground-truth entry — potentially far smaller.
A run could therefore declare itself adequately powered while the endpoint the entire claim rests on had a
handful of items behind it, and nothing in the output said so. `powered` now requires BOTH populations to
clear the minimums, the primary population is printed on its own line even when it is zero, and a run with no
ground-truth file is never powered (the primary is not computed at all, so it cannot satisfy the exit
criterion however many judged pairs it carries).

This is a TIGHTENING — it can only ever move a run from "confirmatory" to "not confirmatory", never the
reverse. It was made before any confirmatory pair existed, which is the only point at which amending the plan
is legitimate. The hash above moved with it, in this same change, as the rule below requires.

**2026-07-30 — analysis amended: the composite DECISION RULE is frozen inside `analyze.py` (§7 added).
Confirmatory pairs accrued at the time of the change: ZERO — and provably so:**

- `tools/shadowbench/confirmatory/` does not exist, in the repository or on the runner host;
- no campaign manifest is committed to the repository, and the runner's untracked
  `tools/shadowbench/out/campaign-manifest.jsonl` carries no confirmatory-mode entry: its only PAIRED
  records (7) are the 2026-07-22 TG-84 shakedown, which predates this plan's 2026-07-27 freeze and the §3a
  pair definition entirely — a pair cannot be accrued under a plan that did not yet exist, and the board
  (docs/BOARD.md, Phase D) records the campaign as not yet begun (D0 estate readiness still open, the
  !708-fixed injector binary not yet deployed, pilot night excluded from accrual by plan);
- the rolling `scorecard.jsonl` on the runner contains only ORGANIC judged pairs — the contamination-control
  arm of §5, which carries no injector ground truth and is outside the primary endpoint by this plan's own
  terms; the 2026-07-27 amendments above were already recorded at "ZERO confirmatory pairs" while that same
  organic ledger was accumulating, so this reading is the plan's established one, not one invented today;
- the only committed pair corpus is the 15-pair pilot (`evidence-rejudge-2026-07-26.jsonl`), which
  `test_analyze.py` asserts FAILS the §3 population bar.

Why the amendment: the plan froze the STATISTICS but left the VERDICT — the mapping from those statistics to
"TG exceeds / TG matches / predecessor holds" — outside the freeze, in prose. A decision rule applied
outside the frozen analyzer is exactly as adjustable-after-seeing-the-data as an unfrozen analysis, because
the verdict is the thing the campaign exists to produce. The 2026-07-26 rejudge showed what that freedom
does in practice: both of TG's apparent pooled wins were manufactured by a one-sided dimension
(`falsifiable_prediction`, scored for TG and structurally null for the predecessor), per the judge's own
written reasons. §7 freezes the composite verdict inside `analyze.py` with the one-sided dimensions and
unilateral axes STRUCTURALLY unable to enter it.

What had been seen when this was frozen, stated so a reader can weigh it: the pre-freeze pilot/rejudge
results (predecessor ahead on every comparable dimension) and the organic contamination-arm ledger — and NO
confirmatory data, which does not yet exist. Every element of the rule is a constraint AGAINST the party
writing it: the counter-leg can only block a TG win, never manufacture one; the unilateral exclusion removes
TG's strongest uncontested dimension from the verdict; the non-inferiority margin concedes only that a
≤ 10 pp deficit on a powered sample is "matches", never "exceeds". Like the amendments above, this was made
before any confirmatory pair existed — the one legitimate moment — and the hash above moved with it in this
same change.

Any deviation from this plan after the first confirmatory pair is accrued MUST be recorded here, in the same
MR that makes it, with the reason and the number of pairs already accrued at that point. An unrecorded
deviation invalidates the campaign — not because a rule says so, but because a reader cannot otherwise tell an
analysis from a search.

## 7. Decision rule — the composite verdict, frozen 2026-07-30 at ZERO accrued confirmatory pairs

The campaign concludes in exactly ONE of three verdicts, computed by `composite_verdict()` INSIDE frozen
`analyze.py` — never applied by hand to the numbers afterwards. A decision rule living outside the freeze is
exactly as adjustable-after-seeing-the-data as an unfrozen analysis, because the verdict is the thing the
campaign exists to produce.

- **TG EXCEEDS** iff ALL of:
  1. the run is `powered` (§3: ≥ 30 pairs, ≥ 15 hosts, ≤ 3/host — on BOTH the judged and the ground-truth
     populations, per the 2026-07-27 amendment);
  2. the PRIMARY judge-free endpoint's McNemar exact two-sided p < 0.05 with the discordant count favouring
     TG (b > c);
  3. neither `correct_diagnosis` nor `evidence_grounded` is significantly in the predecessor's favour
     (two-sided Wilcoxon signed-rank, Holm-corrected across that family of two at α = 0.05) — the
     like-for-like leg, so a primary win cannot stand on ground the judged like-for-like evidence
     significantly contradicts.
- **TG MATCHES (non-inferior)** iff not EXCEEDS, the run is `powered`, and the host-clustered bootstrap 95%
  CI (§4: hosts resampled, n = 10000, seed 20260727) on the primary correct-rate difference
  (TG − predecessor) excludes a predecessor advantage GREATER than 10 percentage points — CI lower bound
  ≥ −0.10. An uncomputable interval (no ground truth, or a single host cluster) fails closed and certifies
  nothing.
- **PREDECESSOR HOLDS** otherwise — including every unpowered run: the burden of proof is TG's, and an
  unpowered campaign cannot discharge it in either direction.

`falsifiable_prediction` and every unilateral axis (A3 heal success, A4 autonomy, A5 fault-class breadth,
A7 false-actuation, A8 safety violations — `docs/BENCHMARK-AXES.md`) are PRINTED as labelled TG-only
properties and can NEVER enter the verdict. This is structural, not procedural: `composite_verdict()`
accepts a closed keyword-only input set that contains no rubric score and no axis value, and it REJECTS any
counter-leg dimension outside {`correct_diagnosis`, `evidence_grounded`} rather than filtering it silently.
The 2026-07-26 rejudge is why: both of TG's apparent pooled wins were awarded on the one-sided dimension,
per the judge's own written reasons.

Pair-supply progress against §3 ("are we done accruing?" as a number) is reported by `accrual.py` —
deliberately UNFROZEN and read-only, and structurally without a verdict path (asserted by
`test_accrual.py`), so supply reporting can improve mid-campaign without opening a second, gameable route to
a conclusion.
