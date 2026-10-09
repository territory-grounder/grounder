# BOARD journal — 2026-10 (verbatim historical archive)

**Historical journal, verbatim, not a queue.** Entries are as-of their stated dates; evidence
anchors (`file:line`, `!MR` states, counts) are as-of their entry date and may since have
drifted. The live board is `docs/BOARD.md`; the open-work inventory is YouTrack
`project: TG #Unresolved`. Nothing may exist ONLY here — every entry names its tracker id.

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
