> **NOT A WORK QUEUE — reference only.** The one authoritative queue is [`BOARD.md`](BOARD.md). This file records
> what the product's documents CLAIM, what the running system and the committed records SHOW, and the basis for
> every correction made to close the gap. It is appended to; a row is never rewritten, a superseding row is added.

# Claims register — product and evaluation claims against verified state

**Opened 2026-11-07 (TG-680, the G4A fix-first program, owner prompt §2.G).** Every row names the file and line
(as of the commit that added the row), the old text, the new text, and a **basis label**:

| label | meaning |
|---|---|
| **owner-approved** | an owner ruling on record (the rulings log, a ticket) authorises the statement |
| **documented** | a committed record in this repository states it (config, a frozen analysis, an ADR) |
| **verified in code** | read in the Go tree at the cited path on `origin/main` 6851ba94 |
| **observed** | read from the running system on 2026-11-07 (boot logs, the live `.env` key names, the production ledger, read-only) |
| **inferred** | follows from the above but was not itself measured |
| **unknown** | the truth could not be established from any of the above; stated as such |

Sources of truth used throughout: `docs/BOARD.md` and `docs/history/BOARD-JOURNAL-2026-11.md` (2026-11-07);
`deploy/docker-compose.yml` (which keys reach which service) and the live `.env` key names (values not read
except two non-secret posture keys); the worker boot logs of 2026-11-07 20:40/20:45 NL; the module catalogue
under `modules/`; `tools/shadowbench/` (PRE-REGISTRATION.md, the frozen `analyze.py`, `confirmatory/`); the G4A
review evidence pack and its §0 corrections C1–C3 (`~/tg-evidence/20261107-g4a-program/`, outside the repo);
`deploy/litellm-config.yaml`; `core/judge/rubric.json`.

## 1. Vision vs deployed / armed / exercised capability

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 1.1 | README.md:16 | "lets a native LLM agent triage and (once evidenced-ready) autonomously remediate infrastructure incidents across a whole estate (Kubernetes, hypervisors, network, storage, security)" | remediation is class-by-class as evidence accrues; a new paragraph "Vision versus what is exercised" names what has executed: Proxmox guest start, systemd/Docker unit and container start/restart, on injected faults on a test pool at two sites; Kubernetes, network, AWX, storage lanes are dark, unwired or armed-but-unexercised | observed | boot logs 11-07 (`actuation regime` lines); BOARD 11-07 G1–G3; evidence pack §10.4 "zero organic executions"; ledger: production ledger 2026-11-07 (read-only): `regime_actuation` 0 rows, `regime_resolution` 0 rows, `deferred_verdict` 0 rows — no regime-lane actuation ever recorded |
| 1.2 | README.md:182 | connector list names "Kubernetes" among deployed connectors | a connector in `modules/` is compiled, not deployed: kubectl/helm module wired into no worker; Cisco read + write dark; AWX unarmed; k8s alerts arrive via Alertmanager ingest | verified in code + observed | `grep -rn "actuation/kubernetes" cmd/` → nothing; boot: "cisco-show: DARK — no TG_CISCO_READ_DEVICES declared", "cisco write lane = DARK", "awx-job (pendingActuator — FAIL-CLOSED refuse)", "k8s audit reader NOT registered" |
| 1.3 | README.md:281 | Phase 3 "Planned" | Planned; regime engine + GitOps MR actuator built and ARMED on the actuation plane, 0 executions; G5 preconditions in `K8S-MANAGEMENT-REGIME-DESIGN.md` §7 | observed | boot (worker-actuate 20:45): "gitops-mr lane actuator = ARMED real actuator (read_only=false, 1 repo(s) with field_rules)", "convergence reader = ARMED"; live `.env` has `TG_GITOPSMR_ARM=true`; ledger: production ledger 2026-11-07 (read-only): `regime_actuation` 0 rows, `regime_resolution` 0 rows, `deferred_verdict` 0 rows — no regime-lane actuation ever recorded |
| 1.4 | README.md:316 | "platform packs … (Cisco IOS and ASA, Kubernetes, Linux) so a fresh install is capable" | the `cisco` and `kubernetes` KNOWLEDGE packs load; the Cisco read/write lanes and the kubectl module they would drive are dark or unwired | observed | boot: "pack pack:cisco@1.0.0 … all declared capabilities resolve", "pack pack:kubernetes@1.0.0 …"; the lane lines above |
| 1.5 | docs/PRODUCT.md:9 | (none — the document read as present-tense capability) | a "Vision, not status" convention: PRODUCT defines; README "Current state" and this register report | documented | this register |
| 1.6 | docs/ARCHITECTURE.md:37 | Execute row: "typed, individually-permissioned adapters (SSH/kubectl/MCP/API)" | same, plus: SSH and Proxmox API exercised; gitops-mr armed/unexercised; kubectl compiled/unwired; Cisco and AWX dark | observed + verified in code | as 1.2, 1.3 |
| 1.7 | docs/CONNECTOR-INVENTORY.md:81 | "Kubernetes (kubectl/helm) … day-1" | row keeps the bill-of-materials tier and gains the 2026-11-07 status: module unwired, audit reader unregistered, alerts site-less, 0 executions; "day-1" is the predecessor-derived plan, not a deployment claim | verified in code + observed | as 1.2; evidence pack §10.4 |
| 1.8 | docs/BOARD.md:74 | (G3 bullet carried no lane status) | lanes by outcome: gitops-mr ARMED 0 executions; Cisco read + write DARK; AWX DARK; kubectl unwired | observed | as 1.2, 1.3 |

