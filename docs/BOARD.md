# Territory Grounder — BOARD (the one authoritative queue)

**Work this file top-down.** It is the only file that says what to do next. Every claim below carries the
evidence that settles it — a `file:line`, a command, or a live check — because its predecessor was retired
for accumulating confident statements nobody could check.

**THIS BOARD IS NOT THE INVENTORY, AND ITS SILENCE IS NOT CLOSURE.** The complete list of outstanding work
is YouTrack: `project: TG #Unresolved` (199 open, 2026-08-02). This board holds the RANKED queue — what to
do next and why — and deliberately does not hold all of it. The previous board declared itself "the only
live status surface" while capping itself at ~8 KB; those two commitments are incompatible, and the cap
won. It shed items by deletion until it cited 20 of 199 open issues, and what fell out included every
`[P0][safety]` finding and an `[URGENT][SECURITY]` ticket open since 2026-07-22. **There is no byte cap on
this file.** The discipline that replaces it: **nothing may exist ONLY here** — every item names a tracker
id. As of 2026-08-02 every queue item does; the five findings that were board-only when this was
written are TG-254..TG-258.

Update on-event (merge, deploy, live-verify), never batched to session end.

## Working rules (owner-set, carried forward)

1. Work the queue top-down. Do not deepen a component above ~75% operational except to fix a defect you
   introduced.
2. **Confirmed live, or it did not happen.** Merged ≠ deployed ≠ operational. Show the rows or the logs.
3. Prefer arming what is already built (a missing caller, blank config) over new code.
4. A component does not advance while its oracles cannot fail.
5. **A change lands only if it moves a named oracle from red to green.** No polish rounds, no audit sweeps.
   This project once spent 13 days and 482 console path-touches on "make it perfect" — 113 fixes, 7 UX
   sweeps at a 23.5% false-finding rate, closed three times and reopened. "Perfect" is unfalsifiable so it
   never converges. Defects found while looking ride the normal bug flow.
6. **Guinea pigs rest healthy.** When the pool is not ACTIVELY in use by a running test, the injector stays
   STOPPED AND DISABLED and every pool guest is left healthy. Injection is a deliberate, bounded act with a
   named consumer, re-armed for the occasion and stopped after.
7. **Parallel sessions follow the TG-488 protocol** (AGENTS.md § Parallel sessions): partition on
   the record, claim-before-touch (lint-enforced in `make all`), worktrees only, session-pinned eval
   ports, and the gateway lock as the box heavy-load mutex.


## Live posture — verified 2026-08-11, never from a stamp

**Re-verified 2026-08-11 session start:** deployed `worker:7ccc6b28` == last buildable main (`59363eef` tip is a
`[skip ci]` board commit; image tag read on the deploy host, container up since 23:10Z). Merge gate
`only_allow_merge_if_pipeline_succeeds=true` re-verified via authenticated `glab api` (the local witness stays
BLIND-but-honest — TG-429). Scheduled delivery-witness: last GREEN 08-09; the 08-08/08-10 scheduled runs were
CANCELED at their 03:53 slot — deploy sync verified directly this session instead. Compose stack Up/healthy.
No injector unit/container present on the box — consistent with STOPPED AND DISABLED (working rule 6).
`make check` green (fails closed to POLL_PAUSE off-box). Items below carry the 08-10 verification where not restated.

Deployed 08-14 wave 4: image `d47ad52c` (contains !1388..!1401 — the TG-114 C-train, TG-109/107 close-out,
the TG-476 compose class filter sealing the judge-rubric seed leak, the eval-gate `skills/` removal
[owner-ruled, law trailer], console-e2e rail-race fix; ancestry verified on-box). Earlier 08-14:
!1381..!1387 (journal). (2026-08-13 run: TG-104 · TG-456 · TG-354 · TG-176 · TG-307 · TG-394;
disk recovered 100%→31% after a CI-storm prune). Prior: `316b7a70`, `ad528ef1`. Console
origin is **https://territory-grounder.example.net** — NOT `:8080`; the session cookie is
`Secure`, so over plain http login returns 200, the browser discards the cookie, and the console
silently stays on the unauthenticated preview shell.

- Mode **owner-set Semi-auto** — worker-actuate boot log 2026-08-09: mode-transition RBAC "mode
  stays Semi-auto", policy engine "active mode Semi-auto (7 rules)"; `tg_may_actuate` reads
  1 actuation / 1 triage / 0 grounder live in Prometheus. Absent/zero/corrupt fails closed to Shadow.
- `TG_SECRET_POLICY=enforce` — live; boot scan 152 vars, 0 raw violations. The boot log's own
  honest caveat stands: 3 DSN vars carry an in-URL password the shape rule never examines (TG-368).
