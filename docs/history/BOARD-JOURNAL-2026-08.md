# BOARD journal — 2026-08 (verbatim historical archive)

**Historical journal, verbatim, not a queue.** Entries are as-of their stated dates; evidence
anchors (`file:line`, `!MR` states, counts) are as-of their entry date and may since have
drifted. The live board is `docs/BOARD.md`; the open-work inventory is YouTrack
`project: TG #Unresolved`. Nothing may exist ONLY here — every entry names its tracker id.
Moved verbatim from docs/BOARD.md on 2026-08-10 (TG-428, board split): first the ranked-queue
progress journal (08-06..08-09), then the superseded 2026-08-03 rankings, then the dated
landed/withdrawn entries.

### 2026-08-16 (cont.) — TG-507 (retrieval VISIBLE to the eval) CLOSED; both lanes drained to the owner-decision floor

**TG-507 CLOSED** (MR !1504 → main `cbc46666`, delivery-bar stamped). Surfaces TG-491's `resolution_recall` as a
REPORTED, never-gated metric on the eval scorecard (`res_recall@3`, beside A4/A5/A6a) so retrieval quality is
finally VISIBLE to the deploy-gating eval — closing the retrieval-invisible-to-eval gap TG-502 exposed (MOVE A).
Reported-not-gated proven by a byte-identical-Verdict oracle; built in a fresh-context fork and reviewed BEFORE
arming (the TG-502 process lesson, applied). MOVE B (a real Retriever in judged sessions) + MOVE C (labelled
hard gate on operator relevance labels) remain OWNER-GATED per the TG-507 assessment.

**Ledger convergence (both sessions, independently confirmed):** `make ledger` 160/166 evidence-bearing, 6 bare
— ALL owner-decision-blocked (TG-494/498/82 attended + TG-244/463/72 arming). Delivery-bar retrofit + the
grandfathered sweep are done across both lanes: I un-barred my 7 + verified/marked 4 (TG-42/46/49/215) + reopened
TG-493 as a false-close; the peer verified/marked 9. `make ledger` 100% is now owner-DECISION-blocked, not
work-blocked. Delivery-bar-at-close is now a habit (recorded), not a retrofit.

Session closes (my lane): **TG-503, TG-499, TG-461, TG-504, TG-75, TG-52, TG-502, TG-507 (8)** + TG-42/46/49/215
grandfathered-marked; surfaced owner-gated: TG-180, TG-56, TG-461-credential; reopened: TG-493.

### 2026-08-16 (cont.) — TG-502 CLOSED (both retrieval paths); a process miss caught + fixed forward

**TG-502 CLOSED** — the tracker-import recency-asymmetry pre-arming blocker, fixed across BOTH retrieval paths.
Imported `ProvenanceTrackerImport` rows carry a `ResolvedAt` (~+0.25 recency) while the incumbent corpus is
UNDATED (0 of ~670 timestamped), so imports floated above + could crowd verified/inherited precedent out of the
agent's top-k once armed. Fix: withhold recency from imports (symmetric channel) + a tie-only guard (an import
never outranks a non-import on an equal score), applied to BOTH `LexicalRetriever.Retrieve` (!1501, lexical)
AND `fuseRRF` (!1503 → main `5026b681`, the LIVE FusedRetriever path — `TG_EMBED_MODEL` is set). Falsifiable
oracles (RED-under-stash) on both paths; provenance still does NOT enter the relevance score (tie-only). No
live harm — TG-244 UNARMED throughout.

**PROCESS MISS (recorded):** I armed auto-merge on the lexical fix (!1501) AND ran the fresh-eyes review in
PARALLEL; the pipeline merged the INCOMPLETE fix before the review's verdict, which then found the fuseRRF gap
(the lexical fix does not reach the armed FusedRetriever path). Fixed-forward with !1503 (review-confirmed
complete + sound, arm HELD until it cleared). Rules now: on a hygiene-critical change get the review VERDICT
before arming auto-merge; a retrieval-scoring fix must cover EVERY ranking site (lexical AND fused).
tg-code-reviewer also confirmed the caution lane is single-provenance → the same defect is NOT-APPLICABLE there.

Session closure tally (my lane): TG-503, TG-499, TG-461, TG-504, TG-75, TG-52, **TG-502** CLOSED (7); TG-180 +
the TG-461 alert-credential surfaced owner-gated.

### 2026-08-16 (cont.) — TG-52 Reflexion caution lane MERGED + CLOSED; TG-75 CLOSED + TG-180 surfaced (done-but-open scan)

**TG-52 CLOSED (MR !1498 → main `3bc62378`, State=Fixed).** The full Reflexion loop: a SEPARATE caution lane
capturing failed/unverified trajectories (which `lessons.Lesson` drops — it gates on match+ConfirmedClear),
surfaced conservatively (same host+rule-family, top-1, a `<caution>` block DISTINCT from `<precedent>`),
feeding the skill flywheel — under the invariant that a failed trajectory NEVER becomes a learned-match
precedent (`lessons.Caution` = the exact negation of `Lesson`; `Source=ProvenanceCaution`). Built by fork
(full reachable loop) + reviewed SOUND by tg-code-reviewer (both invariants — anti-poisoning + over-caution —
structurally enforced, each pinned by a non-vacuous oracle) + merged autonomously. **Eval WAIVED**
(`Eval-Gate-Waived-By: @ncpjfuzl`): prompt-inert in a per-session eval (empty caution lane → `wrapUntrusted`
emits no block → byte-identical seed), so the eval structurally cannot measure it; the oracles carry
correctness. Per the no-invented-signoff-gates ruling — the whole cat-9 retrieval line inherits this waiver
pattern. CI caught a gap the local run missed: `TestComposeEnvParity` red because the 5 new `TG_CAUTION_*`
env keys weren't forwarded in the worker compose service (recurring: `make all` omits `-race`+compose-parity)
— fixed in the same commit (forwarded + documented in `.env.example`), re-pushed green. Shipped DORMANT
(`TG_CAUTION_FILE` blank ⇒ off; boot refuses to share the `TG_KNOWLEDGE_FILE` path).

**Done-but-open scan of my domain (eval/obs/actuation) → 1 clean close + 1 surface.** TG-75 CLOSED
(dual-system outage watcher — all 3 parts on main `a02c4d75`: burst-preflight + live outage-watch +
pair-extraction/judge; the Tiers 3-4 real-outage TRIGGER is owner-gated TG-74/73, not this ticket's
agent-side deliverable). TG-180 SURFACED owner-gated (census LIVE publishing `tg_observation_census{state}`
+ fault-injection probe BUILT default-off; arming live fault-injection = an estate safety decision, and
"coverage of the unmeasured" ships as a Prometheus metric, not yet a `core/axis` scorecard dimension).

Session closure tally (my lane): **TG-503, TG-499, TG-461, TG-504, TG-75, TG-52 CLOSED**; TG-180 + the
TG-461 alert-credential surfaced owner-gated.

### 2026-08-16 (cont.) — TG-499 built + CLOSED: REQ-2902 durable-substitute confirm (owner-greenlit after grounding)

The owner asked me to GROUND the TG-499/461 REQ-2902 posture call before deciding — "how does the predecessor
solve it? what does the wisdom corpus say?" Two independent sweeps converged: (1) the predecessor confirms live
heals on the target's OWN liveness (`qm status`/`systemctl`), never the monitor that fired — **TG was strictly
WORSE on this edge** (held+paged where the incumbent confirms); (2) our own law already blesses durable-substitute
verify — spec/012's confirmed-clear belt confirms incident-close on a captured recovery transition precisely
because "the LibreNMS re-pull lags," and REQ-2902's fail-closed was HOLD-vs-auto-REVERT, not
HOLD-vs-confirm-on-substitute (the T-029-3 killing-mutation test bars confirming on the ABSENCE of a reading, not
on a different reliable source). Owner ruling: **"auto merge is the policy — never ask me to review."**

**TG-499 CLOSED (MR !1490 → main `2ac0884f`, State=Fixed).** spec/029 REQ-2902 amended: a state-preconditioned
guest heal whose terminus was unobservable MAY confirm on a FRESH, POSITIVE, mechanical `guest_liveness` re-read
(fails-differently, TG-captured) reading the guest in its desired end state; every unobservable path
(unreadable/stale/wrong-state/no-reader) STILL HOLDs+pages — fail-closed preserved, T-029-3 drill green. Built by
fork (full context) + adversarially reviewed by me (precondition→desired-state formula verified vs opschema + the
arm's REQ-2908 check) + gated clean (protected-paths PASS, lockstep 47/47 — spec/+temporal/ outside those gates,
no @ncpjfuzl trailer/restamp) + merged autonomously. Graduation flows via the existing
CommitConfirmConfirmed→RecordGraduation. **FOLLOW-ON DONE SAME SESSION — TG-461 option-c CLOSED (MR !1493 →
main `9162fd6f`, State=Fixed):** the service-fault verifier now confirms on a POSITIVE captured
`ingest_transition` recovery (RecoveredSince, spec/012 signal), positive-only + fail-closed, AlertRule threaded
incl. the orphan-sweep re-adoption; oracle + T-029-3 green; the orphan-wiring gap I caught on review was closed
before merge. So the WHOLE /plan split is delivered — **TG-503 + TG-499 + TG-461 all closed, TG-47 landed.**
TG-461's core verify gap is closed; the live-alert credential is downgraded to OPTIONAL defense-in-depth (the
durable belt now works) and dropped from the owner-list.

### 2026-08-16 — /plan close-the-backlog run: 1 closed, 1 landed, 2 surfaced (owner-gated)

Owner /plan: stop the LOC churn, close pending work, split the untouched backlog with the peer (me =
eval/obs/actuation TG-503/499/461; peer = infra/feature). Delivered against my split; the honest closure
picture is thin — 2 of the 3 are genuinely owner-gated, matching the peer's finding that "the closure picture
is thinner than the list." Main clean on `main` throughout.

**Closed (1):**
- **TG-503** (nightly eval-drift stalled 8 days) — root cause a flock SELF-DEADLOCK: the 03:40 cron wraps
  `make eval-drift` in `flock -n /tmp/tg-gateway.lock`, and since e8aefeef (08-01) eval-gate.sh re-locks the
  SAME file → 13 nights exit-75, never a model call (refutes the deepseek-empties + external-:40-job theories).
  Fix MR !1488 (main 110e64c0): eval-gate.sh reads /proc/locks, detects an ANCESTOR holding the gateway lock,
  runs serialized under it (no fail-open — a non-ancestor still waits). CI guard check-gate-lock-report_test.sh
  case 3. Self-verifies via tonight's cron (pulls main first). State=Fixed.

**Landed + merged (1):**
- **TG-47** (recall-optimized observation compaction) — MR !1487 (main 3770306c), qualified-INCONCLUSIVE
  accepted per TG-500 (compaction is inert at eval-session sizes — never crosses 64000B; the oracles carry the
  path proof), review 0.90. Delivers the compaction half; TG-47 stays open for the durable-checkpoint half.

**Surfaced — owner-gated (2), NOT taken:**
- **TG-461** — the actuate-plane live alert re-read needs a NEW alert-capable LibreNMS credential (no admin
  cred in bao → owner/infra action + a TG-337 posture call). Non-owner path = option-c durable-surface verify,
  already shipped for guest ops (!1456). Owner-list + ticket comment.
- **TG-499** — the naive fix (upgrade unverifiable→confirmed) is the exact spec/029 REQ-2902 killing mutation
  commit_confirm_test.go:343 rejects (fail-closed invariant). Spec-clean fix (route guest verify to the reliable
  guest_liveness durable surface) needs the protected core/actuate/interceptor.go (@ncpjfuzl + spec restamp) OR
  a REQ-2902 posture amendment (owner safety ruling). Overlaps TG-461 option-c. Owner-list + ticket comment.

**Dropped (churn avoided):** TG-466 s2 config-drift seed (scoped, not built — LOC, not a close).
**Board hygiene:** removed 2 resolved owner-list entries (TG-500 ratified+merged; TG-499 eval-waiver subsumed).

### 2026-08-13 (cont. 3) — Go! run post-compaction: 2 delivered live + 5 stale-opens closed (66 unresolved, was 69)

Continues the 2nd wave. Theme carried further: the "already-done but never closed" rate is real — a 3rd
audit batch found MORE. Net this wave: **5 closed, 2 delivered+merged live, 1 surfaced.** All via worktrees;
main clean on `main` throughout; every close carries file:line / commit / marker evidence.