**Cisco READ status — resolved as far as the evidence goes.** The read tool exists (`modules/actuation/cisco/showtool.go`, `cmd/worker/worker_cisco_read.go`) and its key reaches both worker services (`TG_CISCO_READ_DEVICES` in `deploy/docker-compose.yml`). On 2026-11-07 it is **DARK** on the reference deployment (boot: no devices declared). Whether it was ever exercised against a live device on any earlier deployment is **unknown** from the records this lane could reach; the one owner-sanctioned live Cisco write (TG-543, the tunnel PSK re-align) is **Obsolete** in the tracker, i.e. it never ran. No repository document claimed Cisco read as live; the conflicting statements were between the notes outside the repo (which say "dark") and the arming of the sibling gitops-mr lane (which is armed) — recorded here so the two are no longer read as one.

**Kubernetes connector claims — resolved.** Three different things were being called "the Kubernetes connector": (a) Alertmanager ingest of cluster alerts — live, host-less and site-less (16 unattributed cohort incidents, 6 of them k8s rules, pack §10.4); (b) the kubectl/helm actuation module — compiled, wired into no worker; (c) the gitops-mr lane with its Argo CD convergence reader — armed on the actuation plane, 0 executions, verifying sync state rather than workload behaviour. Only (a) is exercised. `K8S-MANAGEMENT-REGIME-DESIGN.md` §7 records the G5 precondition (TG-681).

## 2. Single-organization multi-user vs multi-tenant

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 2.1 | docs/TESTING-AND-BENCHMARK.md:10 and 18 further lines (§0.3, §1.1, §1.2, §1.3, §1.4, §2.1, §2.2, §2.3, §2.4, §2.5, §3.1, §3.3, §4, §5) | "multi-tenant / de-solo", "tenant_id + RLS on every table", "Multi-tenancy is a test axis", "partitioned by (tenant, surface, dimension) with tenant_id + RLS", "per tenant", "cross-tenant isolation", "per-tenant chain isolation", "the live/tenant DB" | single-organization, multi-user per ADR-0010: no `tenant_id`, no row-level-security org isolation; isolation is per session and role; "per site/stratum" where a split is meant; cross-session isolation tests | documented | `docs/adr/0010-single-org-multi-user-not-multi-tenant.md`; PRODUCT §1; ARCHITECTURE §1, §10; CAPABILITY-INVENTORY (paradigm-rule 1) |

