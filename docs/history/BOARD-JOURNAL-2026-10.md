# BOARD journal — 2026-10 (verbatim historical archive)

**Historical journal, verbatim, not a queue.** Entries are as-of their stated dates; evidence
anchors (`file:line`, `!MR` states, counts) are as-of their entry date and may since have
drifted. The live board is `docs/BOARD.md`; the open-work inventory is YouTrack
`project: TG #Unresolved`. Nothing may exist ONLY here — every entry names its tracker id.

## 2026-10-23 (day) — the nightly rehearsed by hand: trend baseline RE-ANCHORED and the controls measured; the judge calibration has to be re-judged offline

- **Board bullet retired today (verbatim, "Re-verified 2026-10-23 08:10 local"):**
  - **Re-verified 2026-10-23 08:10 local** (prod `*:9dcc409a`): the Harness² P3 loop is deployed end to end (TG-574 epic; journal
    2026-10-23 night) — evidence pack → INV/DOM proposers on the measured pool → composer → k-replicate REPLAY PROBE through the
    real loop → blind pairwise judge → trial. Armed on the box: `TG_SKILL_PROBE_REPLICATES=1`, `TG_REFLECT_ENABLE=1` (the
    prompt-reaching `TG_CAUTION_FILE` lane stays OFF). Judge tier = Opus (rubric 2026-10-23.1). The nightly trend-watch
    re-anchors on a rubric crossing (TG-595) and its negative-control bar measures again (TG-596 — the cron never had a
    LibreNMS token). The proposer path was REHEARSED by hand (`tools/proposerdryrun`, TG-598) and found unreachable —
    the pack pool dropped every session that loaded any A/B arm (TG-599, one shared CTE now) — then proven live: 6 packs →
    INV+DOM drafts → composer MERGED. Known red: the 03:53Z scheduled `actuation-guard-coverage` (111/195 hosts
    AWX-unreachable, estate). CI: runner03 only, shared — pipelines queue. The 00:50 posture bullet moved to the journal.
- **TG-595 + TG-596 Fixed on same-day evidence.** The 01:40Z nightly (pre-fix) still showed the un-anchored shape (crossing
  notice, five dims red, `GATE: FAIL`, every control EXCLUDED as unobservable). The crontab body was then run by hand at
  09:58–10:13Z under `env -i` (no bashrc, so no `BAO_ADDR` — the exact cron condition): the resolver read `~/.tg-bao.env`
  and the token resolved; `negative controls: 5 checked, 0 violation(s) PASS`; `proposal_recall 0.54 ≥ 0.50`;
  `GATE: RE-ANCHORED — a rubric version crossing … the trend baseline moves to this run (TG-595)`; baseline self-refreshed to
  overall 3.64 @ 7edea5cd under rubric 2026-10-23.1; record `eval/history/2026-10-23-trend-7edea5cd4df5`; committed + pushed
  as `17e13deb`. Instrument deltas on record, not graded: falsifiable_prediction 1.58→4.00, evidence_grounded 3.19→3.71,
  sensible_proposal 3.04→3.45, correct_diagnosis 3.68→3.64, appropriate_band 3.66→3.39, OVERALL 3.03→3.64.
- **TG-591 Fixed:** the eval-evidence multi-record policy's oracle is its drill, which runs in every pipeline's `eval-evidence`
  job (24 arms green on 14 consecutive main pipelines); no real MR has yet carried two same-key records (all were waived).
- **TG-584 — "wait for Opus-judged rows" could never work.** `cmd/judgecal`'s population is `session_triage ⋈ injected_fault`
  (sole-session faults): 1614 faults, last injected 2026-08-29, 2722 sessions all judged under the retired `primary` tier,
  0 under 2026-10-23.1, nothing injects any more. Built: `tools/rejudge -refs` (+ a `<scores>.meta.json` sidecar naming
  model/rubric/selection, removed before new scores land) and `cmd/judgecal -list-refs` / `-scores-in` (cut binarisation,
  duplicate refs refused, no-sidecar refused, `-min-coverage 0.5`, sidecar bound to the file by its scored count) with
  `judge_source`/`judge_model`/`rubric_versions`/`population_judged` in `calibration.json`; the DB read is scoped to the
  population. Review 0.88 → 0.90 → **0.92 ACCEPT**, 14 killing mutations RED, `make all` green, MR !1768 (pipeline 61175).
  The 762-ref Opus re-judge runs detached on the runner (`~/tg584-rejudge/run.sh`, ~2 h, ~9 s/call through the gateway
  tunnel). Next session: `judgecal -dsn <LAN DSN> -scores-in ~/tg584-rejudge/judgecal-opus.scores.json -truth diagnosis
  -json-out eval/history/2026-10-23-judgecal/calibration.json` + README → same branch → merge (one deploy) → bar + Fixed.
  `scripts/release-gate.sh` will read that record as THE §5.4 input.
