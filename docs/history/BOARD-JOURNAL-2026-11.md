# BOARD journal — 2026-11 (verbatim historical archive)

**Historical journal, verbatim, not a queue.** Entries are as-of their stated dates; evidence
anchors (`file:line`, `!MR` states, counts) are as-of their entry date and may since have
drifted. The live board is `docs/BOARD.md`; the open-work inventory is YouTrack
`project: TG #Unresolved`. Nothing may exist ONLY here — every entry names its tracker id.

## 2026-11-07 (night) — lane G of the G4A fix-first program: claims brought to outcome, shared inputs mapped (TG-680, TG-681)

Owner prompt of 21:20 NL §2.E and §2.G (verbatim in the evidence inbox). Two MRs from worktrees `laneG-tg680` /
`laneG-tg681`; both issues stay open for the coordinator.

- **TG-680 — product and evaluation claims (docs only).** Every corrected claim is in the new
  `docs/CLAIMS-REGISTER.md` with its old text, new text and a basis label (owner-approved / documented / verified in
  code / observed / inferred / unknown). The load-bearing corrections, each from the running system or a committed
  record rather than a stamp:
  - README no longer says TG "autonomously remediates across a whole estate (Kubernetes, hypervisors, network,
    storage, security)" or is "built for air-gapped estates"; it states what has executed (Proxmox guest start,
    systemd/Docker unit and container start/restart, on injected faults on a test pool, 0 organic executions) and that
    the reference deployment reasons through external providers, with the local-model path unexercised.
  - The "SINGLE-BRAIN on Claude Opus 5, fallback rungs empty, judge on a different model" paragraph was stale against
    `deploy/litellm-config.yaml` (Azure OpenAI primary/fast, Claude rescue lane, Opus judge sharing a family with the
    rescue lane and the predecessor). Corrected in README, PRODUCT, ARCHITECTURE.
  - `TESTING-AND-BENCHMARK.md` still described multi-tenancy (tenant_id, RLS, per-tenant chains) on 19 lines, against
    ADR-0010 and every sibling doc; rewritten to the single-org posture.
  - The predecessor comparison is labelled a predecessor experiment everywhere (README, BENCHMARK-LADDER,
    BENCHMARK-AXES, TESTING-AND-BENCHMARK, the shadowbench README): campaign #3 = 32 pairs submitted, 30 analysed
    across 13 hosts, TG-only-correct 7 vs 0, MATCHES (non-inferior); concordance is not correctness (over the
    committed ground truth 4 pairs both-correct, 20 both-wrong — every service-down and container-down fault
    defeated both systems); the judged secondaries significantly favoured the predecessor; the judge shares the
    predecessor's model family; the McNemar p-value is not host-clustered although 10/13 hosts repeat; and the primary
    endpoint's documentation differs between PRE-REGISTRATION §1/§7 and the frozen record (hand-assembled ground truth;
    "≥ 15 hosts" vs `MIN_HOSTS = 12`).
  - Lanes by outcome, from the 2026-11-07 boot logs: Cisco READ (`cisco-show`) and WRITE both DARK (no devices
    declared); AWX job lane unarmed; the kubectl/helm module compiled but wired into no worker; the gitops-mr actuator
    **ARMED on the actuation plane** with its Argo CD convergence reader (`TG_GITOPSMR_ARM` set), 0 executions. The
    parent-directory notes that call gitops-mr "dark" are stale on that point — armed-and-unexercised is the honest
    label, and nothing was armed or disarmed by this lane.
  - The resolver is a product role: default `human` (console vote); the owner's deployment runs `autonomous`
    (OPERATOR-QUICKSTART, README). ROADMAP's status note said the live mode was Full-auto; the boot log reads
    Semi-auto. The G3 credits on the BOARD are marked credited-not-verified with the C1–C3 corrections.