## 3. The owner's no-human-vote posture vs optional product roles

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 3.1 | README.md:64 | "the human as a circuit-breaker rather than a per-action approver" (read as the only mode) | the resolver is a role: default `human` (console vote); the owner's deployment runs `TG_APPROVAL_RESOLVER=autonomous`, TG as fallback approver, no human votes (spec/012 REQ-1127) | verified in code + observed + owner-approved | `cmd/worker/approval_resolver_wiring.go` ("human" default, "autonomous"); boot worker-1: "approval resolver: AUTONOMOUS"; live `.env` `TG_APPROVAL_RESOLVER=autonomous`; rulings 2026-09-13 (TG-558/559) |
| 3.2 | docs/OPERATOR-QUICKSTART.md:79 | "(a human approves once)" | human approves once in the console under the default resolver; under `autonomous` TG acts when a missing first look was the only reason to pause and holds everything else | verified in code | `temporal/runner` `resolvePollAutonomously`; `cmd/worker/approval_resolver_wiring.go` |

## 4. Investigation vs autonomous remediation; synthetic vs organic incidents

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 4.1 | README.md:241 | "Phase 2 is built and actuation is enabled … the operating evidence … is still being earned" (2026-07-31 text) | Current state by outcome: investigation on every ingested alert; autonomous remediation executed only on injected faults (G1–G3 soak, credits per class); in the review cohort 159 sessions, 42 organic, 0 organic executions | observed | BOARD 11-07; journal 11-05→11-07; evidence pack §10.4 ("Zero organic executions. The window's execution evidence is therefore entirely synthetic") |
| 4.2 | README.md:26 | (implicit estate-wide autonomy) | explicit: no organic remediation yet | observed | as 4.1 |
| 4.3 | docs/ROADMAP.md:34 | "live mode owner-set to Full-auto" | Semi-auto as of 2026-11-07 (boot log `mode=Semi-auto may_actuate=true`); the earlier note is quoted as superseded | observed | boot worker-1 20:40 "mode=Semi-auto may_actuate=true"; BOARD "owner-set Semi-auto" |

## 5. External model use vs the unproven air-gapped / local path

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 5.1 | README.md:28 | "It is built for sovereign, regulated, and air-gapped estates. Everything runs inside the adopter's own trust boundary with no SaaS dependency and no phone-home" | designed for sovereign/regulated estates; control plane, state and ledger stay inside; model calls go to external providers on the reference deployment (Azure OpenAI primary, Anthropic rescue lane + judge); the local/air-gapped path exists only as configuration and has not been exercised for triage or remediation | documented + unknown | `deploy/litellm-config.yaml` (model_name primary → azure/gpt-4.1; fast → gpt-5.4-mini; fallback-cc-sonnet/-fable; judge → Claude Opus; embed-nomic local); no record of a local-model triage run — unknown whether one has ever been attempted |
| 5.2 | README.md:102 | "This deployment currently runs SINGLE-BRAIN on Claude Opus 5 with the fallback rungs deliberately empty … the benchmark judge is pinned to a different model entirely, so the referee can never share a brain with a system it scores" | Azure OpenAI primary/fast, Claude rescue lane, Opus judge with no fallback; the judge shares a model family with the rescue lane and with the predecessor — a disclosed confound, not an isolation guarantee | documented | `deploy/litellm-config.yaml` comments 2026-08-22 and ROUND 4/5 2026-08-31; `tools/shadowbench/PRE-REGISTRATION.md` §6 2026-08-25 "Judge disclosure" |
| 5.3 | README.md:348 | "even those can point at a local model" | a configuration option not exercised for triage on any cited deployment; only the embedding lane runs locally | observed + unknown | boot: `semantic retrieval: enabled` via `embed-nomic` (BOARD); no local triage record |
| 5.4 | docs/PRODUCT.md:121, docs/ARCHITECTURE.md:226 | "Local-first-$0 is a mode" / default ladder z.ai → DeepSeek → Mistral | same definitions, each with the reference deployment's actual ladder and the unexercised status | documented | as 5.1 |