- **Cron-bound, unchanged:** TG-585 (no trial crossed the finalize criteria at 03:17Z; falsifiable_prediction trials fill at
  4–5 judged/arm vs 49/56 on the appropriate_band trial 169), TG-590 (12 judged sessions carry the current fingerprint, 20
  needed before the pool scopes). Prod after the 06:21Z restart on `7edea5cd`: judge batches 08:13Z/10:13Z ran, breakers
  closed, probe + reflector armed.

## 2026-10-23 (night) — Harness² epic: evidence → proposers → pairwise gate → replay probe → composer; a deploy-blip judge outage and a false trend regression found and fixed

- **Merged + deployed:** TG-581 evidence pack (`9a70284b`; live pack from a real session is answer-free — Fixed), TG-580
  replay follow-up (recorded estate reads win over the live graph; `484cacc4` — Fixed), TG-582 evidence-grounded
  INV/DOM proposers reading the SAME pool the trigger measured (`31369ee6`), TG-583 slice A — blind pairwise judge +
  the offline admission gate decides by forced choice (`30bd24ef`), TG-593 — the judge batch retries after the breaker
  cooldown, a cancelled model call records nothing (`819d26cf`), TG-592 — fixture + replay arms render the LIVE tool
  catalog (`83794b42`), TG-594 — the pipeline watcher refuses an unresolvable short sha (`26332d69`).
- **Merged, deploy queued / reviewed ≥0.90 awaiting that deploy:** TG-583 slice B — the replay probe (the REAL loop
  against each incident's recording, k replicates, divergence; `b1f33a32`; armed on the box at k=1 after a live 5 s
  measurement) · slice C — the composer (INV+DOM → one candidate: merge / adopt-one / KEEP_BASE with evidence-cited
  notes; !1762) · TG-595 — the trend-watch RE-ANCHORS on a rubric-version crossing instead of grading the new judge
  against the old anchor (!1763) · TG-596 — the negative-control bar measures again (!1764): the nightly cron never had
  a LibreNMS token (bao's address lived only in an interactive bashrc), and three control hosts were dead since 08-06.
- **Armed on the box (ledgered, verified in the 03:20Z boot log):** `TG_SKILL_PROBE_REPLICATES=1` (TG-583 B) and
  `TG_REFLECT_ENABLE=1` (TG-579 — the reflector's lessons feed the flywheel only; the prompt-reaching caution lane
  stays off). First probe-backed pairwise admission at 04:37Z: 3 candidates admitted, each `W5/L1/T0 … 6 probed
  through the real loop against their recording`.
- **Rehearsed by hand, and a dormant path found unreachable:** `tools/proposerdryrun` (TG-598) runs the real
  proposers + composer against the live pool with every write shadowed and the connection read-only. Its first run
  showed the trigger measuring 11 sessions and the proposers receiving 0 packs — `PacksFor`'s private copy of the
  pool CTE dropped every session that loaded ANY skill's A/B arm, so the INV/DOM proposers had never run in
  production (TG-599: one shared CTE constant, set-parity + membership oracles). Fixed and re-run live: 6 packs,
  INV + DOM drafts citing record refs, composer MERGED one candidate with evidence notes, 20 rows before/after.
  TG-597 (empty threshold in a boot line) fixed on the way.
- **Live:** the 02:13Z judge batch wrote 53 judgments under rubric 2026-10-23.1 (Opus) across 8 sessions; the judge
  breaker re-closed on its half-open probe.
- **Incidents:** (1) the 00:13Z judge batch judged 0/6 — the TG-591 deploy recreated worker + gateway in the cron's
  second and three refused calls opened the DURABLE `model-judge` breaker → TG-593. (2) the first nightly after the
  judge tier change graded the Opus scale against the old anchor and reported a regression → TG-595. (3) every nightly's negative-control bar
  UNMEASURED — blamed on the estate, actually a credential resolver that only worked in a terminal → TG-596.
- **Found, filed:** TG-592, TG-593, TG-594, TG-595, TG-596, TG-597 (Fixed + deployed), TG-598, TG-599 (MRs open).
- **Known red, not ours:** the 03:53Z SCHEDULED pipeline (`actuation-guard-coverage`) has failed every night since at
  least 10-16 — 111 of 195 allowlisted hosts are AWX-unreachable on the degraded estate (pve01 down, GR site), so the
  key-coverage proof cannot complete. `delivery-witnesses` went green again tonight. Main's push pipelines are green.
- **Lessons recorded:** a trigger's consumer must share the trigger's pool definition; transport ≠ judgment; a deploy
  in the cron's second trips a durable breaker; both local test DBs accumulate rows (a limit-based list oracle fails
  at >200); no merges to main during the drift run (the cron pushes without re-pulling); Temporal crons are UTC; a
  body swap must recompute the content hash; IsActivity is true inside another activity; a cron shell never reads
  bashrc — reproduce a nightly's credential path under `env -i` before blaming the estate for an UNMEASURED bar; a
  "mirrors X's query" comment is not parity — share one SQL constant and rehearse dormant paths by hand.