**Delivered + merged + deployed (2):**
- **TG-465 part 1** (straddle-tolerant cluster join) — `AlertClusterStore.Join` folds a wider-than-span
  storm's fragments into one cluster (real temporal containment, fails-safe on ambiguity; membership-only, the
  collapse stays causal-gated). **The "a wrong fold cannot silence" guarantee is PINNED** by a new
  `temporal/runner` regression test (collapse decision invariant to `clusterID` VALUE) — I personally verified
  its RED-under-mutation. MR !1375 `575a5ecf`, review 9.5, deployed #48191, worker live (`CASCADE COLLAPSE
  ARMED`). Part 2 (names→prompt) eval-gated, stays open.
- **TG-188 slice 2c** (recovery-time/MTTR edge learning) — chaos-ground-truth MTTR via a `LEFT JOIN LATERAL`
  over observed `ingest_transition` recoveries (byte-for-byte-unchanged Injections/delay; not the injector's
  `restored_at`; learner-fed path honestly deferred). MR !1376 `ea175a4f`, review 9.0, DARK-until-injection.

**Closed — stale-opens found by the audit (5):**
- **TG-117** (eval quick-pass — already default, cites TG-117), **TG-450** (hostdiag auth fix proven live: 12
  successful reads), **TG-467** (eval concurrency already shipped 07-25; my `grep -iv test` recon hid it — MR
  !1377 salvaged real value: extracted it to a CI-tested `eval/dispatch.go`), **TG-60** (proposal-timeout
  bistable archetype remediation — ~30 markers, 3 fix commits on main), **TG-362** (negative-control corpus
  unsoundness fixed by `ControlExclusionReason` — ~15 markers, CI-wired). 6 stale-opens total this run
  (+TG-249 re-scoped, 2nd wave).

**Surfaced (owner):** **TG-37** (prompt-policy flywheel delivered via the skill-store/spec/014, not the literal
`eval/` artifacts — recommend close-as-superseded), **TG-229** (world-model framework + 2/3 discovery kinds
built; guest/NetBox/edges remain). Follow-up **TG-468** (hostdiag canary-dial probe — flagged owner
design-decision: the module deliberately does NOT dial).

**One hazard caught + recovered:** a review subagent's own mutation testing `git checkout`'d and wiped the
build's UNCOMMITTED `RecoverySeconds` field (`estate.go`+`chaos.go`); caught because I re-ran `go build` with a
REAL exit code (an earlier `| head; echo $?` had masked it), restored faithfully from the `DelaySeconds`
template, re-verified. Lesson → memory: commit a build subagent's work BEFORE dispatching a mutation-testing
reviewer on the same worktree.

**Honest bottom line:** the clean-AFK *delivery* well is exhausted — the last three "candidate" items
(TG-117/450/467) all turned out already-built. The remaining 66 need an owner decision (TG-122 sign-off ·
on-box eval-session · TG-82 SDD · TG-109-write P1 · estate-write · TG-348 re-injection). All worktrees cleaned;
main clean on `main`.



Continues the WRAP below. Owner said "Go!" after I recommended the TG-122 design. Theme: the clean-AFK
*close* well was thinner than the count implied — several concrete-engineering tickets were STALE-OPEN
(landed, marker in code, never closed). Verified + reconciled with skeptical eyes.

**TG-122 — GitOps-MR + k8s-declarative lane DESIGN drafted, committed (`50ca0786`), surfaced.** Pure design,
zero actuation, Shadow throughout. Key finding: the 2026-07-18 draft's protected-path-heavy async back-half is
SUBSUMED by the shipped `core/regime/asyncverify.go` (its doc literally names "gitops-mr merge request" a tenant)
— so the two lanes reduce to a mostly non-protected build (only 2 small `core/actuate` touches). Doubly-gated
(TG chain + human/Atlantis; bot=Developer-not-merge), never direct-applies, Shadow-opens-nothing. 6 owner
questions gate the spec/017 restamp. `docs/design/TG-122-gitops-mr-lane.md`. → owner list.

**Closed (2), proven:**
- **TG-117** (eval-gate 2h bottleneck, owner-flagged HIGH) — STALE-OPEN. The fast quick-pass IS already the
  default (`eval-gate.sh:149-154`, cites TG-117; `RUNS=1`/`LIMIT=8` ~10-15min; `TG_EVAL_FULL=1` = full rigor;
  deterministic-skip via `lint-eval-evidence.sh` behavior_re; tunnel-reuse guardrail cites TG-117). Blocking DoD
  met. Split residual (concurrent session dispatch, still sequential in `eval/*.go`) → **TG-467**.
- **TG-450** (hostdiag auth-rejected on the pool → evidence path cold) — key drift fixed AND **proven live**:
  prod `agent_step_evidence` shows 12 successful authenticated hostdiag reads since 08-12, incl.
  `check-host-services on dc1librespeed01 (read-only, via root@…): === derived: down services` — the exact
  guest that was refusing. 2b (classify→AUTO on a fault) = TG-348 leg-c (owner-gated, ticket's own scope);
  item 3 (reachability probe) → **TG-468**.

**Re-scoped (1): TG-249** — all 7 engineering findings verified LANDED against current `main` (each carries a
`TG-249 item N` code marker: `auth.go:281`, `sessions_read.go:150`, `axis_read.go:102`, `graduation_credit.go`,
compose `730-733`; #5 delivered this run). Summary+description updated; only the owner-gated frozen-analysis §6
trio (`shadowbench/analyze.py`, needs a `Law-Change-Approved-By` amendment) genuinely remains. → owner list.

**Systematic stale-open audit (10 tickets, 2 parallel subagents) — NO further stale-DONEs; board honesty
VALIDATED.** TG-414 (estate-write + deliberate revert-window hold; git marker a red-herring on an adjacent
scrape-repoint), TG-233 (128 `waitForTimeout`/73 files, sweep unstarted), TG-439 (retirement deliberately
abandoned — fixtures oracle-load-bearing) = correctly gated. TG-188 (delay-learning done, recovery/MTTR slice 2c
not), TG-109 (read surfaces live off `/v1/credentials/*`, write/config half not), TG-55/TG-36 (real slices
landed w/ markers, named work remains) = legitimately PARTIAL. TG-215/TG-56/TG-49 = genuinely OPEN (unbuilt;
TG-56 deliberately locked OFF by a test). Pattern → memory `stale-open-tickets-verify-before-gating`.

**Follow-ups:** TG-467 (concurrent eval dispatch), TG-468 (hostdiag canary-dial auth-probe — flagged as an owner
DESIGN decision: the module's selftest deliberately does NOT dial, which is why the config-only probe stayed
green through the 07-31→08-12 auth outage).

**Net:** 2 closed + 1 re-scoped + 1 design surfaced. The remaining ~66 are genuinely gated/partial/epic/eval —
the audit confirms the clean-AFK-*close* well is drained; the high-value next tranches need one owner decision
each (TG-122 sign-off · an on-box eval-session for the ~15 agent-behavior items · TG-82 auto-revert SDD ·
TG-109-write P1 priority · estate-write for the security/TG-414 set).



**+ TG-394 slice 3** (10th; deployed `a3239bec`): per-capability reachability + `tg_capability_degraded` gauge +
session degraded-set stamp — reachability keyed on the `runs_on` placement above the tombstone-confidence floor,
so a silent-hypervisor tombstone reads UNreachable (the first review pass, 0.35, caught that reusing bare
`g.fresh()` would have masked the feature's own motivating incident for 7 days; fixed + re-review 0.93). LIVE:
journal-evidence=14 hosts all reachable, embed/secrets/tracker/notify honestly UNMEASURED via the coverage
denominator. **Eval-gate WAIVED** (`Eval-Gate-Waived-By: @ncpjfuzl`) — behavior_re false-positive on
`temporal/runner/activities.go` (a compute+stamp of metadata, never fed to the agent; per TG-117 a deterministic
non-behavioral change is scoped out of the eval). 3rd follow-up: **TG-460** (non-guest reachability-fallback edge).

Owner mission: deliver+deploy+e2e as many of the 81 open as tractable in 24h (Max-AFK + surface-rest). Outcome —
NOT 81/81; the honest set. All deployed on `316b7a70`.

**Delivered + deployed + closed (9)** — each: named red→green oracle + killing mutation + live-verify + fresh-eyes
review ≥0.90:
TG-449 (`tg_estate_nodes` freshness — reconciled+closed) · TG-293 (fallback-ladder honesty, comment) · TG-457
(syslogng reader-self attribution, 0.95 — boot log "recognised 2 of TG's OWN read-only identities") · TG-179
(unknown_relation counter, 0.90 → byte-equivalent-fix after the review caught a silent ingest-widening) · TG-104
(policy ruleset editor: slice-1 backend + slice-2 faithful-round-trip editor, 0.94, fail-closed on an
unrepresentable default) · TG-176 (offline identity-token null-control harness, 0.95) · TG-456 (canonicalize site
vocab `dc1`/`dc2` at the single ingest chokepoint, 0.94) · TG-354 (entry-tracker seam STARVED-not-DARK +
dedup fail-toward-surface, 0.91, register-text live-verified) · TG-307 (diagnosis signal on the AuthReadOnly
proposals lane; text stays behind AuthTraceRead; 0.94 — the CI contrast-tokens gate caught+fixed a `--line2`→
`--ink3` accessibility defect). **TG-447 deleted** per owner ruling.

**Reconciled ALREADY-SHIPPED** (verified genuine by mutation-drill, NOT duplicated; umbrella tickets stayed open):
TG-146/S2 (`ec0b873e`) + TG-146/C3 (`6a07bfba`/TG-182); TG-249 items 2/3/4/7 (`fbfa8928`/`8ff9b084`/`25debf51`/
`15b4d9d4`). The "81 open" count is inflated by delivered-but-unclosed umbrella/sub-tickets.

**Surfaced with per-ticket determination + what-unblocks (~46):** the 44 non-AFK items + TG-117/414/32 assessed +
TG-249 items 10-12 (owner §6 amendment). Buckets: owner-decision, eval-gated, blocked/external-dep,
estate-write-non-granted, epic-slice. See the surface-list entry below.

**Follow-ups filed:** TG-458 (site backfill — blocks TG-454's site-scoped read), TG-459 (dedup short-window
recency fallback).

**Operational incident, OWNED:** 4 rapid merges → a CI image-build storm filled the 20G deploy-box (dc1tg01)
root disk to 100% → postgres/journald in D-state → the AWX deploy failed 3× → TG-456/354/307 couldn't deploy.
Diagnosed via `df -h /` (the load/iowait was a symptom), recovered with `docker image prune -af` (reclaimed
28.37GB, disk→30%, volumes untouched), serialized the last merge, retried the deploy job → green. Lesson saved
([[ci-storm-filled-deploy-box-disk]]) + mutation logged. **SERIALIZE ON THE DEPLOY, not the merge.**

### 2026-08-12 (cont.) — Go! run: surface-list for the non-AFK open items (Max-AFK + surface-rest)

Owner mission: deliver+deploy+e2e as many of the 81 open as tractable in 24h; strategy **"Max AFK + surface the
rest."** Determinations below verified against origin/main + the live estate (a research pass + direct reads).
Each carries why-non-AFK + what-unblocks. **Deliverables are TAKEN, not surfaced** (owner rule: never punt
deliverable work).

**MISBUCKETED-AFK → taken, not surfaced:** TG-354 (entry-tracker seam DARK — TG's `librenms-dc1-*` refs vs
estate `IFRNLLEI01PRD-*` are provably non-corresponding namespaces — + dedup no-longer-suppresses a re-fire after
the parent resolved; the sharp defect). Carves delivered from non-AFK parents: **TG-146/S2** (LibreNMS
pagination), **TG-146/C3** (require a non-empty observation before crediting `verified=true`; protected-path),
**TG-176** (identity-token null, offline), **TG-55** serializer (AFK part; progressive-disclosure piece is
eval-gated).

**OWNER-DECISION (a call only the owner can make):**
- TG-82 — commit-confirmed auto-revert: owner sign-off + mutation-ON canary.
- TG-122 — GitOps-MR / k8s effect lanes: owner sign-off + target repo + bot token.
- TG-146 — S3/S4/S6 owner-gated (append-only corpus, multi-worker ladder). [S2+C3 carved AFK above]
- TG-313 — Temporal WORKFLOW-task timeout = host memory pressure (0 avail/8 GB, no swap): owner RAM/swap/floor.
- TG-429 — owner mints a read-only GitLab token + sets CI var `TG_READONLY_API_TOKEN` (merge-gate wants it).
- TG-407 — measurement half (CoveredButEmpty) ALREADY SHIPPED; remaining = pin the "observed mutation" premise +
  supervised live enforce-flip.
- TG-73/74 — benchmark ladder Tier-3/4: owner triggers real Proxmox / cross-system outages.

**EVAL-GATED (on-box eval-gate run + scorecard baseline, supervised):** TG-38 (parent audit epic) + R-series
TG-42/46/47/49/50/52/53/55/56/58; plus TG-36/37/60/215. Deps: TG-50/53 need the R1 pgvector retrieval plane
landed first; TG-60 destabilizes the gate itself.

**BLOCKED / external-dep:**
- TG-91 — CONFIRMED blocked: Slurpit/NetDisco/SuzieQ/vSphere sources ABSENT (estate is Proxmox-only).
- TG-168 — model-gated: needs an on-prem IR model deployed (forensic corpus already exists).
- TG-30 — needs ITBench/SREBench task corpus (availability unconfirmed); publishing owner-gated.
- TG-72/75 — benchmark ladder, gated on Tier-1 + TG-69 + owner-triggered outages.
- TG-376/385/387 — cascade-collapse / cluster-identity / recovery-join: land TG-375 causal edges first
  (TG-376 REFUTED as-shaped by TG-385).
- TG-58 — Phase-2 mutation prereqs (undo/txn/cage): large protected-path build gating the owner flip.
- TG-102 — approver federation: internal dep TG-101 (LDAP/OIDC already on the estate).

**ESTATE-WRITE (non-granted: OpenBao WRITE / firewall):**
- TG-320/422/423 — OpenBao dynamic-secret / db / ssh-CA engines: box is READ-ONLY on Bao → owner Bao
  provisioning; TG-422/423 also supervised (outage / CA-trust blast radius).
- TG-381/420 — host-isolation firewall / egress proxy: supervised firewall apply (bypass-log first, then enforce).
- TG-315 — PREMISE STALE: the `authlog` ingest module is built + wired at the composition root; residual = live
  syslog-ng→`/v1/ingest/authlog` activation + verify the cross-source rule fires (within the standing grant —
  will assess/take).

**EPICs (slice; umbrella stays open):** TG-70/94/107/114/128/129/130/132/155/175/187 + TG-78 — deliver AFK
slices; umbrellas track remaining leaves. TG-128/129/132 are far-future north-stars pending owner spec ratify.

**DEFERRED / latent (nothing to fix now):**
- TG-448 — G6 loop-bypass: structurally 0 in prod; act only when auto-revert (TG-82) lands (add a G6 rollback
  oracle then).
- TG-439 — SESSIONS/LEDGER console fixtures are ORACLE-LOAD-BEARING (they prove suppression on a failed live
  read; emptying breaks console-e2e). Recommend won't-do or re-scope — owner call.

Honest framing: this surfaces the non-AFK items; the AFK deliverables are worked to DoD in parallel. The true
24h outcome is the delivered-count + this surface-list — never "81/81."

### 2026-08-12 — Go! run start (surfaced TG-348, filed TG-450)

Live-verified posture (deployed `a49cea1c` on the box, stack healthy, merge gate on, main clean). Took
category-3 item TG-348 (exercise the four never-closed loops). Drove a real Service-up/down fault on
`dc1librespeed01` (LibreNMS rule-9 forced as `librenms` on nms01; safety-restore timer armed + cancelled;
pool restored healthy; injector left stopped per working rule 6): **detection→ingest→session works** (session
`librenms-dc1-184751`), but hostdiag SSH is auth-rejected (`tg-syslog-ro` key), classify→POLL_PAUSE, no heal.
Last real actuation was 2026-07-31 — the heal path is 12 days cold. **Filed TG-450** (hostdiag key drift,
credential-plane/owner) and determined **TG-348 owner-blocked** on all four legs (console login for adopt/ratify,
TG-450 for credit, TG-82 for rollback); both recorded on the board owner-list + TG-348 comment. Took the next
AFK item (TG-177 op-class lineage) per the Go! contract.

### 2026-08-12 — TG-449 DELIVERED + FIXED (tg_estate_nodes freshness)

MR !1328 merged (`1280197d`), deployed, box-verified (worker:1280197d, `tg_estate_nodes`=363 live). New
`estate.Graph.FreshNodeCount()` counts distinct endpoints of FRESH edges; `tg_estate_nodes` derives from it
instead of `len(Export().Nodes)` (which counted every entity ever an endpoint — Export iterates g.edges
unconditionally and Upsert never removes an expired edge), so during a source-goes-quiet degradation the gauge
no longer holds high while the fresh graph shrinks. Oracle `core/estate/fresh_node_count_test.go` red→green +
killing mutation (drop the g.fresh guard). Option (b), Export() untouched. Observe-only, eval N/A, QA 0.95.
Closes the general case behind TG-394 slice-1's `hosts_resolved` fix. **State: Fixed.**

### 2026-08-12 — TG-380 slice 3 DELIVERED (correlate decision-stage instrument)

MR !1327 merged (`3665a407`), deploy pipeline 47894 green, worker:3665a407 live. Promoted `correlate` to
`DecisionStages` and wired its offered/eligible/acted triple in CorrelateActivity (offered=every activity,
eligible=`!v.Degraded`, acted=`v.Correlated`); shared StageTally via runner.Deps; the producer-scan guard
gains a correlate exerciser driving the REAL activity (RED without the Record). Stage-exposition live-proven
(`tg_stage_offered_total{stage="suppress"}`=2 in Prometheus, same sink); the correlate series populates on the
next non-suppressed incident. **classify deliberately left pending** — `execclass.Classify` is only ever called
with `Correlated` wired (correlate.go:91,152), so its verdict is a pure function of the correlate stage and a
classify triple would duplicate it; noted a latent gap (the classifier's Novel/Ambiguous/criticality inputs
are never populated in the live pipeline). Eval waived (single observe-only Deps field on the behavior-path
file activities.go; @ncpjfuzl). QA 0.93. TG-380 stays open — slices 4+ = predict/gate/breaker + `tg_breaker_state`.

### 2026-08-12 — TG-177 slice 1 DELIVERED (op-class fail-closed trust inheritance)

MR !1326 merged (`36dd4a64`), deploy pipeline 47890 green, box-verified: both planes run `worker:36dd4a64`,
the composed-registry overlay refresher (the loop carrying the ladder-cache eviction) live at 60s cadence.
The fix, entirely at the ratify verb + its enforcement-cache coherence: graduation is keyed on a bare
op-class string while the ratified slug is operator-authored and unbound to the candidate's cluster slug, so
a rename/split or revoked-then-re-ratified name could inherit `auto`/`auto_notice` it never earned
(privilege escalation via refactoring). Default inheritance is now NOTHING — `gradInheritance` records the
boundary (parent/child/kind/reset_from) on the `opclass:ratify` ledger reason and resets the ladder to
approve; `policy.Ladder.Forget` + the refresher's `WithLadderEvict` carry that reset into every enforcement
process's per-process cache so `GraduatedVerdict` can't serve a warm pre-reset level. **The first-pass review
(tg-code-reviewer 0.8) caught that the durable reset alone was invisible to the enforcement cache — a real
CRITICAL; the eviction was added to close it, re-review SHIP 0.92, CRITICAL confirmed closed with both
killing mutations independently reproduced.** Named oracles: `lineage_test.go`,
`TestRefreshCarriesTheRatifyResetIntoTheEnforcementLadder` (drives the REAL ladder through the REAL
refresher), `TestForgetEvictsCachedStateSoAReloadSeesAnExternalReset`. `core/policy` carries the
Law-Change trailer; eval N/A (deterministic safety control). Durable lesson graduated to memory
([[durable-write-invisible-to-enforcement-cache]]). **Slice 2 (TG-177 stays open):** structured
`opclass_lineage` table (migration 0082) + decision-tracer surfacing + formal spec/028 REQ + godog.

### Category 1 progress — 2026-08-06

Worked in this order; the corrections are recorded because two of the three fixes this queue asked for
turned out to be wrong or already done, and that is the part worth carrying forward.

| ticket | state | note |
|---|---|---|
| TG-337 | **Fixed** | actuation plane now holds a topology-scoped LibreNMS token (device reads only; 403 on alerts/eventlog/rules, verified per site with a real device count beside each refusal) |
| TG-172 item 3 | already delivered | the `search-host-logs` per-session cap exists, is wired at the composition root, and REFUSES by naming the bound. No work; recorded so it is not re-opened. |
| TG-172 item 2 | merged (!1017) | the gap was **Dutch**, not "a few languages" — the estate has two operator sites and the screen covered one. `spec/001` REQ-012 now binds screen coverage to the deployed estate. |
| TG-172 item 1 | in review (!1018) | the "gate on graduation" half was **refuted in TG-296** and is now pinned by a test rather than a comment. The real gap was the ungated merge primitive. |
| TG-173 | in review (!1020) | the human approval queue had **no depth gauge at all** — 47 metric families, none of them the queue every residual safety property of Full-auto rests on. |
| TG-324 §1 | precondition met, flip pending | 0 off-allowlist across 10,751 requests on both planes, with non-zero rule counts. Re-measured from Prometheus, not per-process `/metrics` — counters reset on every deploy, so the original precondition was unmeetable with the wrong instrument. |

**Filed while working it:** TG-341 (`omitempty` on a `time.Time` is a silent no-op), TG-342 (every LibreNMS
token is one shared **admin** identity — both planes can delete devices and edit the alert rules TG triages
against), TG-343 (the estate graph the mutation gate reasons over has no size gauge, so an empty graph and
a healthy one are indistinguishable).

**Two failures worth carrying forward.** The first TG-337 role passed every 403 and returned ZERO devices —
perfectly scoped, and it would have blinded the mutation gate. The first TG-172 provenance labels passed
every unit test and **failed the eval gate**: a caveat printed on every precedent row measurably suppressed
the agent's willingness to commit (`falsifiable_prediction` 4.00 → 3.33, `proposal_recall` 1.00 → 0.67).
Neither was visible from the code. Both were caught by measuring the thing itself.


### Category 2 progress — 2026-08-06

| ticket | state | note |
|---|---|---|
| TG-343 | **Fixed** | estate-graph size gauge. Found the category-2 defect below within minutes of deploying. |
| TG-232 | **Fixed** | the proxmox guest allowlist resolves at actuation time; the provider was already live, main just called it once at boot |
| TG-66 | **Fixed** | headline was stale (approval_choice/verdict ARE populated, 91 and 52 of 123). Only `ToolCalls` was real — deleted, plus a floor so no field on the spine can be declared-but-dead |
| TG-221 | **Fixed** | verified, not rebuilt — `model-primary/fast/embed-nomic` breakers live and closed |
| TG-302 | **Fixed** | recorded "do NOT seal", with the measurement: 0 redaction markers / 0 PEM / 0 provider keys across all 172 rows |
| TG-112 | partial | `tg_may_actuate` + a `plane` label; both workers had published `component="worker"` |
| TG-241 | **Fixed** | the vacuity floor the ticket asked for and nobody built |
| TG-146 | S2 refuted, C3 already delivered | see below |
| TG-226 | corrected | the cheap fix would not have caught the incident that produced the ticket |
| TG-346 | open | **I took the actuation plane down twice fixing this. See below.** |

**The gauge paid for itself.** `tg_estate_edges` shipped and read **392 on triage, 17 on the actuation plane**, `sources_failed` 0 on both. The plane that starts and stops guests computes blast radius over 4% of the estate the triage plane models, and nothing was erroring — no sweep would have found it.

**Then I broke production with the fix.** Adding NetBox to `worker-actuate` made it refuse to boot: the plane split is enforced on the SECRET REFERENCE, not the env-key name, and `secret/data/tg/netbox` is classified read-triage. I had checked `triagePlaneEnvKeys`, seen the key absent, and read that as permission. It went down twice — the CI deploy re-applied the compose after my first box-side fix and before the revert merged. The guard failed CLOSED, which is the only reason it cost a restart instead of silently widening the split. The guard is now turned around and forbids what I added.

**Two audit findings did not survive contact with the running system.** TG-146 S2 assumes `/api/v0/devices?limit=500` truncates; this LibreNMS **ignores `limit` and `offset` entirely** — the described false-clear is unreachable and the prescribed pagination fix is not implementable. C3 was already delivered by TG-182, in the exact shape it asked for. Both audit `file:line` anchors had drifted.

**Filed while working:** TG-345, TG-346, TG-347 (a latched judge-death trip, OPEN for five days on a demonstrably live judge, halting the skill flywheel — and my first write-up of it named the wrong cause, corrected on the ticket).

**Still blocked:** TG-172 item 1. The full-rigour eval has degraded on 429s across two launches; !1018 stays open rather than merging on the fast gate's mixed result.

### Category 4 progress — 2026-08-06 (Alerting / observability)

| ticket | state | note |
|---|---|---|
| TG-231 | **merged** (!1034) | the estate's most important container — TG's single brain, no fallback by design — had zero monitoring. `SidecarDown` + the vacuity floor + `ModelBreakerOpen`. |
| TG-343 | **Fixed** | estate-graph size gauge (carried from cat 2; it is what found the 392-vs-17 plane split) |
| TG-344 | merged ×3 | see below — this one shipped blind and I found it only by looking at the box |
| TG-222 | **Fixed** | premise was stale for judge-liveness (already scheduled and running); the frontier anchor was genuinely unarmed. Now armed on an INDEPENDENT vendor and proved by a triggered run. |
| TG-291 | merged (!1036), open | the only security-telemetry ingest could not be provisioned at all; estate side is TG-349 |
| TG-336 | open | the collapse is real and ongoing; TG-344's denominator proves TG is **not** deaf — the estate is quiet |
| TG-238 | **Fixed** | doc half was already delivered; verified and closed, MECH-114 carried to TG-50 |

**TG-344 is the lesson of this category.** It merged, CI-green, content verified on main, deployed to a
healthy worker — and did not exist at runtime. Every boot logged `upstream probe: no prober wired`, a line
I wrote, that nothing read. Two independent causes in one `if`: it required the LibreNMS *alert poller*
(production is push-only) and read `TG_LIBRENMS_DEPLOYMENTS_AGENT_TOOLS`, a key this deployment does not
set. The coupling was backwards — a pull deployment already has an independent read of its upstream; the
push deployment, whose only signal is silence, was the one excluded.

Then the fix itself nearly caused a false page: the actuation plane published `upstream_readable=0`, which
is TG-337's deliberate 403 working exactly as intended, and `UpstreamProbeUnreadable` would have fired on
it within 30 minutes. Caught on the box before it fired (!1037).

**Three times this session a resolver was guarded and its wiring was not** — TG-344 mutation C, TG-344
mutation H, TG-291 mutation C. Each time the unit tests were green and the composition root was wrong,
which is the same shape as the defects being fixed. Composition-root assertions (comment-stripped, with a
prose-only fixture) are now part of the same tests.

**What the new gauges bought immediately.** `tg_ingest_upstream_available` read **0** on both LibreNMS with
`readable=1`, which settles TG-336: the intake is at ~0.3% of its late-July rate because there is ~0.3% as
much to ingest. That question previously took a hand-run API call to answer.

**Filed while working it:** TG-349 (nothing in the estate is configured to *send* CrowdSec or auth-log
events; TG's front door is now ready and no sender exists).

**A correction to my own recent work:** TG-315's authlog receiver is push-only and had no `sources` row
either, so the connector I shipped last week could not be authenticated against. Fixed alongside crowdsec.

### Category 5 progress — 2026-08-06 (Estate model / discovery / ingest)

| ticket | state | note |
|---|---|---|
| TG-271 | MR !1042 | the gate the ticket asked for. **26 real hosts still have no key** — every switch, both routers, the Synology, all four DMZ hosts |
| TG-346 | MR !1043 | my own ticket, **reframed**: the 17-vs-1660 asymmetry is the plane split working as designed, not a misconfiguration |
| TG-207 | MR !1044 | edge-triple schema validation, observe-only |
| TG-206 | MR !1045 | item (b): decay the mispredicted path, not every edge touching a surprise host |
| TG-350 | MR !1040 | filed and fixed: pve-liveness read with the actuation WRITE token |
| TG-314 | verified | claim holds; option (a) is unsafe until !1043 lands — see below |
| TG-200 | analysed | P1, actionable, needs its eval run in the same change — not shipped blind |

**The correction that matters most is to my own ticket.** TG-346 proposed giving the actuation plane the
NetBox credential. That proposal is wrong: `core/credential/plane_split.go` forbids read-triage references
on the actuation plane *as a security property* — "an actuation process that also holds the estate read
tokens is a process an attacker can pivot INTO the triage plane from". It is why that fix took the
actuation worker down twice. The answer is to pass the derived GRAPH, not the CREDENTIALS, and
`estate_snapshot` already holds it.

**Which surfaced a live defect.** Both planes publish to `estate_snapshot` and the table had no plane
column: 410 nodes/1863 edges from triage and 20/17 from actuation, written two seconds apart, with
`Latest()` ordering by time alone. 191 of 502 snapshots in 24h are the impoverished graph. It had not gone
wrong only because triage happens to write last — a latent race, fixed in !1043 before it fired. TG-314's
option (a) is unsafe until it lands.

**Measurement discipline that changed two designs.** TG-271's coverage gauge would read 34/86 and be red
forever, because half the "uncovered hosts" are Kubernetes component names in the Alertmanager `host` label
(`cilium-agent`, `coredns`, `kube-etcd`, `node-exporter`). TG-207's schema table, derived from the live
graph alone, would have silently dropped `member_of` and `routes_via` edges on the first deploy — the live
graph holds three triples while the adapters build more. Both ship observe-only/warning for the same
reason: a fail-closed gate against an unmigrated config gets silenced and then never speaks again.

**Everything is held, not merged.** TG-351: AWX has been unreachable since 02:41 and every deploy fails, so
the code on main is not the code running. Merging each MR would redden main once per merge for a cause no
code change fixes.

### Category 3 progress — 2026-08-06 (Policy / credential / graduation)

| ticket | state | note |
|---|---|---|
| TG-350 | in review (!1072) | pve-liveness read with the ACTUATION lane's write token while being triage-plane-scoped. The credential plane split withheld it, and TG's fastest detector went silent on 2026-07-31 — 200 rows, then nothing. |
| TG-48 | triaged, two halves refuted | usage accounting and the tool-output ingestion cap are already delivered (TG-44, `untrustedBlockBudgetRunes`). The real gaps are narrower: no `max_tokens` anywhere, and the cost breaker is complete-but-configured-with-zeros. |
| TG-173 | measured, bound still absent | !1020 merged the depth gauge. There is no cap, no shed, no coalescing. |
| TG-321 | not started | the graduation ladder is still writable from the triage plane. |

**A duplicate cost me a full implementation cycle, and was worth it anyway.** I built TG-350 from scratch
while **!1040 sat open with the same diagnosis and the same design**. The check that would have caught it is
one command (`glab mr list | grep TG-NNN`) and it is now a memory. But !1040 **would not have fixed the
outage**: it moved the endpoint and token to the estate READ pair and left the TLS flag reading
`TG_PROXMOX_INSECURE`, which is unset on this box. Measured from dc1tg01:

```
https://dc1pve01:8006/api2/json/version   verification ON  -> curl exit 60
                                              verification OFF -> HTTP 401
```

It would have swapped a missing-credential failure for a certificate failure, and **both report "no down
guests", which is byte-identical to a healthy estate**. The generalisable question this produced: *after a
fix, what does the remaining failure look like, and is it distinguishable from success?*

**The TLS measurement then produced a better finding than the ticket had.** The certificate is not
self-signed — it is a valid Let's Encrypt wildcard `*.example.net`. `TG_PVE_URL` addresses the host
as the bare name `dc1pve01`, which no wildcard can match. Under the FQDN, verification succeeds
(`HTTP 401`, handshake OK). So `TG_PVE_INSECURE=true` is a workaround for an **address string**, not for a
certificate — while both `cmd/worker/main.go` and the pve-liveness descriptor assert a self-signed cert in
prose. Filed as TG-367.

**Two "unmeasured" claims that were already measured, and one that was not.** TG-48 says cost is unbounded
*and* unmeasured. `tg_model_tokens_total` accounts 3.18M tokens across three tiers; the cost breaker in
`core/cost/` is a complete implementation with daily and session ceilings, cross-worker shared trip state and
a ledger entry. Production runs it with every `TG_COST_*` set to `0`, and the worker says so honestly every
boot. The defect is what happens next: **disabled means the gateway is left un-wrapped, so `tg_cost_breaker_state`
and `tg_cost_usd_today` are ABSENT from Prometheus** — not zero. "TG is running unmetered" has no signal at
all, and `absent()` is the only rule that could ever raise it.

**The eval gate refused a change for the second time this session.** TG-57's behavioural half — screening the
tool-error branch, a path that has fired **6 times in 16 days** — came back `falsifiable_prediction -0.80`
against a `-0.30` max-drop, both arms `INTEGRITY: OK`. The MR was split: the test-only guard merges (!1073),
the behavioural half waits for `TG_EVAL_FULL=1`. My first attempt to exonerate it was **vacuous** — I grepped
an 844-byte aggregate scorecard for `TOOL_ERROR` and got 0, which the artifact could never have answered.

### Category 4 progress — 2026-08-06, second pass (Alerting / observability)

Worked by reading the **live alert list** (`/api/v1/alerts`: 22 active instances) and the worker's boot log,
rather than the ticket titles. Both turned out to be better sources than the backlog.

| finding | state | note |
|---|---|---|
| TG-370 | **fixed** (!1079) | `SidecarDown`, severity CRITICAL, fired all day on a **healthy** sidecar |
| TG-369 | **fixed** (!1078) | 216 of 636 primary model calls counted as `outcome="other"`; they were `breaker_open` |
| TG-48 | **fixed** (!1077) | the spend guard published NO series when disabled — absent, not zero |
| TG-368 | filed | the TG-164 database plane split is not in force; the worker logs `LIVE EXPOSURE` every boot |
| TG-219 | open, correctly | `suppression.tier1: starved — 171 admitted, 0 suppressed`. Not a bug: no tier-1 chain is configured, and the seam register says exactly that in prose. |
| TG-271 | held (!1042) | the `syslogng.read` starvation is host-key and deadline failures on estate hosts, already measured |
| calibration | **no action — already covered** | see below |

**The alert list is a better backlog than the backlog.** Every one of the 22 firing instances mapped to a
real condition; three of them had no ticket at all until today. Reading it takes one API call.

**Two findings came from the same question**, asked of a system that was already answering it: *what is TG
telling us that nothing reads?* TG-368 (`credential plane DB: LIVE EXPOSURE`, naming all 8 writable tables),
TG-369 (`outcome=breaker_open`, discarded at a label clamp), and earlier TG-344 (`no prober wired`). Each
diagnosed itself correctly, in prose, into a stream with no consumer.

**A near-miss worth recording, because it is the failure mode this board keeps hitting from the other side.**
The calibrator log reads `skill=-2.20 ECE=0.58 MCE=0.90` — TG's stated confidence carrying *less* information
than a constant. I was about to file it. Three alerts already cover it, all currently firing, and their
descriptions warn against exactly the misreading I was about to commit: the outcome variable is
`blast_radius_exact` (fp=0 AND fn=0), and *"scored against diagnosis correctness instead, the same population
gives ECE 0.1354 with the agent UNDER-confident above 0.7"*. The codebase had documented it better than my
finding would have. **Check what exists before filing; the population of a measurement is part of its claim.**

**And one mutation that lied to me.** S1 on !1079 — remove `tg-egress` from prometheus — reported SURVIVED.
The edit had replaced the first matching `networks:` line in the compose file, which belongs to a different
service, so it never reached prometheus at all. Re-applied against the prometheus block with an assertion
that the parsed value changed: RED on both guards. *A mutation that does not apply is not a surviving guard*
— third time this rule has earned its place.

| TG-336 | **detector fixed** (!1081) | the collapse is real (08-01→08-05 ≈ zero) and **today is a recovery: 159 alerts**. What was missing throughout was a working detector. |
| TG-350 | fix held (!1072) | `pve-liveness` is the one genuine TG defect among the four sources |
| TG-371 | filed | the ingest front door counts acceptances and never refusals |
| TG-372 | filed | TG cannot describe its own ingest path |

**A correction to this board.** The category-4 entry says `tg_ingest_upstream_available` read 0 on both
LibreNMS, "which settles TG-336: there is ~0.3% as much to ingest". It reads **83 and 4** now. That entry was
true when measured and is not a settled conclusion.

**I nearly shipped an always-on false alarm while fixing an unfireable one, and that is the lesson of this
pass.** `IngestConnectorDeafToItsUpstream` could never fire — `increase()` applied to a GAUGE, which reads a
downward drift as counter resets and extrapolated **4276.64** where the rule tested `== 0`. I fixed it, keyed
on `available > 0`, verified it *would* fire on the state that motivated the ticket, and pushed. Then I
checked what it does to **today's** data:

```
tg_ingest_upstream_available{librenms-dc1}
  02:59  80   03:29  85   03:59  83   04:29 … 10:59  83      flat for 7 hours
```

LibreNMS intake here is PUSH, and an edge-triggered transport fires on a **state transition** — a stable set
of long-firing alerts produces no pushes at all. My fix would have fired continuously through seven hours of
perfect health: the same `SidecarDown` pathology fixed in !1079 that morning. Corrected to
`delta(available[1h]) > 0` before merge, and the guard now forbids the level form by shape.

Two traps made it easy: I quoted the **metric's own HELP text** as authority (*"this non-zero with nothing
arriving is a broken connector"* — written for the PULL case), and the level form **reads** correct.

> **The check that catches it: after repairing a rule, evaluate BOTH expressions against live data. Proving
> it fires on the bad case is half the test; the other half is that it stays SILENT on the good one.**

**What I could not determine, stated rather than glossed.** TG's push endpoint is `127.0.0.1:8081` on tg01.
The only TG listener exposed off-host is the console — nginx serving a static SPA that returns `index.html`
for every `/v1/*` GET and **405 for every POST**. `territory-grounder.example.net` resolves to
dc1npm01 and lands on that same nginx. No second DNS name resolves, no host cron forwards, nothing holds
a connection to :8081, and the pull poller is off. Yet 89 LibreNMS alerts arrived today. TG stores each
source's token ref but **not the URL it issued**, and records nothing about an accepted push's origin — so
the path is not knowable from TG. That is TG-372.

### Category 1 progress — 2026-08-08 (Security hardening)

| ticket | state | note |
|---|---|---|
| TG-381 | **Fixed, live-drilled, MERGED** (!1204 → `44df50f9`) | the egress tier can no longer reach the LAN it lives beside — see below. Review clean (nothing revert-worthy); two findings folded in (`_TOPOLOGY` scan, tier-CIDR assertions) |
| TG-382 | **partial — restore path guarded, MERGED** (!1204) | the reload-restore drop-in (already merged f9c7f572/cc113e1b) had NO guard; deleting its ExecStartPost went green. `deploy/nftables_persistence_test.go` now pins it (both chains, `-` prefix, 5 mutations RED). Still OPEN for runtime-absence detection → TG-419 |
| TG-324 | **items 1/2/4 delivered + verified live; item 3 → TG-420** | NO code change: the enforce flip is live (`tg_egress_enforcing=1` on all 3 planes, rules 33/10/15, off-allowlist 0, refused 0) and guarded by 4 alerts+posture tests; host reachability closed by the isolation chain (item 2); non-HTTP egress bounded by TG-381 (item 4). The board's 08-06 "flip pending" was STALE — the flip happened. Item 3 (litellm→provider egress proxy) is the costing piece, carved to TG-420. Ready to close. |

**TG-324 is the "verify before you build" case of this session.** The board said the flip was pending; the
running system said it was live and guarded. A defaults-disagreement I was about to file (grounder compose
default `:-meter` vs workers `:-enforce`) was refuted by reading the comment: the grounder earns enforce via
the shared override because it cannot resolve its own OpenBao credential under a wrong allowlist. The whole
ticket resolved to tracker reconciliation + one carved successor — no code, and that was the correct outcome.

| TG-315 | **safety prerequisite delivered, MERGED** (!1206 → `8ac78a39`); arming = owner decision | the authlog collector (correlator's 2nd, non-availability witness) is built/wired/oracle'd/DARK. Found + fixed its ticket-named self-DoS: a username spray would mint one Opus triage session PER username (single-brain, TG-231) — the TG-376/TG-384 cascade shape. `capEnumeration` bounds distinct-principal envelopes per (host,kind)/poll at 8, counts the suppressed tail (`tg_authlog_enumeration_suppressed_total`). Zero live change (DARK). **Arming still blocked**: needs the explicit 15-host set (syslog key not on my access path) AND is a consequential agent-loop-feeding posture decision (surfaced to owner). Follow-ups: TG-421 (aggregate-sweep envelope), TG-349 (push sender). |

**TG-315 is the "surface the consequential arming" case.** The connector is arm-ready and its arming is
explicitly designed as an operator decision. I did the safe half unattended — the flood cap that makes
arming safe, a genuine red→green with zero live blast radius (the collector is DARK) — and did NOT
blind-arm a new autonomous-loop input on the single brain. The killing-mutation drill re-taught the TG-381
lesson the hard way: `git checkout` to restore a mutation on UNCOMMITTED work wiped the whole change, not
just the mutation. Commit before drilling; restore from a `cp` backup, never `git checkout`, on dirty work.

| TG-320 | **scoped (its own pre-build gate); build is supervised, not AFK** | classified every target cred: ~20 are app-issued API tokens = **static-only forever** (OpenBao can't lease a credential another system mints — the ticket's LibreNMS/AWX example is unachievable, now written down). SSH → **TG-423** (ssh-CA, gated on rolling TrustedUserCAKeys across ~26 hosts). TG's own Postgres → **TG-422** (database engine, self-contained first slice). No engine is provisioned; `SecretRef.Resolve()` returns fixed values with no lease concept. Parent stays open; the `dyn:` resolver is credential-path-critical (a bug = total auth outage) = supervised. |
| TG-168 | **blocked on dependency** | needs an on-prem forensic IR model to run the timeline/IOC analysis; none provisioned (single-brain by design, TG-231). Not an autonomous slice. |

**Category 1's tail is supervised-build / blocked / eval-gated.** After TG-381/TG-382/TG-315 (real code
landed) and TG-324/TG-320 (scoped with live evidence), what remains at the top — TG-320's `dyn:` resolver,
TG-168's forensic model, TG-57's eval-gated behavioural half (degrading on 429s per the cat-3 note) — are
all changes that should not land unattended on the credential/forensic/eval-gated paths. The correct AFK
move on these is scope + carve + surface, which is what TG-320 got; the builds want a supervised session.

| TG-57 | **~75% absorbed; remainder eval-gated** | 4-item cluster verified per-item: 1a tool-output redaction DONE (`screenToolOutput`), 1b ledger redaction DONE via detection (`governance_ledger_shape.go`, measured clean), 4 SSE streaming DONE + wired (`/v1/sessions/{ref}/stream` + console `liveOpenWfStream`). Only item 2 (tool-retry behavioural half) genuinely open — eval-gated (!1073 merged the test half). Recommend narrowing. |

**Category 1's AFK-tractable pass is COMPLETE** (all 7 report items worked: TG-381/382/315 built, TG-324/320
scoped, TG-168 blocked, TG-57 verified-absorbed). The remaining category-1 code is supervised-only. Next
AFK work moves to the **pve03 cascade family** (category 5, TG-375–394 + TG-398/399/401/405) — concrete,
bounded, non-eval-gated defects that fit red→green/arm-what-exists far better than cat-1's eval-gated tail.

### Cascade family — 2026-08-08 (first AFK build)

| ticket | state | note |
|---|---|---|
| TG-405 | **Fixed** (MR pending) | an operator `category` label could drive TG's poll-forcing safety clamp — the estate uses that key for subsystem names, so a future `category: maintenance` would force POLL_PAUSE forever. `demoteCollidingSafetyCategory` moves a colliding Alertmanager category to `alert_category` (off the safety key, kept for RAG); non-colliding values untouched (the TG-405 visibility gauge from `6c7c847f` is intact). Behaviour-neutral live (0 high-risk passthrough today); 2 killing mutations RED. Option 1 (the gauge) was already shipped; this is the durable option-2 half done contained (passthrough boundary, not a repo-wide key rename). |

**TG-405 is the "verify found option 1 already shipped" case.** `cmd/worker/category_coverage.go` (commit
`6c7c847f`, attributed to TG-405) already emits the collision-visibility gauges. The remaining durable fix
had a coherence trap — a naive key-rename to `tg_category` would have REGRESSED that gauge — so it landed at
the passthrough boundary instead: neutralize only operator category values that collide with the safety set,
leaving the gauge's signal and RAG context intact.

### Category 2 — 2026-08-08 (cascade family, actuation/safety)

| ticket | state | note |
|---|---|---|
| TG-404 | **Fixed** (!1210, verified on real pg) | an executed inverse had no durable record — "did the rollback run?" was a log-string parse. `action_execution` gains `inverts_action_id` (migration 0071); `ExecutionSink.Record` carries it (last param, no positional shift); the interceptor passes `Request.InvertsActionID`. Behaviour-neutral (mutation off, 0 inverses run); the oracle drives an inverse end-to-end. 3 killing mutations RED against real Postgres; all 71 migrations + full core/db (233 tests) green. Unblocks TG-82 + TG-348's loop-closure register. |
| TG-378 | **scoped — supervised** | 3-of-4 cascade `start` proposals were on VMs running the whole time; the precondition (target observed NOT running) needs a running-state source that does NOT exist queryably (graph is topology-only; pveliveness keeps status in-memory; only a live `GET /cluster/resources`). Fail-mode design (cluster-read resilient to node-down, fail-closed-on-unknown, over-block avoidance) makes it a supervised gate build. Prereq: persist guest liveness. |
| TG-348 | **observability half advanced** (!1211, stacked on !1210) | the loop-closure register excluded the bound-rollback loop pending a durable inverse record; TG-404 delivered it, so `bound_rollback` is now watched (`tg_loops_never_closed`). DB oracle on real pg, 2 mutations RED. Ticket stays OPEN for the live-EXERCISE half (adopt/ratify/credit/rollback), which is operational/owner work + the rollback exercise is blocked on TG-82's executor being built. |
| TG-112 | **decomposed + first slice** (!1212) | retire the `mutation_enabled` binary → 4-mode `tg_may_actuate`. Partially done already (may_actuate exists, alert rules migrated, gauge marked deprecated). Landed: shadowbench's two safety preflights now gate on tg_may_actuate (behaviour-neutral). Ordered remaining slices on the ticket — consumer-first, because it renames a LIVE safety gauge (alert-rule + shadowbench consumers), drops a DB column, touches a Law config + protected paths, and is agent-facing (eval gate). A blind 82-ref sweep would break monitoring. |

**TG-112 is the "large owner-requested refactor → decompose, don't blind-sweep" case.** ~82 live Go refs +
a DB column + a Law config + a live safety gauge with alert-rule consumers. Working it consumer-first keeps
`mutation_enabled` valid until nothing reads it; the gauge/column/config removal is the LAST slice and
carries the acceptance oracle.

**TG-378 vs TG-404 is the category-2 shape:** the precondition (TG-378) touches the actuation gate + needs a
new running-state dependency → supervised; the durable record (TG-404) is additive, behaviour-neutral, and
verifiable end-to-end → landed. When top-down hits a supervised chokepoint item, scope it and take the next
non-chokepoint red→green.

### Category 2-4 tail — 2026-08-08

Category 2's remainder is large/owner-gated/supervised: **TG-82** (commit-confirmed auto-revert) is
design-first + owner-sign-off by its own text — noted that TG-404 delivered its inverse-record prerequisite;
**TG-58/TG-122** are large builds. **TG-146** reconciled (S2 refuted, C3/A4 delivered, C1 reworked; A3 → TG-122
prereq; S3/S4+S6 owner-gated). **TG-152** scoped (defense-in-depth on an inert AWX path; activation-coupled).

Category 3 top: **TG-177** — the fail-closed split-graduation property is ALREADY satisfied and tested
(`TestStartsAtApprove`/`TestLoadAbsentFailsClosedToApprove`); remaining is operator-driven lineage records +
op-class content-identity (protected, design-first). Not an AFK slice.

Category 4: **TG-371 DONE** (!1214) — the auth layer now counts an ingest push it turns away (`reason=auth`,
fires even when the brake is unwired) + `AlertSourceRejected`; completes the handler half from aa1e513f. 6
oracles + a wiring assertion, 3 mutations RED, `core/auth` non-protected.

**The pattern across categories 1-3 tails:** the AFK-tractable items are worked; the tops are now
large-build / owner-gated / already-satisfied. Concrete bounded red→greens live in category 4 (observability,
where TG-371 landed) and the category-5 cascade family (TG-389 empty-host ingest, TG-399 first-occurrence
intake, TG-379 backwards-causality edge) — the board's stated Band-2 work. Next AFK work belongs there.

### Cascade family (category 4-5) — 2026-08-08

| ticket | state | note |
|---|---|---|
| TG-389 | **Fixed** (!1216) | AM normalizer resolved a machine from only instance/node; now every machine label (kubernetes_node/nodename, pod still refused per TG-373), + dedup refuses to match on an empty host. THE PREREQUISITE for TG-377 (dedup can't now collapse distinct nodes). 2 killing mutations RED. |
| TG-379 | **Fixed** (!1217) | incident learning wrote backwards (pve03 depends_on its guest) + cross-site edges at 0.75, live in blast-radius. `reconcileLearnedEdges` drops incident depends_on edges that invert an authoritative runs_on (canonName-matched — the learner's TypeHost vs PVE's TypeLXC would defeat an edge-key compare) or span sites. 2 mutations RED. |
| TG-377 | **Fixed** (!1266) | the broken-stage zero (0 of 171 suppressed) had TWO defects, both now fixed: the KEY defect (empty-host collapse) → TG-389 (!1216); the CLOCK defect → this MR. The chain's single `now` is the alert's OBSERVATION time, and the dedup stage used it to BOTH stamp the recent-triage log's LoggedAt AND run the [now-window, now) age test; 171 out-of-order/ingestion-lagged parallel workflows meant a re-fire's ObservedAt could precede a prior's LoggedAt → age<0 → prior rejected as future-dated (REQ-408 fail-open) → escalate, every pair. Fix isolates the DEDUP lane onto an evaluation clock (`Clock`, default time.Now() — active in prod, no wiring); freeze/scheduled deliberately keep the observation clock (a maintenance-window alert triaged late still freezes). The prior board triage here MISLABELLED the remaining bug as the ephemeral per-worker log — that is a separate, documented, fail-safe design choice (a durable shared log can replace it behind the same seam), NOT the 0-of-171 cause (the ticket + review both confirm). Oracles: dedup-uses-eval-clock (killing mutation RED, strengthened to discriminate all 3 clock sites) + freeze-non-regression guard. Fresh-eyes APPROVE; not lockstep/protected/eval-gated. LIVE storm confirmation is opportunistic (next real cascade / replay) — the oracle faithfully models the cascade. |
| TG-178 | **PARTIAL — first slice landed** (!1267) | boundary-case margins on the gate spine (epic-child of TG-175). `interceptor_gate_verdict` recorded WHICH gate fired, not BY HOW MUCH. Landed the plumbing + the FIRST gate: a signed NON-SECRET `margin` (value−threshold) on the OBSERVE-ONLY gate-verdict trail (migration 0076, nullable, append-only REVOKE intact), produced for the policy gate's min_confidence clamp (`confidence − min_confidence`, off the non-secret refine record), plus a `GateVerdictsWithinEpsilon(ε)` boundary-case reader. `core/actuate/interceptor.go` is protected + spec/013 lockstep — emit is observe-only (fresh-eyes APPROVE **specifically confirmed it cannot alter/leak an actuation decision**: runs after the verdict switch, value-type `dec`, no panic/control-flow, ordinal 1-per-gate intact). Oracle: boundary-vs-comfortable margin (killing mutation RED) + unset-gate nil-margin guard + real-pg round-trip/within-ε. Not eval-gated. Surface API landed (!1268): `GET /v1/gates/within-epsilon` (AuthTraceRead) serves the within-ε boundary-case queue, wired end-to-end (store→appended buildPublicAPI param→Deps.GateMargins→registered route; contract regen 58 routes; oracles incl the positional-rebind guard — review APPROVE). Surface COMPLETE: API !1268 + CONSOLE !1270 (`gatemargins` module — real GateBoundaryPage DTO, honest live/503/empty states, never a fixture on a live shell; console-verify byte-repro + e2e falsifiability-proven; fresh-eyes APPROVE). band PRODUCER LANDED (!1272: policy-band margin = verdict-rank distance to the band floor; ComposeRecord.BandMarginRank; observe-only, lockstep restamped, 2 killing mutations RED). **REMAINS:** graduation/ACL/mode/floor producers + flywheel-preferential wiring. |
| TG-380 | umbrella | per-stage offered/eligible/acted across all 6 decision stages + a completeness guard — a large cross-cutting instrumentation build (protected stages, eval-adjacent). TG-377's suppression metric is its first stage. |
| TG-398 | **Fixed** (!1219) | `step_count` (axis A6a) was set only on the investigation SUCCESS path, so all 135 `failed:investigate` rows read 0 while carrying 321 `agent_step` rows — the "134 did ZERO steps, mean 0.60" outage headline (severity basis for TG-376/TG-380) was a measurement artifact (true DEEP_INVESTIGATION mean 2.79). `RecordTriage` now derives step_count from the durable per-session `agent_step` transcript (keyed by `external_ref`, NOT the collapse-prone `action_id` — TG-142) when the incoming value is 0; a stand-down with no transcript stays 0, so 0 means exactly "no cycle ran". DB-write derivation chosen over the ApplicationError-detail route so `investigateRetryPolicy`'s type-keyed short-circuit is untouched. No migration; no gate reads it → no eval gate. Killing mutation RED, full core/db suite green on real Postgres. |
| TG-391 | **Fixed** (!1221) | `get-estate-context` rendered a 0.75 co-occurrence GUESS identically to a 0.95 PVE fact — the agent offered 37 fabricated parents for kube-etcd (honest-unknown → confident-wrong), straight into its own observations. `Impact` gains a `Learned` flag propagated through BlastRadius (path-product) + Siblings (penalised) since the number can't recover provenance the way a single `Parent.Source` can; the renderer splits UPSTREAM/DEPENDENTS/SIBLINGS into observed vs learned counted+capped blocks, and a learned-ONLY entity gets the honest "NO OBSERVED TOPOLOGY — treat as not known" stance instead of a dependency tree. Killing mutation = RENAME the marker token → RED. **Eval-gate judgment:** alters model observations, but `eval-evidence`'s `behavior_re` deliberately excludes core/estate + modules/estate ("a wider set is a wall, not a gate") → PASS, gated by `make all` + the oracle; noted on the ticket for an owner who wants the number anyway. |
| TG-399 | **Fixed** (!1257) | the naive `ON CONFLICT DO UPDATE SET occurrence_count` was correctly refuted (append-only `ingest_alert`, UPDATE revoked). Built the OTHER shape: append-only `ingest_alert_occurrence` (migration 0074) takes one row per accepted delivery, so count/first-seen/last-seen are derivable by query without ever updating the canonical row. Resolved the open call-path question by tracing the front door: `AlertLogStore.Append` IS called on every accepted non-recovery delivery (`ingest.go:192` single, `:266` batch), after the idempotent `StartTriage` — so the re-fire reaches Append and was dropped only by DO NOTHING. `Append` now also writes the occurrence log; new `Occurrences(ref)` reader. Real-pg oracle `TestIngestAlertRefiresAreRecorded` (3 deliveries → canonical stays 1, occurrence count 3, honest first/last-seen); killing mutation RED. `ingest_alert_occurrence` added to `TriageContentTables` (actuation plane reads, never writes). Not lockstep/protected/eval-gated. Fresh-eyes review: no blocker; 2 non-blocking findings addressed by honest doc qualifications (count is a delivery FLOOR under transient loss, not exact) + tracked as **TG-427** (occurrence-write durability + distinct-refire measure). **REMAINS:** wiring occurrence counts into the dedup stage (still-open vs resolved-and-refired) + console/metrics surfacing — a behaviour-changing follow-up. |
| TG-388 | supervised (large) | learned-tier lifecycle has no effect: hourly decay overwritten by the 5-min refresh (11/11), age-out unreachable (Floor=0), tier is process-local (erased by redeploy). Multi-face design with a documented trap in the obvious fix (naive durability ages out ~69% off one disproof) — needs the TG-206 per-edge disproof substrate consulted at Build. Not AFK-bounded. |
| TG-390 | **Fixed** (!1223) | a NetBox virtualization cluster was typed `TypePVENode`, so a logical placement group impersonated a physical hypervisor (`dc1-pve` 133 children, a Synology DSM cluster typed as Proxmox) and at 0.90 > GroundTruthCutoff kept `HasGroundTruth` true for a cluster-only guest — 11h confident-but-blind, never reaching TG-202's stay-silent. New `estate.TypeCluster`; netbox fallback emits `member_of TypeCluster` not `runs_on TypePVENode`; `siblingParentEligible` flipped deny-list → explicit allow-list (cluster + future types ineligible; Host/VM/LXC preserved); `HasGroundTruth` skips cluster edges; DefaultEdgeSchema gains the triple. **THE PRECONDITION for TG-375** (a tombstoned+decayed pve edge can no longer be re-hidden by the 0.90 cluster edge). 3 killing mutations RED; the old netbox test asserted the exact defect (encoded the bug) — inverted. |
| TG-385 / TG-376 / TG-387 | supervised (large/coupled) | the dispatch/correlation rebuilds: TG-385 (durable cluster identity + causal election), TG-376 (make the verdict load-bearing — elect one member, open one session), TG-387 (join the recovery ledger back to open sessions on the natural key). All large, mutually coupled, and touching the dispatch/actuation-adjacent path (TG-376 changes how many workflows spawn; TG-387 obsoletes pending proposals). Not AFK-bounded — supervised. |
| TG-225 | **Fixed** (!1256) | learned suppression lane was in-memory only — a restart forgot every learned reboot schedule. Registry now mirrors each mutation to a durable pgx store + rehydrates at boot; migration 0073 adds the safety-critical timezone (a reloaded window would else evaluate in the wrong zone → wrong-wall-clock suppression). Fresh-eyes review (no blocker) → 3 fixes: bounded mirror Save (was unbounded under the read-path mutex), no live-count regression on reload, tz preserved on re-discovery. 3 falsifiable oracles RED on mutation; full core/db green on real pg. Deferred-verify (SSH boot-reason reader) → future ticket. |

### Gate integrity + eval-infra — 2026-08-08

| ticket | state | note |
|---|---|---|
| TG-406 | **Fixed** (!1225) | an `eval/ci/*_test.sh` self-test could be unwired from `.gitlab-ci.yml` and nothing went red (the !1059-replay surviving mutation — a correct control nothing invokes, one level up from Go wiring). New `deploy/ci_eval_selftests_wired_test.go` globs every self-test from the TREE and asserts each is invoked in a non-comment CI line; tree-derived so a newly-added unwired self-test also reds. 3 killing mutations RED. |
| TG-424 | **DONE — code MERGED (!1264) + LIVE-CONFIRMED 2026-08-08** (baseline re-anchored 4.64→4.49 @58d4bfda; filed delta: mistral appropriate_band -0.30/sensible_proposal -0.40) | `baseline-freshness` was RED on every main pipeline: the committed eval baseline was ~9d stale and the trend-watch refused to refresh because the anchor predated the opus-cc→mistral swap (stuck fixed-point). `gate.ShouldRefreshTrend` now RE-ANCHORS past a STALE anchor (>8d) even on a regression verdict (a stale anchor is an invalid comparator; the CHANGE gate — candidate vs fresh same-window base, NOT the committed baseline — remains the real regression guard, so re-anchoring never hides a regression; the re-anchor run still exits non-zero/files the delta). Pure oracle in `eval/gate` (killing mutation RED); fresh-eyes APPROVE. Self-heals on the next on-box nightly, un-sticking the eval-gated class (TG-354/412/200/360pt1/307). |
| TG-416 | **Fixed** (!1265) | 38 `specvalidate` phantom-owned paths (a `completed` task owns a files_owned path that does not exist) cleared to **0**; `phantomOwnedCeiling` ratcheted 38→0 (zero-tolerance) + spec/007 amendment + lockstep restamp. TWO honest causes, treated differently (conflating them is the trap): the **020/023/028 Go paths were repointed** at the differently-named file that carries the concept, content-verified per entry (triage_judgment.go, activities.go, risk/classifier.go, `ladderRungFor`); the **spec/010 console tasks were flipped to `pending`** — ADR-0015 removed the React frontend and the served console is a partial preview, so per the repo's own acceptance audit (`_test_mapping.json`) every T-010-1..8 defining REQ was already pending/no-feature (REQ-607 replay, REQ-610/611 kill-writes). **A first cut repointed the console `completed` tasks at real-but-unrelated e2e files — a fresh-eyes review caught that false green (the exact trap) and it was fixed-forward to the pending flip BEFORE merge.** Two evidence sweeps + review; not eval-gated. |
| TG-200 | **deferred (eval-gated)** | seeds an `<estate>` block into `composeSeed` (`temporal/runner/activities.go` — IN `behavior_re`), so it needs on-box eval evidence to merge and changes every investigation's context estate-wide. Deferred to an eval-capable session AFTER TG-424's re-anchor, so the number is trustworthy. Box is reachable; Effort S. Reasoning recorded on the ticket. |
| TG-360 | **PARTIAL — observability half done** (!1227) | two deterministic judge axes had graded 2 and 1 of 3,371 sessions and nothing published judged/eligible, so a silent axis read as healthy. `AxisReadStore.JudgmentCoverage` + the worker sampler now publish `tg_judge_axis_scored_total` (declared set emitted AT ZERO) + `tg_judge_sessions_judged_total`, fail-quiet like falsifiability. 2 killing mutations RED; REQ-2529 + T-025-27 + lockstep restamp (axis_read.go is a spec/025 measurement-plane surface — a 3rd lockstep catch this session). **Part 1 (populate the typed diagnosis on the ordinary path — TG-79-shaped, eval-gated in activities.go) REMAINS**; sequence after TG-424. |

### AFK reachability status — 2026-08-08 (what is LEFT and why it is not AFK-bounded)

After landing TG-398/391/390/406/360-obs this session, the CLEAN bounded AFK red→greens in the top of
the ranked queue are exhausted. The remaining top items are each blocked on a class of work that should
NOT be done unattended — recorded here so the next tick (or the owner) skips re-triage:

| ticket | gate | why not AFK |
|---|---|---|
| TG-384 | **PARTIAL — chokepoint landed INERT** (!1230, fix-forward !1231) | the self-DoS root. `Gateway.Concurrency` semaphore + `SetMaxConcurrency` park excess completions (defer-release, never drop; ctx-timeout → typed error), armed by `TG_MODEL_MAX_CONCURRENCY` (default 0 = unbounded = inert). Ships inert because the deploy health gate checks HTTP liveness only, not a completion, so an active hot-path bound can't go live unattended — activation is a supervised config toggle. Killing mutation RED. **!1230 reddened main build-test** (test shared a non-sync capObs across concurrent calls → `-race`; + compose-env-parity for the new var); fixed forward in !1231. **REMAINS:** worker `MaxConcurrentActivityExecutionSize`; couldn't-investigate-vs-found-nothing retryable distinction; live activation + sizing (supervised). |
| TG-200 / TG-360 pt1 | **eval-gated** | both touch `temporal/runner/activities.go` (∈ `behavior_re`) — mandatory on-box eval to merge. Sequence after TG-424 re-anchor so the number is trustworthy. |
| TG-385 / TG-376 / TG-387 | **large + coupled** | the dispatch/correlation rebuilds (durable cluster identity, elect-one-member, recovery→session reconcile). Mutually coupled, touch the dispatch/actuation-adjacent path. |
| TG-424 | **on-box eval (~2h)** | re-anchor the stale eval baseline (model-swap-frozen). Owner/nightly. |
| TG-313 / TG-293 | **owner/infra** | host memory tuning; single-vendor-vs-resilience posture call on the brain config. |
| TG-354 | **supervised (policy)** | dedup fail-danger is real but the naive fix floods storm dedup; tangled with TG-399/TG-377. See the ticket's mechanism comment. |
| TG-181 | **corpus-blocked** | needs organic per-class material the corpus does not have (ticket says wait). |
| TG-176 | **on-box eval build** | replays sessions through the reasoning path = model calls; a harness, not a bounded fix. |
| TG-186 | **owner (constitutional)** | amends CONSTITUTION.md §6 + ratifies ADR-0013 — requires the owner AS the ratifying authority. |
| TG-403 | **not in this repo** | subject is the predecessor's `scripts/qa/` suite, absent here. |

Next AFK tick: prefer a supervised item ONLY if the owner is watching; otherwise there is no clean
unattended red→green left in the top categories — the honest state, not a gap to paper over.

### Deploy/merge observation — 2026-08-08

- **CONTINUOUS DEPLOYMENT is live**: `.gitlab-ci.yml` `deploy` is `when: on_success` on main and runs via `needs: [image-*]`, so it deploys on green IMAGES **regardless of the `baseline-freshness` red** (verified: `deploy: success` on a `status: failed` main pipeline). Consequence for AFK: (a) merges auto-deploy to dc1tg01 — a hot-path change like TG-384 would go live UNATTENDED, which is why it is supervised-class; (b) TG-424's dead-man is fully defeated — red every pipeline AND deploys proceed. Merges here have also been observed completing **before** the MR pipeline finishes, so confirm the post-merge main pipeline, not just the merge.
- **`make all` does NOT run `-race`; CI `build-test` runs `go test ./... -race -count=1`.** A green local `make all` can still red main on a data race — it happened on !1230 (a test shared a non-synchronised observer across concurrent calls; caught only under `-race`). Before pushing anything with concurrency (goroutines, a semaphore, a shared fake), run `go test ./<pkg>/ -race` locally. Also: a new `getenv`/`envInt` key in cmd/worker or cmd/grounder must be forwarded in that service's `deploy/docker-compose.yml` environment block or `TestComposeEnvParity` reds build-test.

**Category-4/5 cascade shape (2026-08-08):** the concrete data/graph defects (TG-389 host, TG-379 edges)
are clean bounded red→greens and LANDED; the ones that change a live decision stage's BEHAVIOUR (TG-377
dedup feeding) or instrument every stage (TG-380) are supervised/large. Two lockstep misses this arc
(TG-404 interceptor, TG-371 auth) cost re-work — `specvalidate lockstep --check` is now a mandatory
pre-push step (memory), and TG-389/TG-379 were checked clean before push.
| TG-418 | filed | the new boundary's drill is on-demand; a scheduled witness with a paging path (+ absent() half) is the follow-up |
| TG-419 | filed | TG-382's second clause ("no signal that it is gone"): no host→prometheus path exists; needs a node-exporter-as-compose-service design, not a config edit |

**TG-381 in one paragraph.** `TG-EGRESS-LAN` in DOCKER-USER (FORWARD *is* the path for tier→LAN — the
script header's INPUT argument does not transfer): conntrack → docker-tier RETURNs (br_netfilter means
same-bridge c2c traverses DOCKER-USER) → unconditional DROP of the LAN gateway derived from the default
route → source exemptions for the two workers pinned at 172.23.0.10/.11 (their estate reach is wildcard
SSH `dc1*`, not destination-enumerable) → the derived allowlist → RFC1918 default-drop.
`cmd/egresslan` derives the allowlist with `DeclaredDestinations` — the HTTP meter's own mechanism — from
the stack's `.env`: 25 LAN destinations on the box, router absent because nothing declares it. Both of the
ticket's killing mutations executed RED **live** (DROPs removed → router :22 OPEN from litellm again;
blanket DROP → the positive control died), five static mutations RED against the parity test, drill 3/3
PASS after restore, worker probe sweep 10/10 ok including hostdiag+syslogng from the pinned IP.

**The residue is the exemption.** Workers can still reach non-gateway infrastructure on allowlisted
subnets — the price of wildcard SSH; in-process controls govern there. And a control drilled once is not
a control watched: TG-418.

**A process lesson recorded because it cost a redo:** the first mutation drill used `git checkout` to
restore between mutations while the work was UNCOMMITTED — the first restore silently reverted the whole
extension, so mutations 2–5 ran against a baseline missing the code under test and their RED was vacuous.
Commit first; a mutation drill against the wrong baseline reads exactly like a passing one.

### 2026-08-09 — TG-412 tracer boundary + TG-178 3rd producer + a red-main incident

| ticket | state | note |
|---|---|---|
| TG-412 | **Fixed** (!1274 + red-main follow-up !1276) | the decision tracer had no representation of REQ-2001's 7th boundary, "regime select". Added `StepRegime` (record.go), `RegimeRecord{Present,Lane,OpClass,DecidedAt}` + an assemble arm emitting 'Regime select: <lane>' before verify (assemble.go), and a spine read of the action's `regime_actuation` row (trace_spine_read.go — lane+op-class only, no secrets, migration 0020, pgx.ErrNoRows⇒absent). Oracles killing-mutation RED: `TestAssembleRegimeSelectBoundary` + the DSN-gated round-trip. The req2001 boundary-coverage guard + spec/020 `_test_mapping` note updated — all seven boundaries now representable. The REQ-2001 acceptance SCENARIO stays @pending on a SEPARATE concern (spec says "emit … side-write", impl DERIVES — a spec/007 wording reconciliation), NOT a missing boundary. YouTrack State→Fixed with evidence. |
| TG-178 | **PARTIAL — 3rd producer landed** (!1275) | the actuation-limit / actuation-frequency gate (TG-166a) now emits its RATE-BUDGET margin: `min` over session/target of `(per-window cap − trailing-window count)` = `ActuationLease.headroom`; 0 = last actuation before the throttle. Tracks the per-window rate budget, NOT the in-flight mutex (binary, always 0 slack on pass). Observe-only (pure side effect, never read back, refusal path unchanged, emits no margin). Protected + spec/013 lockstep restamped (46/46, design.md amendment extended); NOT eval-gated. Oracles both killing-mutation RED: `TestAdmitReportsRateBudgetHeadroom` + `TestActuationLimitGateEmitsRateBudgetMargin` (the reachability guard). Producers now: policy min_confidence (!1267), policy-band (!1272), actuation-limit (!1275). **REMAINS:** remaining producers (graduation/ACL/mode/floor where a numeric margin is meaningful) + flywheel-preferential wiring. |

**RED-MAIN INCIDENT (two, both mine, both fixed forward same-session).** (1) !1272 (TG-178 band producer) was **merged with a FAILING pipeline** — its band-margin emit added a 14th gate-verdict row (`policy-band`) but the spec/020 acceptance oracle still hardcoded 13 gates (`got 14, want 13`). Its own `build-test` was red at merge; that broke main. Fixed by **!1273** (teach the oracle the policy-band row), CI-confirmed green. (2) I then **immediate-merged TG-412 (!1274) before its MR pipeline finished**; the `harness` job then caught that the DSN-gated `TestTraceSpineRoundTrip` asserts the assembled walk, which went 12→13 with the new regime step — I'd updated my own new assertion but missed the pre-existing count, and my local `TG_DB_TESTS_MAY_SKIP=1` run never executed it. Fixed by **!1276** (12→13), **verified live** against a fully-migrated Postgres (both `TG_TEST_DSN`+`TG_TEST_POSTGRES_DSN`, pgvector).

**Two process lessons re-paid this arc (both already on this board — and I repeated them):**
- **Never merge a protected/behaviour-changing MR before its pipeline is green.** !1272's red build-test WAS the warning; merging red broke main. Auto-merge on this project frequently falls through to an immediate merge ("No pipeline running") when the MR pipeline hasn't spawned yet — so it does NOT protect against merging red. For anything DB-gated or touching the assembled walk, run the two-DB harness locally (below) or wait for the MR pipeline.
- **`git checkout <file>` to revert a mutation reverts to the COMMITTED base, destroying uncommitted work** (the exact TG-381 lesson at the end of the 08-08 block). I hit it again reverting the actuation-limit mutation drill; use an Edit-based revert, or commit first.

**Running core/db DSN-gated tests locally (this session, worth keeping):** two local pg containers exist — `tg-local-emptypg` (127.0.0.1:55468) and `tg-local-testpg` (55467), image pgvector, `postgres/goldtest`. Create a migrated DB (apply every `core/db/migrations/*.up.sql` in sorted order) and a second EMPTY DB, then `TG_TEST_DSN`=migrated, `TG_TEST_POSTGRES_DSN`=empty. The package REFUSES a partial run (only one DSN) rather than skip — that guard is what makes a local DB run trustworthy.


## RANKED 2026-08-03 (evening) — from `project: TG #Unresolved` (169 open)

The owner drove this ordering by operating the live console: three of the five below were found because
they opened `#reasoning` and clicked things. Landed the same day, all live-verified by Playwright click +
screenshot: TG-269 (!904/!907 — the page is navigable, deep-linkable, states how a walk ended; CLOSED),
the blank-modules fix (!903), the every-view smoke gate (!905), TG-268 (!906), TG-272 (!908 — evidence
store + working citations; deployed, `agent_step_evidence` live with 0 rows pending first post-deploy
session). TG-271's HOST fix is applied (18 cross-verified host keys added; worker's exact strict-SSH path
re-run green) — the ticket stays open for the missing GATE.

| # | id | what it costs while broken | evidence anchor |
|---|---|---|---|
| 1 | TG-271 | a diagnostic capability failed 100% for weeks and NOTHING noticed — the gate (boot coverage ratio, yield gate on the unreachable sentinel, vacuity floor) is the deliverable; the config fix alone will rot again | worker log silent; 478 calls, 0 stored yields |
| ~~2~~ | ~~TG-265~~ | **DONE 2026-08-03 (!909, live-verified):** hostdiag + syslog known_hosts are UX config with Save/Test; 3 os.Getenv bypasses closed; live TEST pressed via Playwright — "PASS — 84 host-key entries" on the real page. CLOSED. | screenshot in TG-265 |
| 3 | TG-264 | `deploy/module_descriptor_test.go` matches raw text, so a key named only in a comment satisfies the oracle — same disease as TG-270; fix both with one structural pass | AST replacement exists in cmd/worker/boot_config_test.go |
| 4 | TG-270 | the served-console self-containment guard reads JS `new URL(` as an external stylesheet — cost two red pipelines in one day; each future toucher of `_live/js.txt` pays it again | deploy/served_console_test.go:102 |
| 5 | TG-272 | open only for the LIVE payload demonstration — first post-deploy session closes it; needs traffic (injector is an owner lever, rule 6) | agent_step_evidence = 0 rows, correctly |

## PREVIOUS QUEUE — CLEARED 2026-08-03

Every ranked item landed and closed: TG-260, TG-257 (both halves), TG-227 (all four blockers + the oracle
window), TG-251, TG-196, TG-194/195 (migration 0052), TG-234, and the tracker-hygiene sweep. The next
queue forms from `project: TG #Unresolved` (170 open) — all three named candidates (TG-267, TG-266,
TG-237) landed the same day and are recorded below. The remaining known set is TG-261..265 (the TG-260
follow-ons: no config DELETE route, write-time shape validation, grounder env parity, the text-match
descriptor oracle, the syslog known-hosts dialog) plus whatever the next read of the tracker ranks above
them. Rank on merit when the next session opens it.


## WITHDRAWN 2026-08-02 — TG-255, ranked #2 here for one hour and wrong

I claimed four seeded op-classes held "promoted-only autonomy without ever being promoted". **A live check
refutes it.** The ladder on the running system:

| op_class | ACL says | ladder level | provenance |
|---|---|---|---|
| restart-service | auto | **auto** | `verified_clean` — earned |
| start-service | auto | **auto** | `verified_clean` — earned |
| start-container | — | **auto** | `verified_clean` — earned |
| reload-service | auto | **approve** ← downgraded | seeded |
| restart-container | auto | **approve** ← downgraded | unverified |
| start-guest | auto | **approve** ← downgraded | unverified |

**Zero classes sit at auto with `seeded` provenance.** Five ACL rules deliberately authored by an operator
(`updated_by: operator:p4-1-start-service-rule`, 2026-07-27) grant `verdict: auto` to those op-classes, and
the graduation ladder is DOWNGRADING three of the five to `approve` right now. `core/policy/engine.go:362`
is a one-way filter — *"an ungraduated `auto` downgrades to `approve`"* — so the ACL proposes and graduation
only tightens. The system is working as designed.

**How I got it wrong, because the failure mode matters more than the item.** I read
`core/policy/defaults.go:96` — a FRESH-DEPLOYMENT seed path — and asserted it as live state without querying
the running ladder. Working rule 2 exists for exactly this, and I broke it in the item I ranked second.
`reload-service` still carries `last_outcome: seeded` and sits at `approve`, which is the evidence I would
have seen had I looked.

**Residual, low, tracked on TG-255 not here:** on a genuinely fresh deployment the seeder does write
`LevelAuto` directly. Reaching actuation from there needs two further deliberate operator acts — authoring
auto ACLs and leaving Shadow — and the seeder logs the provenance as it does it. Worth a guard, not a rank.

---

## Landed 2026-08-03 — TG-267, TG-266, TG-237: the three post-queue candidates

**TG-267** (!897): the registry learns the whole catalog. `GET /v1/modules/schema` now answers
**29 of 29 known, 0 unknown** (was 4 of 29 — verified live); the projection publishes 43 rows, up from 18.
The fix rode TG-252's existing per-construction chokepoint instead of the 25 hand edits the ticket
sketched, and REPLAYS the offer set so it is immune to construction order. Two self-inflicted bugs caught
by its own oracles pre-merge: the wiring first sat one line ABOVE the capability pin (would refuse boot on
a pinned deployment), and the AST oracle that caught it had first matched an unqualified `.Reconcile`
1,800 lines away. Vacuity floor asserts 29-of-29. Residue noted on the ticket: 14 registry pairs have no
descriptor — the inverse gap.

**TG-266** (!898): exactly-once ladder credit. `graduation_credit` shipped with migration 0050, stated its
contract in its own `COMMENT ON TABLE`, and had zero code touching it and zero rows ever. The claim is now
consulted before any streak increment. Only the PROMOTING outcome is deduped — a streak-breaking outcome is
a safety action and must never be withheld by a bookkeeping key; a store error means not-credited, so a DB
blip cannot mint the double-credit the table forbids. The async lane needs no claim and gets none (checked:
one record per action_id + refuses to re-resolve). DB oracles against real Postgres; `DO NOTHING → DO
UPDATE` executed RED.

**TG-237** (!899): the eval gate finally blocks something. TG's whole eval apparatus gated nothing because
CI has no model. A new `eval-evidence` MR job (no `allow_failure`, drilled by 8 arms incl. fail-closed-in-CI)
refuses an agent-behavior change carrying neither the on-box gate's committed record nor a named
`Eval-Gate-Waived-By` trailer; a FAILING record is refused outright. The behavior set is narrow because the
measurement said so — ~3 of the last 40 commits touch it. My first count said 38 of 40 (`git log -40 -- path`
returns the last 40 commits that TOUCHED the path) and would have led to the opposite conclusion; recounted
before building. MECH-611 moves PARTIAL → PORTED with the constraint stated: what blocks is absence of
evidence, not a fresh judgement.

Tracker: 206 → **167** unresolved across the day.

## Landed 2026-08-03 — TG-194/195, TG-234, and the tracker-hygiene sweep

**TG-194/195** (!894, migration 0052): every judgment names its rubric (bump-enforcer makes an un-bumped
rubric edit a red build) and its action (derived under an exactly-one rule; 10,099 of 14,923 rows
backfilled live). Means pool one rubric version; counts stay version-blind; `VerifyComparable` refuses
mixed-rubric arms. Both closed Fixed.

**TG-234** (!895): the verifiability floor fails closed by DESIGN — the nil-observer crash path replaced
by a ledger-visible backstop refusal, and the gate drill pins 4c's own wording so the backstop cannot
absorb the gate's deletion invisibly (which it did, in the first draft — the fix for a non-discriminating
control nearly shipped another one). Three RED controls executed. Closed Fixed.

**Tracker hygiene**: 36 issues closed with per-issue evidence — 22 delivered-but-never-moved (including
this arc's own TG-252/254/260/248/258/259 and older landed work TG-31/41/125/183/3/23/71/121/139),
8 duplicates linked to their canonicals (the wisdom-theme re-scores → their TG-38 children; OTel emit →
TG-44 with TG-32 kept as ingest; TG-43→TG-237, TG-54→TG-197, TG-83→TG-188), 4 obsolete under
ADR-0013/0016 (mutation-ON gating and hand-registered op-classes — both models retired), and 2 done-work
closes the board itself had ordered (TG-211, TG-253). Unresolved: 206 → 170. Detection ran three
angles (pairwise, delivered, obsolete) with every finding verified against descriptions or the tree;
TG-215 was examined and deliberately KEPT (distinct from TG-54).

## Landed 2026-08-03 — TG-227 complete, TG-251 and TG-196 closed

**TG-227** — all three MRs merged (!887 overlay+thresholds, !888 ReadyResolver+barred coupling, !890 the
law-trailer oracle window: ten spec/028 mapping rows flipped to real oracles, T-028-11 ownership,
spec/027's first acceptance runner). Closed Fixed. Live: loader honest-zeros every 60s; the cluster pass
correctly refuses itself until the occurrence intake proves alive; stamp confirmation follows the first
organic occurrence. Residue: spec/027 REQ-2707 step file (one-file change now), TG-266.

**TG-251** (!891, migration 0051) — the worker publishes its module enablement every 60s; the API reads
it through a 3×-interval staleness cutoff. `notifier/matrix` answers `enabled_known:true, enabled:true`
live; a dead worker degrades to unknown within the window, proven by a clock-driven oracle. Closed Fixed.
Residue filed as **TG-267**: the registry holds 18 pairs vs the catalog's 29 — connectors constructed
without registering stay unknown; registration lifts them all through this channel unchanged.

**TG-196** — `TG_DECAY_INTERVAL=1h` + `TG_DISCOVERY_FLUSH_INTERVAL=10m` armed live (flush to
`/knowledge/`, the one writable mount — the compiled default path could never have worked on the
read-only rootfs). Boot pass executed; arming lines verbatim in the ticket. The infragraph stops being
ratchet-only; deviations survive restarts. Closed Fixed.

## Landed 2026-08-03 — TG-257, the deleted frontend stops being carried (BOTH halves)

Tracked tree: `8,609 files / 117 MB` → **`1,904 files / 18.7 MiB`**, zero under `frontend/`. And on the
owner's order the same day, the HISTORY half too: `git filter-repo` stripped `frontend/node_modules` +
`frontend/dist` from every commit. **Fresh clone: 41.16 MiB → 17.96 MiB.** Verified before the push:
main's tree byte-identical, 2,142 commits preserved, `archive/frontend-react` still holds all 45 source
files, gitleaks allowlist SHA untouched (it was never on a branch). Branch protection restored to its
captured config; restore-tested pre-rewrite bundle at
`/root … /home/tg/tg-repo-backups/20260803T025812Z-pre-filter-repo/`.

Aftermath: the force-push re-triggered the 5 open MR pipelines; 3 went green with the allowlist merge,
and the 2 that stayed red (!785, !764) turned out to be **superseded** — their content is already on main
(byte-identical driver; head commit present as `3796a0a9` + two later fixes). Both closed with evidence,
branches intact. TG-257 comment trail carries the full record; state flip to Verified needs an owner
click (API state transitions are permission-blocked for this account).

ADR-0015 deleted the unreachable React frontend on 2026-07-30, and the commit that did it removed the 52
SOURCE files while also deleting the `.gitignore` lines hiding that build's output — so the deletion left
the build behind. Nothing was lost here: the tag `archive/frontend-react` still holds the 52 source files
including `frontend/src`, and every remaining mention of `frontend/` in the repo is a comment recording
that it was removed. No image copies it.

**The ignore rules are not the protection** — they existed before and a commit deleted them, which is the
whole cause. `deploy/tracked_tree_test.go` checks the INDEX, so re-adding the tree fails the build whatever
`.gitignore` says: no path inside `node_modules`, nothing under `frontend/`, no unexplained blob over
4 MiB, and the rules themselves still present. All four verified RED by hand. The restored rules are
unanchored, which the previous ones were not — `/dist/` matches only the repository root.

Nearly shipped broken: `go test` runs with the CWD set to the package directory, so a bare `git ls-files`
in `deploy/` lists `deploy/` and nothing else. The first draft reported a 3.0 MiB tree while 98.5 MB sat
one level up, and passed green. Every git call is now anchored with `-C` to `git rev-parse --show-toplevel`.
Worth remembering: a gate that measures the wrong scope reads exactly like a gate that passes.

MR !884, merged `556e5578`. The history-rewrite half is an owner decision, listed above.

## Landed 2026-08-03 — TG-260, module config now comes from the database

**Confirmed live, not merged-and-assumed.** The host's `.env` says `TG_PVE_LIVENESS_POLL_INTERVAL=45s`.
`37s` was saved through the console's own route. The worker now runs on **37s**:

```
module config: 1 of 115 settings resolved from the console (the rest from the environment)
module config:   TG_PVE_LIVENESS_POLL_INTERVAL ← module.ingest.pve-liveness.poll_interval
pve-liveness: guest-liveness pull every 37s over 20 allowlisted guest(s)
```

Was: 115 settings saveable, **3** ever read (the Matrix notifier's, through a use-time holder); the other
112 were saved durably, ledger-recorded, and consulted by nothing, because every consumer resolved through
`getenv` = `os.LookupEnv`. Now `getenv` is the resolution point — console → environment → compiled default
— so all 115 are reachable and a module added tomorrow is configurable with nothing to remember to wire.

The write plane was never the bug: `SetModuleKeys(catalog.ConfigKeys())` already ran in both roots and the
write path already accepted all 115. `/v1/config` reports 126 keys (11 compiled + 115 module), 1 currently
console-sourced. What was missing was a reader.

Structurally excluded from the store: the DSN (read with `os.Getenv` directly — a database cannot supply
its own address), bootstrap knobs, secret VALUES (same filter as `ConfigKeys`, which drops
`TypeSecretValue`), and LAW. Two hazards the inversion created, both answered in the same change —
`bindingValueFault` validates a stored value against the field's own `Pattern`/`MaxLen`/`MaxItems`/Type
before serving it (constraints 29 descriptors declared and nothing enforced), and `TG_CONFIG_IGNORE_STORE`
is the break-glass, because the only writer that can fix a bad row runs *inside* the worker that row would
stop from booting.

Five oracles, each naming its killing mutation; all verified RED by hand before shipping. The load-bearing
one sets BOTH layers to different sentinels across all 113 distinct settings — an earlier draft set only
the store, which passes just as happily with the precedence swapped, i.e. with TG-260 fully reintroduced.

MR !882, merged `9af8777c`. Follow-ups filed: **TG-261** (an override cannot be un-set — POST-only route),
**TG-262** (`Pattern`/`MaxLen`/`MaxItems` unchecked at write time), **TG-263** (the API process still reads
env, so the two halves can disagree), **TG-264** (the descriptor-env-key oracle matches comments, not
reads), **TG-265** (`TG_SYSLOGNG_KNOWN_HOSTS` gates every syslog read with no dialog).

Same restart carried the OpenBao host bindings: `openbao added=12 covered=12`, up from 5. The 7 named hosts
are keyed by FQDN — the earlier "named hosts don't resolve" call was an artefact of a musl/alpine test
container with `options ndots:0`; Go's resolver applies the search list and the worker resolves them fine.

## Landed 2026-08-03 — the top three of this queue

- **TG-254 — the approver set now gates the vote.** It was computed, guarded across four resolution paths,
  rendered in the console, and consulted by nothing: any authenticated operator could approve any governed
  action at Semi-auto. The set is now resolved at gate time, carried into the workflow in history, and the
  voter admitted on signal receipt. **INERT until a bundle declares an approver** — 0 of the 5 live rules do,
  so strict enforcement would have admitted nobody and timed every poll out to deny after 24h. The
  implementing agent justified strict mode by asserting the deployment runs Shadow; the live system answers
  Semi-auto with `may_auto_actuate:true`. Caught by querying the running system, not by reading the report.
  Every admitted vote is now ledgered as `human:vote-admitted-unconfigured`, so the remaining exposure is
  countable. **Arms itself the moment `approve_by` is declared on any rule.** (!880)
- **TG-256 — one authority.** 15 reference docs bannered, the archive's two buried imperatives annotated in
  place, the Dockerfile's false CI claim corrected, and `AGENTS.md`/`CLAUDE.md` now name the tracker — which
  is what made 179 open issues invisible to a fresh session. (!877, !878)
- **TG-258 / TG-259 — the gates fail now.** `ratify` printed RATIFIED over Draft specs and checked only that
  a filename existed; the adversarial pass found three more mutations that survived the first fix. `core/db`
  reported `ok` while skipping 83 of 122 tests, one of them TG-184's own single-writer oracle. On its first
  honest run the fixed gate caught 18 spec/010 scenarios claiming oracles deleted with `frontend/` — 6 are
  now wired to oracles that genuinely cover them, 12 are honest debt with the unasserted clause named. (!879)

### LIVE INCIDENT — brain outage / judge-death (2026-08-08, resolved)

The matrix room was spamming `GOVERNANCE judge-death: judged fraction below threshold` every governance
tick. ROOT CAUSE: the tg-claude-proxy **Max subscription hit its seven-day limit** (`rl_type:"seven_day"`,
rejected → empty error bodies). `primary` (judge) and `fast` (agent loop) both resolve to `opus-cc` with an
EMPTY fallback ladder (TG-293), so the whole brain went down: the session judge got `no json object` on 100%
of calls → judged fraction 0/29 → the judge-death dead-man (`core/governance/judge_liveness.go`) HALTED skill
accrual and re-warned every tick (no dedup; can't auto-rearm below the 0.75 life threshold). Dead ~33h.

FIX (owner-directed, verified live): owner supplied the Mistral + LiteLLM master keys; written to OpenBao
`secret/data/tg/litellm` (read-merged, version 3 — they were EMPTY, which is why the ladder was decorative:
tg-secretenv `-skip-empty` dropped the blank refs). Repointed primary+fast `opus-cc → mistral/mistral-large`
live on the box AND in the repo (!1233, merged, so a redeploy won't revert). Verified: litellm healthy;
mistral returns parseable JSON on the REAL golden judge prompt (all 5 dims); deepseek tier also verified
working. Judge recovers at its `13 */2 * * *` cron; sub resets 2026-08-12 05:00Z (revert primary/fast to
opus-cc then is a one-line owner call).

NEXT (durable TG-293 close, teed up): wire a real cross-provider `router_settings.fallbacks` — primary/fast
→ `[fallback-deepseek]` (both rungs verified). Deferred until judge-recovery confirms, to avoid a litellm
restart colliding with the 12:13Z cron. SECURITY NOTE: the bao write needed a ROOT token (box `tg` identity
is read-only, verified); a scoped writer role is the right standing mechanism, not a stored root token — the
root token was NOT persisted.

### TG-314 — the OFFLINE plane now computes estate_grounded (2026-08-08, !1254)

TG-202 shipped estate_grounded in the LIVE judge cron but not offline: `eval.Aggregate` had no graph, so the
offline scorecard published diagnosis_grounded and NOT estate_grounded — the flywheel's pre-filter and the
committed baseline could not see the axis. `Aggregate` now takes an optional estate snapshot (VARIADIC — every
existing caller compiles unchanged) and computes it the same way the cron does (`GroundInEstate` →
`ScoreEstateGrounded`). Design call (a): score against a CONSTANT `estate_fixture.json` held fixed across a
scorecard — isolates a skill's effect from estate drift (and `LoadEstateGraph` reads a captured fixture, so it
is reproducible). Kept OUT of Overall's fixed denominator + `gate.Dimensions` (like diagnosis_grounded); nil/
absent snapshot ⇒ honestly N/A. Wired reachable through `tools/rejudge` (the eval-gate A/B producer, loads the
fixture by default). Oracle `eval/estate_dim_test.go` (killing mutation RED); full eval suite green; not
eval-gated (eval/*.go, not corpus.json); not lockstep/protected. Remaining: the shadowbench (python) side.

### TG-206 (part c) — durable pgx discovery corpus, surviving a restart (2026-08-08, !1252)

MemDiscoveryCorpus held the verify-time discovery corpus (live-scored mispredictions, keyed by deviation
signature, reproductions counted as the promotion signal) only in the worker's memory — a restart dropped it
and reset every count. `core/db.DiscoveryStore` (migration 0072) is the pgx-backed durable
`falsify.DiscoveryWriter`: one row per signature, `ON CONFLICT … RETURNING (xmax=0)` for new-vs-reproduction,
typed breakdown as jsonb. Wired as a DUAL writer beside the Mem buffer when a DB is present (Mem still serves
the flush drain; pgx adds persistence). Oracle (real Postgres): a capture SURVIVES a restart — fresh store
reads it back with reproductions intact + jsonb round-trip; killing mutation RED. Full core/db suite green
(the credential-plane governance test caught a missing declaration → fixed to 'both'). Not eval-gated; not
lockstep/protected. TG-206 part (b) path-scoped decay was already done; remaining part (a) per-edge disproof row.

### TG-206 (part a) — attach the contradiction to the edge: durable per-edge disproof (2026-08-08, !1258)

decay-on-disproof lowered a learned edge's confidence and threw the `DecayReport` away — the contradiction
vanished. Now `DecayReport.Disproofs` carries one attributable `EdgeDisproof` per decayed edge (the edge, the
misprediction that disproved it via `DeviationKey`+`action_id` carried on `DisproofPath`, the confidence decayed
TO, aged-out?), and the worker persists them to the append-only `edge_disproof` (migration 0075, plane 'both')
via `estate.EdgeDisproofStore` (pgx `EdgeDisproofs` + `MemEdgeDisproofStore` twin) — best-effort, the graph swap
stays authoritative. `disproofPaths` now emits one path PER CAPTURE carrying the attribution (the decayed edge
SET is identical to the merged-by-target form — part b unaffected). Oracles: estate unit (attributed disproof;
aged-out; attribution doesn't widen scope — killing mutation RED) + real-pg Record/List round-trip. Completes
TG-206; gives TG-388 the durable per-edge disproof substrate it needs at Build. Not eval-gated/lockstep/protected.

### TG-325 (observe half) — per-session recon FAN-OUT flag, the sweep the volume bounds miss (2026-08-08, !1259)

TG-165 bounds the read lane by VOLUME (25/session, 500/hour, burst 150/5m). A volume bound cannot see an actor
UNDER it: 12 reads of 12 DISTINCT hosts is a methodical sweep whose count is unremarkable but whose COMPOSITION
is not. The `ReconGovernor` already metered per-session distinct `targets` (TG-166) but nothing reasoned over
them (`tg_recon_targets_hour` exported+unused). Added `ReconBudget.FanoutObserve` (default 12): when a session
reaches the distinct-target ceiling, `Record` raises an OBSERVE-ONLY flag ONCE per session — counts
(`tg_recon_fanout_flagged_total`) + logs — and does nothing else: it NEVER refuses a read and NEVER forces
Shadow (unlike the burst alarm), so the volume bounds remain the only guards; it earns its way toward acting on
live evidence first (TG-165's observe-first pattern). `FanoutObserve` is the one budget field legitimately
disable-able (0=off) — disabling an observation is not disabling a guard, so `sane()` doesn't force it back.
Oracle: a POLL of one host (fan-out 1, volume 8) raises 0 flags while a SWEEP of 8 distinct hosts (SAME volume)
raises exactly 1, and Admit never refuses either — killing mutation (key on read volume) RED. `core/safety` is
protected → `Law-Change-Approved-By: @ncpjfuzl` trailer; NOT lockstep; not eval-gated. **REMAINS (supervised):**
cross-session composition anomaly + ACTING on the signal — a shape detector must not gate live triage until
calibrated.

### TG-407 — arm the covered-but-empty intrusion signal (REQ-2304 half 2) (2026-08-08, !1260)

`attributed-suspicious` had fired **0 times in 3,383 sessions**: REQ-2304 half 2 (a reader that AFFIRMATIVELY
COVERS the subject's audit trail returns NO entry for an observed mutation ⇒ suspicious) was structurally
unimplementable — `Covered` rode on evidence ROWS, so "covered and found nothing" had no row to carry the flag.
Fix: a coverage MARKER (`attribution.CoverageMarker`, Covered=true + empty actor) that the covering readers emit
on a CONCLUDED clean miss; `Attribute` holds markers aside from actor evidence and, when no admissible actor
exists, SURFACES covered-but-empty via `Finding.CoveredButEmpty` + a warning while keeping `Taxonomy =
Unattributable`. **Observe-only, deliberately NOT escalated** — the first draft minted `AttributedSuspicious`,
and fresh-eyes review (agreeing with my own trace) caught that as a CRITICAL flood: covered-but-empty is the
COMMON case (a crash, an in-flight job, a system-triggered change all leave no actor entry, indistinguishable
here from an unaudited mutation), so escalating would route the majority of no-actor sessions to SECURITY and
neuter auto-heal. Escalation is deferred to a downstream check carrying a confirmed observed-mutation signal (the
ingredient REQ-2304 half 2 actually needs). Also fixed the review's awx bug: a still-running job is "answer not
in yet", NOT a clean miss — it suppresses the marker (`unconcluded`). Avoids the eval gate: markers flow through
the unchanged reader aggregation (`activities.go` untouched; `enrichSanctioned` skips empty-actor rows). Oracles:
covered-but-empty ⇒ Unattributable + surfaced (two killing mutations RED — escalate reds the no-flood test, drop
the flag reds the surfaced test), blind ⇒ not covered-but-empty, marker never overrides a real actor, out-of-
window doesn't raise it, the common no-actor fault does NOT flood; reader tests assert marker-on-concluded-miss
(awx still-running ⇒ no marker). Not protected/lockstep/eval-gated. **REMAINS:** the mutation-confirmed escalation.

### TG-424 — trend-watch re-anchors past a STALE baseline (unblocks the eval-gated class) (2026-08-08, !TBD)

The committed eval baseline sat 9 days stale and the nightly trend-watch DECLINED to refresh it: self-refresh
fired only on a non-regressing run, but after the opus-cc->mistral model swap every clean nightly read as a
"regression" vs the pre-swap anchor (a cross-model comparison), so it never refreshed — a stuck fixed-point, and
the baseline-freshness dead-man (since removed from CI, part 1) went red on every commit. Fix (the code half):
`gate.ShouldRefreshTrend` re-anchors past a STALE anchor (older than `TrendMaxStaleness` = 8d) even on a
regression verdict — a stale anchor is an invalid comparator whose "regression" can't be told from drift/a
swap, and the CHANGE gate (candidate vs FRESH same-window base) remains the real regression guard, so
re-anchoring the long-horizon anchor never weakens regression detection. The re-anchoring run still exits
non-zero / files the issue, so a genuine model-quality delta is surfaced, not hidden. Decision extracted to
`gate.ShouldRefreshTrend` so it is UNIT-TESTED with fixtures (pass→refresh, inconclusive→never, fresh+regression
→no, stale+regression→re-anchor, unparseable-date→stale; killing mutation RED) rather than only exercised by the
~2h on-box nightly. eval/gate + tools/evalgate — not protected/lockstep/eval-gated. **Unblocks** the whole
eval-gated class (TG-354/412/48-remainder/200/360pt1/307) once the next nightly re-anchors on-box. **REMAINS:**
the actual live re-anchor is the next nightly's run.

### TG-48 — per-SESSION output-token budget at the gateway (2026-08-08, !TBD)

Another SLICE of TG-48 (after per-call `max_tokens` !1251): `Gateway.SessionTokenBudget` caps the CUMULATIVE
output tokens ONE session spends across all its completions. `max_tokens` bounds a single call; a looping
session (the pve03 cascade shape) makes many in-budget calls and still spends without bound, and the daily cost
breaker only trips globally, after the fact, with no per-session attribution. Keyed on the per-session `user`
(`runner:`+external_ref, `activities.go:665`), once a session reaches the ceiling its NEXT completion is refused
with `ErrSessionTokenBudget` — checked BEFORE the breaker/concurrency slot; fail-SAFE (incomplete, never empty).
Ships INERT (0 = unbounded); armed from `TG_MODEL_SESSION_TOKEN_BUDGET` (compose env + parity), mirroring the
MaxTokens (TG-48) + Concurrency (TG-384) guardrails at the same chokepoint. Bounded per-session ledger
(reset-on-overflow = fail-open). Oracle: A's 3rd call (120 spent >= 100) refused while B (0 spent) unaffected —
per-session, not global; 0 = unbounded; killing mutation RED. adapters/model — not protected/lockstep/eval-gated;
-race clean. **REMAINS:** the agent-loop plan-only-degrade handling of the refusal + live activation/sizing + the
separate tool-output-ingestion cap.

### TG-142 — tracer verify step: label the content-addressed verdict honestly (2026-08-08, !1261)

`action_verdict` is content-addressed by `action_id` and append-only first-wins, so ONE row serves every session
that proposed the identical action — a session's tracer could show a SIBLING's verdict/timestamp. The serious
half was already fixed (`deriveStatus` stopped the executed-LIFECYCLE inheriting a stranger's verdict — the 22/30
live measurement, 2026-07-29). This closes the remaining DISPLAY half: `StepVerify`'s `Reason` now labels the
verdict as content-addressed (shared across any session proposing the identical action; the timestamp is its
first recording, not necessarily this session's), so an auditor does not over-attribute it. core/trace-only,
non-protected/lockstep/eval-gated; oracle asserts the provenance label (killing mutation — plain "deterministic
verifier" — RED). **REMAINS:** a true per-session verdict row would re-key `action_verdict` off (action_id,
external_ref), which changes the verdict/heal-rate surface — consequential, deferred (supervised).

### TG-386 — page a human on a substantive proposal-less handoff, INERT by default (2026-08-08, !1249)

A session that concludes "I know what is wrong, no safe action exists, a human is needed" reached a Postgres
row and STOPPED — the notify call sat ~200 lines downstream in the propose path, gated on a band the
no-proposal path never sets. On the pve03 cascade the ONE investigation of 156 that named dc1pve03 as root
cause concluded "no guest-level action is safe — a human is needed on the PVE host", wrote its row, and
returned, eleven hours before a human found it. The `!inv.Proposed` terminal branch now routes a FILTERED
handoff to `NotifyActivity` — non-empty conclusion AND (DEEP_INVESTIGATION OR handoff-limit escalation), so a
bare no-action stop does not page (naive paging fires on 35.8% of sessions). Version-gated for replay
determinism.

**INERT by default** (the TG-411 pattern): opening a new outward Matrix paging path is a deliberate opt-in —
`NotifyActivity` short-circuits `Delivered:false` until the operator sets `TG_HANDOFF_NOTIFY_ENABLED`
(`Deps.HandoffNotify`). Governance notices (proposed lane, `Handoff=false`) are unaffected. Oracle
`temporal/runner/handoff_notify_test.go`: filter unit cases; a workflow test (real `RunnerWorkflow` via the
Temporal test env, only the LLM boundary mocked) that a substantive terminal SCHEDULES a page carrying the
conclusion (killing mutation → RED); empty-conclusion pages nobody (killing mutation → RED); inert-until-armed.
Full `temporal/runner` suite green under -race. **Eval gate**: `activities.go` ∈ `behavior_re` at the path
level only — the change is notify-delivery (recordTriage persists all judged fields BEFORE the notify block;
no rubric/corpus/prompt touched; NotifyActivity makes no model call). Waived with traced rationale
(`Eval-Gate-Waived-By: @ncpjfuzl`, verified via `%(trailers:key=...)`). Fresh-eyes review: no material
findings. **ARMING = owner decision** — one env flag once the filter is trusted. Residual (separate ticket):
a `Notified` column on `TriageRow` for delivery-status-in-DB (a pre-existing observability gap on every notify
lane, not introduced here).

### TG-375 — tombstone a silent hypervisor's guest edges instead of deleting them (2026-08-08, !1247)

The estate refresh is a full rebuild from current source visibility, so when the PVE cluster API stopped
listing a down node's guests, that node's authoritative `runs_on` parents vanished — absence of evidence
written as evidence of absence, into the ONE structure correlation uses to fold N guest-down alerts into one
hypervisor incident. Measured on the 2026-08-06 pve03 cascade: `runs_on→pve03` went 52→0 three minutes after
the NVMe failed, and blast-radius then predicted an EMPTY set for the outage it should have explained.

`Holder.Refresh` now carries a prior `source=pve` `runs_on` edge the rebuild didn't re-emit forward as a
TOMBSTONE (confidence 0.5, bounded `ValidUntil`, reusing the decay/`fresh()` mechanics) — but only with
POSITIVE evidence the node is silent, not the guest gone: a guest that reappears MIGRATED (delete), a guest
gone from a still-populated node was DESTROYED (delete), only a guest whose node hosts nothing is preserved.
A silent node is distinguished from a genuinely empty estate (preserve on a partial picture or a whole-cluster
PVE read failure; a healthy fully-empty rebuild stays empty — the honest-empty install is unchanged). Two
known limits documented in-code (empty-but-up over-retention; guest-identity), both fail toward the safe
direction for this estate. Oracle `core/estate/tombstone_test.go` — 9 cases incl. the ticket's specified guard
(two snapshots, 2nd missing a hypervisor → parents survive + pve03 blast-radius non-empty) AND a two-cycle
`Holder.Refresh` reachability test; 2 killing mutations RED. Fresh-eyes review clean (findings addressed).
Outside `behavior_re` (no eval gate), not lockstep/protected. Activates only during a real hypervisor outage
(inert in steady state); the oracle is the ticket's specified acceptance.

### TG-411 — Cronicle maintenance-window sensor WIRED into tier-1 suppression (2026-08-08, !1245)

spec/019 was Ratified and its connector (`modules/schedule/cronicle`) complete, but NO composition root
ever constructed it — the maintenance-window sensor yielded nothing while Cronicle ran on the same box for
weeks (boot: `0 freeze … 0 schedule(s)`). The honest fix was wiring, not a build (the `ratified ≠ reachable`
shape — a Ratified spec whose capability no composition root reaches). `cmd/worker` now builds providers
behind `TG_CRONICLE_DEPLOYMENTS` (config-not-code, INERT when unset) and projects each ACTIVE maintenance
window into the freeze plane on the existing reload; `core/schedule` gained `Recurrence.ActiveWindow` /
`WindowRule.ActiveSpan` (pins a recurrence to an absolute span; `WindowContains` delegates, behaviour-equal).

Semantic note: a maintenance window is a FREEZE (suppress the change's expected alerts), so the boot line's
**`freeze`** count — now reported from the armed gate — reflects it; `schedule(s)` stays the learned-reboot
lane, a different mechanism. Fails closed (REQ-1903): unset → dark; unreadable scheduler → no window (estate
stays open to triage); only maintenance-kind projects; a GLOB target is dropped VISIBLY (FreezeGate matches
scope exactly) rather than projected as a silent no-op. Oracle `cmd/worker/cronicle_freeze_test.go` (5 cases,
2 killing mutations RED, verified). Fresh-eyes review: no blockers. Not agent-behaviour (outside `behavior_re`)
→ no eval gate; not lockstep/protected.

**DEPLOYED + INERT — VERIFIED LIVE (dc1tg01 = vmid 101100112 on dc1pve01, via pct):** worker image
`worker:afe3955c` running; boot line `suppression: tier-1 gate active — 0 freeze, 0 fold(s), 0 schedule(s)…`
with `TG_CRONICLE_DEPLOYMENTS` unset — the code is live and correctly DARK (no regression). cronicle-demo (Up
27h) already holds the real inputs the connector consumes: a `tg-window=maintenance` event *"Nightly
maintenance window (librespeed01)"* (`tg-duration=3h tg-target=librespeed01`, 02:00–05:00 Europe/Amsterdam →
`KindMaintenance`), two `tg-window=freeze` estate windows, and one plain job (correctly NOT projected). At the
current hour the maintenance window is inactive, so a live read would project 0 — correct.

**REMAINS = ARMING, which is an OWNER POSTURE DECISION (TG-315 precedent), not an AFK flip.** The `freeze→1`
boot-line proof requires (a) a Cronicle **API key** created in cronicle-demo (none exists — only its internal
`secret_key`) and (b) setting `TG_CRONICLE_DEPLOYMENTS` — i.e. turning ON suppression of librespeed01's
expected alerts during its declared window. Everything is staged; arming is one keyref + env away, but it
changes what TG investigates in production and feeds the autonomous loop, so it is surfaced for the owner
rather than flipped unilaterally. Scope item 4 (TG-362 observable change window) follows arming.

### TG-293 durable fallback ladder LANDED + the sidecar-200 finding (2026-08-08)

Judge RECOVERED (verified live: 129 judgments / 90m, 24h judged/eligible 29/29 = fraction 1.0 → the
judge-death dead-man auto-rearms, matrix spam stops). With the brain stable, wired the durable cross-provider
ladder: `router_settings.fallbacks: primary/fast → [fallback-deepseek]` (!1235). Failover PROVEN live — a
throwaway model at an unreachable upstream (hard error) with fallback=[deepseek] returned deepseek's answer.

★ CRITICAL FINDING (TG-426): the tg-claude-proxy returns **HTTP 200** with `"You've hit your weekly limit"`
as the completion BODY on a rate-limit (confirmed: opus-cc → 200). A 200-with-refusal defeats EVERY
error-based safeguard at once — litellm fallback never fires (proven: opus-cc+deepseek-fallback still
returned the limit prose), the model breaker never trips, ParseScore reads prose (`no json object` →
judge-death), and the agent loop reads a refusal as an answer. The fallback ladder is necessary but
INSUFFICIENT; mistral returns proper 429s so the ladder protects the current mistral-primary setup, but
`opus-cc` callers (arm-* campaign) stay exposed until TG-426 (sidecar must 429/503, not 200-prose).
Also filed TG-425 (judge-death re-warns every tick, no dedup — the alert-hygiene half of "why so chatty").

### TG-425 — judge-death pages ONCE on the transition (2026-08-08, !1237)

The alert-hygiene half of "why so chatty": the judge-death dead-man re-paged the matrix room every ~hourly
tick for the whole 33h outage. `JudgeLivenessMonitor.Run` now warns only on the false→true transition (reads
the dead-man's durable breaker state via a new optional `HaltStateReader`); the HALT still runs every tick.
Safe — the ongoing state is covered by the `AnyCircuitBreakerOpen` Prometheus alert, so this drops a redundant
un-deduped path, not the signal. Killing mutation RED. Sibling `frontier_crosscheck.go` shares the shape (also
Warns "judge-death" every tick) — a small follow-up to apply the same transition-gate there.

### TG-424 — baseline-freshness REMOVED from GitLab CI (owner directive, 2026-08-08, !1239)

Owner directive: the baseline-freshness dead-man must NEVER be a GitLab CI dependency again. During active
dev it reddened every main pipeline + emailed per-merge for a once-a-day, time-based fact (red ~9d,
un-actioned — alert fatigue, not signal). It never blocked (verify-stage needs-DAG; deploys always
succeeded). Removed the job + deleted the check & its drill (eval/ci/check-baseline-freshness{,_test}.sh)
together so no orphaned self-test trips the TG-406 wiring guard; AGENTS.md updated; protected-path trailer
applied. The eval quality-trend concern is KNOWINGLY de-scoped from blocking CI for this phase — if wanted
later it belongs on the nightly SCHEDULE as a non-blocking notice, and the real baseline re-anchor waits
until the production brain settles (opus-cc resets 08-12; currently stopgap mistral). Silent-drift risk
knowingly re-accepted for now. **Lesson: a per-merge email for a once-a-day time-based fact is the CI analog
of TG-425's every-tick page — match alert cadence to how often the underlying fact can change.**

### TG-425 sibling — frontier cross-check death dedup (2026-08-08, !1241)

Applied the same page-once-on-transition dedup to `frontier_crosscheck.go` (it re-warned "judge-death" on
every run of an ongoing death, like judge_liveness did). Both monitors drive the SAME judge-death breaker, so
gating on the shared state dedups the page ACROSS both — first detector pages once, neither re-pages while
open. DRIFT left paging (intentional standing human-adjudication signal). Killing mutation RED; cross-monitor
dedup proven (breaker-already-open → halts, no re-page). Both halves of "why so chatty" now fully closed.

### TG-384 second half LANDED — runner worker activity cap (2026-08-08, !1243)

The gateway semaphore bounds model CALLS; this bounds the investigate ACTIVITIES at the Temporal layer.
`runnerWorkerOptions(envInt)` sets `MaxConcurrentActivityExecutionSize` from `TG_MAX_CONCURRENT_INVESTIGATIONS`
on the RUNNER worker, INERT by default (unset → Temporal default 1000). Only the runner queue — the actuate
worker is already capped tighter by the actuation limiter. Oracle + killing mutation RED; compose-parity green.
TG-384 now has both belts (gateway semaphore + worker cap), both inert; live activation (setting the envs,
~8-16) remains the operator step.

### 2026-08-10 — corrections recorded at the split (TG-428)

- The 08-09 entry above says TG-178's 3rd producer "landed (!1275)". **Premature**: !1275 was
  OPEN with a red `harness` job — a stale-base artifact (branched after TG-412's regime step
  merged but before !1276's 12→13 test fix, so the fixture walked 13 steps against the old 12
  assertion; the MR's own diff never touched that file). Rebased onto main 2026-08-10: diff is
  now the pure margin producer (both oracles PASS locally, `make all` green, trailer intact),
  auto-merge armed — and merge-on-red is now impossible server-side
  (`only_allow_merge_if_pipeline_succeeds=true`).
- !1242's journal note (TG-425 sibling, !1241) is folded in above verbatim; !1242 closed as
  superseded by this split.
- Live-posture drift found at the 2026-08-10 re-verification (current values on the board):
  `TG_DECAY_INTERVAL` now `1h` (08-02 stamp: UNSET), module probe sweep 10/10 (was 8/8), OpenBao
  credential source ON (was "deliberately OFF").
- Worktree hygiene: 24 stale `wf_*` worktrees removed + 2 ghost registrations pruned; 2 kept
  DIRTY with patch backups (feat/tg201-console-shows-the-claim, fix/tg169-real-correlation-stage)
  pending triage — their uncommitted diffs are 5.5k/8k lines from 08-04..05.

### 2026-08-10 — TG-428: the PM overhaul lands (11 MRs, one night)

Owner rulings: 100% = per-issue delivered/deployed/e2e/evaluated/QA≥0.90 over the tracker (DoD
v1.1); the burn-vs-completion gap is a PM defect; cosign retirement accepted (TG-417 CLOSED with
evidence); no scheduled ticks — manual sessions + the Go! contract.

| MR | what |
|---|---|
| !1275 | TG-178 3rd margin producer — rescued from its stale base, merged, DEPLOYED+verified |
| !1278 | the board split: BOARD 13KB queue; 962 journal lines moved here verbatim (43/43 id census) |
| !1279 | steering coherence: multi-tenant residue, dangling refs, stale posture claims retired |
| !1280 | `specvalidate tally`: the INDEX lattice block is generated; hand edits red CI |
| !1281 | cosign chain retired per ruling; spec/022 REQ-2206 annotated retired-not-failed |
| !1282 | AGENTS operating loop + DoD v1.1 + the standing trailer authority documented |
| !1283 | `lint-resume-budget`: the resume path can never silently exceed its 10k-token budget again |
| !1284 | deployed-sha drift witness + merge-gate-setting witness (scheduled lane) |
| !1285 | cold-start drill: the machine-local parent kit gets a falsifiable spec |
| !1286 | `tgledger`: the generated DoD v1.1 meter. Baseline: 420 total · 123 unresolved · 297 resolved |
| !1287 | the Go! contract in CLAUDE.md |

Non-MR: `only_allow_merge_if_pipeline_succeeds=true` on grounder+www (it gated every merge the
same night, including refusing two conflicted merges glab reported as silence); parent-dir router
kit; 24 stale worktrees pruned (2 dirty kept with patch backups, pending triage); tg01 ssh
stanza; www git identity; glab default host. Acceptance executed: coldstart 6/6 probes green;
LIVE fresh-session orientation proof — TOP=TG-428 in **1,308 of 20,000 budgeted tokens** (the
pre-split board alone cost ~28k to read); every new gate's killing mutations executed RED
(recorded in the MRs). Known follow-ups: TG_READONLY_API_TOKEN CI var for the merge-gate witness
(BLIND-but-honest until provisioned); the delivery-witnesses schedule rides the existing nightly;
the 297 grandfathered resolved issues re-verify via the sampled sweep (TG-339 precedent).

### 2026-08-10 morning — INCIDENT #47554: main red on vanished :latest tags; CD stalled ~15 min (TG-431)

At 04:04–04:07Z the overnight session's armed tail merges (pm9/pm10) hit main; #47554 (0a920adc) failed
`image-grounder`+`image-worker`: buildkit named BOTH tags, the `:$SHA` push succeeded, and the `:latest`
push found the tag GONE — both repos' fresh `:latest` tags were deleted out of the shared runner daemon
inside a ~2s window (04:07:38–40Z). Deploy skipped; the box sat on 9fb01301 (the only green of the
overnight auto-cancel chain) until #47556 (50da72ef) went green and AWX-deployed at 04:22Z — box-verified
(docker ps, all four TG containers on 50da72ef). Exclusions from the runner host: no prune cron/timer;
the runner is max_builds=2 and BOTH slots were held by the failing jobs themselves — the deleter was
external to CI; dockerd's event ring had rolled, so attribution is OPEN on TG-431. Mitigation MERGED
(!1288, 200ed8e3): `docker tag $IMG:$SHA $IMG:latest` immediately before every `:latest` push — an untag
self-heals, a truly-deleted image still fails honestly, window shrinks ~90s→ms. Fresh-eyes close review
caught `image-sidecar` missing the same fix (verdict fail 0.72) → !1291 covers it (4 of 4 `:latest`
pushes retag-preceded). NOT retrying #47554: its deploy would roll prod BACK behind 50da72ef.

### 2026-08-10 morning — reconciliation closes + the ledger's first real movement (TG-428 flow)

Delivery-bar closes with evidence, ledger 2/2 → 6/6 evidence-bearing (`make ledger`): **TG-425**
(dedup, !1237+!1241; fresh-eyes pass 0.90 — its real finding, the cross-monitor check-then-act race +
genesis-page blip window, filed as TG-432, linked, not absorbed), **TG-424** (baseline dead-man removal +
stuck re-anchor fix; pass 0.93 — its finding, the "clean, non-regressing" provenance lie on the stale
re-anchor path, filed as TG-433, FIXED same session: red→green oracle, !1289 MERGED 9ade6f9c), **TG-406**
(wiring guard !1225; killing mutation EXECUTED FRESH at close: unwired check-governance-schedules_test →
guard RED naming it → restored GREEN), **TG-324** (egress enforce; fresh Prometheus read at close:
tg_egress_enforcing=1 on worker/worker-actuate/grounder, off-allowlist 0; residuals stay TG-420/TG-415).

**TG-430 CLOSED — the two kept dirty worktrees were anti-work, discarded with proof:** both tickets
(TG-201/TG-169) already RESOLVED and both branches MERGED; the staged 13.4k lines were mass deletions of
files ALIVE on main (core/safety/recon_budget.go −517/+0, core/actuate/limiter.go −282/+0, …) plus
TG-201's REJECTED second-store draft (its own migration collides with main's real 0056). Patches remain
at ~/tg-worktree-dirt-2026-08-10/. Main tree carries only active MR worktrees again.

**TG-403 CLOSED upstream (claude-gateway !207 MERGED):** fail_test wrote a VARIABLE, so 7 assertions
inside `( … )` env-scoping subshells could never fail. Now a per-suite-process mark file survives the
subshell; new in-band self-suite proven RED against the old lib (2 subshell cases) and 4/4 green after.
Un-masking exposed a second stacked defect: the dark-path tests read the REAL ~/gateway.mutations_off
(armed 07-18) — suite now isolates GATEWAY_HOME; 11/11 PASS with all assertions live. The lesson shape:
the dead oracle was HIDING an unhermetic test — fixing the first exposed the second.

**In flight at this entry:** !1290 (TG-419 isolation-presence signal: host timer LIVE on the box since
04:45Z first-read 1/3/1 + 1/35/1, compose host-metrics + 5 alert arms in the MR), TG-418 built on top
(drill witness, branch ready, MR follows !1290), !1291 (sidecar retag). Live killing mutations for
419/418 run at deploy. TG-429 (CI token) surfaced on the board owner list.

### 2026-08-10 midday — isolation-witness cluster LANDED + three chokepoint/observability closes (Go! continuous)

**Category 1 isolation-witness cluster landed** (each armed detection before restore machinery): **TG-419**
(`tg_host_isolation_present` runtime-absence signal, !1290), **TG-418** (scheduled TG-EGRESS-LAN boundary
witness), **TG-382** (tg-host-isolation restore-on-reload). Estate mutations appended to the memory ledger.

**Three delivery-bar closes, each VERIFIED FROM THE RUNNING BOX (not a doc stamp):**
- **TG-426** (!1301, merge `efcd8c2e`) — the tg-claude-proxy served a Max weekly-limit envelope as HTTP **200**
  with the limit prose AS the completion body: the 2026-08-08 outage AMPLIFIER that defeated litellm fallback,
  the model-tier breaker, and the judge at once. Now **429** (rate-limit) / 502 (other errors). Running sidecar
  on dc1claude01 = the merge build, healthy. Killing mutation (drop the `is_error` guard → 200) reproduced
  RED by fresh-eyes review. Review follow-ups filed **TG-438** (verify+clamp `resetsAt` unit, test the 502
  branch, tighten the rate-limit prose predicate); linked to TG-426.
- **TG-436** (!1302, merge `280cf92d`) — the async graduation feed reached `ladder.Record` bypassing
  `credits.Claim` + the 0064 grounded trigger. **Fail-closed against promotion** (defense-in-depth; demotes
  still flow) + a closed-enumeration `GRADUATION-WRITER` seam guard. The route-through-Claim work is gated on a
  live async producer that has no non-test caller (dormant), so the DEFECT is closed and the residual feature
  self-announces via the seam guard + a loud canary. Running worker on dc1tg01 = merge sha.
- **TG-432** (!1303, merge `31a23256`) — cross-monitor judge-death paging was check-then-act (two worker
  PROCESSES could double-page; a fail-closed read blip could swallow the genesis page). A cross-process
  **compare-and-open** (pgx conditional-upsert CAS + MemStore twin + `TripOpen`/`HaltOpen`) unifies halt+detect,
  closing BOTH the race (finding 1) and the swallow (finding 2); + a Run()-level DRIFT test (finding 3). THREE
  killing mutations executed RED; the FULL `core/db` suite ran locally against real pgvector (`REQUIRE_DSN`,
  246 tests); `-race` + specvalidate lattice 6698/6698 + lockstep 46/46. `core/breaker` protected-path trailer
  `@ncpjfuzl`, no restamp owed. Running worker/grounder on dc1tg01 = merge sha, healthy.

**TG-421 DELIVERED (!1304, merge `18485c16`, worker on dc1tg01 = merge sha, healthy).** Over-cap authlog
enumeration now folds into ONE aggregate sweep incident (distinct-principal count + folded total) that **NAMES
the loudest principal** as the top offender — a design refinement over the ticket's bare-count proposal, so a
targeted attack inside a spray is not masked (the account `TestCapEnumerationKeepsTheLoudest` protected
survives the fold). Killing mutations RED (revert to keep-8 → 8 not 1; fold the loudest name away → RED);
fresh-eyes review confirmed no rewritten test asserts less than before.

**Batch: 5 delivered — TG-426, TG-436, TG-432, TG-433, TG-421 (+ TG-438 filed). Ledger 2 → 36 of 45
evidence-bearing closes; unresolved 124 → 94.**

**Surfaced on the owner list (scoped, NOT taken — each a substantial focused effort, not a clean AFK slice):**
- **TG-376** (cascade → one investigation subject) — highest-value in the queue (the pve03 self-DoS root; 85%
  of a storm uninvestigated), but the fix DECIDES NOT TO INVESTIGATE certain alerts (max blast radius); it
  alters core investigation behaviour → warrants eval verification while the eval plane is on the mistral
  stopgap (measured to drop safety dims) until opus-cc returns ~08-12; subject-election couples to TG-375's
  parent edges. Best done WITH eval verification post-08-12 or on explicit owner acceptance of the deferred risk.
- **TG-354** (dedup suppresses real re-fires) — PARTIALLY addressed: the production `OpenIssue` (main.go:5610)
  already fails-open; needs live-state verification of whether `IssueRef` is ever populated before any change.
- **TG-378** (proposes `start` on running VMs) — protected `core/actuate` (manifest sealing) + an estate-graph
  / live-read precondition.
- **TG-319** (litellm drops the OpenAI `user` field) — a change to the LIVE model-gateway config.

### 2026-08-10 afternoon — TG-401 (6th delivery) + safety-spine verification sweep

**TG-401 DELIVERED (!1305, merge `1f7c6b47`).** Retired the fabricated console ALERTS fixture — 8 fake
alerts wearing estate-shaped hostnames AND invented CORRELATION provenance (a grouping capability TG-376
proved TG lacks), which survived onto the live surface on a FAILED `/v1/alerts` read (TG-366 shape). Emptied
`const ALERTS` (source `console.html`, `index.html` re-assembled byte-for-byte); guard extended with the
fixture-empty test + the `al-<n>` shape rule; both killing mutations RED (reintroduce one alert with a REAL
hostname). Verified FROM THE BOX: served console (localhost:8080) carries **zero** `al-9` alert ids. Sibling
fabrications (SESSIONS fake sessions + LEDGER fake hash chain) filed **TG-439** with a live-leak-verification
nuance (those views appear already guarded on the live path, unlike ALERTS — do not blind-empty them).

**Safety-spine verification sweep (resolved-issue sweep, TG-339 precedent) — CLEAN, no false-greens.**
Read live from Prometheus on dc1tg01: every `circuit_breaker_state` (mutation / judge-death / cost /
model-primary/fast/embed) **closed** (armed, not tripped); the mode chokepoint LIVE and CONSISTENT across
both planes (`tg_policy_mode`: Semi-auto=1, Shadow/HITL/Full-auto=0 on worker AND worker-actuate — no split;
Semi-auto matches the owner-in-the-loop approve/vote flow); `tg_may_actuate` 1 on both workers, 0 on grounder
(read plane, correct); `tg_actuation_limit` armed (2–3 / 600 s); `tg_egress_enforcing`=1 ×3; `tg_host_
isolation_present`=1 ×2; `tg_recon_halted`=0; `tg_worker_halt_total`=0. The core safety controls are
genuinely armed in the running system — not inert green.

**Session total: 6 delivered (TG-426, TG-433, TG-436, TG-432, TG-421, TG-401) + TG-438/TG-439 filed. Ledger
2 → 37 evidence-bearing closes.** Owner list holds the substantial tier (TG-376/354/378/319/307/380).

### 2026-08-10 afternoon — sweep continued into autonomy/wiring/ingest + a LIVE owner-surfaced defect (TG-440/441)

**Sweep extended, still CLEAN.** Autonomy: `policy_graduation` = 3 op-classes legitimately at AUTO
(restart-service/start-container/start-service, verified_clean), 3 at APPROVE; both 0064 grounding triggers
present, 0 ungrounded credits. Wiring: `tg_wiring_seam_dark`=0 on every seam. Ingest/attribution: the TG-376
1:1 fan-out CONFIRMED live (3199 alerts → 3415 sessions); attribution discriminates correctly (attributed-
authorized → 0 heals, unattributable-failure → heals); POLL_PAUSE→mutated=18 all old (07-27..28) graduation-
proving heals, not a leak. No false-greens across safety/detection/autonomy/wiring/ingest/attribution.

**TG-440 DELIVERED (!1306, merge `0ce1e684`) — owner surfaced it LIVE** (Matrix handoff for
am-ContainerMemoryNearLimit-notrf01dmz01). A Docker/cAdvisor container alert names its container in
`name`/the compose-service label (job=*cadvisor), never in k8s container/pod — so TG rendered "Container  in
 ()" and collapsed the incident onto the HOST. `workloadSubject` now resolves it from BOTH vocabularies (bare
`name` gated behind a container signal after a review finding — an overloaded `name`, e.g. systemd
name="sshd.service", must not re-key a host alert), naming the container + scoping the ref to it. Verified on
the box: worker=merge sha. **TG-440 was blocked by an UNRELATED flaky vault test → filed + fixed TG-441**
(!1307, test-only): the standby retry tests shared http.DefaultTransport across t.Parallel(), so a sibling's
`httptest.Server.Close()`→`CloseIdleConnections()` poisoned an in-flight request under -race in CI (TG-384
"green local, red CI" shape); newTestClient now uses a private transport. -race×25 green.

**Session total: 8 delivered (…+ TG-440, TG-441) + TG-438/439/440/441 filed. Owner list unchanged
(TG-376/354/378/319/307/380). Core control spine verified live-armed, no false-greens.**

Merged worktrees removed; main tree clean on `main` at every pause. Ledger re-read after each close.

### 2026-08-10 evening — TG-443: the console's unauthenticated surface, hardened in TG code (owner-directed)

Owner escalation out of the "K. Papadopoulos in the console fixture" thread: an unauthenticated request to the
operator console was served the FULL SPA bundle (app + demo fixtures + every `/v1/*` endpoint name) behind only
a CLIENT-SIDE gate. First alarm CORRECTED — the console is INTERNAL (192.168.181.43, no public DNS; I reached
it because this session sits on the estate LAN) and the grounder API is mandatory-auth (INV-01), so no DATA
leaked — but the static shell did, and for a console that can actuate the estate the bar is an authenticating
reverse proxy: unauthenticated → a login challenge and nothing else. Fixed in TG code (owner ruled: NOT the NPM
edge — code, version-controlled + gated): `deploy/console/nginx.conf` `location /` → `auth_request` → grounder
`/v1/whoami`; unauthenticated → a minimal self-contained `login.html` (own root); `error_page` fails EVERY auth
outcome incl. a grounder blip to login, never a bare 500; hardened headers (CSP/XFO/nosniff/COOP/server_tokens).
Bundle also sanitized (a real operator name + 3 real hostnames in defect-explanation comments → `demo-`).

Verified to the DoD AND the standing Playwright-live rule: 6 static config oracles + a COMMITTED runtime
integration test (real nginx + stub grounder via `console/itest/authgate.sh`, 21/21 incl. grounder-down→login);
tg-code-reviewer MERGE after three findings fixed + independently RE-verified (it reverted the `error_page` line
to confirm the oracle is load-bearing, not decorative); console-e2e green; and LIVE Playwright against the URL
with the real `kyriakosp` login — 11/11: unauthenticated leaks nothing, a real operator reaches the app (incl.
the `/#workflows` deep-hash entry). Deployed `console:ab35be3f`, confirmed on the box. **TG-443 delivered — the
console is the only off-host TG surface, and its last unauthenticated leak is now closed.** (Also filed/closed:
TG-439 abandoned — those fixtures are oracle-load-bearing; TG-442 folded in; TG-441 flake fix.)

**TG-438 — claude-proxy 429-path polish (the 3 TG-426-review follow-ups), delivered.** retry_after clamped
(≤7d; the CLI's resetsAt unit is unverified so the clamp keeps the body honest regardless), the rate_limited
predicate gated on a DEFINITIVE non-429 api_error_status (a context-length 400 → 502 regardless of prose),
and the 502 branch now has a committed fixture + test. Fresh-eyes review reversed a DON'T-MERGE: my first cut
also required one of three exact prose phrases — which risked misrouting a real subscription-limit variant
(e.g. a "5-hour limit") with an empty status to 502, reintroducing the TG-426 amplifier; dropped it for the
status gate alone. 58 cargo tests + both killing mutations RED (reviewer independently re-verified). Merge
636648e5; deployed to dc1claude01 (deploy-sidecar's post-deploy image-confirmation flaked once — AWX
succeeded but the guard couldn't read the running image back, correctly refusing to false-green — retry
confirmed it). Proxy idle today (opus-cc rate-limited → deepseek fallback serves), so no live traffic exercises
it yet. **Session: 11 delivered incl. the console auth-gate hardening (TG-443, live-verified). Clean-mechanical
queue now exhausted; the remainder is careful-pass tier (auth-Router / protected-safety / migration / eval-gated).**

---

**2026-08-10 (cont.) — queue reconciliation + TG-142 + TG-384 (careful-pass tier).** Stale-audit: TG-403/419/418/382/435/430
already Fixed (plan-named, re-verified). TG-319's ticket hypothesis is disproven in-config (the `user`-field drop is
inside LiteLLM's openai passthrough, unreachable from `drop_params`/headers) — real investigation, de-prioritized.

**TG-142 — closed (stale-done, verified).** The tracer verify/commit sibling-row leak (action_id join shares one
first-wins row across sessions proposing the identical shape) was already resolved by the action_execution
per-occurrence work (migration 0043): the verify tail reads action_execution external_ref-scoped FIRST
(incidentAnchor-guarded pre-0043 fallback), the commit projects deterministic action content only (no timestamp).
The ticket's proposed action_verdict/action_manifest migration was superseded. Proof: DB-gated
`TestTraceWalkIsScopedToOneIncident` (+2) run green locally — incident B doesn't inherit A's verdict, C shows its own
`deviation`, A keeps its own (anti-vacuity converse).

**TG-384 — delivered ([BLOCKS pve03-class] brain self-DoS).** The model-concurrency bounds were built + unit-tested
but shipped INERT — blank compose defaults, and the prod workers carried both empty, so the 157-alert self-DoS was
still live. Shipped both armed as compose defaults, as CODE (owner: "protection must be code inside TG"):
`TG_MODEL_MAX_CONCURRENCY=8` (gateway cap, parks/never-drops) + `TG_MAX_CONCURRENT_INVESTIGATIONS=16` (durable
Temporal-queue overflow). Oracle `deploy.TestModelConcurrencyBoundsShipArmed` — RED on the empty-default killing
mutation (TG-365). tg-code-reviewer MERGE; its 2 findings fixed in-MR (stale "INERT" comments synced; the journal's
supervised-toggle rationale rebutted — the mechanism is fail-safe, parks/never-drops). Merge `4251af2d`; deployed
`worker:4251af2d`, both bounds verified armed on the box (docker inspect), and `tg_model_calls_total{ok}` growing
since the armed boot proves completions flow through the bounded gateway (the completion check the deploy health-gate
cannot do). TG-48's two token-ceiling bounds stay inert BY DESIGN — a mis-sized max_tokens TRUNCATES, so those remain
operator-armed. Lesson: a mechanism can pass every code gate yet ship disarmed by config — verify the SHIPPED default
+ the running box, not the merge.

---

**2026-08-11 — TG-388 Part 1 delivered (learned-tier decay, faces a+b); careful-pass world-model change.** The hourly
reconciliation decayed the DERIVED estate graph, but the 5-minute refresh rebuilds learned edges from the learner's
counts and OVERWROTE it (net-zero over 11 passes); age-out was structurally unreachable (graph decay `Floor=0` needs
`Confidence≤0`, never reached by halving 0.75). Fix: route the disproof INTO the learner —
`core/learn.CoOccurrenceLearner.DecayOnDisproof` shrinks the co-occurrence COUNTS (pruning below the existing 0.5
floor = age-out), so the 5-minute refresh RECOMPUTES the decayed/pruned confidence — the refresh becomes the ally.
`counts`+`delaySum` lockstep (mean preserved); `trials` untouched (sibling base-rate intact); ground-truth immune.
Oracle `TestDecayOnDisproof{PersistsThroughEdgeRecompute,AgesOutBelowFloor,EmptyIsNoop,Concurrent}` — persistence
proven through a `LearnedSource().Edges()` recompute, RED under a no-op mutation + empty-input (TG-365), `-race` clean.
tg-code-reviewer MERGE (5 design questions validated: Laplace smoothing damps the count-halving to a ~44% confidence
drop, both-directions decay is exact parity with the shipped estate tier, `trials`-untouched is right for base-rate).
Merge `0bebfb73`, deployed `worker:0bebfb73`, `runDecay` active on the box (the disproof-decay line awaits an organic
misprediction — quiet estate). Face (c) persistence — the in-memory tier a redeploy wipes — remains OPEN on TG-388;
the audit `DecayedTo` fidelity gap the review surfaced is filed as **TG-444**. Lesson:
[[decay-the-source-not-the-derived-layer]] — mutate the source the read path rebuilds from, not the derived layer.

**Session tally: 3 delivered end-to-end (TG-142 stale-close · TG-384 self-DoS bounds armed · TG-388 Part 1) + TG-444
filed.** The clean-mechanical queue is exhausted (verified); the remainder is careful-pass tier worked one item at a time.

**2026-08-11 — TG-388 CLOSED (face c: learner persistence — the last of three faces).** The co-occurrence learner
was process-local (in-memory maps, no load/save), so a redeploy wiped the whole self-learning tier (1,524 edges -> 0).
Added `core/learn.Snapshot()/Restore()` over the RAW decay-state floats (the CoOccurrences() view is lossy), migration
0077 (`co_occurrence` + `co_occurrence_host` — a MUTABLE competence-plane cache, `plane: both`, NOT the append-only
spine: the tier decays pairs OUT so Save is a DELETE-all + bulk-insert reconciliation), `core/db.CoOccurrenceStore`
(atomic Load/Save), and cmd/worker load-on-boot-before-the-Observe-feed + a 15m save ticker (`TG_LEARN_PERSIST_INTERVAL`,
armed by default; `0`/`off` genuinely disables). Oracle: `TestSnapshotRestoreRoundTrip` + `TestCoOccurrenceStoreRoundTrip`
(DB round-trip identity + a decayed-out pair DELETED on re-save). Reviewer MERGE (all 7 design Qs validated); the one Low
"`0 disables` was false" fixed in-MR. Merge `7ccc6b28`, deployed `worker:7ccc6b28`, verified live: the grounder DB carries
the tables (0077 applied), boot Load succeeded, the save loop is ARMED. Also fixed a latent `observePairs` fixture
cross-contamination trap (a stateful learner's shared `recent` window bled between seed calls — a real mysterious
failure). **TG-388 COMPLETE: the learned tier now DECAYS effectively (Part 1) AND SURVIVES restart (face c).** Audit
`DecayedTo` fidelity remains as TG-444.

**Session tally: 4 delivered end-to-end (TG-142 · TG-384 · TG-388 Part 1 · TG-388 face c = TG-388 COMPLETE) + TG-444
filed.** Working the careful-pass tier one deliberate item at a time; whenever paused the main tree is clean on `main`.

**2026-08-11 — Session start (Go! v2, owner-scoped cats 2–7): board head reconciled — TG-435 + TG-436 were already
DELIVERED.** Live-verify: deployed `worker:7ccc6b28` == last buildable main; merge gate true via authenticated API
(local witness BLIND-but-honest, TG-429); scheduled witness last green 08-09 — the 08-08/08-10 runs CANCELED at the
03:53 slot, deploy sync verified directly; stack Up/healthy; no injector present (STOPPED); ledger 436 total · 90
unresolved · 346 resolved · 36/56 evidence-bearing closes since 08-10. The board's ⚠ TG-435 head was STALE: filed
07:37 on 08-10 and fixed at 09:18 the same morning (!1298 `68c5502c`, killing oracle
`TestUnobservableSuccessIsWithheld` executed in both directions; TG-436's promotion-refusal `280cf92d` + the
closed-enumeration writer guard landed the same day) — but the board was never updated on-event: a miss of working
rule "update on-event, never batched", corrected today. Re-ranked the 90 unresolved into the dated 2026-08-11
category table (board); the owner scoped this run to categories 2–7. TG-439 verified legitimately open (re-scoped
08-10 on-ticket to leak-path verification first — SESSIONS/LEDGER fixtures appear design-mode-only, unlike ALERTS's
real leak). Filed **TG-445**: the spec/017 acceptance oracle still asserts the promotion production refuses
(`asyncverify_steps_test.go:63,182-187` vs `main.go:7003-7008`) + stale "migration 0061" comment refs
(`graduation_credit.go:25`). Queue resumes at TG-112.

**2026-08-11 — TG-112 DELIVERED end-to-end (the 4-mode vocabulary is the only vocabulary).** The ~530-reference
mutation_enabled terminology retired from every live surface in one MR (!1314, merge `7e8035d71e6a`, 138 files):
both processes' deprecated gauge alias deleted, alert renamed `ActuationPermittedWhileModeForbidsIt` (alias
OR-leg dropped), migration 0078 renames the posture column AND adds the owner-set `mode` the worker now
publishes each heartbeat, the APIs speak {mode, may_actuate} (contracts regenerated), the law-pinned config key
is safety.may_actuate, core/safety's refusal message speaks the mode model (trailer under standing authority),
the console topbar renders the real MODE chip with postureClaim() as the single label producer, helm's
advertised-no-op mutation.enabled is REMOVED with spec/009 REQ-906 rewritten bidirectionally-falsifiable, and a
NEW closed-enumeration guard (deploy/mutation_terminology_retired_test.go, 1,934 files, comment-stripping keeps
retirement history writable) executed red→green with a TG-365 anti-vacuity arm. Deploy-verified on the box:
posture rows mode=Semi-auto/may_actuate=t fresh, retired series EMPTY in prometheus, renamed alert loaded. QA
0.92 (the one real finding — an e2e oracle still driving the retired liveState field — reproduced, fixed in-MR
`db95bec8`, re-verified). Process defect caught and recorded: my "e2e green" claim rode a broken instrument (two
concurrent run.sh + stdout greps); the suite verdict is the RUNNER's exit code from ONE invocation — memoried as
[[suite-verdict-is-the-runners-exit-code]]. **TG-378 slice 1 built the same session**: guest_liveness projection
(0079, plane: both) from the pve sweep's already-fetched status field, closed-vocabulary fail-closed reader,
5 executed killing mutations, review MERGE 0.90 — !1315 merges on green, slice 2 (seal-time precondition,
spec/002 + core/predict) next.

**2026-08-11 — TG-378 DELIVERED end-to-end, both slices, same session as its filing gap was mapped.** The
pve03 defect (start sealed for guests running 897h/2,103h) is closed at the root: the estate graph stays
placement-only BY DESIGN and the power state now lives in guest_liveness (0079, projected from the SAME pve
fetch, latest-wins, absent-ages-to-unknown), read under a closed vocabulary (running/stopped; everything else
UNKNOWN), and enforced at the SEAL — `requires_target_state` declared per op-class in the registry (closed
enum, omitempty hash-stable for ratified overlays), established in PredictionGate.Commit BEFORE anything
commits (violated/unknown/unwired all refuse; no prediction row survives a refusal), the satisfying
observation recorded on the manifest (0080, outside the hashed action). Freshness bound = max(15m, 3× the
configured sweep) with a loud not-armed warning (slice-2 reviewer's Medium, fixed in-MR with its own executed
mutation). ELEVEN killing mutations executed RED across the mechanism; wire-tests pin every main.go call site
(the "implemented ≠ reachable" guard). Deployed `a959cc6e`, box-verified: 195 guests projected fresh, column
live, bound logged. QA 0.90 (slice 1) + 0.93 (slice 2). spec/002 REQ-112 + T-002-378. The execute-time
re-check + disk-op activation guard stay TG-152 (L1 AFK next; L3 owner-gated → owner list).

**2026-08-11 — TG-152-L1 DELIVERED (the symmetric execute-time re-check) + TG-146 narrowed + TG-446 filed.**
!1317 `b11b405d`: awx-launch classes now re-validate required params at the interceptor's structure-schema
gate (their params ride extra_vars, outside the argv the len==0 arm watches); the reviewer independently
re-executed the killing mutation and watched the pre-fix request REACH THE LEAF (execs=1) — the recorded
asymmetry, closed. QA 0.94; deployed and witness-certified (deployed=main 3/3 via AWX ad-hoc). TG-146's C3
verified STALE-FIXED on-ticket (TG-182's observedOK + REQ-1223's terminus-only promotion already closed it;
file:line on the ticket), narrowing the basket to S2+A3 (AFK) with S3/S4/S6 owner-gated. The scheduled
witness pipeline surfaced two estate conditions: TG-429's blind half now hard-reds the schedule (owner), and
TG-446 — dc1ghostfolio01, allowlisted for actuation, produces NO AWX probe line while RUNNING (verified
against guest_liveness: the TG-378 projection answering its first operational question hours after landing).
Session tally: board reconciliation + TG-112 + TG-378 (both slices) + TG-152-L1 delivered end-to-end; TG-445
+ TG-446 filed; ledger 89 unresolved · 348 resolved.

**2026-08-11 — TG-82 design-first slice delivered: spec/029 commit-confirmed actuation, drafted and merged
awaiting sign-off.** The Junos inversion over Temporal (armed revert as the default outcome; only the
mechanical terminus confirms — verdict==match AND the TG-182 observedOK bit, the coherence review's key
catch: the 3-value verdict alone folds an empty observation into the quiet match, so the bit is
load-bearing). Eight EARS REQs, five pending tasks, honest NO-ORACLE mapping, threat model; a
MERGE-with-fixes @0.80 coherence review resolved in-MR before the owner reads it (the unverifiable→revert
decision now lives in ONE marked place; the revert-failed → page+breaker worst case gained its scenario).
!1318 `78cf5076`, index status Draft, lattice 29 specs/337 tasks/539 scenarios. Sign-off + four flagged
questions on the owner list; the build stays gated on ratification. Queue continues at TG-146 (A3 + S2).
(Process note: this entry's first commit attempt ran in the WORKTREE on the merged spec branch by cwd
mistake — nothing wrong pushed; stranded commit dropped; redone here in the main tree.)

**2026-08-11 — TG-146-A3 second half DELIVERED (adapter floor sees the params) + basket closed to the owner set.**
!1319 `1ff054c9`, deploy-verified green: manifest.Action.SafetyParts() is the ONE shared derivation both
floor depths consume (classify-time runner delegates to it), the never-auto floor derives destructive from
the full parts, and a NEW band-aware stateful floor refuses a stateful identity under a non-voted band (the
voted POLL_PAUSE lane passes — defense-in-depth over graduation, reviewer-verified). The decision tracer's
closed enumeration caught the new gate row (15-vs-14) exactly as designed and now speaks the enlarged walk;
the gate-drill matrix drills it. Four oracles + two executed killing mutations; QA 0.93. TG-146 basket now:
A3 ✓ · S2 ✓ (verified stale-fixed, own executed suite) · C3 ✓ (verified stale-fixed) · A4 ✓; the remainder
(S3/S4 ladder CAS, S6 corpus tamper-resistance, C1 verify-ordering) are owner/deferred, on the ticket + board
owner list. Two process slips caught and corrected on the record this stretch: a wrong-predecessor `&&` that
let a red gate push (twice — memoried as the exit-code lesson), and a lockstep restamp that passed on a stray
EMPTY file (redone against the real spec note). TG-348 loop-3 (first-ever graduation_credit) is being
exercised live now — bounded guinea-pig injection on librespeed01, dead-man restore armed.

**2026-08-11 — TG-444 DELIVERED (edge_disproof.decayed_to relabelled as a decay-time snapshot) + TG-348 loop-3
exercised.** !1320 `8c3cfee3`: migration 0081 + the Go struct doc make clear decayed_to is a decay-EVENT
snapshot superseded by the TG-388 source recompute, never live confidence — zero live readers today. DB-gated
col_description oracle, killing mutation RED, QA 0.95. TG-348 loop-3 exercised live: NO credit, two honest
findings (service heal correctly fail-closed on an un-SSH-able guest; guest-down trigger MUTED at LibreNMS) —
the credit ledger's emptiness is now attempt-backed. Estate restored + verified. Next: TG-380 decision-stage
observability. (Process note: the board's first commit attempt again ran in the tg378 WORKTREE by cwd slip —
stranded on the deleted tg444 branch, nothing wrong pushed; redone here in the main tree.)

**2026-08-11 — TG-380 slice 1 DELIVERED (the decision-stage triple — TG can now be WATCHED, not only autopsied).**
!1321 `f790590a`: tg_stage_{offered,eligible,acted}_total{stage} (offered≥eligible≥acted by construction, the
denominator always beside the numerator) + core/observe.StageTally + suppress wired on the /metrics scrape
surface. The load-bearing part is the producer-scan guard that EXERCISES each declared stage's real decision
and asserts the tally moved (a grep passes on dead code — this proves live wiring, the "implemented≠reachable"
discipline applied to an instrument); both the ticket's killing mutations executed RED. QA 0.93; two review
Lows folded (eligible-granularity comment + slice-2 flags). Frontier named honestly in PendingDecisionStages:
classify/correlate/predict/gate/breaker (each touches a runner/eval surface) + tg_ingest_predrop_total. Ticket
stays OPEN for slices 2+. Ninth item delivered end-to-end this session; main tree clean on main throughout.

**2026-08-11 — TG-380 slice 2 DELIVERED (the pre-admission drop counter).** !1322 `916b8fa0`:
tg_ingest_predrop_total{reason} counts alerts the front door ACCEPTS but that mint no new triage —
recovery_transition (exercised end-to-end via the new Deps.IngestPredrop seam) + reject_duplicate
(StartTriage re-fire, source-scan-pinned per the refusals precedent), mirroring the TG-371 counter. Three
killing mutations RED; QA 0.94. TG-380 now has both halves of the standing instrument (the decision-STAGE
triple, live-proven on the box in slice 1, + this pre-admission-drop counter); the remaining stages
(classify/correlate/predict/gate/breaker) stay in PendingDecisionStages, each on a runner/eval surface.
Tenth item delivered end-to-end this session. Both working trees clean on main throughout.

**2026-08-11 — TG-191 DELIVERED (the MISSION GUARDRAIL as an auditable number — G6 loop-bypass).** Epic
TG-187's tripwire that keeps the Hands/Proof mission from eroding the falsifiable core while chasing A5/A3
breadth. An executed heal that acted with no committed prediction, or whose outcome core/verify never graded,
bought breadth by skipping the prediction→verify loop — drift. Two slices:
- **Slice 1** (!1323 `96702fbf`): `core/db.AxisReadStore.LoopBypass` + the `cmd/axisscore` G6 line
  "loop-bypassing heals: N (must be 0)". Counts `action_execution` rows with no committed
  `infragraph_prediction` (EXISTS semi-join by the INV-07 `action_id`) OR no per-execution `core/verify`
  grade, split into both limbs. The load-bearing design call: grade off the PER-EXECUTION
  `action_execution.verdict`, NOT the first-wins `action_verdict` shape row — migration 0043 exists because a
  recycled shape inherits its first verdict forever, so an ungraded re-execution must be judged on its own
  NULL verdict. Oracle real-Postgres, killing mutation (OR→AND collapses Bypassing 2→0) executed RED. spec/025
  REQ-2530 + T-025-28, lockstep restamped, tally regen. Review **0.90**.
- **Slice 2** (!1324 `69d61ed8`): published the guardrail as a CONTINUOUS worker metric —
  `tg_axis_loop_bypass_{executed,total,no_prediction,no_verdict}_total` from the axis sampler, fail-quiet under
  its own gate, absent-is-not-zero. This is REQ-2524 (a measurement reachable only by running a command is not
  a measurement of a running system) applied to G6 — the same defect TG-192 fixed for falsifiability. Slice-1
  shipped it CLI-only; this makes it reachable. Review **0.93**, all three correctness questions
  mutation-verified by the reviewer.

**Real-data verification** (prod `grounder` DB, read-only): all-time `executed=460, null_verdict=0,
predictions=2054` → NoVerdict structurally 0; combined with the interceptor's structural gate (execution
refused when `!r.Gated` — no committed prediction — BEFORE `action_execution` is written) → NoPrediction 0 →
**Bypassing = 0 on the live estate**: the join is meaningful and there is no loop-bypass drift. The interceptor
gate is WHY the guardrail reads clean, not luck. One latent future gap (an inverse/rollback execution sealed
outside `PredictionGate.Commit` would false-flag NoPrediction; 0 inverses have ever run) filed as **TG-448**.
**LIVE-VERIFIED + RESOLVED:** worker redeployed to `worker:c2c2c6ed`; the sampler publishes all four
`tg_axis_loop_bypass_*` series on /metrics (all 0 — the 168h window has 0 executions; the series are PRESENT,
`executed_total=0` beside `total=0` = "nothing to audit", not a broken read). TG-191 State→Fixed. Eleventh
item delivered end-to-end this session; main tree clean on main throughout.

**2026-08-11 — TG-394 slice 1 DELIVERED (TG's self-dependency placement concentration — the blind spot that hid
7 of 26 deps on one node).** !1325 (`a49cea1c`, merged on green). TG held no inventory of where its own
dependency hosts run, and nothing reported the concentration when 7 of 26 sat on the one hypervisor it was
diagnosing — silently degrading retrieval to lexical-only for 11h12m. This makes the single-point-of-failure
risk knowable at BOOT:
- `core/estate.(*Graph).InfraParentGroups(hostNames)` — groups hosts by their best-confidence `runs_on` parent
  (filtered to `siblingParentEligible` cascading-infra types); a group with 2+ is a concentration. Only runs_on
  is a common cause (a shared site is co-location, not co-failure); an unresolvable/expired-placement host is
  OMITTED, not assumed safe.
- `cmd/worker` publishes `tg_self_dependency_concentration{parent,capability}` from the LIVE estate graph
  (scrape-path read via the holder — no DB, no sampler interval), with the always-emitted coverage pair
  `tg_self_dependency_{globs_declared,hosts_resolved}`. Slice 1 covers the journal-evidence capability (TG's 14
  declared SSH journal hosts, resolved from TG_JOURNAL_DEPLOYMENTS globs against the estate — NO estate
  identifiers compiled in; the STONITH gate caught one hostname literal in a comment, corrected). `alert.rules.yml`:
  `SelfDependencyConcentration` (>=2) + `SelfDependencyConcentrationUnmeasured` (absent-heartbeat); a deploy
  source-scan guard pins metric+rules shipping together.
- **Review 0.82→0.93 across two passes.** The first pass caught a REAL defect in my own coverage claim:
  `hosts_resolved` counted stale once-seen nodes (`Export()` existence) while the concentration used fresh
  `Parents()` — a false "live placement" number during exactly the source-goes-quiet failure this catches. Did
  NOT merge at 0.82; fixed `hosts_resolved` to derive from the same fresh `InfraParentGroups` grouping, plus two
  hardening fixes (infra-type parent filter, PVE-node exclusion doc), each with a mutation-verified oracle → 0.93.
  The broader root cause (`Export().Nodes` counts stale entities, so `tg_estate_nodes` overstates too) is filed
  as **TG-449**. QA 0.93; make all green, -race clean.

Twelfth item delivered end-to-end this session (the second full feature this continuation, after TG-191).
TG-394 stays OPEN for slices 2+ (other capabilities). Deploy in flight at journal time (worker redeploy → the
scrape-path metric appears; watcher armed). Also this continuation: **TG-448** filed (TG-191 inverse/rollback
latent gap), **TG-449** filed (Export freshness); **TG-313** + **TG-414** scoped+surfaced to the owner list.
Main tree clean on main throughout.

## 2026-08-12 — TG-348 leg (c): service-fault auto-heal is COLD behind 3 stacked defects
Driving leg (c) (first `graduation_credit`) on librespeed01 exposed three independent fail-closed gates, each masking the next:
- **TG-450 (FIXED + proven):** hostdiag SSH key rotated in bao but not on the guests → auth-reject → classify POLL_PAUSE with no evidence. Fix: appended the current tg-hostdiag pubkey to librespeed01 + myspeed01 authorized_keys (reversible, `.tg450.bak`). Proven `HOSTDIAG_AUTH_OK` + a rich grounded classify diagnosis. This is the item the owner challenged as "mine" — it was, and it's done.
- **TG-453 (FIXED-mitigated + proven):** with hostdiag able to log in, its OWN classify-SSH read `attributed-suspicious` (the `journal` domain has no self-actor; `SelfActors` is derived from the ACTUATION key, plane-scoped) → security-escalate → blocked the heal even after an operator approve (`Vote=approved, Mutated=false`). Mitigated by adding the hostdiag fingerprint to the pool-scoped `shadowbench-pool-ssh` carve-out → attribution flipped to `authorized-test` (proven on re-inject). Proper fix is a design pass (exclude investigator reads vs. a reader-self class) — TG-453.
- **TG-454 (FILED, turnkey fix on ticket):** even authorized-test + approved, `ExecuteActivity`'s necessity-probe (`ClearObserve` = the LibreNMS active-alert pull, which is OFF/push-only) fail-closes → `Mutated=false`, silently, never dispatched to tg.actuate. Fix: fall back to TG's own open-incident ledger (`OpenIncidents`), mirroring the clear-direction belt. Blocks the first `graduation_credit`. Eval-gated (edits temporal/runner/activities.go) → supervised.
- Also proven: **`vote.inbound` is LIVE** — the owner's two Matrix approves were admitted end-to-end (`human:approve voter=@dominicus:matrix`, ledger seq 10486/10493); the old TG-251 "dark" claim is stale. **TG-230** verify-and-closed (delivered via TG-227/spec-028; residual REQ-2818 `[O]` on the owner list). Two estate mutations ledgered (guinea-pig authorized_keys, attribution carve-out), guests restored healthy.

## 2026-08-12 (cont.) — Go! run: TG-446 fixed, TG-447 deleted, TG-453 DELIVERED (product-codebase security P0)

Owner ruling this session: the **product-codebase-security** ticket set is **P0 for delivering**;
**TG-447 deleted** (owner instruction — its read-only diagnostics left nothing to undo; board line removed,
ticket 404'd). Diagnostic kept for whoever provisions runner02: `ghcr.io/berriai/litellm` is PUBLIC and
neither runner carries a ghcr secret — runner02's anonymous pull fails for an environmental reason on pve04
(unreachable to me), not a missing credential (the ticket's premise was wrong).

- **TG-446** — root-caused + fixed: ghostfolio01 disk 100% full (16.79GB stale watchtower images) → Ansible
  `No space left` → AWX UNREACHABLE → guard-coverage witness RED. Freed 17.56GB (disk 100%→19%); containers
  recovered unhealthy→healthy. `WATCHTOWER_CLEANUP=true` is already set yet images accumulated (durability
  caveat noted). State Fixed.
- **TG-453** — proper reader-self attribution recognition DELIVERED (!1349, merged+deployed). New
  `SelfReaders` identity class in core/attribution: TG's hostdiag classify-SSH login (its OWN diagnostic
  access during triage) no longer reads `attributed-suspicious`/security-escalate → no longer blocks a
  legitimately-approved heal. Derived from the reader CREDENTIAL (key-not-token, plane-scoped), recognised
  before carve-out/sanctioned, mints no candidate; REQ-2304 intact (a real intruder still dominates). Verified
  by composition: boot-log ARMED (`recognised 1 … investigation identit(ies)` from the real bao: hostdiag key)
  + the 12:52 session's live login-match (`root!SHA256:Dc2a…`) + oracle/round-trip/killing-mutation tests +
  review 0.95. Direct re-injection observation attempted twice, inconclusive (LibreNMS detection/flap
  mechanics — documented on the ticket, not a fix issue). Supersedes the on-box carve-out mitigation (left as
  a redundant safety net). **TG-457** filed for the syslogng-reader scope gap (review-flagged, out of TG-453 scope).

**Next P0 (queued):** TG-105 policy packet-tracer. Recon done — it's a cross-process build: `/v1/policy/*` is
served by cmd/grounder (read-only gateway) but the policy Engine lives only in cmd/worker, so a faithful
tracer needs a worker-side `/v1/policy/trace` (real `Engine.Decide`) + a grounder→worker relay + the console
UI (deferred placeholder at policy/js.txt:287). `Engine.Decide` writes a policy_decision audit row per call —
a tracer must mark trace evals distinctly. Multi-slice.

## 2026-08-12 (cont.) — TG-105 slice 1 (policy packet-tracer BACKEND) delivered + verified live

The Policy console's packet-tracer had no worker-side evaluation endpoint (deferred placeholder). Slice 1 builds
it FAITHFULLY: grounder POST /v1/policy/trace → existing Temporal channel → worker PolicyTraceWorkflow → the REAL
Engine.Decide → result. No grounder-side engine (a second policy that could disagree with the live interceptor),
no new channel. Decider is a SEPARATE bare Engine (a review caught that reusing policyEng would alias the stateful
rate governor — WithRateGovernor/WithGraduation mutate the receiver); read-only, no audit row, no rate-budget use,
honestly flagged (RateGovernorSimulated=false + a Reason note). Built via a fresh-context subagent (spec bug caught
by it), independently verified, review 0.93, merged !1350.

LIVE e2e: route deployed (POST 401, parity with /v1/policy/rules, not 404); a direct Temporal trigger ran the REAL
Engine → {Verdict:approve, Band:AUTO, real BundleVersion sha256:32494c2f, multi-stage Reason + honesty note}; 0
policy_decision rows written (no audit pollution) — all verified on the box. Slice 2 = the console UI (Playwright/
local-e2e), TG-105 left OPEN for it.

## 2026-08-12 (cont.) — TG-105 COMPLETE (policy packet-tracer, backend + console)

Slice 2 (console UI) delivered: the deferred placeholder → the live tracer (form → POST /api/v1/policy/trace →
verdict + reason + the mandatory rate-governor caveat). Built via a fresh-context subagent, reviewed 0.95, merged
!1351. Honest (verdict only from a 200, no fabricated verdict on any error path, caveat unconditional); mode
defaults to the live mode not Shadow (a review fix, proven on the wire). CI console-e2e GREEN (the pipeline's
browser gate ran the new policy-tracer.mjs oracle); console-drift byte-clean. Deployed: the console container
serves the 1.13MB bundle with polRunTrace/polTraceForm/v1/policy/trace, old placeholder gone.

TG-105 COMPLETE end to end (backend !1350 + console !1351): a faithful, read-only, honest policy packet-tracer
over the ONE worker engine the interceptor consults. Architecture lesson recorded (worker-side trace over the
existing Temporal channel; the WithGraduation/WithRateGovernor receiver-mutation gotcha).

## 2026-08-12 (cont.) — TG-106 DONE (policy console mode selector, safety-critical)

The disabled "Change mode…" placeholder → a live, safe mode-transition UI over the EXISTING chokepoint-bound
backend (POST /v1/mode, AuthAdminSession; operator+adminAuthorized from the principal, ZERO Go changes). Admin-
elevation reused (cfgElevate step-up on 401); RED double-warn on Full-auto (the only allow-all, grounded in
core/policy/mode.go), WARN not BLOCK; 409 stale-CAS "changed underneath you", no silent overwrite; success only
on 200. Built via a fresh-context subagent, SAFETY review 0.97, merged !1352 (after fixing a tracked-symlink
slip that reddened build-test — see the console-e2e memory). CI build-test + console-e2e green; deployed (console
bundle serves polModeSubmit/POST v1/mode). Did NOT trigger a real mode change (a live governance write the owner
performs). TG-104 (rule editor) remains — its slice-1 backend (rulesetwrite workflow) is building now.

## 2026-08-13 — Go! continuation (Max-AFK + surface-rest, then owner-pushed into the substantial tier). 13 delivered, deployed live grounder `0d0d1ce4`.

The owner pushed twice against over-gating ("NOT WHAT YOU PROMISED"), so this run drove the tractable-but-not-owner-class tier, not just the quick wins. Every item: delegate-to-fresh-subagent → independent verify → fresh-eyes review ≥0.90 (safety-emphasis on actuation/security) → merge-serialized-on-the-deploy → deploy-verify → close. Two safety reviews CAUGHT REAL BUGS pre-merge (below).

**DELIVERED + DEPLOYED + closed (13):**
- **TG-249 item 5** — #modules live facets filter honestly (console `88450f47`, 0.97).
- **TG-458** — migration 0083 backfills the canonical site vocabulary into ~2,400 legacy rows across 9 tables (grounder `62c68dc3`, 0.94); the append-only tables update under the tg_migration role.
- **TG-454** — necessity probe falls back to the open-incident ledger on a live read-error: **un-inerts the service-fault auto-heal plane** (3rd/final stacked blocker after TG-450/453). worker-actuate `4e65dc4e`, safety 0.95, eval-waived. Consequential/safety — surfaced.
- **TG-459** — dedup restores short-recency suppression ONLY when no tracker is wired (grounder `5f30f295`, 0.92). **CI harness caught a TG-354 regression** in the first cut (recency-suppressed a confirmed-RESOLVED re-fire); held on red, narrowed to OpenIssue==nil, re-reviewed.
- **TG-460** — non-guest reachability requires an authoritative-source edge; learned-only hosts excluded not degraded (grounder `0352fb5c`, 0.95), one classification pass so numerator=denominator (no TG-449).
- **TG-429** — self-served the read-only CI token + masked `TG_READONLY_API_TOKEN` (I'm Owner) — the merge-gate witness un-blinds.
- **TG-35** — wired the missing console favicon (the header already carried the icon; the ticket's premise was stale). console `1d9d4a9f`.
- **TG-102** — read-only LDAP/OIDC approver federation behind TG-101's PrincipalResolver, TTL-cached, fail-closed, injection-safe (grounder `9970ec78`, 0.95). Protected `core/policy/federated.go` AVOIDED by living in non-protected `modules/policyident/`. Capability, not live-wired → **TG-463** filed.
- **TG-387** — ingest-time reconciler joins recoveries back to open sessions+proposals on (host, rule-family) (grounder `95521b6d`, 0.95, migration 0084). Safe: the recovery INSERT autocommits before the fire-and-forget reconciler.
- **TG-462** — operator manual rollback: `POST /v1/actions/{id}/rollback` + RollbackWorkflow over InvertsActionID, every gate, fail-closed at necessity, reversible-only (grounder `0f734c14`, safety 0.95 after the review CAUGHT two real bugs: a `start-guest` rollback that re-ran `start` and lied "executed", + a dead InversesOf idempotency check). Inert until armed → **TG-464**.
- **TG-146 S2** — LibreNMS reader under-confirms on a cap-filled page (worker `489f1135`, 0.91). Also proved the PRIOR merged S2 fix VACUOUS (its `got!=count` guard can never fire — LibreNMS returns count≡len). **TG-146 C3 resolved as superseded** (verified against current code — the immediate observation never promotes per REQ-1223; the terminus requires ConfirmedClear).
- **TG-385 + TG-376** — durable cluster identity + causal election + collapse a cascade to ONE session (grounder `f29d9dc8`, safety 0.97, eval-waived). Fixes the pve03 157→157 fan-out. The review CAUGHT a silence-risk (earliest-ref collapse could silence a real incident); added a guard: collapse ONLY on a CAUSAL election (in-degree/parent-fanout) — time-coincidence never silences. → **TG-465**.
- **TG-407** — REQ-2304 half 2: a covered-but-empty audit read on an OBSERVED MUTATION is attributed-suspicious (grounder `0d0d1ce4`, security 0.97). Fail-safe by construction (all ~28 callers delegate a zero Observation → escalation unreachable). Observe-only until a grounded mutation-source is wired → **TG-466**. Verified live it added ZERO new suspicious (the 1 pre-existing row predates the deploy by 27h — a half-1 actor lane).

**SURFACED with determinations (owner list / scoping):** TG-233 (e2e fixed-sleeps, 151 sites, slice plan), TG-32 (OTLP, unstarted feature), TG-109 (credential UX, owner-first-class P1), TG-313 (Temporal timeout = host-resource decision), TG-146 S3/S4/S6 (owner-gated), TG-461 (actuate-plane FetchActive read-error).

**FILED (follow-ups):** TG-461..TG-466.

**OWNER-DECISIONS still open (the throughput unlock):** TG-82 (auto-revert mutation-safety epic — I explained WHY; owner sign-off per the ticket), TG-122 (GitOps-MR + k8s lanes — owner confirmed the live cluster; I recommend build), TG-180 (estate fault-injection — safety call). Plus the ~19 eval-gate agent-behavior items (TG-38 R-series) needing a supervised on-box eval session.

**Process:** two fresh-eyes safety reviews caught real actuation/silence bugs BEFORE merge (TG-462, TG-385+376) — the rigor is load-bearing, not ceremony. The full CI harness caught a flake (TestAppendRecoversOnRetryAfterTransientFailure, 3s-retry) unrelated to the diff — re-ran green. "Verify live state" caught my imprecise "reads 0" on TG-407 (it was 1, pre-existing).

## 2026-08-13 — speculative-tail disposition run (owner table + Go!; plan approved)

Owner mandate: the five speculative sub-categories (the category-15 EPIC containers). Dispositions
grounded in two full repo sweeps + live-box queries, not the epics' own stale text.

- **TG-132 CLOSED (State Duplicate → TG-128)** — same federation epic twice; the "QUEUED after
  TG-130" condition is met and its "PENDING owner: GitHub repo/CI vars/DOI" block was stale (done
  2026-07-21, DOI 10.5281/zenodo.21466523).
- **TG-130 evidence roll-up posted** — 11/12 children resolved with live proofs (start-guest
  auto-graduation 07-22 · cross-alias hands-off heal 07-22 · the novelty→precedent→autonomy loop
  closing inside 45 minutes on 07-28). Epic close awaits only the TG-146 ruling (owner list).
- **Key verified fact: the skill flywheel GRADUATED an artifact 2026-07-23** — `skill_trial`
  `triage-protocol` finalized `completed`, candidate won (Welch 4.324 vs 3.857, p=0.0033,
  winner_version_id=21). FEDERATION-VISION §7's "has never happened yet" stands corrected in this
  MR; the spec/021 un-defer ratification is now an owner-list item with the full evidence (TG-128).
- **TG-114 DECOMPOSED → TG-470..TG-479** — ten oracle-bearing children mirroring the C-1..C-10 MR
  plan. Store decision (design-verified): metadata-only `artifact_class` column on the EXISTING
  skill tables, live trials preserved; safety/screen prose never enters the DB.
- **spec/016 lattice reconciled (!1379)** — eight tasks flipped to completed on bound green
  scenarios (the tasks.json under-reported ~17k LOC of shipped engine); a phantom `files_owned`
  path was caught by the TG-416 ratchet and repointed at the content-verified file;
  `_test_mapping.json`'s "Nothing is built yet" note contradicted its own 18-of-29-present data.
  T-016-5/11/12/13 stay honestly pending (the REQ-1617 bindings ride the TG-109 train).
- **This MR** — the 2026-08-13 standing scope on the board · owner-list additions (TG-146,
  spec/021 un-defer, TG-129 positioning, LDAP-source arming, C-6 arming + `behavior_re` gap) ·
  ADR index gains its missing rows 0009/0013–0016 · TGCTL vision renumbered spec/022→spec/030
  (022 taken by credential-delivery).

## 2026-08-14 — speculative-tail run, wave 2 (B/C phases; overnight into morning)

**Merged + deployed (each verified via `scripts/verify-pipeline.sh` or the armed merge gate):**
- **!1381 + !1382 — TG-188 completed and CLOSED** (delivery bar v1.1 on-ticket): chaos-measured
  ExpectedAlerts under the winning-provenance ratchet; organic recovery learning (onset→clear pairing,
  learned-edge MTTR, migration 0086, watermarked clear feed). Fresh-eyes QA: first pass 0.72 with a
  CONFIRMED tied-timestamp/cap-boundary data-loss finding — fixed with a `(received_at, id)` cursor + a
  cap+52-identical-timestamp reproduction oracle before merge; re-verdict **0.93 both diffs**. The
  review process caught a real defect pre-merge; that is what it is for.
- **!1383 — go1.25.13 toolchain directive**: seven stdlib advisories published against the pinned
  golang:1.25.12 image mid-run and redded EVERY pipeline (the 1.25.13 image did not exist yet — the vuln
  DB leads Docker Hub). go.mod's `toolchain` directive unblocked the whole queue on the existing images;
  verified locally with govulncheck at 0 reachable vulns. Trailer per the gate's own recorded hygiene loop.
- **!1384 — TG-109 backend**: the credential "Sync now" temporal lane + precedence published into the
  coverage projection (migration 0087), ending the read surface's honest omission.
- **!1385 — TG-480 backend**: the axis scorecard extracted VERBATIM to core/axis (lockstep hash-bind
  FOLLOWED the computation into its new home — extracting without re-binding would have unlocked the
  measurement plane's definitions) + authenticated GET /v1/axes serving the same bytes the CLI prints.
- **!1386 — TG-471 (C-2)**: the 8 seed skill bodies → embedded markdown, byte-identity golden-pinned;
  the golden caught a REAL mangling during extraction (the debugging-protocol backtick concatenation).
- **!1387 — TG-472 (C-3a)**: the base prompt → embedded markdown, protocol/guidance split, goldens over
  fixed inputs incl. both empty-input paths. Both externalizations carry Eval-Gate-Waived-By with their
  goldens as the evidence, per the owner-approved plan.

**Tracker:** TG-188 CLOSED · TG-480 (axes surface) + TG-481 (object-groups carve — no group model exists
anywhere; membership is sync-derived) FILED · TG-109 re-scoped on-ticket (config is ~90% shipped as
`module.credsource.*` keys; surface 4 carved to TG-481) · **TG-463 analysis posted**: the sketched
live-resolver wiring is unsound against the frozen-at-gate determinism ruling; two resolutions
(retire the dead MayApprove/policyident lane vs repurpose as alias normalization) put to the owner —
supervised class, not taken unattended.

**In flight at write time:** nativedb per-target mapping MR (TG-109), TG-470/C-1 artifact-class MR
(law trailer on ADR-0017), the axes console module (TG-480). TG-187 closes when TG-480 lands.

**Ops:** the 03:40 nightly reset passed clean (all work in worktrees); deploy box disk 35% after six
deploys; worker:7e1c1fbb verified == main tip mid-run, containers healthy. One near-miss recorded for
process honesty: a relative-path `git worktree add` + drifting cwd briefly dirtied the MAIN tree with an
extraction (caught, transplanted to the worktree, main restored clean, absolute paths since).

## 2026-08-14 — speculative-tail run, wave 4 (owner "Go!" ×2; the C-train close-out + two owner rulings)

**Nine closes, full bars:** TG-107+TG-109 (credential-engine epic done — sub-category 1e; object-groups
carved to TG-481; sync-now rig QA finding fixed same-day in !1396) · TG-470..476 (C-1..C-7). Batched
fresh-eyes QA over the six C-train MRs: 0.93/0.94/0.93/0.90/0.95/0.90; TG-476 0.91; TG-482 0.94;
TG-477 0.93; TG-478/479 0.93. Ledger at wave end: 473 total · 66 unresolved after the 476/474 closes.

**The compose leak (found + sealed same day).** C-7's pre-build verification proved the composer had NO
class filter: the C-5 boot-seeded judge-rubric mirror composed into every production session's guidance
from the ~09:22 deploy until !1398 (53bdae71) — the REQ-1305 pin rule only guards compiled-name matches,
and a then-green test (TestComposeCapIsPerClass) pinned the leak as intended. Sealed by the class filter
with red-first oracles driving the real chain; deployed in `d47ad52c` (ancestry verified). Window recorded
on TG-474. Lesson → memory: a stored row's law lives in its CONSUMER; writer + consumer-law ship same-MR.

**Owner ruling 1 — no invented sign-off gates.** !1399's by-design eval-evidence red (skills/ in
behavior_re, waiver-at-merge) was ruled "a feature nobody asked for — stupid". !1400 (law trailer citing
the ruling) removed `skills/` from the behavior set: the eval gate binds LOADED surfaces only; prose
becomes behavior at the seeding/graduation rail, which is where evals bite. Drill extended red-first;
agent/ still refuses without evidence. Distilled-content review is AT LEISURE post-merge, never a gate.

**Owner ruling 2 (implicit, honored):** salvage-don't-rebuild — the batch-2/3 agent died mid-manifest on
a connection error; resumed from its surviving worktree, delivered 9c0a77bb (67 sources: 32 distilled
12-skill/20-runbook · 10 merged · 25 skipped, zero prompt/rubric).

**Also:** console-e2e narrow-viewport rail race fixed (!1397 — settle-before-measure; A/B/C proof with a
slowed transition; falsifiability preserved; evidence on TG-233, systemic sweep stays open) · TG-482
axes-payload e2e (67 label-bound checks, two-cell red-proof) built + armed (!1401) · eval-gate drill
10/10 arms · stale worktrees pruned to live set · memory: store-row-law-lives-in-the-consumer,
no-invented-signoff-gates-eval-binds-behavior-only.

### 2026-08-14 — The autonomy boundary + full owner-list unblock (TG-488; interactive owner session)

The owner ruled that sessions were manufacturing artificial "owner decision" blockers and sat a full
boundary + unblocking Q&A. Recorded verbatim on TG-488: the boundary (decide-do-record default; reserved
R1–R7; [R#] tags + lint + retro-application) and 26 unblocking rulings. Applied same session — closes:
TG-30 (build-nothing ack) · TG-37 (superseded) · TG-128/129/32 (deferred beyond v1). Re-scopes: TG-91
(Slurpit EXISTS per owner, overriding the 08-12 research; vSphere via a nested test VM; NetDisco/SuzieQ
dropped) · TG-74 (switch-port bounce via netmiko/paramiko FIRST; wide partitions on fresh ask) · TG-439
(leak-path oracle only). spec/029 RATIFIED: unverifiable→HOLD+page (REQ-2902 amended at sign-off) ·
awx classes ELIGIBLE with windows past the deferred-verify bound · v1 classes restart+reload+start-guest ·
TG-146-S3/S4 before threshold>1 demotions. Armings granted: TG-464 (manual rollback live) · TG-466/407 ·
TG-114 C-6 gated (AFTER TG-489 hash-chain; behavior_re widening + carried-trailer batch-ratify approved) ·
TG-463 (owner identities only) · TG-315 (authlog = the guinea-pig pool, shadow-first) · TG-348 (session
drives all four loops as operator-admin, journaled). Estate grants: TG-422 Bao db engine cutover · TG-423
SSH-CA pool-first then 26 hosts after a clean week · TG-420 litellm fenced to sidecar+ollama+z.ai (z.ai
KEPT, owner funds) · hostdiag reader key to Tier-B/real hosts INCLUDING pve04 · secret/tg/hosts populate ·
a real security-telemetry sender. Tier-3 = pool at session discretion (rule 6 stands). TG-58 builds NOW in
parallel. Campaign #2 strictly LAST. TG-122 APPROVED (session creates target repo + Developer bot token).
Owner-knowledge delegations: the 227 retrieval labels (TG-491, session investigates the estate and labels),
the 33 distilled bodies (session reviews NOW as delegated reviewer of record; verdicts on TG-477/478/479),
WAN-provider name → public-mirror denylist. New tickets this date: TG-484 · TG-485 · TG-488..491.
Session-claimed under the boundary: TG-461(c) · TG-313(1+2 swap/reservation) · TG-168 litellm→gpu01 ·
TG-180 cadence · TG_LESSONS_SOURCE_FILE wiring · k8s apiserver audit · TG-483 · production-query-set
landing. The pre-boundary "Not our backlog — owner decisions" board section moved here verbatim below.

#### Moved verbatim from docs/BOARD.md § "Not our backlog — owner decisions" on 2026-08-14 (superseded by TG-488)

## Not our backlog — owner decisions

**Wave-5 additions (2026-08-14, rows 3–8 research; priced/evidenced on-ticket, never blocking):**
- **TG-461 — the TG-337 safety tradeoff.** The actuate plane's LibreNMS token is alert-blind BY DESIGN, so
  the heal necessity-probe AND the post-state verifier read 403 → every heal executes (TG-454 belt) but is
  UNVERIFIABLE, no graduation credit. Three priced options on-ticket: (a) alert-capable token ~0–100 LOC
  (reverses TG-337); (b) a second scoped alerts-probe var ~100–200; (c) verify off TG's own durable surfaces
  ~300–500 (TG-337-clean, self-referential). Recommend (c) or (b). Unlocks verification + credit for all heals.
- **TG-483 (carved from TG-146 C1)** — collateral-cascade verdict frozen at execute time; a sibling cascade
  in the settle window escapes deviation detection. Async-verify ordering fix, protected path, co-designed
  with spec/029/TG-82. Supervised.
- **TG-315 arming** — the authlog connector is BUILT and dark; arm with `TG_AUTHLOG_POLL_INTERVAL` +
  `TG_AUTHLOG_HOSTS` (recipe on-ticket). New intake to the loop → posture decision. Falco/Wazuh/osquery
  descoped (absent estate-wide); Tetragon (live) is the next connector behind a transport decision.
- **TG-168 forensic model lane** — old "no on-prem model" blocker refuted (gpu01 ollama, 24 models); choose
  the litellm route to arm the eval-gated forensic workflow.
- **TG-30** — build-nothing verdict for ack (external benches can't drive the estate; recorded P6-5 spike).
- **TG-420 enforce flip · TG-422 bao cutover (5432 path + CREATEROLE + engine mount) · TG-423
  TrustedUserCAKeys rollout (~26 hosts)** — credential/egress posture; repo halves build default-OFF.
- **TG-73/74 real host-restart / partition triggers** (their risk, by ladder design) · **TG-180 probe
  arming cadence** · **TG-91 NetDisco/SuzieQ/VMware** (not deployed) · **TG-81b HMAC verdict signing**
  (protected path) · **TG-58 Phase-2 flip-gate timing** (excluded by design).
- **C-6 flywheel arming + eval `behavior_re` widening** to compose_seed.go + core/skillstore (these SHAPE
  what loads — unlike the inert `skills/` tree removed 08-14); widening is itself law surface. Plus the
  carried trailer batch-ratify (ADR-0017, C-4) and the distilled-content veto (33 bodies, at leisure).

- **TG-146 ruling** — S3/S4 (graduation-ladder CAS/reload — WHEN, behind the multi-worker canary + spec/029
  sequencing) + S6 (de-novel corpus tamper posture). C1 carved to TG-483; a ruling here closes TG-130.
- **spec/021 un-defer** — all four §7 blockers cleared or in-flight, incl. the flywheel
  GRADUATING an artifact 2026-07-23 (p=0.0033; evidence memo on TG-128). If ratified: T-021-1 + T-021-5
  (LOCAL-ONLY, default-OFF) become workable. Until then nothing is built (CONSTITUTION §4.14 HOLD).
- **TG-129 positioning** — the named near-term brick (versioned ruleset) is SHIPPED; fund the
  ~8k reconciler/tgctl phase or keep parked? (Vision renumbered to spec/030 — 022 was taken.)
- **Arm the federated LDAP approver source** once TG-463 ships (lands config-OFF; arming = who may approve).
- **TG-114 C-6 arming + gate coverage** — (a) may the flywheel trial the base-prompt guidance half?
  (b) widen eval `behavior_re` to compose_seed.go + core/skillstore — these SHAPE what loads (unlike the
  inert `skills/` tree the 08-14 ruling removed); widening is law surface. (c) batch-ratify the carried
  trailers: ADR-0017 + C-4 law signatures, C-2/3/7 eval waivers (golden/restriction proofs), !1400's
  law change (cites the in-session ruling). (d) C-3b store-backed prompt rows: build when armed.
- **Distilled prose content (TG-477/478/479)** — review the 33 bodies AT LEISURE (merged inert; nothing
  loads until the seeding wire, which ships gated). Optional: add the WAN-provider name to the mirror
  denylist if it should be secret (one line; the lint then enforces everywhere).

- *(DONE, journaled: Approve-by arming TG-254 — ENFORCING since 08-03, hardened to concrete owner
  identities 08-10; `vote.inbound` NOT dark, TG-251 residual to re-verify. Runner02 started + proxmox MCP
  resolver fixed 08-10 via claude-gateway !208.)*
- **TG-429 — provision `TG_READONLY_API_TOKEN` as a CI variable** (Settings → CI/CD → Variables, masked):
  until it exists the scheduled merge-gate witness runs BLIND-but-honest, and local
  `scripts/check-merge-gate-setting.sh` is BLIND too (verified 2026-08-10; the setting itself re-verified
  via authenticated `glab api` instead — still true).

- **Item 2 above** — should curated seeded classes hold the SILENT rung, or the one that pages?
- **Campaign #2** — the previous board said both "RUNNING" (`:68`) and "stopped, restart is an owner call"
  (`:120`). State it once, here, when decided.
- **Relevance labels** — CORRECTED 2026-08-03: the previous board (and my first rewrite of it) cited
  `eval/retrieval/production-queries.json` as if it were in the tree. **It is not on main.** It exists only
  on the unmerged branch `feat/production-query-set` (`ef00325f`, 2026-08-01) — the only occurrence of
  `must_retrieve_any_of` anywhere in this repository was this board describing a file that isn't there. An
  item carried forward without being checked, in the document whose whole point is that its claims are
  checkable.
  What the set actually holds: 227 rows keyed on `host + alert_rule` (the query TG makes when it searches
  its incident memory before triaging), each with `must_retrieve_any_of: []` — the answer key, deliberately
  left empty rather than invented. Outcome-derived labels were tried and REJECTED because ~96% were
  recoverable from `host + alert_rule` alone, so a retriever that ignored the question and matched on
  hostname would have scored ~96% and the metric would have rewarded the tie bug.
  The rows also carry `cut_is_a_tie` and `adversarial_near_misses` — e.g. `dc1ghostfolio01` /
  Service-up/down has 96 observed incidents with 16 scoring identically, so TG picks 3 of 16 tied
  candidates and the tie breaks arbitrarily. That is the defect the labels would measure.
  TWO decisions, in order: (1) land or abandon `feat/production-query-set`; (2) if landed, someone who
  knows the estate labels the rows. Until (1), there is nothing to label.
- **`TG_LESSONS_SOURCE_FILE`** unset — the lessons lane is wired and fed nothing.
- **`secret/tg/hosts/*`** — populate to re-arm the OpenBao credential source, or leave it off.
- AWX inventory authority; k8s apiserver audit enablement (blocks T-023-9).
- **TG-152-L3** — the disk-op control-plane guard (pool-floor / rate-cap / sentinel), owner-gated on AWX
  activation; do as part of the disk-grow activation work.
- **TG-82 SIGN-OFF (2026-08-11)** — spec/029-commit-confirmed-actuation is DRAFTED, coherence-reviewed and
  merged (!1318 `78cf5076`; index status Draft; every task pending). Four flagged questions await the owner
  in design.md § open questions: unverifiable→REVERT vs HOLD+page (REQ-2902's marked position) · awx windows
  vs the deferred-verify bound · v1 class scope · TG-146-S3/S4 sequencing. Ratifying the spec unlocks
  T-029-1..5.
- **TG-429 escalation note (2026-08-11)**: the scheduled delivery-witnesses job now hard-FAILS on the blind
  merge-gate half (CI_JOB_TOKEN 404) while its deployed-sha half PASSES — the schedule stays red until the
  token exists.
- **TG-354 (owner decision, 2026-08-11) — does an entry ticket EXIST for TG's push-sourced incidents?** The
  tracker.entry seam is bound but resolves TG's external_ref (`librenms-*`) against a different namespace
  (`IFRNLLEI01PRD-*`), so 299→∞ lookups 404 and four capabilities stay dark. VERIFIED the dedup over-suppression
  fear is stale-safe (the 404 fails OPEN → escalate, not suppress — on-ticket). Remaining is the owner call: if
  TG's incidents have no originating ticket, declare the seam DARK with that reason; if a correspondence exists,
  build a session_triage ticket column + external_ref→ticket resolver. Estate-ticketing knowledge = owner.

- **TG-313 (owner decision, 2026-08-11) — host-resource remediation for Temporal workflow-task timeouts.** The
  10.5s "slow secret write" (TG-277) was a Temporal WORKFLOW-task timeout while temporal-postgres stalled under
  memory pressure, not the write (~12ms). Re-measured live 08-11: memory is NOT exhausted now (6.1 GiB free vs 0
  on 08-04), but 5 `context deadline exceeded` persistence errors still occurred in the last 2h, with **no swap
  and no resource floor** on temporal-postgres — so a future spike reproduces the cliff with nothing to catch it.
  No red→green oracle in the repo; every fix mutates estate resource config. Owner menu (on-ticket): (1) add
  swap [low-risk floor], (2) temporal-postgres memory reservation, (3) investigate the residual pool/IO stalls;
  do NOT raise workflow/activity timeouts (rejected in TG-277). Recommend (1)+(2). Needs owner host-capacity call.

- **TG-450 — FIXED 2026-08-12** (key-rotation drift, mine): hostdiag pubkey → librespeed01+myspeed01, PROVEN, ledgered.
  Owner residual: reader key barely deployed (Tier-B/real hosts lack it) + pve04 SSH boundary.
- **TG-348 (owner) — four never-closed loops.** Leg (c) exposed service-fault auto-heal COLD behind 3 stacked defects:
  TG-450 (fixed) → TG-453 (mitigated) → TG-454 (filed, blocks the first credit). `vote.inbound` proven LIVE.
- **REQ-2818 (spec/028, [O] — TG-230 residual).** opcover overlay-exemption honesty un-observable (0 ratified rows) → no
  clean red→green today. — leg-c/TG-450/453/454 narrative: `history/BOARD-JOURNAL-2026-08.md`.



## 2026-08-14 — wave 5: the rows-3–8 plan + the TG-488 boundary (afternoon, two sessions in parallel)

**The plan** (owner /plan + Go!): rows 3–8 of the speculative table — 43 tickets, ~25k speculative.
Six research agents verified all 43 against main+box first: ~10k evaporated (stale-opens TG-381/229/55;
delivered-but-dark TG-315/86/180/465p1; refuted TG-53/TG-30), ~10k buildable, ~5k owner-parked.
Verification-before-building held: every group had tickets whose text no longer matched the tree.

**Phase 0–2 closes/merges (this session):** TG-478/479 · TG-381 (live drill 3/3) · TG-229 (spec-027
10/10) · TG-55 (→476) · TG-469 (sole-inverse guard, mutation executed) · TG-75 (watcher v2, replay
RED-on-quiet proof) · TG-464 (rollback arms, leaf-local + spec/008 restamp, QA 0.93) · TG-466 s1
(config-hash signal, INV-09 negative drill) · TG-72 (tier-2 harness, expectations.json deliberately
NOT touched — pre-registered endpoint) · TG-86 1c (parity fix; arming blocked on TG-486/487 findings)
· TG-81a (claim-before-touch, atomic mkdir, in make all) · TG-80 P1#1 (ledger-HEAD anchor, executed
superuser tamper caught) + P1#2 (load harness, exit-code-is-verdict) · TG-39 (correlate-logs, fail-
closed reader) · TG-91 (Slurpit @0.82) · TG-180 (probe orchestrator, default-OFF, false-observable
trap red-proven ×3) · TG-420 s1 (log-only proxy, ruled fence) · TG-483/486/487 filed. TG-146 C1
confirmed real → TG-483; TG-461 diagnosed to the TG-337 tradeoff (3 priced options).

**TG-488 (owner, recorded by the peer session):** the decide-do-record boundary (R1–R7 reserved,
everything else executed + veto-after) + 26 unblocking rulings. Under it this session: !1418
behavior_re widening (compose_seed.go + core/skillstore IN — the TG-476 leak's lesson mechanized;
gap red-proven), !1419 TG-492+485 (concurrent-safe authgate — two simultaneous runs 21/21 both;
resource_group deploy serialization — the 08-13 storm mechanized), TG-315 ARMED shadow-first
(collector over the 20-guest pool ×2 syslog servers), !1424 the B11 fence correction (the ruled
three would have cut the JUDGE + the LIVE BRAIN — peer-surfaced, fence now five, pending veto).

**INCIDENT (self-inflicted, ~2 min):** TG-91's compose default `env:SLURPIT_TOKEN` boot-refused the
enforcing triage plane via the deploy train. Hotfixed on-box (bao: ref), class-fixed in !1420 —
the guard test then found TWO latent instances (PVE, NETBOX). Estate ledger appended.

**P3 eval-gated chain:** TG-42 (deterministic skip / lean-deep / class cap; zero-model-calls oracle;
deep trio byte-identical) + TG-215 (per-class disclosure, 65.7% catalog reduction for the fast class;
index-listing ≠ capability removal) merged on byte-identity WAIVERS — both record the standing
obligation that the classifier-signal MR runs the full gate. TG-465p2 (cluster-member seed block,
REACHABLE → real change-gate) built + parked; TG-49 authored ahead.

**Eval-infra incident chain (with the peer):** two real root causes behind the degraded arms —
shared tunnel port 4010 (opener's exit kills the adopter's tunnel; per-session ports now, TG-ticketed)
then sshd MaxSessions 10 (one connection, N channels; burst-kills mid-arm; fixed live to 64).
My waived merges exonerated; the diagnostic gap stands: BYTE-IDENTITY WAIVERS COVER PROMPT BYTES
ONLY — the correction lands in docs/EVAL-GATE.md with the 465p2 MR. Load-mutex + per-session-port
conventions adopted across sessions; TG-39's missing-stream finding split (peer ships the estate side).

**Coordination:** partition with the peer session settled and held all day (their C-claims + B5/B8/
B10/B12/B13/B19/B25/B26; my P3/P4/drills + adjacencies); claims protocol in use by both; core/actuate
frozen to them until their T-029 ping; my seeder holds until their TG-489 chain-live ping.

### 2026-08-14 (evening) — TG-489 delivered end-to-end; the eval-gate incident chain resolved; census 132/132

**TG-489 CLOSED at the full v1.1 bar** (session dfdbf8fa): the distillate tamper chain is live —
both binaries boot-verify prod's corpus (`distillate chain OK — 58 of 58, head=9e3fbe4b…`, matching
heads), five killing-mutation classes + refuse-to-heal executed in throwaway DBs, change-gate PASS
with the candidate BEATING base on every judged dimension, QA 0.8→required-fast-follow(!1433,
REPEATABLE READ verify snapshot)→0.92. The TG-488 B8 ordering constraint is satisfied: the C-6
seeder is unblocked and chain-witnessed. Follow-ups stated on-ticket: ledger head anchor,
incremental verify at scale, TG-495 (schema_version reader-guard gap, review-found).

**The gate saga, fully resolved** (TG-493): eight runs, four root causes — shared tunnel port
(session-pinned now), sshd MaxSessions 10 (→64), the TG-378 GuestRunning harness parity gap
(!1429; the gate was silently un-runnable 08-11→08-14 while everything merged on waivers), and the
nightly trend-watch lock-starved since 08-09 (03:30–06:00 lock window now RESERVED, !1430,
peer-endorsed; the peer builds the absence dead-man). Meta-lesson recorded: the gate guarding
behavior was broken, the nightly guarding the gate was starved, and nothing alarmed on either absence.

**Also this evening:** TG-39 closed (both halves: reader+span export armed via bao-resolved token
after an owned 2-min env:-ref outage; the syslog-ng→OpenObserve shipper live with estate logs
flowing; probe oracle 3× green) · TG-484 closed (census 40→132 of 132 evidence-bearing, 0 bare, 0
closes refuted; tgledger now NAMES bare closes) · T-029-1 merged (!1428, commit-confirmed registry
data; start-guest refused-by-construction until its stop inverse) · the parallel-session protocol
mechanized (!1426 claims lint + resume-path docs; !1430 nightly window) · k8s apiserver audit
ENABLED on all three NL controllers (T-023-9 estate half; two kubelet traps memory'd; the
etcd.yaml.bak quorum-adjacent landmine filed as TG-494).

### 2026-08-14 (night) — T-029-2 merged; the auto-heal collapse diagnosed jointly; T-029-3 in review

**T-029-2 MERGED** (!1436, verified server-side): the armed revert's durable half — commit_confirm
(0095) + the child timer armed BEFORE the effect, refuse-forward-if-unarmable, change-gate PASS
(4.28 vs base 4.30, behaviorally inert as designed), fresh-eyes 0.90, three executed killing
mutations. Eligible classes stay UNDEPLOYED until T-029-3's inverse arm lands (loud-but-inert
interim by design).

**TG-496 (joint diagnosis with the parallel session):** the estate's auto-heal lanes are DARK
under the post-swap brain — all 72 liveness-lane proposals live in Jul 26–31; the Aug-11
librespeed01 session shows "proposal failed the single grammar" (the agent tried, pre-today);
today's drill stood down with an empty diagnosis; a second drill (nginx service-fault) failed
across the OTHER lane. NOT a merge regression (evidence pair predates today). Corroborated by the
base arm's proposal_rate 0.13 vs 0.25 floor on plain main. The TG-293 opus-vs-mistral head-to-head
is the discriminator — owner list [R3] (opus quota + provider choice). Detection/triage remain
flawless; stand-downs are safe-direction.

**T-029-3 in flight (!1439):** confirm-from-terminus + the fired inverse. Its QA arc earned its
keep twice — the 0.55 fresh-eyes CRITICAL (chain-gap executions left NO durable trace; consult
would abort over a live mutation → fixed both ends, absence now HOLDS) and the change gate
catching a real −0.35 from registering stop-guest (Catalog() renders every class into the
preamble → inverse-only classes now excluded by construction). Doubled-rigor battery re-running.

### 2026-08-15 (early) — the armed revert is LIVE: T-029-3+4 merged and deployed, the spine proven in prod

**T-029-3 (!1439)** and **T-029-4 (!1444)** merged and auto-deployed (tg01 on aa7b2177): the fired
inverse through the full chain, confirm-from-the-terminus-only, the orphan sweep, the canary
mandate, and confirmed-only graduation. QA arcs earned their keep: 0.55→0.90 (chain-gap trace
loss; the unreachable class-inverse fire path caught by my own drill) and 0.55→0.93 (the silent
retry path that dropped ledger+feed — fixed by making the retry RECOVER the tail). SIX+FOUR
killing mutations executed. The change gate FAILED three times (−0.35/−0.25/−0.72 full-rigor) and
was RIGHT each time — measuring a stale branch against a main that had merged the peer's behavior
improvements; rebased, +0.04 PASS. Two memories minted (rebase-before-battery; registry-data-is-
prompt-surface) plus the auto-merge-ordering lesson (!1442 merged before its 0.55 verdict — fixed
forward review-first, zero exposure).

**TG-490 (!1442+!1443)**: TG files its own entry tickets — two-phase reserve→create→complete with
a search-adopting resolver (QA 0.93). Dark until TG_TRACKER_CREATE_PROJECT; the writes-arming
question is owner-list [R3] (shared-corpus contamination vs Campaign #2's input integrity).

**LIVE E2E**: a synthetic pre-aged armed window on prod was swept, adopted, consulted, resolved
CONFIRMED, ledgered, and fed the ladder exactly once — the whole spine, zero noise. Deploy-posture
correction on the record: merge = auto-deploy here; the T-029-2/3-without-4 exposure window was
verified EMPTY (0 executions — the TG-496 collapse meant no heals ran). The peer's fix (c)
deterministic heal fast-path is green-lit on the deployed train. T-029-5 remains.
### 2026-08-15 — T-029-5 merged: spec/029 fully built (session dfdbf8fa, lane item #3)

**!1445 (feat/t029-5-chips, 2 rounds + 1 pipeline-red fix-forward).** REQ-2908's empty-diff
no-op guard (guest-first target resolution after the round-2 HIGH) + REQ-2906's rendered half
(commit_confirm chip on the session walk + Workflows view). Review 0.94 (round 2) and 0.93
(fix-forward delta); full make all green on the private migrated DB both times.

**The pipeline red was self-inflicted tech debt surfacing**: T-029-3's shipped
`match.inverse_only` rule broke console-e2e's rule-editor identity round-trip — the policy
editor AND the cmd/grounder read projection silently dropped the unknown dimension (the exact
deny-becomes-implicit-allow drop the round-trip contract exists to refuse; here it would widen
an inverse-scoped auto rule onto forward stop-guest), and the suite's hardcoded 6-rule indices
left an id-less blank blocking Save. Fixed end-to-end (DTO + projection + editor tri-state +
suite made count-agnostic); four executed killing mutations across the fix (2) and the round-2
guest-first drill (2), each witnessed red then green. Learned: console-e2e is changes-gated —
a backend-only MR that grows default_ruleset.json lands broken and the NEXT console MR inherits
the red. Review's LOW (specifiesAny() omits InverseOnly — an inverse_only-only rule wrongly
rejected as "no dimension"; fail-closed direction, inert today) filed as TG-497, folded into the
TG-483 window.

**Spec/029 status: T-029-1..5 ALL merged + auto-deployed.** Live e2e so far: the synthetic
pre-aged armed window walked sweep→consult→CONFIRMED→ledger→graduation-feed-once→cleaned on
prod (T-029-4 evidence on TG-82). The ORGANIC pass arrives with the peer session's fix-(c)
librespeed01 auto-heal drill (they wait for !1445's deploy so the chip is live — the
armed◔→confirmed✓ chip + the commit_confirm row + zero grounding calls is the operator-visible
restoration proof). Remaining in lane item #3: TG-483 terminus collateral re-check (design
settled: estate BlastRadius members × TG's own durable alert capture, positively-typed,
ANDed into reconcile's clean; core/actuate untouched) + TG-497 one-liner, then the TG-82 close.

### 2026-08-15 — TG-483 built: the terminus collateral re-check (+ TG-497)

The TG-146 C1 residual: the deviation verdict freezes at the interceptor's ~1s read and
ConfirmedClear watches only the incident's own host, so a sibling cascade inside the settle
window graded clean. Now the terminus scans the action target's blast-radius members (guest-first
anchor) against ingest_alert_occurrence for FIRST-surfaced (host,rule) pairs since the heal —
TG's own durable capture, INV-11-clean. A positive reading blocks auto-close (new reconcile rule,
outranks the frozen match) AND the graduation credit; unknown (unseeded graph / no reader)
changes nothing — the residual closed is the false CLEAN, and the healthy case was explicitly
drilled to stay clean. Three executed killing mutations: the workflow freeze restored (ticket's
named one), the pre-existing NOT EXISTS exclusion dropped (real-DB drill), the TG-497
specifiesAny term dropped. TG-497 fixed in the same MR (inverse_only alone is a dimension;
the exported JSON Schema const also gained the field it was missing).
### 2026-08-15 (early) — TG-483 shipped · C-3b/C-6 built · B13 armed · TG-122 prerequisites

**TG-483 MERGED + deployed + closed** (!1447, worker 37ac6e3e, boot log "terminus collateral
re-check ARMED"): the terminus now scans the action target's blast-radius members against
ingest_alert_occurrence (first-surfaced-since; own rule-family excluded; pre-existing guard) and
an earned positive blocks auto-close + graduation — the frozen execute-time MATCH no longer
outranks a sibling cascade. Unknown changes nothing (positively-typed). Three executed killing
mutations; review 0.90; change gate PASS +0.42 (run-1 FAIL documented as noise with its
decomposition — and the instrument lesson: candidate-arm wall-clock includes the worktree
compile, so arm-time asymmetry alone is NOT a shed signal). TG-497 fixed in the same MR.
**TG-82 CLOSED at the bar — spec/029 fully built and live.**

**C-3b/C-6 (TG-114)**: the base-prompt guidance half is now a ClassPrompt store row — seeded
from the embed (byte-identical; goldens untouched), composed store-first with trial-arm
participation through the same deterministic assignment as skills, content-hash re-checked
(review MEDIUM fixed + third killing mutation), embed as the floor. Review 0.90. The fast gate
failed twice on DIFFERENT dims with a sign-flipping dimension (byte-identical arms by
construction — the eval env seeds no row); per the gate's own INCONC guidance escalated to the
pooled FULL gate rather than re-rolling; merge rides its verdict.

**B13 residuals**: secret/tg/hosts populated — 21 new entries (the 20-guest pool + pve04, the
residual's named gap), every key pre-verified authenticating before write, zero clobbers
(all v1), the openbao credsource synced clean post-import (33 total). The telemetry-sender half:
TG's side is fully armed (crowdsec+authlog ingest tokens minted + refs live on the grounder);
the remaining work is the ESTATE sender config (candidate host sec01 — not directly reachable
with session keys; AWX-lane arc).

**TG-122 prerequisites**: target repo confirmed (infrastructure/dc1/production, project 7);
`tg-gitops-mr-bot` project token minted (Developer/api, expires 2027-08-01, anti-self-merge
structural), live-verified, sealed at secret/tg/gitops-nl#token; recorded on-ticket.

**TG-422 finding**: the db-engine slice has an unlisted NETWORK prerequisite — tg01's postgres
publishes no host port and OpenBao lives on another subnet; the exposure + nftables scoping is
its own off-hours arc (recorded on-ticket).

**Peer session (recorded for the record)**: fix-c + the liveness-freshness fix (the monotone
observation-time upsert my review supplied) merged and re-drilled — the TG-496 reachability gap
is CLOSED (fresh liveness → DETERMINISTIC → reversible proposal → the operator-vote lane, which
is CORRECT policy: start-guest sits at level=approve since the hands-off ruleset was deliberately
backed out — that re-arm question is now a board [R3]).

**C-3b landed (!1449) on the pooled FULL gate: PASS +0.03, every dim within ±0.12** — the fast
gate's two incoherent FAILs (different dims, one swinging −0.34→+0.34 on an identical sha)
resolved as pure judge noise, exactly what the pooled escalation exists for. Then the boot log
caught the seed refused live (skill_kind_check — Kind "prompt" ∉ {behavioral, catalog}; the
embed floor held, zero exposure): fixed forward with Kind "behavioral" (the established
convention; Class is what says prompt) + a real-DB drill seeding the exact worker row shape and
pinning the vocabulary. The drill's first form taught its own lesson: skill_version is
CHAIN-LINKED — a DELETE cleanup is tampering and broke the package's reads; the drill now uses
unique names and deletes nothing.

### 2026-08-15 (03:00–05:40Z) — TG-463 both halves · TG-498 found→diagnosed→fixed→PROVEN · the first successful live deterministic heal

**TG-463 CLOSED** (!1451 + !1452): the voter-alias normalizer armed live (chat identity →
canonical login, surface-side, never-widen drilled + mutation-killed) and the live ruleset's
raw-MXID workaround trimmed (version 3b9ee0ab, ledger 10987 — the approver set is exactly the
canonical owner identities, B26's letter); then the dormant actor-side lane retired (~1,100 LOC:
MayApprove/VoteAdmission/PrincipalResolver + modules/policyident, zero callers independently
re-verified) with the spec/015 surgery (T-015-10 removed, T-015-9 re-scoped to the live
enforcement it verifies) and spec/010's REQ-605 oracle re-pointed (the ratify gate's catch). One
approval-identity architecture remains: gate-time expansion → frozen VoterAdmitted → surface
resolution.

**TG-498, the full arc in one stretch**: the peer's approve-drill (the FIRST live
commit-confirmed traversal) surfaced an immediate abort; diagnosed from the workflow history —
the deterministic proposal carried ZERO captured ToolResults, so the evidence gate (INV-11)
refused and the window aborted as designed (no determinism defect; the stale-cache warning was a
benign deploy-restart artifact); fixed (!1453) by capturing the fast path's own confirmed-stopped
observation as cited evidence (zero workflow changes, two compiled killing mutations reproducing
the live failure verbatim, review 0.93); **proven by the clean re-drill**: injection 05:20:31 →
DETERMINISTIC → vote through the REAL surface as kyriakos (the TG-463 lane) → the heal EXECUTED
— guest up TEN SECONDS post-vote → commit_confirm armed and STAYING armed → the verifier's +1s
LibreNMS observe failed → the terminus routed HELD_UNVERIFIABLE with the page and the inverse
correctly withheld on the healthy guest — REQ-2902's hold branch exercised live for the first
time, and the T-029-5 chip carries the honest held state on the session walk. The operational
residual (the +1s observe near-always fails for a just-booted guest → a page per heal + no
CONFIRMED graduation via this lane) filed as TG-499 with the guest_liveness-backed verify
proposal.

**CI honesty notes**: the 03:53Z scheduled actuation-guard-coverage red was the drill's stopped
guest at probe time — retried green after restoration; a harness core/db timeout
(TestTrackerEntryReserveCompleteLifecycle under nightly load) and a registry 504 on image-worker
were retried green (the documented transient classes; each verified before retry, never assumed).


## 2026-08-15 — board resume-budget trim (superseded scopes moved here verbatim)

The resume-budget gate (scripts/lint-resume-budget.sh) went red at 42574/40000 bytes; per its own
guidance the SUPERSEDED standing-scope narrative below was moved out of docs/BOARD.md's queue region
into this journal verbatim. The live owner asks it carried (TG-499 waiver, TG-500 ratification, hands-off
re-arm) were folded into the board's canonical Owner list — nothing was lost.

### Standing scope (owner-set 2026-08-14 — speculative-table rows 3–8; supersedes 2026-08-13)

**Mandate = rows 3–8 of the owner's 2026-08-14 speculative-LOC table** (agent-loop · benchmark · predecessor
port · estate · security · actuation — 43 tickets, ~25k speculative). Six research agents verified all 43
against main+box: ~10k evaporates (stale-opens, delivered-but-dark halves, refuted specs), ~10k buildable,
~5k parked behind the owner list below. INTERPRETATION owner-approved by plan: eval-gated agent-loop changes
are IN scope, taken with the mandatory on-box eval gate red/green BEFORE merge (red ⇒ surface, don't merge);
arming tickets execute+inform; estate-config halves needing owner creds stay surfaced. Phases A–5 in the plan
(`~/.claude/plans/mutable-tickling-island.md`). Journal 08-14 wave 5.

**Wave-5 closes (08-14):** TG-478/479 · TG-381 (live drill 3/3) · TG-229 (spec-027 10/10) · TG-55 (→TG-476) ·
TG-469 · TG-39 (both halves: my fail-closed reader + peer's estate shipper; 3 green probe sweeps; QA 0.93) ·
TG-465 (p2 gate PASS +0.97) · TG-49 (gate PASS +0.56). Record-corrected: TG-36/168/53/30. TG-146 C1 → TG-483.
**Delivered+merged (~30 MRs):** the P0–P2 train + ruling work (behavior_re widening !1418, CI-hardening !1419,
fence correction !1424, nightly dead-man !1431) + P4 seeding rail (32 distillate drafts LIVE in prod, chain
58→90 verified) + runbook-promote rail + P3 gated loop improvements (465p2/49/46, each verdict in-range).
**HEADLINE → owner list [R3]: the self-heal proposal propensity is COLLAPSED under the Mistral brain (TG-496,
two live drills) — TG-293 swap-back is the decision.** Two eval-doctrine corrections landed (byte-identity
waivers cover prompt bytes only; fixture-armed recall masks live propensity). Filed: TG-483/486/487/494/496.
**2026-08-15 → TG-496 fix (c) DELIVERED + PROVEN (the brain-INDEPENDENT complement to the swap-back).**
The deterministic guest-down fast-path (!1446, review 0.93, eval +0.23) was found UNREACHABLE by a live
drill — guest_liveness projection lagged the 37s detector by up to 5 min, so the classification always
read stale "running". The reachability fix (!1448: feed the projection from the detector's OWN 37s fetch,
upsert-before-dispatch, monotone-observation-time upsert for multi-writer safety; + a CRITICAL typed-nil
sink crash caught in review and fixed) shipped + auto-deployed; re-drill PROVEN (exec_class=DETERMINISTIC,
reversible start-guest proposal → operator vote). Restores confirmed-guest-down auto-heal under ANY brain.
**NEW owner list [R3] — re-arm the hands-off ruleset?** start-guest is now level=approve (POLL_PAUSE): the
hands-off ruleset was deliberately backed out (policy_ruleset_bak_handsoff), so the heal VOTES instead of
auto-executing. Re-arming is an owner ruling. (An organic approve will re-graduate start-guest via the
ladder once it earns clean runs.) Lesson: [[fast-path-reads-projection-slower-than-its-trigger]].
TG-244 PARKED (prompt-inert; 3-run gate noise −0.64→−0.35→−0.21 converging; ships dormant until a tracker is wired).
**2026-08-15 (through the night) → the auto-heal lane REACHES + HEALS live end-to-end.** TG-498 (INV-11
evidence citation, !1453) + TG-463 (approve-admission) closed the chain: a real guinea-pig guest-down
healed in ~10s via the operator vote. GRADUATION to hands-off is TG-499 (guest_liveness-backed post-heal
verify → CONFIRMED window → +1/5/clean → auto after 5) — BUILT + reviewer 0.80 + REQ-2902 verify-lane
approved + reachability-guarded (branch b0e7fe4b), but PARKED on an **OWNER eval-gate-waiver**: activities.go
is on behavior_re so the CI eval-evidence gate fires, yet the change is provably eval-PATH-inert (the verify
lane is never reached in the eval, which runs investigate+propose mutation-OFF), and the 3-run pooled gate
false-fails ONLY on falsifiable_prediction −0.50 judge-noise. A waiver is CODEOWNERS-only. **NEEDS: owner
`Eval-Gate-Waived-By:@ncpjfuzl` on b0e7fe4b → merge → auto-deploy → the co-drill confirms graduation** (the
parallel session's deploy-watch runs it). **META (blocks more than TG-499):** falsifiable_prediction has now
false-failed THREE provably-inert changes (TG-244 −0.78, TG-47, TG-499 −0.50) — behavior_re is a PATH filter,
and this dim's judge variance means path-in-but-effect-inert changes can't reliably clear the gate at all;
the eval-gated inert-change series is stuck on it. **DIAGNOSED + filed TG-500**: falsifiable_prediction and
diagnosis_grounded score on n=5 sessions/run vs n=20 for the four passing dims, so one uniformly-harsh
candidate run (proven zero-mean small-sample noise, NOT a rubric bias — run1 was down on EVERY dim) dominates
their n=5 pool while the n=20 dims average it away. Fix: a sample-aware resolution band (widen the floor by
√(n_ref/n_dim); UNMEASURED-not-FAIL; can't fail-open — a real regression is consistent across runs). **NEEDS:
owner ratification of TG-500** — one systemic control that unblocks TG-244 + TG-47 + TG-499 in one stroke,
vs three separate waivers.

### Standing scope (owner-set 2026-08-13 — speculative-tail disposition)

**Mandate = the owner's speculative table: the category-15 EPICs + residuals (TG-128/132 · TG-129 · TG-114 ·
TG-130/187 · TG-107).** 08-11 work classes hold; north-star remainders park behind owner ratification.
Queue: TG-109 (→TG-107) → TG-188 (→TG-187) → TG-463 → scoreboard → TG-470..479 → grooming. Journal 08-13.

**Wave-4 (08-14, journal):** TG-107+109 (1e) · TG-470..476 (C-train; TG-474 seed-leak window sealed by
TG-476, red-first) · TG-482 (axes e2e) · TG-477..479 (distillation) closed; !1400 owner-ruling removed
`skills/` from eval `behavior_re`. Full detail in the journal.

*(The 2026-08-11 "categories 2–7 only" scope and the 2026-08-05 145-ticket ranking are two-generations
superseded — verbatim in git history + journal. The consequence principle + work classes below remain
operative.)*

## RANKED BY CATEGORY — 2026-08-05 (SUPERSEDED)

The original 145-ticket ranking, its band principle, the 16-row table with commentary, and the 08-05
standing scope (TG-333 deferred · enforce-flips in scope) live verbatim in git history (this file before
2026-08-14) and the journal. The consequence principle and work classes below remain operative.

## 2026-08-25 — graduation plan approved; !1634 + !1645 land; board narrative moved here (resume-budget)

**The owner approved the graduation plan** (fastest path to campaign #3 + predecessor cutover; plan file
`~/.claude/plans/binary-wandering-frost.md`, rulings appended to the session rulings log): evals resume AFTER
code-complete as one concentrated week; campaign #3 arms = AS-DEPLOYED STACKS (§6-declared; parity-on-opus only
as the diagnostic rerun); §5.2 VISR deferred by ruling [R5]; TG-437 remedy = add the Matrix approver post-exam
[R4]; B16 read as "buildable non-exam code tickets" (exam-program + post-exam armings are the exam and its
aftermath). TG-536 RULED the same session: WIRE `AuthorizeRestamp` (operator-token actor + ledger append) —
off the owner list, into Phase 1.

**Merged this date:** !1634 (T-022-5 redaction + derived env-parity/wiring guards + spec status reconciliation
40→10 pending; merge `36bc5e2b`) · !1645 (TG-537 break-glass reachability + this board trim; stacked on !1634).

The four LANDED blocks (2026-08-10/11/14) and the 2026-08-23 reachability-session narrative below moved here
VERBATIM from `docs/BOARD.md` (the resume-budget gate's instruction: narrative belongs in the journal).

### Moved from BOARD.md — LANDED blocks (2026-08-10 · 08-11 · 08-14)

**LANDED 2026-08-10 (board updated 2026-08-11 — the on-event update was missed): TG-435 [P0][safety] + TG-436.**
The async deferred-verify path is fail-CLOSED (!1298, merge `68c5502c`: the observer seam returns `(alerts, ok)`;
nil observer / unread post-state → withheld BEFORE the verdict computes; killing oracle
`core/regime/asyncverify_test.go:128` executed in both directions) and the async graduation feed REFUSES promotion
(merge `280cf92d`, `cmd/worker/main.go:7003-7008`, closed-enumeration writer guard). Full delivery bar incl. QA 0.93
on the tickets. Residual observer/external_ref wiring is future work gated on an async launch producer that does not
exist (`Reserve`/`BindHandle`: zero production callers — the channel is dormant; spec/017 REQ-1718 refuses the sync
drive). The spec/017 acceptance oracle still models the PRE-fix promotion — filed as TG-445. Journal 2026-08-11.

**LANDED 2026-08-11 (full detail above in this journal):** TG-112 (mutation_enabled terminology
RETIRED, QA 0.92) · TG-378 (guest_liveness projection + seal-time state precondition, both slices, 0.90/0.93) ·
TG-152-L1 (interceptor re-checks awx-launch params at execute, 0.94; L3 owner-gated). Filed same day: **TG-446**
(a running allowlisted target produces no AWX probe line — the guest_liveness cross-check's first operational use).

**LANDED 2026-08-14 evening (journal): TG-489 chain live (58/58 both planes) · TG-39 closed both
halves · TG-484 closed at 132/132·0-bare · T-029-1 merged · protocol mechanized (!1426/!1430) ·
k8s audit on · the eval-gate incident chain resolved (TG-493, four root causes).**

**LANDED 2026-08-10: TG-428 — the PM-overhaul series** (11 MRs, journal entry of this date).

### Moved from BOARD.md — 2026-08-23 reachability session (what is present vs what is REACHABLE)

`make ledger`: **520 total · 12 unresolved · 508 resolved · 349/349 evidence-bearing · 0 bare.** TG-58 closed
(all four Phase-2 prerequisites delivered), plus TG-80/81/180/422/501/527/529/530/531/532/535 and the TG-175
bare-close swept.

**The session's finding, one sentence:** a control can be built, tested, documented, boot-logged and STILL be
unreachable — because compose never forwarded the key that arms it, and the boot line then reports honestly on
what the process received while an operator reads it as a statement about what they configured.

Eight instances, all found by making guards derive their own subject lists instead of maintaining them by hand:

| Unreachable | Consequence |
|---|---|
| `TG_VSPHERE_*` (4 keys) | the vSphere source (TG-91) could not be configured at all |
| `TG_GITOPSMR_ARM` | + the plane split below, the gitops-mr lane was unarmable in EITHER process |
| `TG_CISCO_WRITE_DEVICES/_ARM` | TG-85's whole slice-4 config surface (peer's code, caught within minutes) |
| `TG_OBSERVE_PROBE_*` (9 keys) | TG-180's probe harness — the "not configured" boot line was unfixable |
| `TG_CONFIG_IGNORE_STORE` | **TG-537**: the break-glass for a worker that will not boot (!1645) |

**The guards that were covering less than they claimed** (each now derived, each with a vacuity floor, each
killing-mutation-verified): env-parity targets (11 of 30 files) · the worker wiring inventory (20 of 25
workflows, 11 of 12 seams, 15 of 16 jobs — and the unlisted job was dead) · and the parity extractor itself,
which silently dropped every constant-keyed read. A ninth would have shipped: the plane-pairing check PASSED on
the state it was built to catch until its own mutation exposed that a `${VAR:-both}` compose default is not a
guarantee.

**The rule this session earns:** the only reliable question about a gate is *what does it do when I break the
thing it claims to watch* — never what its PASS says. Three of these (with the peer's deadcode rooting and
contract-drift findings, five) were caught that way in one day and none by reading a green.

**In flight (at 08-23):** !1634 (22 commits: T-022-5 redaction, the vw:/passbolt: backends, the tier selector,
gate 4d2 authn-compose, the k8saudit reader, TG-437's namespace fix, the derived guards, spec status
reconciliation 40 pending → 10) and !1645 (the break-glass). Both merged 2026-08-25.

### Moved from BOARD.md — the 08-16 delivered/drained narrative (superseded by the 08-25 plan block)

**Delivered 08-16:** TG-80 #1 (governance-ledger write-domain airgap) · TG-422 slice-1 (the `dyn:` OpenBao
database-engine lease scheme) — both build-ahead behind a flag; merging changed nothing live, arming is the operator's step.

**The clean AFK-build queue is essentially drained** — TG-422 was the last self-contained build slice. The 42
unresolved are each blocked on the owner, a live-attended window, the eval box, or are epics/peer-owned. Per
"Queue exhausted ≠ done" the next AFK step is the resolved-issue verification sweep (TG-339 precedent) once a
quiet-box + live-access window aligns; otherwise an eval-gated build when the box frees. Worked under TG-488.

## 2026-08-25 — the Phase-0/1 wave: eight merges, four closes, the campaign-#3 law landed

Executed under the approved graduation plan, all in one session, every merge carrying a fresh-eyes review
verdict ≥0.90 (three of them after FIX-FIRST rounds whose findings were real and fixed same-session):

- **!1634** (inherited, merged `36bc5e2b`): T-022-5 redaction, derived env-parity/wiring guards, spec
  statuses 40→10 pending. **!1645** (merged `285a5bfe`): TG-537 break-glass reachability + the
  resume-budget board trim. TG-537 CLOSED (QA 0.95; `TG_CONFIG_IGNORE_STORE` live-verified present on
  worker + worker-actuate, prod image == merge sha).
- **!1648** (merged `84f4df81`) + **!1656** (in flight): TG-539 — the deployed-sha witness resolves a
  skip-ci tip's newest BUILDABLE ancestor over FULL commit messages (git walk, else commits API), age
  falls back to CI_COMMIT_TIMESTAMP; drill 6→13 arms incl. two REAL-git arms. The review's headline
  catch: the fix's own first-draft commit subject spelled the marker verbatim and GitLab SKIPPED the
  MR's own pipelines — the fix shipping as an instance of its own bug. Scheduled run 50473 then red on
  the new arms' SUPPRESSED fixture-construction errors in the ci-deploy-tools image (no usable git
  there, hypothesis) — !1656 makes construction loud + skip-with-reason. Close pends one green
  scheduled run.
- **!1649** (merged `4834cdc5`): the governed §6 CAMPAIGN-#3 amendment — fresh `ACCRUE_FROM`
  2026-08-26T00:00:00Z declared at zero accrued pairs; TG-526's manifest-membership population (organic
  pairs can no longer crowd GT pairs out of per-host cap slots; no manifest ⇒ never powered; snapshot
  applies the same membership); `accrual.py`'s stop-rule gains the ground-truth term (campaign #1
  stopped at bar-met with 18/24 GT-carrying); as-deployed-stacks arms + judge direction-of-bias
  disclosure (a pred-brained judge is CONSERVATIVE for a TG secondaries win); hash re-pinned
  `d382f048…`. TG-526 CLOSED (QA 0.90).
- **!1651** (merged `5cb2774c`): TG-538 — gate DRILLS join the protected law surface (a weakened drill
  disarms its gate unattributed). TG-538 CLOSED (QA 0.95; reviewer reproduced the matched-set diff:
  exactly the 17 family drills flip, nothing else).
- **!1652** (merged `c7c558b6`): TG-536 — `AuthorizeRestamp` wired for real: TG_RESTAMP_ACTOR, preflight
  then append to the IN-REPO hash-chained `spec/.restamp-ledger.jsonl` (travels in the same MR as the
  moved hashes; full VerifyChain walk on every check; forked tail refuses), end-to-end CLI kill test,
  its own two restamps are chain entries seq 1–2. TG-536 CLOSED (QA 0.93 after a 0.78 FIX-FIRST round —
  the reviewer deleted the call site themselves and watched the new test go red).
- **!1650** (merged `060ad00c`): env-parity guard — resolved-but-malformed constants now REPORTED; the
  review's "dead exemption entries" counter-finding was REFUTED by the gate itself (the discovered-sweep
  consumes them) — recorded in the map's comment.
- **!1653** (merged `a567d400`): TG-5 §5.4 — judgecal gains `-json-out`; the release gate reads the
  newest COMMITTED calibration record (GREEN/RED/UNCALIBRATED/stale/unreadable/absent each its own
  state); §5.2 states the owner's [R5] VISR deferral verbatim. Review caught a decoy-directory
  false-GREEN (anchored glob + drill arm) and a days-floor staleness slack (seconds compare + boundary
  arm) — SHIP 0.93.
- **!1654** (in flight, SHIP 0.90): TG-4's retention inventory — 81/81 tables declare reaped/ttl/
  retained two-way-complete vs the live schema; also surfaced nonce.go's stale "pruned by a Temporal
  schedule" claim (corrected in-diff). **!1655** (in flight): TG-533 — opt-in confighash incidents
  (must-fire + spurious-suspicion control) + the mechanical SecurityCheck outside the judged dims; the
  instrument is delivered, its RUN is Phase-2 per the no-evals ruling.
- **TG-122 verified CODE-COMPLETE** — both lanes built AND wired (`worker_actuation_wire.go:175-177`);
  the plan's "build the renderer" line was stale before it was written. Arm = Phase 4 (on the ticket).

Session defects worth their memory entries (both written): the skip-ci-literal commit-subject self-trip,
and multi-worktree cwd drift landing edits in sibling trees (three near-misses, one caught by the
deadcode gate reading a stale baseline).

## 2026-08-25 (evening) — the Phase-2 eval week opens: TG-533 instrumented, cisco competence lands end-to-end, two live incidents mechanized

Nine more merges, all reviewed, each with its gate evidence (this session, continuing the wave):
!1658 (CI auto-retry defaults + deploy retry:0 carve-out — the no-red-emails ruling mechanized),
!1659 (witness drill arms probe-first on the toolless deploy image), !1660+!1661 (TG-533 SecurityCheck:
three-state checked/unreached/violations, RunnerResult.SecurityEscalated as the real disposition bit,
fixture worlds made host-coherent — CLOSED at the bar), !1662 (TG-85 cisco-triage skill + 2-incident
corpus extension, full-rigor GATE PASS Δ-0.02), !1663 (cisco-show read tool: closed catalog, allowlist
arg token, DARK-by-default boot line), !1664 (deploy AWX poll 10→30 min + job timeout 45m — the
double-actuation incident's fix), !1665 (TG-85 item 6: the cisco pack literal `pack:cisco@1.0.0` +
the pack boot attestation — pack.Resolve's first caller; full-rigor GATE PASS overall Δ+0.07;
prod attests `pack pack:cisco@1.0.0: domains=cisco tier-hint="primary" band-floor=true`).

Two live incidents diagnosed first-hand and mechanized:
- **Deploy double-actuation** (AWX 37171/37173): the 10-min CI poll died mid-play, released the
  resource_group, and a second play launched concurrently; disk hit 91% from the image storm.
  Fix in !1664 + `docker image prune -af` (24.8G freed). The poll now outlives any real play.
- **TG-540 dual-A DNS poison reached the PROD box** (main #50564's deploy: pull JWT auth
  `context deadline exceeded` ×3). dc1tg01 was never pinned — the deploy-side consumer the
  morning sweep missed (absence-from-one-list, again). Pinned tg01 + gpu01 (17 registry images,
  also unpinned), re-ran the exact failed pull clean, retried the deploy job → #50564 green,
  prod converged on 506baf94 then b326e7f1. Pinned estate now: runners ×3, dev box, tg01, gpu01.
  Zone cleanup remains the owner's [R4]. Ticket updated; memory written.

Phase-2 scoreboard after this wave: TG-533 CLOSED · TG-85 items 1–3+6 delivered at the bar
(4/5/7 remain: arm-live routing dormant construction, operator reversible-op set, spec/008
task-row reconciliation) · TG-78 host-domain slice merged; proxmox NODE/STORAGE/CLUSTER-plane and
k8s eval incidents + their pack literals are the next slices. Main green end-to-end; prod healthy
on b326e7f1 (first-hand inspect).

## 2026-08-25 (night) — eleven owner rulings in one sitting; the Phase-4 gate is pre-authorized; three more merges

The owner asked to be walked through every pending decision one-by-one (AskUserQuestion rounds, full
context each). All eleven RULED — verbatim in the session rulings log; the queue-relevant core:
[R2] mutation-ON canary arms ON a winning verdict (the verdict IS the flip) · [R3] TG-490 tracker
writes arm at exam-window close, win or lose · [R3] start-guest re-promotes via the ladder once votes
work — no separate flip · retire stays the owner's word, standby indefinitely · recipe #1 = the
intersite-tunnel heal, ratified; owner names the attended-drill slot · TG-129 (tgctl) is the first
north-star to become a spec post-cutover · TG-481 object-group model builds post-cutover · TG-529 was
already ruled+built+Fixed (stale owner-list entry; prod seeds 12 runbooks — corrected in the log) ·
TG-315 builds+arms WITH Phase 4's parity set · TG-74 runs post-cutover on the owner's day.

Merges: !1667 (TG-78 node-plane routing; full-rigor PASS; the FIX-FIRST→remedy story on the ticket),
!1668 (spec/008 T-008-37 — TG-85 item 7), !1669 (TG-85 items 4+5: the cisco-interactive lane, arm-live
construction, operator reversible-op set; review caught the ops-only arming gap, fixed pre-merge).
TG-85 closure pends only the !1669 deploy evidence.

Incident: main #50584 (the !1669 merge pipeline) red on a configwrite timing test — NOT the merge's
code: the timed() wrapper started the budget clock before capturing start, under-measuring Took on
contended runners (green locally ×30, green in the MR pipeline). TG-541 filed; the one-line reorder
fix is in flight; the job retried to heal main. The flake found a real measurement bug.

Also learned structurally: the fast change gate runs corpus[:8] — appended corpus rows are INVISIBLE
to it; full rigor is the only honest gate for corpus extensions (recorded in the k8s slice's commit).
## 2026-08-25 (late night) — the eval week's build half CLOSES: k8s gated, all three packs live, TG-85 + TG-541 resolved

Four more merges, each full-rigor gated or waiver-ruled, each reviewed: !1671 (TG-541: the configwrite
timed() ordering fix — the flake found a real measurement bug; main #50584 healed via retry), !1672
(TG-78 k8s trio: k8s-triage change-gated for the FIRST time; corpus 27→30; labels derived from the
actual tool surface — no k8s read tool ⇒ escalate; the fast gate's corpus[:8] blindness recorded as the
standing rule for corpus extensions), !1673 (the proxmox + kubernetes THIN pack literals — the review
empirically confirmed a proxmox band floor would override even an earned AUTO, so thinness is pinned by
test, not prose), plus the board rulings update (!1670).

TG-85 RESOLVED at the bar (all seven items; the first complete platform pack). TG-541 RESOLVED at the
bar. TG-78's eval program: every build slice done — remaining are the two scope calls (proxmox runbook
scope; skills/README via the prosedistill source).

The §5 evidence line closed the same night: the sealed HOLDOUT ran (gap 9.60 vs the ≤20 bar — PASS,
§5.1 leg 2) and cmd/judgecal ran for the FIRST time over the real population (n=619 single-session
faults): TPR 0.966 but **TNR 0.060** — the judge approves 110 of 117 injector-refuted diagnoses, κ≈0.04.
§5.4 flips BLOCKED→readable-RED, the honest state; TG-542 carries the finding (remedy shape: refusal
calibration for the rubric). The §6 amendment's judge-bias disclosure is now QUANTIFIED; the exam's
judge-free primary is unaffected. TG-4 verified already Fixed; TG-5 stays OPEN on the §5.4 red (the
[R5] ruling covered §5.2 only — an honest red is not a deferral).

**[R2 — surfaced AND ruled the same night: AS-DEPLOYED, ALL FOUR CLASSES.]** Context kept for the record: The actuation plane runs may_actuate=true
(the standing live posture behind TG's scored wins) and THREE heal classes sit at AUTO
(restart-service, start-container, start-service) — so exam faults of class container-down /
service-down could be AUTO-HEALED mid-window. The plan's preflight says surface exactly this before
arming. The options: (a) as-deployed purism — run all four fault classes; an auto-heal IS TG's
deployed behavior and the pairing still scores (the alert fires before any heal; GT is the injector's
record); (b) restrict the injector to device-down + disk-fill (start-guest is at approve with
timeout-deny votes — fully protected; loses breadth on the other classes); (c) temporarily demote the
three classes for the window (a governance mutation — least attractive). The as-deployed ruling leans (a); real heal frequency is LOW (newest execution 07-31 + one 08-15
liveness try) but the injector raises the trigger rate by design. Ruled: no restriction, no demotion — the exam arms on the 08-26 morning checks.

P2-6 ARMED same night (TG_SCREEN_KILL_TERMINAL=1 verified in both RUNNING workers, healthy; the plan's
Phase-2 ruling satisfied by the eval week's four PASS records + the holdout PASS). Phase 2 remainder: the §5-evidence MR (!1674) merge only. Then the exam. The §6 accrual boundary opened at 2026-08-26T00:00:00Z.

## 2026-08-31 — TG-556 CLOSED: the model plane's failure story ends on the Claude lane; the wallet ask is withdrawn

Two owner rulings reshaped the fallback doctrine in one sitting. **ROUND 4** ("wire sonnet/fable"): the
Max-sub Claude lane enters the contestant content-policy rescue — `fallback-cc-sonnet` → `fallback-cc-fable`
over litellm01 (litellm01 already mapped both names; pre-verified serving claude-sonnet-5 / claude-fable-5
through TG's own key), content chains primary/fast → cc-sonnet: acyclic, both-deployments trips covered,
fast's rescue restored. Fresh-eyes 0.90, two findings applied (the rescue rung's own content-policy
dead-end; the validator now walks content_policy_fallbacks — that block had shipped unvalidated through
ROUNDs 2–4). Merged !1724 `9969ae3d`, deployed, live-probed. **ROUND 5** ("we do have so many models
available"): the general 429/5xx chains become [fallback-cc-sonnet, fallback-deepseek] — live rung first,
the balance-dead provider rungs (deepseek/mistral/z.ai — every endpoint, z.ai's coding-plan lane included,
probed) demoted to auto-heal slots, the provider top-up ask WITHDRAWN (board [R3] entry removed; verbatim
rulings in the session rulings log). Fresh-eyes 0.90, two findings applied (the board's dangling [R3]
cross-reference; comment drift). Merged !1725 `c02457b2`, deployed, live-probed 15:42Z: gateway healthy,
0 errors, rung answering. **TG-556 CLOSED at the bar** (delivery-bar on the ticket); tg78-pve-guest-02's
corpus re-add moved to TG-78 with the same-family-judge caveat (Opus scoring Sonnet-rescued sessions).
`#Unresolved` = 1 (TG-78, vSphere-only). End-state also recorded on TG-293/TG-548. Ride-alongs: core/wiring's
validBecause fixture rotted by calendar at 2026-08-31T12:00Z and redded EVERY tree incl. unmodified main —
fixed to the real clock (152aaa0b, the tg440 lesson); the 08-29 board draft left uncommitted in the main
tree had blocked the 03:40 cron pulls for two nights (no trend-watch baselines 08-30/08-31) — discarded
(its content was already on main as 3b02b946, twice superseded), tree clean before the nightly.

## 2026-08-31 (afternoon) — TG-437 verified already-fixed; the vSphere handoff; session-close state

**TG-437 (the Matrix approval channel): owner ruled "fix this"; live forensics showed TG-463 had already
fixed it.** The ledger + config timeline: MXID votes ADMITTED 08-12 (seqs 10486/10493); the 08-15 ruleset
rewrite dropped the MXID mitigation (`updated_by=kyriakos`, all 7 rules down to `user:kyriakos/kyriakosp`)
— the regression the boot detector caught 08-23; TG-463's voter-alias layer
(`TG_VOTER_ALIASES=@dominicus:…=kyriakos`) then delivered TG-437's own option-(b) design, and the detector
(same `VoterAdmitted` predicate, now run at boot AND every ruleset write) is silent at every boot since —
0 refusals post-08-23, 120 `human:approve` with full approve→actuate chains. No mutation needed; the stale
[R4] board entry (which claimed "every Matrix vote refused in production") removed via !1727 `f33b4276`.
Residual named on the ticket: no vote has flowed VIA MATRIX since the alias arming (recent voters are the
API lane), so the owner's next real Matrix tap is the fresh transport round-trip proof. Full delivery-bar
on TG-437. Process note, recorded in session memory: one push chained past a red local gate behind a
grep-as-display (`MAKE_ALL_EXIT=2` matched, chain ran) — auto-merge disarmed via API, claim-name mismatch
fixed, re-pushed only on a value-gated green; the never-chain lesson hardened with the mechanical rule.

**TG-78 vSphere: the owner is provisioning VMware/vSphere access personally (08-31) and will signal when
deployed and ready — the last pack build waits ONLY on that signal.** Market research backing the
vSphere-last ruling (sources on request, summary to the owner in-session): VMware still ~40–60% of installs
but sliding ~70%→~40% (Gartner trajectory); post-Broadcom, 4% of enterprises fully left while 86% actively
shrink; Proxmox the fastest riser (~16–21% mindshare, >1.5M hosts). Deferral costs nothing; a live
ESXi/vSphere box arriving is the trigger. Build needs on signal (on TG-78): worker-reachable API endpoint,
bao-delivered read creds, 1–2 sacrificial guests; TG-91 (vSphere/Slurpit e2e) rides the same deployment.

**Session-close state:** queue = 1 (TG-78, waiting on owner hardware); every pipeline green; main tree
clean at `f33b4276`+; model plane on the ROUND 4/5 posture (journal entry above); owner list: TG-536 [R4],
the R3 armings, TG-180 [R3].