## 6. Credited episodes vs independently verified causal recoveries

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 6.1 | docs/BOARD.md:71 | "G3 REACHED … every heal class ≥ 15 verified-clean autonomous runs" | credits are CREDITED, not yet verified causal: the predicate trusted a commit-confirm `match` (deep-review F3/F8); recompute under the corrected predicate (TG-662 open, TG-666 open) before citing | documented + observed | `FINDINGS-LEDGER.md` F3, F8, §4 containment ("credits … recomputed under the corrected predicate, never trusted as-is"); YouTrack TG-662/TG-666 Submitted |
| 6.2 | README.md:246 and docs/BOARD.md (same bullet) | (the earlier episode join: fault 1670 healed; 76 executions / 75 faults; GR 48 / NL 28) | corrections C1–C3 carried: fault 1670 NOT healed by TG (its 18:29Z start-guest belongs to 1675); 69 credited faults (GR 44 / NL 25); 2 executions outside any ownership interval; an execution's site is stamped by the session | documented | evidence pack `sql/15-episodes-idbased.sql`, `FINDINGS-LEDGER.md` §0 C1–C3 |

## 7. The predecessor experiment (campaign #3) — never a third-party comparison

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 7.1 | README.md:228 | "Head-to-head benchmarking. A blind, unified-rubric harness scores Territory Grounder against a predecessor system on the same real incidents" | a predecessor experiment: TG vs the system it replaced, same author, injected faults; no third-party suite run | documented | `tools/shadowbench/README.md`, PRE-REGISTRATION §6 (as-deployed arms) |
| 7.2 | README.md:257 | the 2026-07-31 campaign #1 paragraph (21/30 pairs, "ten injected faults where only TG proposed…", PREDECESSOR HOLDS) | campaign #3 result with all caveats (below); campaign #1 kept as the under-powered prior | documented | `tools/shadowbench/confirmatory/final-campaign3-verdict.txt`; `ground-truth-campaign3-provenance.md` |
| 7.3 | tools/shadowbench/README.md:16 | "TG vs predecessor shadow head-to-head" (unqualified) | the "What this is, and is not" block | documented + verified | the committed files below |
| 7.4 | docs/BENCHMARK-LADDER.md:12 | "escalating fault-injection head-to-head (TG vs the predecessor)" | "comparison with the predecessor (an internal experiment, not a third-party benchmark)" + a status note pointing at campaign #3 | documented | as 7.2 |
| 7.5 | docs/BENCHMARK-AXES.md:10 | "The project's v1.0 gate is a neutral third-party benchmark" | same, "(not yet run as of 2026-11-07; the only completed comparison is the predecessor experiment)" | unknown → documented | no record of an ITBench/SREGym run in the repo, the journal or the tracker searched by this lane |
| 7.6 | docs/TESTING-AND-BENCHMARK.md:215 | "shadowbench head-to-head (TG vs predecessor, blind-judged)" | "the shadowbench predecessor experiment … not a third-party benchmark" | documented | as 7.2 |

**The facts of campaign #3, each from a committed file.**