**Moved verbatim from the BOARD Live posture (superseded 2026-10-23 night):**

- **Re-verified 2026-10-23 00:50 local** (prod `*:7c70792b`, Go 1.26.10): the Harness² P1 set + the P2 reflector are
  deployed (TG-574 epic; journal 2026-10-22/23). Post-deploy sessions carry `preamble/5` and a `harness_fp`; a
  proxmox incident loads only its domain skill; both new lanes (`TG_REFLECT_ENABLE`, `TG_CAUTION_FILE`) are OFF by
  config. Open: !1749 (TG-590), !1750 (TG-584 judge → Opus, deploy flips `TG_FRONTIER_JUDGE_MODEL` to `primary`),
  !1751 (TG-580 replay), !1752 (TG-588). CI: runner03 only, shared with other projects — pipelines queue for hours. The 2026-10-09 posture bullet moved verbatim to the journal.

## 2026-10-22/23 — Harness² learnings epic (TG-574): P1 + CI unblock + P2 reflector LANDED and DEPLOYED; judge-tier A/B measured

**Context.** The owner asked for a read of Google's Harness² (arXiv 2610.14224, `google-research/harness2`) and a
ranked list of what TG should borrow. The verdict table, the ranking and the do-not-adopt list (model-authored
scripts/hooks, model-invoked skills, sub-agents, the k-sweep machinery, reviving TG-37) live on TG-574.

**Landed on main and deployed (image `7c70792b`, worker up 22:44Z; each one MR, fresh-eyes review ≥0.90, CI green):**
- **TG-561** !1744 `d78c9289` — `toolchain go1.26.10` + base-image re-pin + `.trivyignore` (certifi advisory, exp
  2026-11-21 → TG-589). The CI blocker since 2026-10-21.
- **TG-578** !1741 `c2688eb7` (H2-4) — `<caution>` joins `seedPreamble`'s untrusted enumeration and `seedDelimiterRE`
  (`preamble/5`). FULL gate PASS (3.73 → 3.81). Live: 3/3 post-deploy sessions `prompt_version = preamble/5`.
- **TG-576** !1743 `d7a82c51` (H2-2) — the boot import flattened every compiled predicate and the store row's predicate
  won at compose: seven skills loaded on 399/499 sessions. Selector law: a compiled name composes with the COMPILED
  selector; graduated rows that diverge are recorded `predicate-shadowed:<name>`. Live: a proxmox device-down trio
  loads `proxmox-triage` and none of k8s/linux/storage/cisco/exec-safety.
- **TG-577** !1742 `68363aa4` (H2-3) — grader-verdict tripwire over the full system+seed of every corpus case and
  production skill row ("find the read path, do not widen the marker list").
- **TG-575** !1745 `904d17ae` (H2-1) — `harness_fp` per session (system half; tiers excluded), tracer + scorecard.
  Live: one distinct fingerprint on every post-deploy row. Per-version pool filter → TG-590 (!1749, 0.90).
- **TG-585** !1746 `792875c2` — losers of a TERMINAL trial move to `rejected` (never the 9 admitted-awaiting-start
  rows: `trial` status double-serves as "admitted"). Live check at the 03:17 finalizer.
