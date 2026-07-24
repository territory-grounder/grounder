# Territory Grounder — Backlog & Sequence (SINGLE SOURCE OF TRUTH)

> **This file is authoritative.** It reconciles three drifting surfaces:
> YouTrack (a *reference index* only — its `Submitted` state is meaningless, the workflow never transitions),
> [`docs/ARCHITECTURE-DESIGN-WISDOM.md`](ARCHITECTURE-DESIGN-WISDOM.md) §4 (the verified improvement backlog),
> and the assistant memory (context). **When they disagree, this file wins.**
> Re-verify every "done/open" against `git log` before asserting it — never carry a claim forward.
> _Last reconciled: **2026-07-24** (verified vs `git log origin/main` + 0 open MRs + deployed tag. Secret plane:
> !552–!555 MERGED, box `582b6f33` (!556), TG_SECRET_POLICY=enforce LIVE, gate-enumerated secrets + LiteLLM
> (master+4 providers) on OpenBao. Host-reader identity CORRECTED to root|one_key (my prior tgread
> over-provisioning UNDONE — see [[estate-mutations-i-have-made]]). Earlier handoffs SUPERSEDED where conflicting._

> **2026-07-24 side-fix:** boot-posture publish ordering (`b38afd5`) DEPLOYED+healthy (worker:b38afd50, restarts=0) — console no longer flaps to a transient Shadow after each restart. Review APPROVE 0.99.

> **2026-07-24 B2 MERGED (`926450d`):** `feature/regime-per-target-ssh-lane` (spec/017 REQ-1717) — the native-ssh actuation lane can now resolve its effect leaf PER ACTION TARGET (fleet restart/start-service), gated behind `TG_ACTUATION_SSH_PER_TARGET` (default OFF → dormant/behaviour-preserving). Plane-split preserved (actuation identity ≠ read identity); B1 host-match gate stays defense-in-depth. make all green (lattice 5124); 4 unit + 1 acceptance test. **Arming = CONSEQUENTIAL (flag on + mutation + fleet known_hosts) → owner-surfaced.** 23-agent adversarial review = APPROVE (0 confirmed); 3 hardening lows addressed. **P3 CODE COMPLETE** (detection + B1 gate + B2 lane all merged) — only the ARMING (mutation flip + config) remains = OWNER.

> **2026-07-24 RECONCILE — single-host canary ARMED (inert), by a parallel session (NOT me):** box `.env` (mtime 15:20Z) now has `TG_ACTUATION_SSH_HOST=dc1librespeed01.example.net` + `IDENTITY=root` + `KEY=bao:secret/data/tg/actuator#key` (DEDICATED actuator key ≠ read key ✅ plane-split) + `ALLOWED_UNITS=nginx`. **Mutation OFF → INERT.** My deployed B1 host-match gate makes it safe. **CONCERN:** host is FQDN but action targets are SHORT form → B1's exact-match would REFUSE until aligned. **B2 (per-target) SUPERSEDES this** — it uses Action.Target directly as the host, so B1 passes by construction + it works fleet-wide. Flipping mutation = OWNER decision (surfaced, not done). See [[estate-mutations-i-have-made]].

> **2026-07-24 ★ POSTURE CORRECTION (important, honest):** the live actuation posture is `mode=Semi-auto, may_actuate=TRUE`, SSH effect leaf `read_only=FALSE` — the estate IS in the standing owner-authorized MUTATION-ON posture (Proxmox guest heals are live). My mid-session "mutation OFF" notes were reading the PRE-BindMode transient (the display bug the boot-posture fix corrected). **Consequence:** with B2 regime-routing deployed, an SSH service-restart now routes to the native-ssh lane = the sibling-configured single-host canary (librespeed01+nginx, dedicated bao key) → the SSH auto-heal path is WIRED + mutation-capable, guarded by B1 host-match (should refuse the FQDN/short name-form mismatch, fail-safe), the nginx allowlist, graduation, and the single-host binding. 0 regime_actuation rows so far. **I flipped NOTHING** — B2 fleet path dormant (flag unset). CONSEQUENTIAL levers that would make SSH auto-heal FIRE (owner decisions, NOT done): align the canary name-form (short), or flip TG_ACTUATION_SSH_PER_TARGET (fleet via B2). See [[estate-mutations-i-have-made]].

> **2026-07-24 ★★ FLYWHEEL FIRST CYCLE = PROVEN LIVE (was mis-tracked as "pending"):** the self-improvement flywheel (TG-130) ran ONE full generate→trial→judge→graduate cycle end-to-end and AUTO-GRADUATED a winner. Verified in the live DB: `skill_trial` 5 (triage-protocol) completed → `skill_version` 21 (1.1.0-cand3, author=flywheel) = **status=production** (a model-generated prompt rewrite is the LIVE triage body); `governance_ledger` seq 582 (supersede) + 583 (graduation: mean 4.324 vs control 3.857, p=0.0033) hash-chained. 4 crons live (gen 04:37, finalizer 03:17, judge 2h); trial 8 ACTIVE now (self-driving). **CONSEQUENTIAL — surfaced:** auto-graduation has NO human in the path (autonomy grant covers it, but the owner should KNOW a live reasoning prompt self-changed). **Residual (QUALITY, not wiring):** the loop wastes trials on the unmovable `falsifiable_prediction` monoculture (6/7 trials, 5 aborted) — the 50-day TG_SKILL_TRIAL_DURATION defeats the anti-starvation steering (flywheel.go:123-170); win was at TUNED-DOWN rigor (15/arm vs default 30) + knobs only in box .env (repo defaults would RE-STARVE); durability watch for v21 OPEN til 2026-07-30; the "lesson"-from-resolved-incident trigger is UNBUILT. **TARGETING-FIX MERGED+DEPLOYED+VERIFIED (`b6c11cc4`, restarts=0, skillgen cron armed 04:37):** dimensionFillsTrial now projects over min(TrialDuration, FillHorizon=14d) so the loop steers to dense completable dimensions (produces winners) instead of the unmovable falsifiable_prediction; compose+.env.example reconciled. Residual (owner/design): tuned-down rigor (15 vs 30 samples/arm) reconcile; the resolved-incident "lesson" trigger (unbuilt); durability watch for v21 (open til 07-30).

> **2026-07-24 ★ CONFIDENCE SCALAR PERSISTED (T-020-1 / spec/020 REQ-2003) — MERGED (`89bbac2`) + deploying:** `session_triage.confidence` persisted 0 on every row because the workflow read `inv.Proposal.Confidence` (a NESTED key the model never emits) instead of the top-level directive value the agent loop already uses LIVE to gate (stop<0.5 / escalate<0.7 / propose≥0.7). Fixed the field-identity drop end-to-end: `agent/loop.go` emits it → `activities.go` carries it on `InvestigateResult` → `workflow.go:350` now reads `inv.Confidence` (not the always-zero nested field). **Observability only — the confidence GATE stays OFF** (no verdict/actuation change); a prod-shaped fixture proves it (0 under old wiring, 0.85 after). Unblocks the tracer "Conf" column + reliability-curve calibration ([[precedent-reaches-prompt-but-inert-on-confidence]]). Adversarial review APPROVE. **DEPLOYED 2026-07-24 (`worker:0cd0156d`, includes `89bbac2`).** Unit-proven (prod-shaped fixture: 0 under old wiring, 0.85 after). Live-organic verify PENDING: all 481 existing rows are still `confidence=0` (pre-fix baseline confirmed on the box DB); the NEXT organic proposing session records non-zero (not forcing a guinea-pig fault for purely-confirmatory value — the wiring is unit-proven).

> **2026-07-24 Claude-Code-env audit follow-through (dev-tooling, mirror-stripped — NOT shipped artifacts):** committed a shared repo `.claude/` surface (5 dev-workflow subagents + README + shared `.gitignore` runtime-scratch rules) so a fresh clone / teammate / parallel session inherits the harness (`8a0508a`); enabled the `gopls` LSP for Go symbol-nav; re-established the codegraph watch (full re-scan deferred — marginal now); slimmed the always-loaded `AGENTS.md` 257→103 lines (`e9146e2`) — cut a 169-line churning per-spec inventory that duplicated THIS board + carried stale stamps. All internal-only (github-sync strips the `.claude/` tree).

## ★★★ PRIORITY BOARD — leverage-ranked, ETA'd (2026-07-24 reconcile) — DRIVE FROM THIS

**ETAs are in focused WORK-SESSIONS (I do not control heartbeat cadence or owner availability), not
wall-clock. Owner-gated items are BLOCKED-ON-OWNER — they are not my backlog until unblocked.**

**Deployed stamp (verify each reconcile):** box `0f96747c` == DEPLOYED+healthy (worker+grounder+console, up 2026-07-24); includes the confidence-scalar fix (`89bbac2`, T-020-1) + TG-179a (`d5beed1`). MERGED + deploying (in pipeline order): attribution host-case fold (`656c152`, security APPROVE) → calibrator confidence>0 filter (`8b03abc`, review APPROVE). **Observability thread this session:** persist confidence (T-020-1) → its consumer the calibrator now excludes the 197 pre-fix `confidence=0` garbage samples (honest "no evidence yet" until real proposing samples accrue) → attribution matcher hardened (host case-fold). All observe-only; gates unchanged. P3 CODE-COMPLETE (detection + B1 host-match + B2 per-target lane ALL merged — `926450d`); only ARMING (mutation flip + actuator config) remains = OWNER. **`TG_SECRET_POLICY=
enforce` LIVE** on both binaries (restarts=0, preflight GREEN, no fatal). worker+grounder+console current, all
running. **Gate-enumerated secret plane COMPLETE:** every secret the boot GATE enumerates resolves from OpenBao
(`bao:`) or is exempt — gate at **0 plaintext violations**, the deployment REFUSES plaintext (for what it
polices). This sprint migrated the 4 file:-sealed secrets (awx/ldap-bind/proxmox/ssh-key) + LibreNMS ingest
byte-identical (files SHREDDED), delivered LITELLM_MASTER_KEY **+ the 4 real provider keys** (kimi/zai/deepseek/
mistral) via the tg-secretenv `/dev/shm` shim (litellm serves 5 models @ HTTP 200), and flipped warn→enforce.
Fixes: OpenBao source mis-prefix ("entry X has no user")→`tg/hosts`+code (!553); bind_dn clobber-then-restored
(KV-v2 POST replaces all fields); Docker `type=tmpfs` named-volume-not-shared→/dev/shm bind-mount.
**HONEST GAP (P-secret-residual):** the gate polices only its ENUMERATED set, so a few plaintext secrets it
does NOT enumerate survive enforce — TG_AM_INGEST_TOKEN (DB sources-row ref), LIBRENMS_TOKEN/_GR_TOKEN
(compound string), AWX_ADMIN_PASSWORD. Migrating them needs ONLY a fresh token (NO code — see the row below). **OWNER: ROTATE the OpenBao
root token** (exposed in chat + tmpfs during the migration; now shredded from box tmpfs).


| # | Item | Status | ETA | Why this rank (leverage) | Blocked on |
|---|---|---|---|---|---|
| ~~P0~~ | **Novelty stable-key fix** (subject-key, Plan A, TG-124) | ✅ **DONE — MERGED (`2ae8ae8`) + DEPLOYED (`worker:2ae8ae81`) + LIVE-VERIFIED** 2026-07-23 | shipped | THE MULTIPLIER, now live: eval-gate PASS (4.41/4.41) → CI green → GitOps AWX deploy → mealie01 re-fault → **hands-off `classify:AUTO`** (env.Host matched its guest-keyed row), graduation intact. Every future de-novel keys on the stable subject. | — |
| ~~P1~~ | **Clear-source robustness** (recovery-transition fast path, Plan B) | ✅ **DONE — MERGED (!536) + DEPLOYED (`5e80ff03`)** 2026-07-23; follow-ons also shipped: !538 belt-anchor obs · !539 fresh-verdict writeback · !540 seed∪maintained corpus split | shipped (est 1, actual 1) | Completed the writeback-miss fix; C2 unblocked and DONE (!537). | — |
| ~~P2~~ | **Actor-attribution P1** (spec/023: WHO-is-the-actor) | ✅ **DONE — MERGED (!541, `7ddfab9`) + DEPLOYED (`7ddfab9b`) + VERIFIED** 2026-07-24 (dormant: reader unarmed). Re-run adversarial review (82-agent workflow) returned **23 confirmed findings → DO-NOT-MERGE**: the PVE reader was INERT in prod (keyed `vmid` not `id`; self-identity missed the `tokenid` split; self-signed TLS) + a CRITICAL carve-out could mask an intruder as authorized-test. All fixed vs the LIVE PVE API (`aa7b54e`): per-record classification, suspicious dominates the carve-out, `id`/`tokenid` wire shape, window-bounded fetch, panic on short error body, resolved-token gate, TLS. | shipped (est 2–3, **actual ~1** build + ~0.5 review-fix) | Owner-directed epic. **CALIBRATION DATUM:** prior "17/20 proven, at merge gate" was a false-done — the review caught it; discount self-reported "done" until the gate passes. | — |
| ~~P2-arm~~ | **Arm attribution live** | ✅ **DONE + LIVE-VERIFIED 2026-07-24** (owner: "arm it"). Override in `/secrets`, reader armed (no compose change — used the existing RO /secrets bind), test-guest e2e proof: authorized-test attribution recorded + healed. Reversible via the two `.env` vars. | shipped (est 0.5, actual 0.5) | — | — |
| ~~P2b-core~~ | **Attribution Phase 2 CORE** — spec (!542) + journal/sudo reader (!543, T-023-8) + LDAP/FreeIPA identity seam (!544, T-023-12, promotion+demotion) | ✅ **DONE — MERGED + DEPLOYED (dormant)** 2026-07-24. Designed via a judged 3-approach workflow; each reader grounded on the REAL API (journalctl -o json / live FreeIPA schema) BEFORE build (the inert-reader lesson); each adversarially reviewed (journal APPROVE; identity APPROVE 0.92). | shipped (est 2–3, **actual ~1.5**) | Answered the owner's "why only proxmox users?" — WHO now spans PVE + host-sudo + directory identity. | — |
| **P2b-arm** | **Arm the journal + LDAP readers live** | **LDAP + PVE + JOURNAL armed (2026-07-24).** DESIGN CORRECTION (owner-caught): TG must NEVER provision host accounts — it CONSUMES an operator-supplied `(user, key)`. A prior session of mine had wrongly `useradd`'d a `tgread` user on both syslog servers; **UNDONE** — all host readers (journal/hostdiag/syslogng) now use `root\|file:/secrets/one_key` (the operator's existing admin key TG already holds), `tgread` users+group+key REMOVED from both servers, hosts restored to out-of-box (see [[estate-mutations-i-have-made]]). Journal reader armed via root\|one_key on dc1syslogng01 (root reads sudo events — proven on both servers). **GROUNDED 2026-07-24 (before any e2e — inert-reader discipline):** the journal reader IS wired into triage (`InvestigateActivity → AttributeActivity → ActorReaders.Read`, version-guarded, 30-min window; persists to `session_triage.actor_attribution`/`actor_evidence`, mig 0035) and armed on the box (`TG_JOURNAL_DEPLOYMENTS=dc1\|dc1syslogng01`). Grounding surfaced + FIXED a latent inert-reader footgun (`656c152`, security review APPROVE, deploying): `core/attribution/attribution.go` matched `evidence.Target` vs the subject host CASE-SENSITIVELY while the journal reader lowercases its Target — a mixed-case host would silently drop sudo evidence → `unattributable`, MASKING a suspicious actor (same class as the PVE `user!tokenid` bug). Fixed at the matcher (fold all HOST comparisons; ACTOR matching stays case-sensitive, regression-tested). **PENDING (blocked-on-safe-target, SURFACED):** the triage-driven e2e needs an incident on `dc1syslogng01` — but that is a **syslog server (infra), NOT a least-important guinea-pig**, so I will NOT fault it unilaterally; it needs EITHER broadening the allowlist to a guinea-pig host first (an arming/config change) OR an owner nod to touch the syslog host. **SECURITY NOTE:** root\|one_key puts the admin key on the read path — a security-conscious operator can supply a dedicated read identity instead. | ~0.5 session (e2e) | Live estate-wide WHO. | me |
| ~~BUG-hostdiag-inert~~ | **hostdiag reader was INERT** — TG_HOSTDIAG_DEPLOYMENTS used `root\|tg-syslog-ro` (root-denied) + its `journalctl` step needs journal access. ✅ **FIXED 2026-07-24:** box `.env` identity → `root\|file:/secrets/one_key` (root reads df/du/journalctl); worker re-registered "4 host-diagnostics tools". Compose ship-default is empty (no repo change). | shipped — (live tool-call e2e still nice-to-have) | ~0.3 | Host-diag tools now have a real identity. | — |
| ~~P-hostreader-doc~~ | **Doc the principle:** TG consumes an operator-supplied host `(user, key)`; never provisions host accounts. ✅ **DONE (`f28e28a`):** rewrote deploy/.env.example host-reader section — the principle + both options (root\|one_key vs an operator-provisioned read user) + security trade-off; filled the TG_HOSTDIAG/JOURNAL doc gap; removed estate hostnames from the example. make all green (STONITH). Optional follow-up: a note in spec/016. | shipped | Prevents a future session re-provisioning. | — |
| ~~P2b-rest-1023~~ | **netbox+awx actor-evidence readers (T-023-10)** | ✅ **DONE — MERGED (!556, `582b6f33`)** 2026-07-24. Grounded live (actor + target-matching confirmed on real records) BEFORE build; 2 read-only readers wired DORMANT behind opt-in flags (TG_AWX/NETBOX_ACTOREVIDENCE). Adversarial review found 4 real REQ-2306/2307 defects (blank-actor, running-job attribution, silent pagination truncation ×2, per-job-error-kills-all) → all FIXED + tested → re-review APPROVE 0.98. spec T-023-10 done, 2 acceptance scenarios realized. | shipped | Extends WHO to CMDB + automation. | — (arming = owner) |
| ~~P2b-rest-1123~~ | **gitops-mr reader (T-023-11)** | ✅ **DONE — MERGED (!557, `ee287f72`)** 2026-07-24. Grounded live → built (target-agnostic, deploy/**-touching merged MRs → merged_by; X-Next-Page pagination for MR list AND diffs; Covered=FALSE — candidate evidence, not per-target coverage) → adversarial review REQUEST-CHANGES (3 findings: Covered-false, diffs-pagination, per-MR-error-skip-not-abort) → all fixed → re-review APPROVE 0.98. **docker DROPPED** (grounded no-human-actor; spec amended with rationale). Dormant behind TG_GITOPSMR_ACTOREVIDENCE + operator RO GitLab token. spec T-023-11 done. | shipped | GitOps deploy attribution. | — (arming = owner RO token) |
| **P2b-rest-923** | k8s-audit reader (T-023-9) — **BLOCKED (grounded):** apiserver audit is DISABLED on both clusters (no --audit-* flags, no sink); fallbacks only expose controller/SA identities (argocd), not humans. Building now = INERT. | BLOCKED | — | Owner must enable apiserver audit + a Loki scrape first. | **OWNER** (enable k8s audit) |
| ~~P-secret-INC1~~ | **Secret plane INC-0/1** — spec/024 MERGED (!546); OpenBao migration batch 1+2: 5 TG SecretRef secrets env:→bao: (pve, ldap-dn, session, operator, admin), verified byte-identical, ZERO downtime; root token owner-supplied then shredded (ROTATE it). | ✅ **DONE** 2026-07-24 (bao: refs 1→7) | shipped | Owner-directed: a fresh install must refuse plaintext .env. | — |
| ~~P-secret-gate~~ | Secret plane INC-2/3 — TG_SECRET_POLICY boot gate (off/warn/enforce) on the GROUNDER | ✅ **DONE — MERGED (!547, `0768555`)** 2026-07-24. Adversarial review (0.95) caught a real REQ-2402 completeness gap (3 inline bootstrap/CA refs unseen by the gate) → fixed + regression-tested. Default off = behaviour-neutral. | shipped | The forcing function: a fresh install can REFUSE plaintext. | — |
| ~~P-secret-gate2~~ | Secret-policy gate on BOTH binaries | ✅ **DONE — grounder (!547) + worker (!548, `6963be14`) MERGED** 2026-07-24; worker enumerates ~30 refs with a SOURCE-SCAN completeness test (automates the drift lesson); review APPROVE 0.95. Default off. | shipped | The whole stack can now refuse plaintext under enforce. | — |
| ~~P-secret-warn~~ | Flip the deployed gate off→WARN on the box | ✅ **DONE + VERIFIED LIVE** 2026-07-24: `TG_SECRET_POLICY=warn` on both binaries (`6963be14`), boot logs now surface every plaintext business secret, health unaffected (restarts=0, /healthz OK). Still-plaintext (the remaining migration): file:-sealed = actuation-ssh-key, awx, ldap-bind-pw, proxmox; env: = admin-token, litellm-key + notifier tokens (email/github/jira/langfuse/librenms-ingest/healthchecks). | shipped | Middle rung of off→warn→enforce; makes the posture visible. | — |
| ~~P-secret-wrapapprole~~ | **Secret-zero: wrapped-AppRole vault auth** (INC-4) — response-wrapped single-use SecretID on tmpfs, no durable secret_id on disk; relocates the trust root to the orchestrator honestly | ✅ **DONE — MERGED (!552, `3ade79a2`)** 2026-07-24; review APPROVE 0.98 (all fail-closed paths, single-use tamper, no leak, honest tests). | shipped | Retires the durable OpenBao secret-zero for Compose deploys. | — |
| ~~P-secret-enforce-ready~~ | **Enforce-readiness** (!553) — 3 hardcoded env: refs → overridable; 14 optional integration refs → `${TG_X_REF:-${RAW:+env:RAW}}` (unconfigured→empty→gate-skips); OpenBao credential-SOURCE empty-prefix→disabled | ✅ **DONE — MERGED (!553, `811cc70c`) + DEPLOYED + VERIFIED** 2026-07-24: box warn 15→1; review adjudicated (one finding was intended behavior, documented + Langfuse consistency fix). | shipped | Makes `enforce` reachable for everything except the litellm key. | — |
| ~~P-secret-filemig~~ | **Migrate the 4 file:-sealed business secrets + LibreNMS ingest to bao:** (awx, ldap-bind-pw, proxmox, actuation-ssh-key, librenms-ingest) | ✅ **DONE + VERIFIED LIVE** 2026-07-24: each written+read-back byte-identical via RO token, flipped, functionally re-verified (awx source / LDAP bind / proxmox actuator allowed_guests=17 / native-ssh + PVE reader / librenms push-auth), plaintext files SHREDDED. bind_dn clobber caught+restored (KV-v2 POST replaces all fields). | shipped | Removes the on-disk business secrets. | — |
| ~~P-secret-litellm~~ | **LiteLLM key from OpenBao** (INC-4 shim, !554) — tg-secretenv init resolves the key into a /dev/shm tmpfs drop; litellm entrypoint sources it | ✅ **DONE + VERIFIED LIVE** 2026-07-24 (`7b192bd2`): init "resolved 1 secret … LITELLM_MASTER_KEY" from bao, litellm "startup complete", plaintext `LITELLM_MASTER_KEY` REMOVED from box .env, `TG_LITELLM_KEY_REF`→bao:; litellm auth HTTP 200 with the shim key / rejects a wrong key. Named-tmpfs-volume bug caught+fixed → /dev/shm host bind-mount. | shipped | THE LAST plaintext business secret — gone. | — |
| ~~P-secret-enforce~~ | Flip `TG_SECRET_POLICY` warn→**enforce** on the box | ✅ **DONE + VERIFIED LIVE** 2026-07-24: gate at **0 plaintext business-secret violations**; `enforce` set on both binaries → both boot clean (restarts=0, running), preflight GREEN, no fatal. **The deployment now REFUSES plaintext business secrets** (the owner's forcing function is live). Reversible → warn. | shipped | The forcing function is live. | — |
| ~~P-secret-provider~~ | **LiteLLM provider keys from OpenBao** (extend the shim, !555) — tg-secretenv `-skip-empty` + init lists every provider with an empty default ref | ✅ **DONE + VERIFIED LIVE** 2026-07-24 (`8522ef4b`): 4 real provider keys (kimi/zai/deepseek/mistral) written byte-identical (master_key preserved via read-merge-write), box refs flipped to bao:, 7 plaintext lines (incl. dups) REMOVED; init "resolved 5 secret(s)", litellm serves **5 models @ HTTP 200**, all services restarts=0. Root token SHREDDED after. | shipped | The real provider secrets, off plaintext. | — |
| **P-secret-residual** | **HONEST GAP: non-enumerated plaintext secrets remain** (the secret GATE polices only its enumerated set, so `enforce` passes while these stay plaintext — the INC-2 completeness hole). Residuals on the box .env: **TG_AM_INGEST_TOKEN** (Alertmanager ingest bearer — ref lives in a DB `sources` row `ingest_token_ref=env:…`, not a .env `_REF`); **LIBRENMS_TOKEN/_GR_TOKEN** (the `TG_LIBRENMS_DEPLOYMENTS` tokenref field — site|url|**tokenref**[|tz] — ALREADY resolves as a SecretRef, currently env:LIBRENMS_TOKEN/_GR_TOKEN); **AWX_ADMIN_PASSWORD** (AWX-server admin cred, not passed to any TG container). Stale **AWX_TOKEN** copy already removed. | pending | ~1–2 sessions | Closes the *true* "no plaintext" goal + the gate hole. CORRECTION: NO code needed — every ref already accepts bao: (LibreNMS tokenref, AM ingest_token_ref). Migrating = ONLY a FRESH OpenBao token (post-rotation) to write the 3 values + flip env:→bao: + drop plaintext; THEN add them to the gate enumeration (can't before — enforce would fail-closed). Purely owner-token-blocked. | **OWNER** (rotate + re-supply token; then ~15 min migration) |
| **P-secret-followups** | INC-5/6 passbolt:/vw: resolvers (need a live instance to ground against) | pending | ~2–3 sessions | Homelab backends. | me / **OWNER** (live vw/passbolt instances) |
| **P-secret-rotate** | **ROTATE the OpenBao root token** (`s.…` was pasted in chat + staged on box tmpfs during the migration; now shredded from /dev/shm) | **BLOCKED-ON-OWNER** | — | The token was exposed during the live migration. | **OWNER** |
| ~~P-lint~~ | **STONITH lint** (check 6/6) — fails the build on any NEW estate literal in a shipped Go binary / compose default | ✅ **DONE — MERGED (!545, `c8ac7af`)** 2026-07-24; shrink-only ratchet. Negative-test proven. | shipped | Automates the owner STONITH rule so it can't be forgotten. | — |
| ~~P-stonith-ldap~~ | Shrink the STONITH baseline — externalize the LDAP estate DNs | ✅ **DONE — MERGED (!549, `f989a8f5`)** 2026-07-24: **baseline 9→0**. Estate LDAP base DN (dc=sec,…) + FreeIPA replica URLs, previously in 5 shipped artifacts, now deploy env (TG_LDAP_USER_BASE/GROUP_BASE/URLs/DN-template); box .env pre-set BEFORE merge so login keeps working; generic defaults fail-closed on no-URLs; review APPROVE no-findings. **Deploy VERIFIED 2026-07-24** on `ee287f72`: all 4 `TG_LDAP_*` non-empty in box .env, grounder Up + console serving (LDAP login path live on externalized DNs). | shipped | Full install-neutrality on the shipped LDAP surface — a fresh install carries NO trace of this estate's directory. | — |
| **P3** | restart-service re-earn (MATCHED fault = service-stop) — **CYCLE-1 RUN + GROUNDED 2026-07-24** (fault RECOVERED): injected nginx-stop on dc1librespeed01 (guinea-pig, ledgered); LibreNMS detected it (http svc_status=2), rule-9 fired after its 300s **issue**-delay (delay gates the notification ISSUE at opened+300, not just the raise) → transport-7 POST → **TG ingested** (session `librenms-dc1-177364`, DEEP_INVESTIGATION) → investigated → **outcome `no-proposal:stop`** (band empty). **ROOT CAUSE (grounded):** TG was BLIND to the guest's in-guest state — **(A)** `TG_HOSTDIAG_DEPLOYMENTS` is EMPTY so the read-only host-diag reader has no SSH identity for the pool → `check-host-services` couldn't verify librespeed01's systemd; TG saw device-ICMP-up + no eventlog + couldn't confirm a down service → correctly-conservative stop. (A clean `systemctl stop` = `inactive(dead)`, `UnitFileState=enabled`, NOT `failed`.) **(B)** `TG_ACTUATION_SSH_HOST`/`ALLOWED_UNITS`/`SSH_KEY` EMPTY → no restart-service actuation route even once confirmed. **`one_key` CONFIRMED root-authorized on librespeed01** (direct + via pve01 jump) → both gaps are config/arming within standing ACL-extension authority, NOT owner-blocked. **DETECTION-FIX MERGED (`a930e54`, deploying)** — grounded that hostdiag WAS wired for the pool (glob `dc1*`, host-key pinned, one_key root-auth) but `check-host-services` only listed failed + inactive units with NO enable-state, so the one down service was invisible among 58 inactive units. Governed MR (`feature/hostdiag-enabled-down-services`): the tool now also reads the enabled unit-files baseline + derives `enabled ∩ (failed∪inactive)` = named restart-service candidates (truncation-visible, fail-safe, live-validated glyph strip). Adversarial review APPROVE 0.85 → truncation-note + tests added. **DETECTION FIX VERIFIED E2E 2026-07-24** (`a930e542` live): re-faulted nginx → LibreNMS fired (rule9 raise gated by `delay:300`, sustained-crit+poll trigger) → dispatched → TG session `librenms-dc1-177368` **`band=AUTO outcome=proposed op=start`** (vs the prior `no-proposal:stop`). Conclusion quotes the new synthesizer verbatim: *"nginx.service is ENABLED in the should-run baseline but currently 'loaded inactive dead' and listed under 'down services (enabled but NOT running)' … directly explains the failing service check."* TG now DETECTS → names the exact unit → diagnoses → **proposes the heal at band=AUTO** (correctly `start`, not restart, for a stopped unit). Journal reader failed-closed advisory (librespeed01 ∉ its allowlist) — correct. Fault recovered (nginx active, rule9 state=0). **The only remaining gap = blocker (B):** TG proposes AUTO but can't execute — the SSH mutate seam is inert. Blocker **(B) actuation route = ARCHITECTURAL + a SAFETY GATE, not just config.** GROUNDED: the interceptor calls `actuator.Exec(ctx, r.Argv, r.Stdin)` — argv ONLY (`systemctl start nginx.service`), NO host; the `ssh.Module` wraps it as `identity@m.host` (the CONFIGURED `TG_ACTUATION_SSH_HOST`), IGNORING the action's `Target`, and there is **no guard that the actuator host == the action target**. So arming the single-host `TG_ACTUATION_SSH_HOST` LIVE is UNSAFE: any OTHER host's admitted service-restart would execute on the configured host (mis-actuation). The #23 seam is a single-host *canary*, not a fleet actuator. **⇒ decided NOT to arm it live.** Safe path = a governed MR: **(B1)** add a host-match refusal guard (interceptor refuses when `Action.Target ≠ actuator.Host()` — purely fail-closed, can only restrict) → **(B2)** per-target host+identity resolution via the credential engine (mirror hostdiag's IdentityResolver) so restart/start-service works fleet-wide + safely. THEN arming (mutation-on + config) is the CONSEQUENTIAL owner-surfaced step. (Guest-lifecycle heals are fleet-wide today only because they use the Proxmox-API actuator, not SSH.) **B1 MERGED 2026-07-24 (239575b, spec/013 REQ-1219):** host-match gate refuses target≠bound-host; 29-agent adversarial review workflow = APPROVE (1 low finding fixed). **B2 GROUNDED:** the regime engine (spec/017, wired) resolves target→lane + the credential engine (spec/016) resolves target→identity — B2 = build a per-target native-ssh leaf instead of the static host; B1 stays defense-in-depth. | ✅ CODE COMPLETE (detect+B1+B2 merged); arming=owner | <1 session | 2nd op-class ladder to AUTO. | me |
| **P4** | Re-run Tier-B sweep (P0 shipped; now sequenced behind P2 deploy) | pool vetted (13 valid) | ongoing, low effort | Keys are PERMANENT post-P0. Re-baseline AFTER !541 deploys — attribution adds authorized-test carve-out semantics to every injected fault; pre-attribution sweeps would measure the old behavior. | P2 deploy |
| **P5** | C-candidates: severity-floor→dedup (C1) · ~~decision auto-close (C2)~~ ✅ DONE (!537) · standing-vote reconsult (C3) | C1 anchored; C3 needs owner design note | ~1 session each | Governance hardening. | C3: **OWNER** note |
| — | Service-fault detection reliability (rule-9 delay/flakiness) | open | ~2 sessions | Unblocks the service-down fault class in rotation. | me |
| B | TG-118 token rotations | owner-gated | — | Security; "highest-urgency untracked". | **OWNER** |
| B | TG-144 Groundnet activation | owner-gated | — | Federation. | **OWNER** (repo+CI vars+DOI) |
| B | Exceed-proof (organic pairs + 2–3 external benchmarks) | accruing | v1.0 gate (weeks) | The actual "exceed" milestone; evidence not code. Injected pairs are contaminated (predecessor recognizes the harness estate-wide) — needs organic pairs or an owner-sanctioned blind protocol. | time + owner |

## ★★ HOLISTIC MILESTONE MAP — the whole-project zoom-OUT (the board above is the zoom-IN)

_Grounded 2026-07-24 from a 193-epic survey of docs/ROADMAP.md + this file + spec/00-INDEX.md + the vision
memories (workflow wf_87702262). Also in memory [[holistic-project-milestone-map]]. The **v1.0 gate = the
exceed-proof.** Keep status current as milestones move._

```
TERRITORY GROUNDER — "a governed robot SRE that EXCEEDS the predecessor"
├─ ✅ FOUNDATIONS ................ DONE  (SDD lockstep 007 · interface contracts+auth 006 · typed spine)
├─ ✅ HEALING CORE (~53) ......... DONE + live-proven  (classifier 001 · prediction gate 002 · Go ReAct
│      agent 011 · Temporal runner 012 · actuation interceptor/mode chokepoint 013 · policy engine 015 ·
│      append-only ledger)   └─ 🔄 still growing: OP-CLASS BREADTH ladder (what it can auto-heal)
├─ ✅ ATTRIBUTION — WHO-did-it (023) ... DONE (this session)   └─ 🔜 arming journal+LDAP readers = owner-gated
├─ 🔄 SELF-IMPROVEMENT FLYWHEEL (TG-130) ... IN PROGRESS  ← the ambitious core (skill store · auto-generate
│      candidates from eval failures · trial · auto-graduate winners)
├─ ✅ SECRET PLANE (024) ......... ~DONE  (plaintext business secrets killed · gate=ENFORCE live @0 violations · residuals owner-token-blocked)
├─ ✅ INSTALL-NEUTRALITY (STONITH) . DONE  (lint check 6/6 green · LDAP externalized + deploy-verified, baseline 0)
├─ 🔄 KNOWLEDGE / DISTILLATION ... mostly done  (RAG plane · corpus seed · infragraph · predecessor distill)
├─ 🔬 OBSERVABILITY / TRACER (020) . in progress  (packet-tracer inspector · confidence calibration · self-mon)
├─ 🔄 UX / CONSOLE (010) ......... PARTIAL  (15-surface console; config-WRITE path + skills CRUD PLANNED)
├─ 🔮 CONTROL-PLANE / tgctl ...... FUTURE v2 north-star  (declarative API · regime engine 017 · k8s/Helm 009 ·
│      worker-role split · cost/budget engine)
├─ 🎯 BENCHMARKS / EXCEED-PROOF .. IN PROGRESS  ← **THE v1.0 GATE** (shadowbench · ladder · ITBench-AA/SREGym/
│      RAGAS · publish scores + groundnet DOI)
├─ 🛡️ SECURITY-POSTURE (TG-153) .. partial  (trusted-substrate: 2 Crit+7 High; secret plane is one slice)
└─ 🔮 FEDERATION / GROUNDNET (021)  FAR-FUTURE north-star  (sovereign instances share re-validated distillate)
```

**ETAs (focused work-sessions; ⚑=owner-gated):** secret-residuals ~owner-token ⚑ · attribution-arm ~0.5 ⚑ · op-class
breadth 1–3 · flywheel-one-cycle 3–5 · tracer/calibration 2–3 · UX config-write 3–5 · exceed-proof WEEKS ⚑ ·
tgctl large(v2) ⚑ · federation far-future ⚑.

### The anti-drift discipline (the METHOD — apply every heartbeat, keep applying)
1. **SSOT-FIRST, three-surface reconcile.** Before doing work each heartbeat/session, reconcile THIS
   board against all three truth surfaces: `git log` (what merged), the open-MR list (what's in
   flight), and the **deployed image tag on the box** (what's live). **A stale board IS the drift**
   (4h stale on 2026-07-23 during waves; stale again after the 2026-07-24 crash — both caught by
   this rule, which is why it is rule 1).
2. **Leverage-ranked, not ease-ranked.** Work the top UNBLOCKED item by leverage. My known bias is toward the easy repeatable loop (fault waves); rank fights it.
3. **Leverage test before any DEFAULT/repeatable action.** Before another fault wave ask: "is a higher-leverage item unblocked right now?" If yes, do that. Waves are the default ONLY when nothing higher is actionable.
4. **ETAs in work-sessions; mark OWNER-gated separately** so blocked-on-owner items never masquerade as my backlog. Record **est vs actual** per shipped row — the calibration data is the only way ETAs improve (current bias: ~2× conservative on build work).
5. **Every status report surfaces board top-3 + what moved**, so drift is visible to the owner in real time.
6. **Update-on-EVENT, not on-exit.** Bump this board at the MOMENT of each state change
   (merge, deploy, live-verify) — never batched to session end. Sessions die mid-flight
   (2026-07-24: API-balance crash killed the reviewer one step before its verdict and left this
   board claiming P1 "shipping" 18h after it was merged+deployed). An on-event board survives any
   crash; an on-exit board is exactly as stale as the last crash.

## ★★★ SESSION HANDOFF — (2026-07-23 00:59, SUPERSEDED by the board above where it conflicts)

**Read these memories first:** `STANDING-AUTHORIZATION-full-autonomy-actuate-estate`,
`organic-guest-down-autoheal-proven-live`, `autonomous-operator-approve-lever`,
`shadowbench-predecessor-recognizes-guineapigs`, `shadowbench-0miss-rootcause-librenms`,
`service-fault-detection-mechanics`, `out-of-box-superiority-plan`.

**⚡ IN-FLIGHT — do this FIRST:** MR **!529** *"fix(agent): recover from an unknown tool name instead of
aborting triage"* is **MERGED + DEPLOYED + LIVE** — worker container on dc1tg01 now runs image
`worker:a3928b17` (the merge commit; up-to-date), grounder `/healthz`=ok, mode=Semi-auto. The only thing
left is the **behavioral e2e-verify** (deferred because rule-9 service detection has a ~300s delay):
stop nginx on librespeed01 (vmid 101020206, PVE node dc1pve01), force the rule-9 service check on that
host ONLY (`check-services.php -h` + `device:poll`, then wait ≥300s + `alerts.php`), and confirm TG now
**recovers** the tool-name miss (`get-host-services`→`check-host-services`), **investigates**, and
**proposes restart-service** — instead of the empty `no-proposal:stop` it did at session
`librenms-dc1-176856`. Restore nginx after. (Mechanics + why-this-was-the-blocker:
`service-fault-detection-mechanics`.)

**★ Owner grant is now PERMANENT (2026-07-22, emphatic; in memory + the heartbeat cron):** TG runs
Semi-auto **MUTATION ON**; TG may **actuate the WHOLE estate** for ORGANIC experience; I create/extend
policy ACLs + inject safely-revertible faults on the **least-important servers** first; full autonomy for
me AND TG; **never ask for this again**. Verified live posture: `policy_mode=Semi-auto`, worker
`runtime_posture.mutation_enabled=TRUE` (the grounder `/metrics mutation_enabled=0` is the non-actuating
API component — a red herring), 21+ historical actuations, ACLs host-unrestricted (already estate-wide).

**★ PROVEN LIVE this session (updates "binding constraint" below):** TG now auto-heals **guest-down** too,
not only restart-service. Stopped guinea-pig `dc1myspeed01` (vmid 101020203) → TG detect device-down →
severity-floor ESCALATE-to-triage → classify **AUTO** → actuate **start-guest** → guest UP →
**verified_clean**, graduation intact — hands-off, mutation ON. Graduated-AUTO op-classes:
`reload-service`, `restart-container`, `start-guest`. `restart-service`=**approve** (demoted by a
**FALSE** deviation — a verify-timing/"surprise-host" artifact of manually-forced `device:poll`; breaker
already re-armed). TG only receives LibreNMS alerts routed to **transport 7 ("territory-grounder")**;
device-down/service/sensor/mem/disk all route there.

**★ Autonomous operator-approve lever VALIDATED:** operator token on-box (`/srv/tg/deploy/.env`
`TG_OPERATOR_TOKEN`/`_NAME`) → `POST /v1/session` (header `X-TG-Operator` + `Authorization: Bearer`) mints
a session → `POST /v1/vote {external_ref,action_id,approve}` approves POLL_PAUSE proposals. Graduated
classes auto-fire WITHOUT a vote; approving accrues re-earn/graduation. No owner/LDAP needed.

**★ Exceed-proof METHODOLOGY correction:** shadowbench guinea-pig pairs are **meta-contaminated** — the
predecessor RECOGNIZES librespeed/myspeed (it has a memory note + detects the shadow-gate hook) and
short-circuits (all guinea-pig triages `couldGround=False`/POLL_PAUSE). So the guinea-pig head-to-head is
NOT a clean exceed-proof. Lean on **ORGANIC** incident pairs (both sides triage the same real alert blind)
+ **2–3 external benchmarks**. Shadowbench harness itself was FIXED this session (0/0-miss root cause =
LibreNMS detection, not triage): !527 (force-detect via `device:poll`, which runs AlertRules — legacy
`poller.php -h` never evaluates rules) + rule-25 scope (add myspeed01 device to `alert_device_map`) + !528
(preflight scope advisory). Post-fix: **6/6 discriminating pairs, 0 miss**.

**Shipped this session:** !527, !528 (shadowbench harness), !529 (agent-loop unknown-tool recovery,
deploying). Earlier in-session: credential Criticals TG-156/157 (T-022-2/4), #secrets console revamp.

**NEXT priorities (after verifying !529 e2e):** (1) service-fault auto-heal — with !529 the loop should
now ground service-down via `check-host-services` + propose restart-service; either approve it (operator
lever) or let it accrue; NOTE service-fault detection (rule 9) has a 300s delay + is flaky (see memory).
(2) op-class BREADTH — add safe reversible op-classes (start-service; activate AWX `disk-grow`) as loadable
`OpClassSpec`s, each a governed MR (spec + lockstep + adversarial review + deploy + verify). (3) keep
exercising the estate organically (least-important servers first) to accrue verified outcomes. (4) the
exceed-proof via organic pairs + external benchmarks. Every governed diff: adversarial bypass-review before
merge (this session's !529 review = SHIP, INV-08 preserved).

## Mission
Reach **and EXCEED** the predecessor `claude-gateway` with **useful, governed, graduated** autonomy —
an ASA-style access-list that EARNS trust per-op-class. Not layers-off recklessness; not an unmanageable cage.

## The binding constraint — RESOLVED (2026-07-21)
The old limiter — "0 real actuations" — is **GONE.** The canary's first governed actuation is **PROVEN**: TG
auto-healed a genuinely-failed nginx on librespeed01 (classify AUTO → execute `restart-service` → verify
MATCH, zero human vote) in Semi-auto. Every downstream engine (verify-time writer, flywheel, benchmark) now
has real executions to convert into evidence. The binding constraint is now **breadth + a proven, learning
flywheel**: TG auto-actuates ONE op-class (restart-service); the predecessor's remediations span container /
pod / lxc / vm / host / disk lifecycle. Closing that gap — **governed and earned** — is the road to "exceed".

## Honest standing vs the predecessor (2026-07-22, verified)
TG does **NOT yet exceed** `claude-gateway`. Real numbers: predecessor = **18,220** `execution_log` actions +
**434** predictions (130 scored, 57% match) over a mature production life; TG = **~20** verified actuations +
56 predictions. On the shadow triage judge TG scores **4.40** (n=404, 87% at 4–5) vs the predecessor's **4.93**
(n=28, 92% saturated at 5 — judge-saturation, TG-87). TG's genuine edge is **foundations** (non-bypassable
auth, argv-only actuation, DML-only runtime role, hash-chained tamper-ledger, fail-closed mode chokepoint, the
10-stage decision-tracer, loadable modules, graduated per-op-class trust). "Superior" therefore means
**governed + earned + verifiable + self-improving**, NEVER "acts more". A workflow-audit **verified the root
cause is NOT a thin corpus** — it is three upstream barriers: grounding/attribution, the graduation/novelty
flywheel, and op-class grammar breadth. Path to genuinely exceed = close those + accrue verified outcomes over time.

## THE ROADMAP — YouTrack epics (living; update THIS file first)
**Priority stack: Tracer (done) → Out-of-box Superiority (ACTIVE) → Groundnet (queued) → far-future visions.**

### ★ EPIC TG-130 — Out-of-box Superiority (ACTIVE)
Keep the graduation gate (TG's safety edge); make a fresh TG capable + self-improving. Every governed
actuation MR gets an adversarial bypass-review, AND the composed path an orchestrated integration audit —
both caught REAL bugs this session.
**★ HONEST out-of-box behavior (audit-verified):** the config triad (curated ruleset + graduation + mode
seeds) makes the 3 reversible classes **auto-ELIGIBLE**, NOT auto-actuating — a fresh TG actuates NOTHING
(Shadow) until the operator ALSO configures the effect leaf (`TG_ACTUATION_SSH_HOST`/`_IDENTITY`/`_KEY`/
`_KNOWN_HOSTS` + a non-empty allowlist) and escalates the mode; then it earns autonomy per `(host,rule)`
(first occurrence POLL_PAUSEs, then de-novels). See docs/OPERATOR-QUICKSTART.md. The
"deployed-semi-auto-auto-actuates" phrasing was an overclaim, retired.

**★ CURRENT PRIORITY STACK (2026-07-22 defragment — supersedes the child-number order in the table below).**
Two of the three barriers moved this session (op-class grammar → loadable multi-lane; graduation ladder →
PROVEN live, the T1 "dead-lock" REFUTED), so the dominant blocker is now **band-classification reliability**:
1. **Band reliability (barrier #1, the NEW #1).** The identical librespeed device-down banded AUTO (11:43,
   11:59) then POLL_PAUSE (12:19); `confidence` persists 0 on every row. Non-deterministic banding, not
   graduation, is what stops repeatable hands-off AUTO — one fix lifts every lane. Diagnose novelty-timing
   vs the **TG-126 manifest-band-freeze** first. See [[triage-actuation-gap-rootcause]].
2. **Persist the confidence scalar + reliability-curve calibration** (gate stays OFF) — the observability
   substrate #1 needs; it is 0 everywhere today ([[precedent-reaches-prompt-but-inert-on-confidence]]).
3. **Re-earn restart-service to AUTO + fix ingest flap-suppression** — proves the flywheel *repeats*, not
   just fires once (NOT the refuted T1 dead-lock).
4. **Breadth on the cheap post-!495 lane pattern** — proxmox start/stop/bounce, reload-service re-earn.
5. **AWX disk-grow activation** — AWX creds now STORED on-box ([[awx-credential-binding-grounded]]); still
   needs an sh-c-free lvextend/resize2fs playbook + per-host trust ACL. Predecessor's signature disk-full fault.
6. **Accrue a verified-outcome track record + shadowbench discriminating pairs** — the real exceed-proof / v1.0 gate.
7. **Owner-gated levers** — read-only lane split (spec/001 amendment), novelty-seed, [URGENT] TG-118 rotation.
8. **YouTrack hygiene** — close verified-done issues stuck in Submitted (done 2026-07-22).

| # | Issue | State | Notes |
|---|---|---|---|
| 1 | **TG-133** ConfirmedClear producer | ✅ **DONE** (!476) | flywheel dead-ladder + auto-close; 2× adversarial review |
| 2 | **TG-134** curated default ACLs + graduation seed | ✅ **DONE** (!477) | fresh-deploy seed; `min_confidence:0`; no-op on the live box |
| 3 | **TG-135** restart-container + reload-service (curated family) | ✅ **DONE** (!482) | curated out-of-box `auto` now = the conservative reversible family (restart-service · reload-service · restart-container); ruleset⟺ladder lockstep; adversarial bypass-review SHIP (empty allowlist + Shadow each fail-closed) |
| — | **deploy hotfix**: secrets-perms busybox syntax wedged the stack | ✅ **DONE** (!483) | unquoted parens in the init `echo` → whole stack stuck `Created`; mitigated live + `sh -n` lint guard (5/5); see [[compose-init-shell-safety]] |
| — | **console UX**: #estatedepth double-hostnames + huge gap | ✅ **DONE** (!485) | dedup was already fixed; gap = list forced the grid row → `ResizeObserver` caps list to inspector height; Playwright real-data audit (367→217 nodes) + strengthened e2e guard; see [[realbox-playwright-audit-technique]] |
| 8 | **TG-140** deploy-time mode config | ✅ **DONE** (!486) | `TG_INITIAL_MODE` seeds initial mode on a FRESH deploy (absent-only, no-op on live box); Shadow default; actuating seed still needs green preflight + ledger; adversarial review SHIP + under-mutex hardening. **Completes the out-of-box config triad** (ruleset+graduation+mode ⇒ a fresh TG can deploy pre-configured to auto-actuate the curated family, governed) |
| 4 | **TG-136** loadable op-class registry (TG-115) | ✅ **DONE** (!488) | op-class SCHEMA → embedded loadable JSON; argv BUILDERS stay compiled; fail-closed init lockstep (panic on schema⟺builder drift); tolerance test over all classes; adversarial review SHIP |
| — | **★ integration audit + safety fixes** (orchestrated 4-agent adversarial audit of the COMPOSED fresh-deploy path) | ✅ **DONE** (!489) | found 2 REAL gaps the per-MR reviews missed: **A1** interceptor never coupled mode-actuating→effect-leaf-read-only + `LocalReadOnly.Exec` ran argv locally (fixed: refuses); **A2** `restartClassRE` never extended for restart-container/reload-service → self-protected veto DEAD for 2/3 classes (fixed + drift-guard binding it to opschema.Specs). + docs/OPERATOR-QUICKSTART retiring the overclaim. Deferred: **A3** 1-deep stateful floor, **A4** corpus-seed novelty-bypass (owner-gated) |
| — | **★★ FLYWHEEL audit — the self-improving loop is SAFETY-BIASED but NOT self-improving** (orchestrated 4-agent adversarial audit) | ⚠️ **partial** (!491, !492) | **T1 HEADLINE — ⚠️ CORRECTED 2026-07-22 by LIVE EVIDENCE (start-guest graduated to auto, TG-141):** the audit's "de-novel dead-locks the climb, learned autonomy UNREACHABLE" claim is WRONG for an approve-LEVEL class. Live proof: start-guest climbed approve→auto via 5 clean cycles; the approve-level GRADUATION gate polls every proposal INDEPENDENT of novelty (a cycle at 6min, past the +6min de-novel settle, still polled). The real re-earn impediment is TG's own INGEST FLAP-SUPPRESSION (rapid repeated downs correctly rate-limited), NOT de-novel — sidestepped by rotating guests. So the flywheel CAN graduate a fresh class; the original T1 analysis (below) stands as a hypothesis the live run refuted. ~~the graduation ladder DEAD-LOCKS once de-novel is wired — de-novel-after-1 removes the very POLL the 5-run climb needs → learned autonomy is UNREACHABLE; only static pre-seeds (TG-134) reach auto.~~ A regression from !476; the fix (decouple novelty-removal from poll-removal / gate de-novel on graduated / count human-approved runs toward N) revisits the owner's "keep gates independent" decision → take to owner. See [[two-dimensional-trust-novelty-writeback]]. **FIXED:** S1 (!491) shipped `"*"` seed rows de-noveled 4 k8s rules FLEET-WIDE — dropped (repo+live), feature kept, drift-guard; C2 (!491) de-novel now requires `s.Executed`; S5 (!492) loud boot-warn when a missing corpus disables novelty. **Follow-ups:** S2+C3 (LibreNMS reader degraded-empty read misread as clear → false de-novel/close/graduation-credit — coupled, needs reader pagination+fail-closed); S3/S4/S6/C1 owner-gated. **VERIFIED-SOUND:** graduation state machine, deviation-blocks-learning, earned-not-self-reported, fail-closed writeback |
| 5 | **TG-137** reboot-host (floor-clamped) | queued | propose-only; never auto even when approved |
| 6 | **TG-138** restart-lxc / restart-vm | 🟩 **start-guest SHIPPED** (!505) | proxmox `start-guest` op-class + the `RegimeProxmox` lane, riding effect-kind routing (!504). Surfaced + fixed a real refinement: `proxmox-lifecycle` is ARGV-encoded (like ssh-argv) yet KIND-routed (like awx-launch), so encoding ⊥ routing (`argvEncoded` predicate). Fail-closed/inert until `TG_PROXMOX_*` set; the actuator floors reboot/shutdown/reset/destroy + default-deny per-guest allowlist. Both reviews SHIP (4 concerns confirmed safe). **LIVE-PROVE gating:** TG's PVE token is READ-ONLY (`*.Audit`, no VM.PowerMgmt — correct posture); actuation needs a SEPARATE write-token (VM.PowerMgmt on the guinea-pigs), provisionable via `pvesh` on dc1pve01 (root SSH available). Then activate `TG_PROXMOX_BASE_URL=https://dc1pve01:8006/api2/json/` + the write-token + `TG_PROXMOX_ALLOWED_GUESTS=librespeed01,myspeed01` + `TG_PROXMOX_INSECURE=1` → `pct stop` a guinea-pig → TG detect→propose start-guest→execute→verify. (~~AUTO graduation still gated on T1 (TG-145)~~ — REFUTED: start-guest later GRADUATED to auto live, TG-141.) **★★★ LIVE-PROVEN 2026-07-22 11:23:** provisioned the write-token (role `TGGuestLifecycle`=VM.Audit+VM.PowerMgmt, scoped to the 2 guinea-pig vmids), activated the lane (`real proxmox actuator, allowed_guests=2`), `pct stop` librespeed01 → LibreNMS device-down → **TG detected + proposed start-guest** (agent connected down-guest→the new op-class) → approved → **proxmox lane executed → `actuate:execute:MATCH`** → guest RUNNING → graduation **approve/1**, NO breaker trip. FIRST live actuation of a NEW op-class beyond the restart family; validates the whole session's machinery (effect-kind + routing + lane + TG-148/149) end-to-end LIVE |
| 7 | **TG-139** start/stop + bounce + disk-grow | 🟩 **disk-grow SHIPPED** (!501 foundation + !502 disk-grow) | The op-class registry now carries a closed `effect_kind` (`ssh-argv` default \| `awx-launch`) — the force-multiplier for EVERY non-SSH op. `disk-grow` (the disk-full origin case) ships as the first awx-launch op-class: the runner `sealEffect` encodes `[LaunchVerb]`+a `LaunchSpec` stdin via `awxjob.EncodeLaunch` (template from the fail-closed `Deps.AWXTemplateForOpClass` seam, params→extra_vars). BEHAVIOR-PRESERVING (3 SSH classes unchanged) + INERT on the box (AWX unconfigured → refused). Both effect-kind tripwires handled; self+hunter review SHIP (fail-closed end-to-end verified). Follow-ups TG-152 (L1 symmetric param re-check + L3 disk-op guard). REMAINING: owner AWX config (`TG_AWXJOB_*` + template allowlist) to ACTIVATE + live-prove; add disk-grow to curated ACLs; start/stop/bounce via proxmox = the next lane (op-aware routing) |
| 9 | **TG-141** flywheel live-proof + first graduation | ✅✅ **DONE — FIRST GRADUATION ACHIEVED 2026-07-22 12:06** | **★★★★ start-guest GRADUATED approve→AUTO via the LIVE FLYWHEEL** (ledger `actuate:graduation:verified_clean … promoted approve→auto after 5 consecutive verified-clean runs`; start-guest=auto/0). A BRAND-NEW op-class EARNED auto-actuation through 5 governed live detect→propose→approve→execute→**verify MATCH** cycles. TG can now auto-heal a down guest hands-off. **★ T1 CORRECTED (live evidence):** the "de-novel dead-locks the climb" claim (below/TG-145) is WRONG for an approve-LEVEL class — the graduation gate polls EVERY proposal independent of novelty (cycle 3 detected at 6min, past the +6min de-novel settle, and STILL polled → approve/4). The real re-earn impediment is TG's own INGEST FLAP-SUPPRESSION (4 rapid librespeed downs in ~15min → correctly rate-limited; NOT T1), sidestepped by rotating to a fresh guest (myspeed01) for the 5th run. So the flywheel DOES graduate a fresh class to auto |
| — | **barrier-3 REFRAMED (see [[triage-actuation-gap-rootcause]] 2026-07-22)** | 📋 owner-input | "75% no-band" is mostly LEGITIMATE `no-proposal:stop`; real polls = novelty 58% (by-design, faithful) + floor 36% (correct) + no-prediction only 4%. The 2 real levers are the READ-ONLY LANE SPLIT (low practical impact, delicate — the classifier only rarely proposes read-only ops) and NOVELTY-SEED from predecessor history (owner-sensitive/corpus-adjacent). Neither shipped unilaterally |
| — | **flywheel provisioning follow-up** | ✅ **DONE** (`0e04d80`) | a FRESH deploy left `/knowledge` root-owned → distroless worker (uid 65532) couldn't write the novelty-writeback → flywheel silently broken. `secrets-perms` init now `chgrp 65532 + chmod 775 /knowledge`; verified on live box (`drwxrwxr-x 0:65532`, corpus written by worker). Out-of-box flywheel now persists de-novel without a manual chown |

**★★ KEYSTONE SHIPPED + e2e-PROVEN 2026-07-22 (!495, merge 85eae93): the interceptor→regime ROUTING is DONE.** The execute activity now dispatches through `RegimeEngine.SelectLane → LaneEffect.Apply → a per-lane spec/013 interceptor` (was: single hardcoded native-ssh leaf; the engine was BUILT-then-DISCARDED by `wireActuationRegime`). Behavior-preserving (default lane native-ssh → identical chain) + fail-closed (no-lane/ambiguous/unwired refuses); a single-source `interceptorBuilder` wires the IDENTICAL 5 collaborators sharing the same chokepoint+breaker instances. `make all` green; MY bypass-review + an independent hunter BOTH SHIP (6 adversarial questions, no gate-bypass); a real ordering bug (oracle no-DB guard short-circuiting the routed path) caught by the tests + fixed. **e2e LIVE-PROVEN**: fault-injected nginx on librespeed01 → TG detected → classify:AUTO → restart-service → **actuated through the router** → nginx healed hands-off. **The interceptor's effect is no longer a single SSH leaf — adding AWX/proxmox lanes is now "wire a lane + config", not dispatch surgery.** Caveat: the immediate verify false-deviated (alert-clear timing, exacerbated by manual detection-forcing) → threshold-1 breaker force-Shadowed; restored Semi-auto via RBAC elevate→POST /v1/mode; filed **TG-148** (pre-existing verify-timing, NOT the routing).

**★★ TG-148 + TG-149 SHIPPED 2026-07-22 — the two flywheel-BREAKING bugs the routing e2e exposed are both fixed.** The routing keystone's "immediate verify false-deviated" caveat above turned out to be TWO distinct, compounding defects, both now fixed + deployed + adversarially reviewed (self + independent hunter, both SHIP):
- **TG-148 (verify baseline)** — merged `a6a9294` (diagnostic !497 + fix !498). `PostStateObserve` returns ESTATE-WIDE active alerts, so a PRE-EXISTING UNRELATED alert (already firing before the heal) was misread as a cascade SURPRISE → false DEVIATION on a SUCCESSFUL heal. Fix: the interceptor captures a pre-execute BASELINE and verifies via `verify.ComputeVerdictDetailWithBaseline` — only NEW-since-execute alerts can deviate (nil baseline == original, fail-safe). Hunter follow-up (non-blocking): the falsify scorer + async-regime verify don't get the baseline yet (stricter=safe).
- **TG-149 (breaker re-arm)** — merged `5a3a944` (!499), spec/015 **REQ-1525**. The bigger finding: the mutation deviation-breaker had **NO governed reset** — it only ever `RecordFailure`→open + reads state, never `Allow`, so no auto-recovery, and nothing wrote it closed. Its durable row is INDEPENDENT of the mode, so restoring the mode left it open. **One deviation — even a false one — permanently refused ALL actuation, estate-wide, forever.** Verified live: I approved a real librespeed decision (/v1/vote 202) and the actuation REFUSED `mutation breaker OPEN`. Fix: an owner-gated escalation INTO an actuating mode (already authz + preflight gated) re-arms the breaker, ledgered `safety:breaker-rearm`, fail-safe (a re-arm error leaves it OPEN), never self-healing. **RECOVERY PROVEN LIVE**: operator→elevate→`POST /v1/mode` Shadow→Semi-auto → `mutation_breaker_state` went open→**closed**, `safety:breaker-rearm` ledgered, mode Semi-auto restored.
- **OPEN (priority #3):** re-earning restart-service (demoted approve→auto by the original TG-148 false trip) via governed approved heal cycles. ✅ **T1/TG-145 REFUTED** — start-guest graduating to auto (TG-141) proved the approve-level gate polls EVERY proposal independent of novelty; the real re-earn impediment is TG's own **ingest flap-suppression** (rapid repeated downs correctly rate-limited), sidestepped by spacing faults / rotating guests. Not a dead-lock.

**★ BREADTH LANE #1 SHIPPED (disk-grow, !501+!502).** The effect-kind foundation (`ssh-argv`|`awx-launch`) + disk-grow are on main + deployed, fail-closed/inert (AWX owner-config-gated). disk-grow is the disk-full origin case ([[triage-actuation-gap-rootcause]] + [[disk-opclass-safety-design]]); ACTIVATION is gated on owner AWX config (`TG_AWXJOB_*` + template allowlist + the per-host trust ACL) + adding it to the curated ACLs.
**★ NEXT THRUST — the first LIVE-PROVABLE breadth lane: proxmox `start-guest`** (a down guest that should be up → start it; PVE access exists, so unlike AWX it can be e2e-proven on a guinea-pig guest). It rides the effect-kind framework: a new `proxmox-lifecycle` kind (argv-encoded `[start, <guest>]`, NOT a LaunchSpec) + a proxmox regime lane (the `modules/actuation/proxmox` actuator is code-complete: start/stop reversible, floor-clamped, per-guest allowlist) + **OP-AWARE lane routing** (the crux: the SAME guest host is native-ssh for restart-service but needs the proxmox lane for start-guest, so the effect-kind — not the target — must drive the lane; today `SelectLane(target)` is target-only). Ship fail-closed (empty guest allowlist); desired-state safety = only start a guest known-down that SHOULD be up (start↔stop reversible). SDD-first + adversarial-review. ✅ **SHIPPED + LIVE-PROVEN + GRADUATED to auto** (TG-138/TG-141); the "gated on T1" note is REFUTED.

### EPIC TG-131 — Decision-Tracer (GRADUATED)
10/10 stages carry real info, deployed, e2e-proven (~11 MRs: !469–!474 + fixes). Residual polish only:
**TG-142** (action_id verify/commit join), **TG-143** (boot-posture display). Non-blocking.

### EPIC TG-132 — Groundnet federation (QUEUED — after TG-130)
Contract live (GitLab proj 51, `ground-net/spec`). Owner-gated activation: **TG-144** (GitHub repo + CI vars +
Zenodo DOI). See `FEDERATION-VISION.md`.

### EPIC TG-175 — Ontology with a losability condition (ontological-multiplicity boundary work) — QUEUED, reconciled + RE-RANKED 2026-07-24
Filed by a **parallel session** (reporter=Claude; TG-175 + children TG-176..TG-181), distilled from a Stanford CS547 lecture on ontological multiplicity. **Premise:** TG's estate ontology is a *choice that stopped looking like one* — the declared edge vocabulary decides what can even BE a cause; every guard we own (novelty/confidence/band/graduation) sits DOWNSTREAM and cannot see a cause we have no vocabulary for. What makes this TG-shaped rather than STS hand-waving: we already own a **losability condition** — INV-22's degree-preserving shuffled control (`estate.ShuffledControl`, `eval/falsifiability.go`). Admission bar for every child: *what same-shape control does it beat?*

**Two flagged claims — VERIFIED against the LIVE system (owner asked):**
- **Corpus composition (the order-critical fact): the "~a dozen organic sessions + replays" assumption is STALE.** Live `session_triage` = **481 organic alert-driven sessions** (433 `librenms`- + 48 `am`-prefixed refs) across **60 hosts** and ~12 alert-rule classes, all in the last 7 days (verified on the box DB, 2026-07-24). This RAISES TG-179 (co-failure discovery now has real clustering material) and TG-181 (holdout has volume in the DOMINANT classes — Devices-up/down 93, Service-up/down 83 — though tail classes stay thin: MeshBFD 5, Service-Down 4). CAVEAT: concentrated on guinea-pig hosts from my own fault-injection (librespeed01 = 127/481) — organic in MECHANISM (real alerts), semi-synthetic in ORIGIN. Corrects [[tracer-corpus-mostly-synthetic]].
- **TG-178 margins vs spec/020 write-side: NO conflict.** The tracer write-side ingest already landed (migs 0033/0034); the only pending spec/020 tasks are T-020-13 (SSE) + T-020-14 (OTel, owner-gated) — neither touches `interceptor_gate_verdict` (mig 0030). TG-178's margin column takes the next free migration (0036).

**Grounding correction (recorded per the handoff — mine wins where I differ):** the epic's "exactly two edge types (runs_on, depends_on)" is imprecise — `core/estate/estate.go:44-47` declares **FOUR** (`runs_on`, `member_of`, `depends_on`, `routes_via`). The finding STANDS and is arguably STRONGER: `eval/discovery.go`'s `relOf()` collapses everything non-`runs_on` into `depends_on`, silently dropping `member_of`+`routes_via` — so the carve-out counter is MORE justified. Other groundings spot-checked REAL (engine.go:364 bare-string graduation; ShuffledControl; gate_verdict mig 0030).

**MY RE-RANK (leverage = value × readiness × safety × in-flight-fit; filing scores were assigned from OUTSIDE the delivery context — where I differ, mine wins):**
1. **TG-179a `unknown_relation` counter** — SHIP FIRST (a few lines at `eval/discovery.go`, additive, zero risk, behavior-preserving). Accrues the residual TG-179-full needs. [agree with filing]
2. **TG-176 identity-token null** [90] — best FIRST real MR: offline replay, zero prod risk, reuses ShuffledControl, returns a number that reprices the rest. "Is TG triaging evidence or names?" [agree]
3. **TG-177 op-class lineage** [86] — the only child closing a LIVE safety hole (graduation keyed on a mutable op-class string, under mutation-ON standing authorization → latent priv-esc via ordinary refactoring). Deterministic test, NO corpus dependency. Cross-ref [[actuation-grammar-verdict-opclass-registry]] (schema is loadable since !488, but trust still keys on the bare string). [agree, high]
4. **TG-178 gate-verdict margins** [83] — cheap append-only instrumentation, now BETTER-TIMED: my confidence-scalar fix (T-020-1, `89bbac2`) just unblocked the calibrator (`temporal/calibrate/job.go` was starved of paired samples); near-miss margins are the densest calibration evidence. No behavior change.
5. **TG-181 flywheel drift (class-stratified holdout)** [77→RAISED] — guards a LIVE self-driving loop already KNOWN to be drifting toward the `falsifiable_prediction` monoculture (see flywheel note above). Corpus supports the dominant classes; extends `evalgate` (low blast radius). More urgent than the filing rank because the loop is live NOW.
6. **TG-179-full Siblings automation** [80, value 29/30] — best VALUE + strategic fit; corpus finding raises its probability, but still multi-MR + touches the prediction/GATING path (blast radius) → sequence after the counter accrues residual + the cheap wins land.
7. **TG-180 observability census + probes** [78] — value-high but SAFETY-lowest (deliberately perturbs production); its own null-test forbids the census without the probe arm. NL-pool-only, reuses [[guinea-pig-injection-procedure]]. Do last, probe arm mandatory.

**Dedup:** none are duplicates of existing/in-flight items (0 open MRs, no branches at reconcile). Adjacencies cross-ref'd above. **Withdrawn (agree):** the "operator marker" lane — no null → the flywheel would graduate noise. **Standing caution (endorsed):** every child manufactures owner attention — build so TG proposes + owner ratifies in ONE action, and un-ratified items decay FAIL-CLOSED. **TG-179a SHIPPED 2026-07-24 (`d5beed1`, review APPROVE, deploying):** grounding revealed the "counter" framing was off — the PRODUCTION declared-edge parser (`ParseDeclared`) ALREADY fail-louds on unknown rels, so the silent-coercion gap was eval-twin-only (`LoadEstateGraph`'s `relOf`, which also silently dropped the declared `member_of`/`routes_via`). Fix binds both parsers to one shared `estate.ParseRelType` over `knownRelTypes` and FAILS LOUD on an unrecognised rel (chosen over a counter: production already rejects, the eval fixture is curated, a wrong graph must halt the deploy-gate not accumulate). Behaviour-preserving on the current fixture. This slightly LOWERS TG-179-full's blast-radius story (no production silent-coercion to fix) but confirms the eval-path fix. Rest of the epic NOT started (owner: "slot into board, then resume").

### Hub-contention rule (binds parallelism)
`cmd/worker/main.go`, `temporal/runner/{activities,workflow}.go`, `core/actuate/*`, `agent/loop.go`,
`modules/actuation/*` allow ONE in-flight owner each — the actuation MRs (#3–#8) touch these, so serialize
hub-touching agents. `core/credential` and `core/regime` are disjoint → safe to parallelize.

## Owner-directed vision capture (2026-07-21) — north-stars; do NOT resequence the roadmap

Three ideas the owner raised and approved this session, captured as governed documents + YouTrack epics.
**These do NOT supersede THE SEQUENCE** — the binding constraint remains proven loops + the canary. Only the
decision-tracer's T0 slice is near-term actionable.

- **Decision tracer — [`spec/020-decision-tracer`](../spec/020-decision-tracer/) (Draft, `specvalidate` green).**
  An ASA-Packet-Tracer-style per-workflow step-through inspector (rule · rationale · tools · prompts · skills ·
  confidence+reason · ACL bundle · credential identity) over the tamper-resistant ledger; the archive doubles as
  the flywheel's **outcome-paired learning corpus**. ~35% present (console Workflows view exists but
  fixture-driven; correlation spine `external_ref ⋈ action_id`; no OTel backbone). **T0 `T-020-1` — ✅ DONE
  (`89bbac2`, 2026-07-24):** persist the confidence scalar (REQ-2003) — fixed the field-identity drop (workflow
  read the always-zero nested `inv.Proposal.Confidence` not the top-level directive value); ALSO addresses the
  precedent/confidence-calibration gap (`wf whixksc00`) and unblocks the tracer "Conf" column. Gate stays OFF.
  Langfuse = optional Tier-3 export only; TG owns
  the governance trace. Prior-art: novel recombination in narrow unoccupied whitespace (nearest neighbor Microsoft
  ACS/AGT) — see [`PRIOR-ART-tracer-and-federation.md`](PRIOR-ART-tracer-and-federation.md).
- **Federation of sovereign instances — [`FEDERATION-VISION.md`](FEDERATION-VISION.md) (far-future north-star).**
  Sovereign TG instances share RE-VALIDATED remediation *distillate* (not raw traces); subordinate-not-authority,
  reputation-by-verified-outcome, signed+authenticated, default-OFF. CrowdSec precedent. Depends on the tracer +
  the flywheel's first graduation + loadable artifacts. Graduates to `spec/021-federation` once prereqs met.
- **tgctl / declarative control plane — [`TGCTL-DECLARATIVE-VISION.md`](TGCTL-DECLARATIVE-VISION.md) (north-star).**
  kubectl-style CLI + k8s-style declarative resource API + reconciliation + k8s-shaped controller/worker topology.
  API-first (console + `tgctl` both clients of one `/v1`); CLI-is-a-client-not-a-side-door; DECLARED(config) vs
  EARNED(trust = `status`-only). TG is ~70% there via Temporal + Postgres (don't rebuild etcd). Near-term brick =
  the versioned/immutable ruleset (spec/020 REQ-2018). Graduates to `spec/022-declarative-control-plane`.

## Reconciled backlog — see THE ROADMAP above (epics TG-130/131/132)
The old canary-first sequence is COMPLETE (canary actuated) and superseded by the epic roadmap above. Items
still tracked but OFF the critical path:
| Item | Class | Notes |
|---|---|---|
| TG-141 flywheel live-proof | **ACTIVE (DoD)** | guinea-pig e2e; validates TG-133; restart-container now curated-auto (!482) so it can be the actuated op-class |
| TG-135 restart-container | ✅ **DONE (!482)** | shipped as the conservative reversible family (restart-service · reload-service · restart-container) |
| Exceed-proof (shadowbench discriminating pairs, TG-87/TG-76 judge, 2–3 published benchmarks) | **v1.0 gate** | evidence not code; needs the accrued verified-outcome record |
| TG-114 prose-artifact / graduate-prose flywheel | POLISH / v1.0+ | unblocked by TG-136 loadable registry |
| gitopsmr / k8s lanes (TG-110 future) | **OWNER-GATED** | signed spec + repo + sealed bot token |
| Security token rotations (TG-118) + myspeed01 power-on | **OWNER-GATED (non-code)** | highest-urgency untracked |

## YouTrack roadmap epics (2026-07-22 defragment)
The roadmap above is mirrored to YouTrack as three epics with child issues hung via `Subtask` links:
**TG-130** Out-of-box Superiority (children TG-133…TG-141) · **TG-131** Decision-Tracer/graduated (TG-142, TG-143)
· **TG-132** Groundnet/queued (TG-144). State fields are unset (the automation user is State-guarded); status
lives in the summary tag (`[DONE]`/`[NEXT]`/`[QUEUED]`) and in THIS file, which remains authoritative.

## YouTrack reconciliation (close-out pass, agent a0fb436f, 2026-07-20)
- **Root cause of the stale board:** the automation user CANNOT write the State field (permission-blocked) —
  so nothing auto-closes. **32 verified-done issues + 1 duplicate** were commented "verified merged — ready to
  close"; they need an **admin to grant State-permission or bulk-close** (owner action).
- **Filed for the previously-untracked clusters:** **TG-118** `[URGENT][SECURITY]` rotate 4 leaked tokens
  (NetBox/LibreNMS/OpenBao-root/YouTrack-Bearer) + redact shadow-gate logging (owner-held); **TG-119** console
  drift(!420)+skills panel; **TG-120** flywheel first-cycle; **TG-121** shadowbench pairs; **TG-122** regime
  GitOps/k8s lanes.
- **CORRECTION — credential integration leaves are ALREADY MERGED** (my source memory was stale): `Resolve`→
  interceptor + audit rows (REQ-1604, migration 0018), AWX↔estate `Target.Groups` (35ed5c1), OpenBao empty-list
  (508ccee), OIDC minter (2ec5e5d) are all on main. The only credential-side open item is **TG-109** (console
  config-WRITE path, gated on the console-native-config track). So the sequence's step-7 "credential polish"
  mostly evaporates — drop it from PARALLEL.

## Methodology (so it does not re-fragment)
1. **This file is the single source of truth.** Update it whenever state changes; it is the entry point.
2. **YouTrack = reference index.** Its state field is meaningless (never transitions) — after the 2026-07-20
   close-out pass, treat it as a searchable index, not a work-signal. Do not trust an "open" count off it.
3. **Verify done/open against `git log`** before asserting — the heartbeat's stale "#1/#2/#9/#12/#15 open" is the
   cautionary example. Never carry a priority claim forward un-reverified.
4. **The heartbeat template's priority order is SUPERSEDED by THE SEQUENCE above** until the template is rewritten.
5. Local session TaskList = ephemeral scratch; reconcile it to this file, don't treat it as authoritative.
