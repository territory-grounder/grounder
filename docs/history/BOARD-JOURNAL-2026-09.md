# BOARD journal — 2026-09 (verbatim historical archive)

**Historical journal, verbatim, not a queue.** Entries are as-of their stated dates; evidence
anchors (`file:line`, `!MR` states, counts) are as-of their entry date and may since have
drifted. The live board is `docs/BOARD.md`; the open-work inventory is YouTrack
`project: TG #Unresolved`. Nothing may exist ONLY here — every entry names its tracker id.

## 2026-09-13 — owner rulings: watch/heal ALL guests (TG-558) and NO VOTING (TG-559); two latent main reds cleared (TG-560/561)

**The status check that started it** (owner: running? for how long? actuating properly?). TG was live for
58 days (first session 2026-07-17) and had been in Semi-auto since 2026-07-30. It executed 477 actions between
07-27 and 08-30, and the post-check verified 457 of them as a match. Every one tracked a campaign #3 injected
fault, so there were zero executions after the injector stopped, while triage continued at about 30
sessions/day. The nightly scheduled main pipeline had been red since at least 09-08: the deployed-sha
witness was BLIND on the [skip ci] tip, and actuation-guard-coverage reported one allowlisted guest with no
probe line. Filed as **TG-557**.

**TG-558 — watch and heal every guest** (owner: "expand these 20 VMs to ALL VMs"). The change was box `.env`
only. `TG_PVE_LIVENESS_GUESTS` and `TG_ACTUATE_PROXMOX_ALLOWED_GUESTS` went from 20 to 195 names: every
non-template guest of the single NL+GR Proxmox cluster, 46 qemu and 149 lxc. Both workers were recreated and
the boot log reads 195/195. No Proxmox change was needed, because the actuation token's user already holds
VM.PowerMgmt on `/vms`. Follow-ups are on the ticket:
- GR guests carry site NL in liveness envelopes.
- Guests created later are not auto-included.
- actuation-guard-coverage now treats all 195 as its SSH population.

**TG-559 — no voting** (owner: "there should be no voting --> TG must act autonomously"). The owner's three
answers are verbatim on the ticket:
- A person's deliberate change: leave it, tell.
- Irreversible, destructive or security actions: hold, tell.
- Stamp the Law-Change trailer citing the session.

It is built as the CONSTITUTION §4.3 fallback approver (spec/012 REQ-1127). A POLL_PAUSE is resolved by
counterfactual classification: the canary pin, un-graduated class and novelty inputs are cleared, and if the
action still polls, TG holds it. A reason table would not work, because the classifier records only its
FIRST polling rule. The ruling also resolves the former [R3] owner-list item "re-arm the hands-off
ruleset", which has been removed from the board. The fresh-eyes review gave 0.90 after 9 production
histories replayed clean against the change. Arming (`TG_APPROVAL_RESOLVER=autonomous`) follows the deploy,
and the live evidence goes on the ticket.

**Two reds already true on untouched main**, found by the first MR in two weeks:
- **TG-560**: the authlog fold-window fixtures, dated 2026-08-07, aged past ingest's 30-day window.
- **TG-561**: govulncheck flagged golang.org/x/crypto for GO-2026-6354 and GO-2026-6355. The only fixed
  version declares go 1.26, which forced go.mod to `go 1.26.0` / `toolchain go1.26.8` and the Dockerfile
  build stage to `golang:1.26.8@sha256`, because the official image sets GOTOOLCHAIN=local.

Both are carried on !1729.

## 2026-09-13 (late) — TG-559 deployed, armed and live-drilled; TG-560/561 deployed

**!1729 (TG-560 + TG-561) merged → `8d411bd4` and deployed.** These were the first Go 1.26.8 images. The
fresh-eyes QA verdicts were 0.85 and 0.82, both recorded on the tickets, and both tickets stay open below
the 0.90 close bar. The stale govulncheck toolchain comment is corrected in this docs MR.

**TG-559 eval gate.** The first fast run (runs=1) failed the absolute proposal_recall floor at 1/3; the
base arm was also 1/3 and flagged degraded. Per docs/EVAL-GATE.md the remedy is more runs, not a waiver.
The runs=3 rerun passed: proposal_recall 0.56, all judged dimensions within their bars, controls 0/2.
Both records are disclosed on the ticket, and the passing record is committed.

**!1730 merged → `46da1a2a`, deployed, and ARMED 21:00Z** (`TG_APPROVAL_RESOLVER=autonomous`, boot line
verified). Live HOLD drill at 21:01:48Z: an operator (`root@pam`) stopped a watched guest on a node that the
authlog collector does not watch. TG detected the transition within about 60s, attributed it
`attributed-authorized`, and recorded ledger `classify:POLL_PAUSE` followed by
`fallback:autonomous-hold:actor-attributed-authorized`, with voter `tg:fallback-approver`. Over 90s+ there
were 0 `pending_decision` rows and 0 `action_execution` rows. The guest was restored by hand and read back
as running, with no side effects. TG-559 closed with its delivery-bar. The former [R3] hands-off-ruleset
owner item is resolved (TG-499 note), and TG-488 carries both rulings.

**Posture** now lives in the board's Live posture section. Operator rollback: unset `TG_APPROVAL_RESOLVER`
and recreate the worker; sessions started after that vote again.

## 2026-09-14 — status check: live 58 days, all 195 guests covered, three side defects filed (TG-558, TG-562/563/564)

This was a read-only check of prod (`*:ab413b91`, workers restarted 2026-09-13 21:44Z). TG has been live since
2026-07-17 and in owner-set Semi-auto since 2026-07-30. It still triages about 30 alerts a day.

**Coverage.** The hypervisor's own inventory, minus templates, lists 195 guests. Those names match the liveness
watch list and the start-guest allowlist exactly, and the liveness projection agrees with the hypervisor's
running/stopped state on every node. The one offline node hosts no guests. The token the actuator actually reads
has power rights on a container and a VM at each site. A second, unused token holds per-guest grants on only the
original 20 guests, so it reads like a coverage gap during an audit; TG-558 carries its removal.

**What has actually run.** There have been 477 executions in total (457 match, 1 partial, 2 deviation,
17 unverifiable), all at one site on 20 guests, the last on 2026-08-30. They tracked the fault injector, which
stopped on 2026-08-29. Since then the only guest-down event was the 09-13 HOLD drill, so TG is idle rather than
broken. A heal at the second site, a VM start and the autonomous ACT path have never run live (TG-558 follow-up 1).

**Filed from the same check.**
- **TG-562**: the actuation worker prints `approval resolver: human` and a CRITICAL `escalation.page` line for
  lanes that only the triage worker runs. Operators are misled; behaviour is unaffected.
- **TG-563**: semantic retrieval has been down since 2026-09-11, when the local embedding server was stopped.
  The model has no fallback, so retrieval is lexical-only. The breaker alerts fired at warning severity, but the
  Live posture line still read "operational"; this entry's MR corrects it.
- **TG-564**: world discovery cannot record a storage appliance. Migration 0047's entity-type CHECK predates the
  `storage_appliance` type (and lacks `cluster`), so that draft fails on every pass and the pass reports it only
  as "1 item error".