- **TG-587** !1748 `60b0c7a9` — evalgate's quality record is append-only in fact (exclusive `os.Mkdir`, `-rNN`).
  Follow-up TG-591 (evidence-lint multi-record policy).
- **TG-579** !1747 `7c70792b` (H2-5, P2) — experience reflector + live caution writer on the exact complement of the
  precedent gate; the lesson feeds the FLYWHEEL, never the agent prompt. Ships OFF (`TG_REFLECT_ENABLE`,
  `TG_CAUTION_FILE`); boot lines confirm both lanes dormant; migration 0116 `session_experience` (plane: triage).

**Measured, not yet merged:** **TG-584** — the eval judge and the 2-hourly session judge ran on `primary`, the
agent's own tier. The same 120 live sessions re-judged under both tiers (`tools/rejudge` DB-replay, record
`eval/history/2026-10-22-judgecal-tier-ab`): `primary` scores 1-or-5 (23 % identical on all five dims),
`judge` (Opus) grades (4 % identical); r = 0.457. !1750 switches `params.model` to `judge` with rubric
`2026-10-23.1` (review 0.91); deploy couples with the `.env` flip of `TG_FRONTIER_JUDGE_MODEL` to `primary`.

**Open MRs (reviewed, awaiting the single runner):** TG-590 !1749 (0.90), TG-584 !1750 (0.91), TG-580 !1751 (0.93 —
the replay tool-set: 60/60 fixture observations replay byte-identically; `tools/replaydump`), TG-588 !1752.
**Remaining epic items:** TG-581..583 (P3 evidence pack, proposers, probe), TG-589, TG-591, TG-588 item 1 [R6].

**Verified live, no defect:** TG-586 (judge failure → integrity refusal, never 1.0).

**Moved verbatim from the BOARD Live posture (superseded by the 2026-10-23 bullet):**

- **Re-verified 2026-10-09** (prod `*:b08b6470`, Go 1.26.9): mode **Semi-auto on BOTH planes** (both
  runtime_posture rows; absent/zero/corrupt fails closed to Shadow). From 09-30 to 10-09 the planes were SPLIT: a
  false recon-burst kill left the stored mode and the triage plane in Shadow, while the actuation plane kept
  Semi-auto in memory and kept acting. Fixed by **TG-570** (each worker re-syncs from `policy_mode` every 10s) and
  **TG-571** (the burst bound scales with concurrent investigations). **No voting** (TG-559, boot line
  `approval resolver: AUTONOMOUS`). **All guests** (TG-558, 195, static); the first EXECUTED autonomous ACTs ran
  on 2026-10-04. **Hypervisors:** one NL node has been offline since 09-30 awaiting parts, and one is
  decommissioned but stays in the cluster config (owner-ruled). TG reads the cluster through a live member
  (interim, **TG-572**; 142 of the 195 watched guests are visible). **CI:** a single runner is online.

## 2026-10-09 — the safety stop never reached the acting worker (TG-570); mass outages tripped the recon alarm (TG-571); TG blind to the hypervisors for 9 days (TG-572); CI back (TG-573)

**The status check that started it** (owner: "is it grounding? active? running? autonomous?"). The stack had been
up 9 days and was triaging: 989 alerts in 24h, 3–10 sessions a day. Approvals were autonomous. But the stored mode
said **Shadow** while the actuation plane's posture said **Semi-auto, may_actuate**.

**Root cause (TG-570).** The triage and actuation workers each hold their own `ModeController` over the one
`policy_mode` row. Each read it once, at boot. On 2026-09-30 00:54Z a recon-burst alarm on the triage worker
force-Shadowed it and persisted Shadow. The actuation worker never saw that, kept serving Semi-auto, and executed
11 start-guest actions on 2026-10-04 after the kill. An operator `POST /v1/mode` had the same gap, because it runs on
the runner queue only, and the next actuation-worker restart would have gone Shadow silently.
- **Fix:** `ModeController.Resync` on a 10s ticker in every worker, failing closed (spec/015 **REQ-1526**).
  - A kill latches its own process until the kill is persisted, and a failed persist is retried every tick.
  - A read that raced a local write is discarded.
- **Review:** two fresh-eyes rounds, 0.80 → 0.87 (ship). The fixes were releasing the latch once the kill is
  persisted, and having each kill release only its own latch.