- `TG_EMBED_MODEL=embed-nomic` — set; semantic retrieval operational.
- `TG_DECAY_INTERVAL=1h` — **SET** (the 08-02 stamp said UNSET; spec/018 recency decay is armed).
- Module probe sweep at 10m: **10 ran, 10 ok, 0 failed, 1 skipped** (08-02 stamp said 8/8).
- Credential sources: **openbao (external substrate ON), awx, ldap, native-hostdiag** — the 08-02
  "OpenBao deliberately OFF" no longer holds.
- Egress meter **enforce** on both workers (33 / 10 declared destinations; off-allowlist BLOCKED).
- Merge gate: `only_allow_merge_if_pipeline_succeeds=true` on grounder AND www (flipped 2026-08-10;
  skipped pipelines do NOT count as success) — merging a red or pipeline-less MR is now refused
  server-side.

# THE QUEUE

**LANDED blocks (2026-08-10 · 08-11 · 08-14: TG-435/436 · TG-112/378/152-L1 · TG-489/39/484 · the TG-428
PM-overhaul) moved VERBATIM to `history/BOARD-JOURNAL-2026-08.md` (2026-08-25 entry) — the resume-budget
gate's rule: landed narrative is journal, not queue.** The queue below is worked under the operating loop
(AGENTS.md) and the Go! contract (CLAUDE.md); progress toward DoD v1.1 is read from `make ledger`, never
hand-written.

Ranked by consequence — what it costs while it stays broken — not by effort. This ordering is on merit.

## LIVE RE-RANK — 2026-08-16 (the 08-11 table below is STALE: it lists now-Fixed items as open)

## 2026-08-25 — GRADUATION PLAN APPROVED (owner) — the queue IS the plan's phases

The owner approved the graduate-and-replace-predecessor plan (journal 2026-08-25 entry carries the rulings:
evals resume after code-complete; campaign #3 = as-deployed stacks; §5.2 VISR deferred [R5]; TG-437 approver
fix post-exam [R4]; the B16 reading; TG-536 = wire AuthorizeRestamp).

**Phase 0+1 EXECUTED same date (full detail: journal 08-25 wave entry; every merge reviewed ≥0.90):**
!1634 !1645 !1648 !1649 !1650 !1651 !1652 !1653 merged; TG-537/526/538/536 CLOSED at the bar; the §6
campaign-#3 amendment is LAW (boundary 2026-08-26 · manifest-membership population · GT stop-term ·
as-deployed arms); TG-122 verified code-complete (arm = Phase 4); TG-78 → draft !1647 (eval week);
!1654/!1655/!1656 in flight; TG-539 close pends one green scheduled run. Prod `worker:a567d400`,
all merges ancestors (first-hand inspect).

**Phase 2 CLOSED (night 08-25, journal: late-night entry):** TG-85 + TG-541 RESOLVED; TG-78's build
slices all done (node-plane, k8s trio, three pack literals — each full-rigor gated); P2-6 ARMED; §5:
4 GREEN / 1 RED (§5.4 judge TNR 0.060 — TG-542) / 1 BLOCKED-by-[R5]. Exam preflight DONE: pool 15/15
restored+healthy, [R2] ruled AS-DEPLOYED all four classes; campaign #3 arms 08-26 06:30.
→ Phase 3 the exam (pool 5→15, one armed 06:30→18:30 window, GT build, frozen verdict) → Phase 4 exceed +
cutover (mutation-ON canary [R2] · TG-490 · TG-437 · hands-off re-arm [R3] · recipe #1 = intersite heal ·
parity-gap ingest · sender repoints · predecessor standby).

**2026-08-23 reachability session** (8 unarmable controls, derived-guard fixes, the attack-a-gate rule):
narrative moved VERBATIM to the journal's 2026-08-25 entry; the durable rules live in AGENTS.md/CLAUDE.md.

---

`make ledger` 2026-08-16: **499 total · 42 unresolved [STALE — `#Unresolved` = 1 on 2026-08-31: TG-78, vSphere-only (train landed d2afc611; TG-464/556 closed; OWNER is provisioning vSphere and will signal ready — the build waits on that signal alone)] · 457 resolved · 167/167 evidence-bearing · 0 bare** — a
CLEAN ledger. The 08-16 bare-sweep reconciled TG-494/244/72/498/463 with `## delivery-bar` markers (each verified:
MR merged + commits on main + the TG-72 scorecard present), and the peer closed TG-82 (the last bare).

(08-16 delivered/drained narrative: superseded; journal 2026-08-25.)