| fact | value | source |
|---|---|---|
| population | 32 pairs submitted, 30 analysed (2 excluded: more than 3 per host, earliest kept), 13 hosts | `confirmatory/final-campaign3-verdict.txt` |
| primary (judge-free) | TG-only-correct 7, predecessor-only-correct 0, concordant 23; McNemar exact p = 0.0156 | same |
| non-inferiority | host-clustered rate difference +0.233 (CI +0.088..+0.423), margin −0.10 | same |
| composite verdict | **TG MATCHES (non-inferior)**; owner accepted 2026-08-26 | same; `ground-truth-campaign3-provenance.md` |
| secondaries | correct_diagnosis and evidence_grounded significantly favour the predecessor (two-sided Holm p = 0.0006 / 0.0003); all five one-sided TG-favouring tests ns | verdict file |
| concordance ≠ correctness | over the 32-entry committed ground truth: 4 both-correct, 20 both-wrong, 8 TG-only, 0 pred-only; per class TG/pred correct — device-down 8/0 of 17, disk-fill 4/4 of 4, service-down 0/0 of 7, container-down 0/0 of 4. On the 30-pair population at most 4 of the 23 concordant pairs are both-correct | computed 2026-11-07 from `confirmatory/ground-truth-campaign3.json` |
| judge-family confound | shadowbench judge alias `judge` → Claude Opus (litellm01 lane); the predecessor ran `claude-opus-5`; disclosed with bias direction in PRE-REGISTRATION §6 (2026-08-25); the named third-vendor re-judge has not been run | `deploy/litellm-config.yaml` ("judge"); PRE-REGISTRATION §6 |
| repeated cases per host | 13 hosts with 4, 4, 3, 3, 3, 3, 3, 2, 2, 2, 1, 1, 1 pairs (32 submitted); CIs are cluster-bootstrapped by host; the McNemar exact p on the 7 discordant pairs is not clustered | `confirmatory/pairs-campaign3.jsonl` (`subject_host`); `analyze.py` `cluster_bootstrap_ci`, `mcnemar_exact` |
| primary-endpoint documentation discrepancy | PRE-REGISTRATION §1: ground truth = `injected_fault.fault_type` scored by `core/diagcorpus` against `expectations.json`. Frozen record: "no automated builder exists; hand-assembled … replicating core/diagcorpus ScoreFault … cross-checked by three independent blind scorers", with a sensitivity file for two disputed predecessor pairs (pred 4/32 strict vs 6/32 lenient; both MATCHES). PRE-REGISTRATION §7 still says powered needs "≥ 15 hosts" while §3 (amended 2026-07-31) and `analyze.py` use `MIN_HOSTS = 12`; the run had 13 hosts, so it is powered under the amended rule and not under §7's unamended text. The provenance headline quotes 12/32 vs 4/32 (the 32 population) while the verdict is computed on 30 | PRE-REGISTRATION §1, §3, §7; `ground-truth-campaign3-provenance.md`; `analyze.py` L88–89; the verdict file |

## 8. Universality and uniqueness claims about other tools

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 8.1 | README.md:50 | "Most AI-SRE tools stop at diagnose-and-suggest, or ship blind, off-by-default auto-remediation." | removed; the feature list is this system's design, not a measured comparison; other products (OneUptime, Kubernaut among them) document overlapping verification and audit features; no third-party head-to-head has been run | inferred | owner prompt 2026-11-07 §2.G names OneUptime and Kubernaut as documenting overlapping capabilities; no measurement exists either way |
| 8.2 | docs/PRODUCT.md:57 | "Blind-auto … No reversibility model, no consequence prediction, no verification, no audit" | "Typically no …" with the category caveat and the competitor note | inferred | as 8.1 |

## 9. Other drift corrected while sweeping

| # | file:line | old claim | new text (abridged) | basis | source |
|---|---|---|---|---|---|
| 9.1 | README.md:303 | "In review (MR !794)" | "Merged (MR !794)" | observed | `glab mr view 794` → state merged |
| 9.2 | docs/PRODUCT.md:75 and 89 | "the `frontend/` console" / "The `frontend/` service (a TypeScript service…)" | the served console is `deploy/console/v2/`; the React `frontend/` was removed | verified in code | `deploy/console/v2/` exists; ARCHITECTURE §5 (corrected 2026-07-30) |

## 10. Unknowns left open by this register

- Whether the Cisco read tool (`cisco-show`) was ever exercised against a live device on any deployment: **unknown**; DARK on 2026-11-07.
- Whether a local model has ever run triage or remediation on any TG deployment: **unknown**; no record found.
- Whether a neutral third-party benchmark (ITBench-AA / SREGym) has been attempted: **unknown**; no record found.
- The two campaign-#3 pairs excluded by the per-host cap: which concordant class they fell in is not derivable from the committed snapshot without re-running the frozen cap rule; the register states the bound ("at most 4 both-correct") rather than a number.
- Occurrences stamped with site labels outside the declared vocabulary (`no` 1,071, `ch` 12, `tx` 5 in the 14 days to 2026-11-07, production ledger): origin unknown; reported to the coordinator, not changed here.