**Why the alarm fired (TG-571).** The flat bound of 150 reads in 5 minutes assumed this estate never runs ~15
investigations at once. On mass outages it runs ~30.
- **Evidence:** all four trips (09-30 once; 10-04 three times) were 28–32 ordinary investigations at 3.8–4.4
  reads each.
- **Fix:** the bound is now `max(150, 12 × distinct investigations in the window)`, set by
  `TG_RECON_BURST_PER_SESSION` (0 = the flat bound).
  - A sweep that spends the 25-read per-session cap still trips it with six investigations.
  - Unstamped and seeded reads earn no headroom.
  - A hot episode freezes the bound that opened it.
- **Residual:** recorded in THREAT-MODEL §5.2.

**What the outages actually were (TG-572).** One NL hypervisor node has been offline since 2026-09-30 00:46Z. That
was 7 minutes before the first trip. Owner ruling: it is waiting on replacement fans, and a second NL node is
decommissioned for good but stays in the cluster config until the owner cleans it up.
- **Impact:** TG read the cluster through the offline node only. Every liveness, estate, attribution and
  config-hash read failed for 9 days, which showed only as log lines, and an autonomous ACT on 10-09 was refused
  at execute ("could not re-observe").
- **Interim:** the prod `.env` was repointed to a live member at 16:23Z. Since then there have been 0 read errors,
  liveness is fresh (100 running, 42 stopped, 142 of 195 watched guests visible), the first poll seeded a baseline
  with no restart wave, and the config-hash sweep covered 146 guests with 0 errors.
- **Durable fix (open):** multi-endpoint failover, plus an alarm on a sustained reader failure.

**CI (TG-573).** No runner had been online, so pipelines sat pending and the nightly ones failed "no matching
runners". A spare runner was enabled for the project.
- The first pipeline then failed both supply-chain gates on advisories published during the gap.
- **Fix:** Go 1.26.9, `x/net` v0.60.0 and `otel/sdk` v1.45.0, and litellm re-pinned (1.93.0 → 1.104.2).
- **Checks:** the old and new gateway were smoke-booted with the real config and gave identical 13 routes; after
  the deploy, live calls succeeded on both `fast` and `primary`.

**Delivered.** !1739 merged as `b08b6470`. Main pipeline 58397 was green (27/27) and the deploy finished at 16:03Z.

**Live e2e.** At 16:11:13Z a governed `POST /v1/mode` Shadow→Semi-auto ran on the triage worker (ledger 18503
mode-transition and 18504 breaker re-arm). At 16:11:14Z the actuation worker logged `mode: resync Shadow ->
Semi-auto (the durable mode changed in another process)` with no restart, and both posture rows read Semi-auto.

**Also found.** The 2026-10-04 run contained the **first EXECUTED autonomous ACTs**: four start-guests approved by
`tg:fallback-approver`, confirmed by commit-confirm (TG-558 follow-up 1). They ran during the split, while the
deployment should have been in Shadow.

TG-570, TG-571 and TG-573 are closed with delivery bars. TG-572 is open for the durable fix.

**Moved verbatim from the BOARD Live posture (superseded by the 2026-10-09 bullet):**

- **Re-verified 2026-09-13** (prod `*:46da1a2a`, Go 1.26.8 images): mode **owner-set Semi-auto** (runtime_posture
  worker-actuation may_actuate=t; absent/zero/corrupt fails closed to Shadow). **No voting** (owner-ruled, TG-559):
  `TG_APPROVAL_RESOLVER=autonomous`, boot line `approval resolver: AUTONOMOUS` — a POLL_PAUSE resolves itself at
  classify time (ledger `fallback:autonomous-approve|hold:<reason>`, no poll, no wait); live HOLD drill passed.
  **All guests** (TG-558): liveness watch list + start-guest allowlist = every non-template guest (195, static).
- **Re-verified 2026-09-14** (prod `*:ab413b91`; live since 2026-07-17): all 195 guests match the hypervisor
  inventory by name and the heal token can start each one at both sites. Never yet run live: a second-site heal,
  a VM start, an EXECUTED autonomous ACT (TG-558 follow-up 1). Re-verified 2026-09-18: 195/195 still. The ACT
  *decision* did fire once (2026-09-14), but the execute-time re-observation refused it with a read error. The
  TG-454 ledger fallback does not cover that Alertmanager-sourced service fault → **TG-569**.