- **OWNER list — ALL RULED 2026-08-25 (night), one-by-one; verbatim in the session rulings log.** The
  return gate is now: [R2] canary arms ON a winning verdict · TG-490 arms at exam-window close (win or
  lose) · start-guest re-promotes via the ladder (no flip) · retire = owner's word, standby indefinite ·
  recipe #1 = intersite-tunnel heal (owner names the attended-drill slot) · TG-129 first north-star to
  spec post-cutover (128/130 stay deferred) · TG-481 object-group model post-cutover · TG-315
  builds+arms with Phase 4 · TG-74 post-cutover on the owner's day · TG-529 was already ruled 08-22 and
  is DELIVERED (prod seeds 12 runbooks; stale entry corrected). Still genuinely open on this list:
  TG-180 [R3] observability-probe arming (not raised — surfaces with the Phase-4 arm set).
- **LIVE-ATTENDED / estate** (code often built; arming is a live prod mutation): TG-423 ssh-CA 26-host roll ·
  TG-420 egress proxy (single-brain model path) · TG-381 egress→router RFC1918 drop · TG-414 off-box-scaffolding
  retire · TG-313 temporal-postgres reservation (substrate restart) · TG-91 vSphere/Slurpit (live e2e) · TG-86 estate-IaC grounding.