- **TG-681 — shared failure modes of the action model, the predictor and the verifier.** Mapped at `origin/main`
  6851ba94 (ARCHITECTURE § 3.1, THREAT-MODEL § 5.4): one `estate.Holder` snapshot, one ingest ledger, one host NAME,
  reader-side freshness. "Independent" means independent of the model, not of the world it read. Smallest
  validations added in existing code, each with tests and executed killing mutations:
  - `estate.Holder` stamps the serving snapshot's install time; a rejected refresh keeps the prior graph AND its
    stamp, so `tg_estate_graph_age_seconds` (new, beside `tg_estate_graph_installed`) grows through an outage.
  - The TG-625 ledger readers are wrapped by a per-SITE front-door arm: the acting site's newest-delivery age is
    logged on every post-state/clear observation; `TG_POSTSTATE_SITE_FRESH_WITHIN` (both worker services) makes an
    older site read UNOBSERVABLE. Default record-only, from data: over 14 days NL delivered 10,449 times (p99 gap
    16 min) and GR 226 times with seven gaps over 3 h and a 90 h maximum — a hard site bound is an operator's trade,
    not a default (no new floor, per the owner's G4A instruction).
  - Pinned by test: a renamed or unknown target is prediction-ineligible → poll → the autonomous approver HOLDS; an
    empty estate holds every target; a stale snapshot still drives the prediction unmarked; a separator-variant name
    resolves at the fuzzy tier and is predicted over as confirmed, while the TG-378 precondition asks the liveness
    projection by the literal string and refuses an unknown name before any prediction commits.
  - k8s site-awareness recorded as a G5 precondition (`K8S-MANAGEMENT-REGIME-DESIGN.md` § 7, deep-review F24): k8s
    alerts carry no site on this estate (16 unattributed cohort incidents, pack § 10.4) and the graph has no k8s edges.
- **Found, not fixed, for the coordinator:** 1,071 occurrences in the last 14 days are stamped site `no` (plus `ch`
  12, `tx` 5), outside the declared `dc1`/`dc2` vocabulary; the site-liveness arm ignores them by design.
  The DSN-gated `core/db` oracles skipped locally (no Postgres on the dev box) and run in CI's harness job.

## 2026-11-07 (evening) — the injector stopped and drained; the pool at rest; one TG-618 hold after the fix

- **Injector STOPPED 17:10 NL** (TG-624; owner ruling 11-07: settle TG-658 before G4). The unit drained its
  outstanding restore obligations instead of abandoning them; the last came due at 17:52. **Pool at rest:**
  all 113 G3 injected faults read `restored` ("the ledger and the estate agree"), and the unit is inactive and
  disabled. The drop-in still names all five classes (max-busy 6, 45-minute restore), so G4 restarts it unchanged.
  The two disposable soak VMs stay until after G4.
- **G4 dry-read** (`axisscore -benchmark-window`, read-only, 11-06 10:13 → 11-07 17:5x; not a valid window): GR
  already clears n ≥ 30 on every axis. NL is short only on the actuation-gated cells (A3 24, A4 29, A7 24, A6b recovery 22).
  The window FAILS on span (1.3 of 14 days) and on THREE harness fingerprints, because each agent-loop deploy moves it.
  So the G4 window needs the harness frozen, not just the rubric.
- **Next: TG-658.** Tonight's nightly measures main's proposal_rate on a quiet estate. G4 (≥ 14 days, rubric +
  harness frozen, all five classes) opens only once that is settled.
- **TG-618 follow-up (still Fixed).** One `rationale-names-a-different-host` hold came after the fix was live
  (11-06 13:14 NL, about 12 h after the deploy). TG does not persist the rationale it judged, so the trigger cannot
  be reconstructed. Main's check agrees on that alert's last recorded rationale. An earlier, never-merged TG-618
  variant on the TG-613 lab branch also discounts the action's own values and a prior incident's reference, but it
  lacks main's component match and holds a case main clears. Recorded on the ticket; if it recurs, the hold should
  record what it judged.

## 2026-11-07 (afternoon) — G3 reached; eight live defects found and fixed while the soak ran

**G3 REACHED 16:4x NL** (TG-624): verified_clean since G1 — start-guest 16 (3 qemu, 7 GR), start-service 15 (10 GR),
start-container 15 (11 GR), restart-container 15 (9 GR; its ladder level rose auto_notice → auto), restart-service 15
(10 GR). As each class met the bar its fault class left the injector, so the slots went to the classes still short.

Defects the soak exposed after the power loss, each fixed, deployed and (for the agent loop) full-gate evaluated:
- **TG-651** (!1799): a 3-second secret-store leader election failed a triage. A standby's no-active-node 500 now
  enters the bounded standby retry.
- **TG-655** (!1803/!1804): every G3 container credit was blocked by "collateral" on a hypervisor and edge hosts, via
  chaos-learned edges ("a hypervisor depends_on a guest of another node") minted under six concurrent injections.
  Collateral now ignores chaos paths, and the learner drops own-fault, ambient and blank-host alerts (89 → 42 pairs).
  Requiring 2 injections stays a recorded decision.
- **TG-656** (!1805): a deploy recreates the model gateway (~20 s) and in-flight triages died on "connection refused".
  Unreachable-gateway dials are retried at the call level.
- **TG-643** (!1807): about 1 session in 6 stopped at step 1 on near-misses of the directive grammar (a list, a label,
  prose, echoed TOOL_OUTCOME lines). There is now an array unwrap plus ONE format re-ask per session: the bad reply is
  never parsed, never re-sent, and a constant placeholder plus a fixed line go back instead (INV-08).
- **TG-652** (!1806, owner-ruled "retry briefly, then hold"): a failed actor-evidence read had made an admin's
  deliberate stop look like a crash. The read now retries, then a start-guest HOLDS.
- **TG-653** (!1801): a hypervisor cluster-service restart passed the approver; a never-touch subject now reads
  destructive at classify time.
- **TG-654** (!1800, owner policy): the pve04 RAM swap is modelled as exclusive guest groups, in the detector and
  the start-guest leaf.
- **TG-659** (!1808): a confidence-threshold stop now records why on its step.

Filed, open: TG-657 (an infrastructure-failed triage is never retried; each mid-soak deploy cost 1-2 heals), **TG-658**
(main's eval proposal_rate 0.15, under the 0.25 floor, on BOTH arms of every gate today; not caused by these changes).
Owner ruling 16:5x: G4 opens only once the nightly settles TG-658; the injector is STOPPED and the pool rests meanwhile.

## 2026-11-07 — the owner list emptied: every entry was already resolved

Asked "are other approvals pending?", the session checked each owner-list entry against YouTrack. All five were
already resolved, so the list said "waiting on the owner" about nothing: TG-490 Fixed (and armed: tracker writes
on, dedicated project), TG-180 Fixed, TG-536 Fixed (3f39b24f), TG-74 Obsolete, TG-129 Won't fix, TG-32 Obsolete.
The same day's owner rulings were taken directly in session: TG-652 "retry briefly, then hold", TG-650 "leave it
as is" (closed Won't fix), TG-649 "apply on all 4 nodes" (applied). The retired entries, verbatim:

    - [R3] TG-490 tracker-WRITES arming: TG's entry-ticket creation (merged, dark) needs TG_YOUTRACK_WRITES,
      which also enables close-out comments/transitions on the SHARED predecessor-driven corpus — the compose
      file's own comment warns an accidental write contaminates the head-to-head comparison at the input
      (Campaign #2's precondition, owner-ruled). Owner call: arm now (dedicated TG project, accepting the
      shared-corpus write surface) or hold until Campaign #2 concludes. Code fully merged; TG_TRACKER_CREATE_PROJECT unset keeps everything dark meanwhile.
    - [R2] Tier-4 WIDE estate-partition scenarios (TG-74) — fresh ask AFTER the netmiko/paramiko switch-port
      tier has produced evidence (TG-488 B15).
    - [R5] Reopen decisions for the deferred north-stars TG-129 / TG-32 — a v2 conversation,
      post-capstone (TG-488 B1). (TG-128/groundnet DELIVERED 2026-08-29 — dormant/default-off, spec/021 merged; tracker has it. Only its far-future live network stays deferred.)
    - [R3] **TG-180 observability probe arming.** The observation census is LIVE (`tg_observation_census{state}`
      splits estate silence into healthy vs structurally-unobservable); the fault-injection PROBE that makes the
      census falsifiable is BUILT but ships `TG_OBSERVE_PROBE_ENABLED`-off. Arming it periodically injects a fault
      into a SAMPLED estate entity to test whether observability surfaces it — a deliberate estate perturbation
      (the ticket's lowest safety sub-score, 11/15). Owner call: authorize probe-arming? Secondary: "coverage of
      the unmeasured" currently ships as a Prometheus metric, not a `core/axis` scorecard dimension — confirm that
      suffices or ask for the axis. Built + dormant meanwhile; ref TG-180. **2026-08-23 correction: it was UNARMABLE, not merely unarmed** — the probe harness read NINE `TG_OBSERVE_PROBE_*` keys that no compose service forwarded, so an operator setting them in `.env` would never have changed the live "observation probe loop: not configured" line. Forwarded in !1634. Nobody should record a failed arm attempt before that as a decision.

    - [R4] **TG-536 — a lockstep re-stamp has no RBAC gate and leaves no ledger record.**
      `core/governance.AuthorizeRestamp` calls itself "the SOLE path by which re-stamped content hashes may be
      accepted" (REQ-703: RBAC-gated, audited, "never a host-local edit") and has ZERO production callers;
      `specvalidate lockstep --restamp` enforces the same-diff rule independently but neither the RBAC check
      nor the ledger append. Owner call because the fix is a decision, not a wire: where a CLI restamp gets its
      actor role, and which ledger a developer-box restamp appends to. Honest alternative: if attribution-free
      host-local restamps are intended, `AuthorizeRestamp` is retired-but-present and should be DELETED with
      REQ-703's prose corrected — the file must not claim a control the system does not run.

## 2026-11-05 → 11-07 — graduation G1 and G2 reached; the G3 injection soak; a site power loss mid-soak

- **Board bullet retired today (verbatim; resume budget, since its full entry is the 2026-11-03 journal):**
  - **2026-11-03 — MR gate unblocked** (!1771 `3535985f`; journal): TG-621 Fixed (a stale trend anchor no longer
    re-anchors over a broken absolute bar; follow-up TG-622); TG-589 Fixed (litellm 1.106.6, `.trivyignore` empty).
    Known red: the nightly's two estate witnesses (TG-557).

Epic **TG-624** (G1–G5). Every number below counts OUTCOMES (executions, verdicts, `graduation_credit`), never config.

- **G1, 11-05 late evening (NL):** the first live autonomous heal end to end. Detect → autonomous ACT → start-guest
  `match` → commit-confirm confirmed → `verified_clean` credit, 25 s. Unblocked by !1772 (TG-569: every ACT had been
  refused since 10-22), !1774 (TG-625: actuation-plane verdicts read TG's own ledger, ending the all-NULL verdicts),
  !1773 (TG-623: guest lists follow the cluster) and !1775 (TG-572: PVE API failover).
- **G2, 11-06 early morning:** live autonomous container and service heals with verdict `match` at both sites, through
  follow-estate leaf mode (!1776, TG-626). Supporting fixes: !1777 (TG-628), !1778 (TG-618).
- **G3 soak, from 11-06 09:43:** a low-rate injection soak on an expendable pool at both sites (8 app guests plus 2
  disposable VMs), with restore-after 45 min. Hang classes (`docker pause`, SIGSTOP) went in at !1782, so the restart
  verbs get runs. The soak found and fixed: !1779/!1788 (injector: qm device-down, every class sweeps the pool),
  !1780 (TG-631 site stamp), !1784 (TG-639), !1785 (frozen units visible; read the host before standing down),
  !1786 (TG-642), !1787/!1791/!1792 (TG-643: empty completion retried once, raw reply recorded, a leading
  `<thinking>` block unwrapped), !1789 (TG-641), !1790 (TG-646), !1793/!1795 (TG-638 hand-off host read, TG-647
  unit grounding, restart-container commit-confirm: the TG-648 ladder deadlock, fixed for that class),
  !1794/!1796 (TG-633 per-run accounting, Fixed), !1797 (TG-640 console test readiness).
- **Credits at 11-06 21:34** (verified_clean since G1; bar 15 per class): start-guest 16 (MET at 21:11; device-down
  then dropped from the injector), start-service 12, start-container 10, restart-container 8, restart-service 7.
- **Site power loss 11-06 21:34 → TG triaging again 11-07 11:47 (TG-649).** The acting hypervisor lost quorum and shut
  down its guests, TG included. On boot its cluster filesystem was late (a hosts-file self-entry typo made the node's
  own name depend on DNS hosted on that node), and the one-shot boot start-all failed and never retried. So no onboot
  guest came up until a manual start-all about 28 minutes after the node booted. No down-page fired: the probe, the
  bridge and the evaluator share the site (TG-650). After boot: both posture planes = policy_mode, resolver
  AUTONOMOUS. The injector unit is disabled, so it stayed stopped (working rule 6) until resumed by hand. Its boot
  reconcile discharged 5 of 5 inherited obligations, one of them a container still stopped through the reboot.
  Main pipeline 63308 lost two verify jobs to the outage (retried green); its deploy failed while the deploy
  dependency was still starting, and the retry was cancelled in favour of the next merge's deploy (no runtime diff).
- **Found after the restart:** a 3-second secret-store leader election failed one triage, because the gateway key is
  read per model call and investigate retries once after 1 s (TG-651). Two new step-1 unparseable shapes appeared:
  a one-element directive array (fix in flight under TG-643) and plain `TOOL:` text (stays fail-closed, INV-08).

## 2026-11-03 — the MR gate unblocked: a calendar-exposed eval-gate defect and a stale gateway pin

- **Board bullet retired today (verbatim; resume budget — its full entry is the 2026-10-24 journal):**
  - **2026-10-24 — TG-600 Fixed** (prod `*:ec6af1bb`, H2-1c; journal): the rendering BEHIND each harness fingerprint
    is persisted (`harness_render`, mig 0117) and diffed via `tools/harnessdiff`; boot line ON, table self-arms on the
    first session. Sidecar follow-up = TG-601 (not started).

!1770 (README: link the Mission and Use Policy) could not merge. Every MR pipeline was red on two failures that
were already on main, so no MR's diff caused either.

- **`build-test`: TestCLI_TrendRubricCrossingDoesNotReanchorOverAnAbsoluteFloor → TG-621 Fixed.**
  - The test hardcoded its anchor as `MeasuredAt "2026-10-18"`. From about 10-26 that date was past
    `gate.TrendMaxStaleness` (8d), so the CLI reached `ShouldRefreshTrend`'s stale branch (TG-424).
  - That branch re-anchored on ANY regression, including a run that had failed the proposal_recall floor
    (0.00 < 0.50). The calendar exposed a real hole, not just a rotten fixture.
  - The stale branch now goes through `ReanchorEligible`, the allow-list the rubric-crossing path (TG-595)
    already used. The fixture is clock-relative and runs fresh and stale. A killing mutation turns RED.
  - No live impact: the committed anchor is fresh (11-03), and the last 8 nightlies held the floor at
    0.54–0.71.
  - Fresh-eyes review: APPROVE. Its one medium finding is pre-existing and shared with the crossing path:
    `ReanchorEligible` ignores `Verdict.Unmeasured`. Filed as **TG-622**.
- **`trivy-image-scan`: 4 fixable CRITICALs in the pinned gateway image → TG-589 Fixed.**
  - The findings: wolfi ca-certificates-bundle CVE-2026-91403 and libbrotli×3 CVE-2026-111497.
  - Re-pinned to main-stable `@37e2dafb`, which moves litellm from 1.104.2 to 1.106.6. It ships certifi
    2026.10.19, so the last `.trivyignore` entry (GHSA-6q48-6c7w-9gw2, expiring 11-21) is retired.
  - Verified with the TG-573 procedure: trivy with the gate's flags exits 0, and the old digest is the exit-1
    control. Old and new were smoke-booted with the real config: identical 13 routes, healthcheck OK.
  - After deploy, live calls on `fast` and `primary` answered.
- **Landed:** !1771 (merge `3535985f`, MR pipeline 62735 21/21, main 62736 green incl. deploy). !1770 rebased
  onto it and merged (`2d1b0339`).
- **Not touched:** the scheduled 03:53 main pipeline is still red every night on `delivery-witnesses` and
  `actuation-guard-coverage` (TG-557).
  - The deployed-sha witness is BLIND structurally: the tip is always the nightly's skip-ci commit, and
    neither git (absent from the witness image) nor the commits-API fallback resolves the last buildable
    ancestor.
  - The coverage check reports 111/195 hosts AWX-unreachable.