- **EVAL-GATED** (in scope WITH the mandatory on-box gate; needs a quiet box — coordinate with the peer's TG-82
  drill): TG-508 · TG-50 · TG-53 · TG-47 · TG-56 · TG-36 · TG-78 · TG-85 · TG-122. (TG-58 left this
  list 08-22: all four Phase-2 prerequisites delivered — cage live in enforce, coordinator+undo via
  spec/030 !1625, pre-state armed — eval waived by the owner's deliver-code ruling.)
- **DONE but one slice remains** (not closeable): TG-506 (warnings on /metrics done; console-UX + admin engine-toggle
  remain) · TG-168 (forensic CLI both halves done; live model lane is shared-litellm-gated) · TG-146 (S2 done; residual findings).
- **EPICS / large in-flight refactors** (don't churn): epics TG-320 · TG-70/73 · TG-155 · TG-4 · TG-81 ; the peer
  (close-untouched-backlog) drives TG-348 + TG-82 loop-completion drills ; TG-233 (console e2e) + TG-501 (main.go split) are large carves.

## RANKED BY CATEGORY — 2026-08-11 — RETIRED 2026-08-23

The 08-11 category table is gone rather than re-marked stale. It derived a working order over **90
unresolved** issues; `make ledger` reads **13** today, so most of its rows named work that has since
closed, and the 08-16 re-rank above had already superseded it — a table that lists Fixed items as open is
the inverse of this board's own rule that its silence is not closure. Verbatim in
`history/BOARD-JOURNAL-2026-08.md`; the live queue is `project: TG #Unresolved`.

### Standing scope (owner-set 2026-08-14 #2 — the autonomy boundary; supersedes rows-3-8)

**Mandate = the ENTIRE unresolved set, worked under the TG-488 boundary.** Default: DECIDE, DO, RECORD
(owner veto after the fact); only R1–R7 items may wait, tagged on the owner list below. Eval-gated changes
are standing-in-scope WITH the mandatory on-box gate (red ⇒ surface, don't merge). The 08-14 rulings
unblocked every formerly (S)/(O)-flagged item except the tagged residuals — where TG-488 rules otherwise,
the (S)/(O) flags in the 08-11 table are SUPERSEDED. Campaign #2 runs strictly LAST (after ALL code is
delivered and e2e-tested — TG-488 B16). Ordering constraint: TG-489 (distillate hash-chain) lands BEFORE
the TG-114 C-6 flywheel arming goes live.

### Prior standing scopes — SUPERSEDED (verbatim in the journal)

The 2026-08-14 rows-3–8 scope, the 2026-08-13 speculative-tail scope, and the 2026-08-05 145-ticket
ranking — plus the 08-14/08-15 wave-4/wave-5 delivery narrative (closes, merges, the brain-collapse
HEADLINE, the TG-500 eval-gate diagnosis) — are SUPERSEDED by the 08-14 #2 mandate above and moved
verbatim to `history/BOARD-JOURNAL-2026-08.md`. The consequence principle + work classes below remain
operative; the live owner asks from that window are folded into the Owner list below.

### Work classes (owner-set; extracted from the 2026-08-08 reachability review)

- **AFK-tractable** (unattended sessions may take): bounded red→green on a NAMED oracle —
  deterministic Go/infra/docs/CI defects, additive observability, tracker/board reconciliation,
  the resolved-issue verification sweep.
- **Supervised-only** (scope + carve + surface, never take unattended): eval-gated behavior
  changes (on-box gate mandatory), credential-path-critical builds (the `dyn:` resolver class),
  hot-path activation toggles the deploy health gate cannot observe (TG-384 class), large
  mutually-coupled rebuilds (TG-385/376/387 class), forensic/IR work, constitutional amendments.
- **Owner-only**: positioning/roadmap (TARGET-*), arming decisions on new autonomous-loop inputs,
  everything under "Not our backlog" below.
- **Queue exhausted ≠ done**: re-read `project: TG #Unresolved`, re-rank by the category
  principle, append the new ranking here dated, surface owner items — then work the
  resolved-issue verification sweep (TG-339 precedent: sampled "Fixed" issues re-verified live).

## Journal

Dated progress / landed / withdrawn / incident entries live in
`docs/history/BOARD-JOURNAL-YYYY-MM.md` (current: `BOARD-JOURNAL-2026-08.md`), moved there
on-event, verbatim. They are history, not queue. Nothing may exist ONLY there either — every
entry names its TG-/! id, and a durable lesson graduates to AGENTS.md or a gate, never lingers
as journal prose.

## Owner list — boundary-tagged (owner-ruled 2026-08-14, TG-488)

Under the TG-488 boundary (default: DECIDE, DO, RECORD; reserved classes R1–R7 only), an entry may live
here ONLY with its clause tag. An untagged entry is a DEFECT — the session resolves it by deciding it.
`scripts/lint-autonomy-boundary.sh` (run by `make all`) enforces the tag; an EMPTY list is the goal state
and the lint prints its denominator either way.

- [R3] TG-490 tracker-WRITES arming: TG's entry-ticket creation (merged, dark) needs TG_YOUTRACK_WRITES,
  which also enables close-out comments/transitions on the SHARED predecessor-driven corpus — the compose
  file's own comment warns an accidental write contaminates the head-to-head comparison at the input
  (Campaign #2's precondition, owner-ruled). Owner call: arm now (dedicated TG project, accepting the
  shared-corpus write surface) or hold until Campaign #2 concludes. Code fully merged; TG_TRACKER_CREATE_PROJECT unset keeps everything dark meanwhile.
- [R2] Tier-4 WIDE estate-partition scenarios (TG-74) — fresh ask AFTER the netmiko/paramiko switch-port
  tier has produced evidence (TG-488 B15).
- [R5] Reopen decisions for the deferred north-stars TG-129 / TG-32 — a v2 conversation,
  post-capstone (TG-488 B1). (TG-128/groundnet DELIVERED 2026-08-29 — dormant/default-off, spec/021 merged; tracker has it. Only its far-future live network stays deferred.)
- [R3] **Re-arm the hands-off ruleset?** start-guest is now level=approve (POLL_PAUSE) — the hands-off ruleset
  was deliberately backed out (policy_ruleset_bak_handsoff), so the confirmed-guest-down heal VOTES instead of
  auto-executing. Owner ruling; an organic approve re-graduates start-guest via the ladder. Ref TG-496/TG-499.
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

Everything else formerly here was RULED on 2026-08-14 (verbatim on **TG-488** + each ticket; pre-boundary
prose in `history/BOARD-JOURNAL-2026-08.md`). Headlines: spec/029 RATIFIED · armings GRANTED (TG-464 ·
TG-466/407 · TG-114 C-6 after TG-489 · TG-463 owner-identities · TG-315 · TG-348 · TG-422/423/420 estate
grants · hostdiag pve04 · secret/tg/hosts · security-telemetry sender) · closes TG-30/37/128/129/32 ·
new TG-489/490/491/484/485.

## Definition of done (v1.0)

1. **Internal exceed-bar:** the pre-registered confirmatory campaign concludes with TG ahead on the
   judge-free primary and not behind on the secondaries, at power.
2. **Every gate can fail** — demonstrated by an executed killing mutation, not by inspection. Four were
   fixed on 2026-08-03; `make all` now requires two databases and runs all 122 core/db tests.
3. **A fresh session orients from `AGENTS.md` + this board in <10k tokens, and no steering file contradicts
   another.** Met on 2026-08-03.

---


### Definition of done v1.1 — owner-ruled 2026-08-10 (TG-428)

**The project is at 100% when every issue in `project: TG` is DELIVERED (MR merged), DEPLOYED
(prod sha ≥ merge sha, drift witness green), TESTED E2E (named oracle red→green with live
evidence on the ticket), EVALUATED (eval-gate record wherever the change touches behavior
surfaces), and has PASSED QA at ≥0.90 stated confidence (fresh-eyes review verdict recorded on
the ticket).** Measured by the generated ledger being built under TG-428 (`tools/tgledger`);
until it lands, any tally is hand-made and says so. v1.0's three criteria stay as sub-goals —
the confirmatory campaign is the capstone, and criterion 3's token budget is mechanically
enforced by `make resume-budget` (this MR).
