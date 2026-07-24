####################################################################################################
SYNTHESIS
####################################################################################################
# Territory Grounder — Synthesis of 8 Predecessor Subsystem Audits

Scope note: TG is now single-org (ADR-0010), so where the raw audits say "per-org/multi-estate tunable," read that as per-source/per-rel_type policy rows for the one estate. All file:line citations below are into the predecessor (`scripts/…`) or TG (`grounder/…`, paths shown as `core/…`, `temporal/…`, `cmd/…`).

---

## 1. ESTATE-GRAPH DESIGN (the immediate build)

### 1.1 The definitive multi-source model

The predecessor materializes ONE causal infragraph into shared tables — `graph_entities` (UNIQUE `(entity_type, name)`) + `graph_relationships` + a 1:1 `infragraph_dynamics` sidecar. Edge convention is **SOURCE depends-on TARGET** (`vm -runs_on-> pve_node`); `blast_radius(H)` walks edges INTO H (who is affected), `deps(H)` walks OUT (what H needs).

**Every data source → edge-type → confidence (this table IS the sourcing policy TG must reproduce):**

| # | Source | Kind | rel_type | Confidence | Notes |
|---|--------|------|----------|-----------|-------|
| 1 | Tunnels (`chaos-test.py TUNNEL_GRAPH_EDGE`) | static | `routes_via` | **1.00** | open-ended (never expires) — seed:265 |
| 2 | Declared operator table (`docs/host-blast-radius.md`) | declared | `depends_on` | **0.85** | carries `expected_alerts`; open-ended — seed:320 |
| 3 | PVE live cluster (`pvesh get /cluster/resources`) | live | `runs_on` | **0.95** | placement source-of-truth; 7d TTL — seed:226 |
| 4a | NetBox DCIM devices | CMDB | `member_of` (site) | **0.90** | 7d TTL — seed:145 |
| 4b | NetBox cables | CMDB | `depends_on` (leaf→net_device) | **0.85** | skip net↔net cables (ambiguous direction) — seed:179 |
| 5 | LibreNMS `dependency_parent_hostname` | monitoring | `depends_on` | **0.90** | sparse but causally exact; skip IP-literal parents; 7d TTL — seed:381 |
| L1 | Incident co-occurrence (learned) | learned | `depends_on` | **min(0.75, 0.4+0.05·count)** | HARD-CAPPED below the 0.80 suppression cutoff — learn:198 |
| L2 | Chaos experiments (learned) | learned | verdict-based | **PASS 0.9 / DEGRADED 0.7 / FAIL 0.5** | learn:102 |

Sources 1–5 run in that fixed order in `infragraph-seed.py:407-421`, **each per-source-isolated**: a failing source rolls back only its own txn and is reported as `{"error":…}`; the others still commit. Learned passes L1/L2 run in `infragraph-learn.py`.

Criticality/tier is **deliberately OUTSIDE the graph**: a hand-curated `CURATED` dict (`refresh-host-blast-radius.py:48-93`, e.g. "CRITICAL - gateway"/"MEDIUM"/"LOW") plus the machine floor `_P0_HOSTS_BASE` (7 hosts: dc1pve01/03/04, dc2pve01/02, dc1fw01, dc2fw01 — `:21-35`), with DRIFT flagged when a curated host is absent from live PVE (`:350-354`). It is operator judgment, **not** graph-degree-derived.

### 1.2 The exact trust/precedence order (this is subtle)

It is **NOT a precedence table.** The CLAUDE.md prose hierarchy "live > LibreNMS > NetBox > IaC" (CLAUDE.md:66-72) is realized entirely as the confidence constants above plus a **MAX-confidence ratchet on a source-agnostic edge key**:

- Edge identity = `(source_id, target_id, rel_type)` only — `upsert_edge` infragraph.py:210-213.
- Re-seed by ANY source does `confidence = MAX(confidence, ?)`, refreshes `valid_until`, **never downgrades** — infragraph.py:238-242. Confidence is monotonic-upward-only.
- All sources for one triple **share ONE dynamics/edge row**; the `source` label is **last-writer-wins** EXCEPT the chaos learner **hard-overwrites** `source='chaos'` because chaos-grade evidence outranks the seed bucket — learn:120-123.
- "Live wins" is **emergent** (0.95 > 0.90 > 0.85) and reinforced by sources mostly populating disjoint rel_types — NOT an explicit ordering.

**TG's ResolutionPolicy must decide deliberately: replicate emergent-confidence-max (predecessor's actual behavior) or build a genuine precedence-overwrite.** Only the numbers are load-bearing; the prose hierarchy is not what the code does.

### 1.3 Build + merge + freshness mechanics TG must replicate

- **Mandatory entity resolution before every edge write.** `resolve_entity(name)` maps a bare hostname to an existing `(entity_type,name)` (infragraph.py:409-421), normalizing (strip domain via `split('.')[0]`, take first CSV parent, skip IP-literal parents `re.fullmatch(r'[\d.]+')`), falling back to a typed default (`ROLE_TO_TYPE`, seed:114-122) only if unknown. **A dropped resolve step is a silent correctness bug** — a wrong-typed edge lands on a "disconnected twin node" invisible to traversal, so blast radius comes back empty even though the seed "succeeded."
- **Self-expiring edges.** Live/CMDB/monitoring edges get `valid_until = now+7d` (`VALID_DAYS=7`, seed:76-81); tunnels/declared are open-ended (NULL). Traversal hard-filters `(valid_until IS NULL OR valid_until > now)` (infragraph.py:333); `health()` counts `stale_edges` → `InfragraphSeedStale` alert (infragraph.py:1013-1015). A dead source degrades to "no edge after 7d," never "silently-wrong stale edge."
- **The persisted SQLite graph IS the cache** — no response cache; freshness is the daily re-seed cron (`10 4 * * * --all`) plus the TTL contract. (`snapshot.py` is per-turn session RunState, NOT estate caching — do not confuse.)
- A **second bi-temporal layer** (`invalid_at` supersession + 0.01/day decay, at-risk <0.30) is **REPORTING-ONLY**, flag-gated (`INFRAGRAPH_BITEMPORAL_INVALIDATE` default off), and **must never feed predictions/suppression** — infragraph.py:1099-1138.

### 1.4 Blast-radius algorithm (port every mechanic)

Recursive CTE (`_TRAVERSE_SQL` infragraph.py:317-341), `DEPTH_CAP=5`, defaults **3** (blast/deps) and **2** (cascade), with three hard-won mechanics:
1. **Path-product confidence:** `walk.conf * MIN(COALESCE(d.confidence, r.confidence), 1.0)` — decays multiplicatively along the path.
2. **Cycle safety** via a path-string `instr()` containment check.
3. **Per-node reduction done in Python, not SQL**, keyed `(distance asc, -conf)` (infragraph.py:372-378) — SQLite's bare-column-with-MIN guarantee breaks once a second aggregate appears.
4. The CTE **INNER-JOINs `infragraph_dynamics`**, so an edge with no dynamics row is **invisible** to traversal — TG must decide this inclusion rule explicitly.

`expected_cascade()` has **TWO** mechanisms (infragraph.py:676-746):
- **(a)** transitive dependents carrying each edge's declared/learned `expected_alerts`;
- **(b)** **common-cause SIBLINGS** — hosts sharing an infra parent (pve_node/network_device/tunnel) with the target, scored at `SIBLING_CONF_PENALTY = 0.6 × edge confidence` (infragraph.py:426,429-458). This catches co-failure where the shared parent itself never alerts — the documented **2026-05-08 pattern (4 VMs on one PVE node flapping while the node's own alert never fires)**. Dropping siblings re-introduces exactly that blind spot.
- **Prediction window** = `max(900, int(2 × max observed p95 delay))` — stretches to 2× the slowest observed propagation so a slow cascade still lands inside verification.
- Per-item confidence = `round(min(node.conf, edge_conf), 4)`.

**Cascade-probability gating** (the precision-recovery layer, infragraph.py:43-49,528-591): Laplace `alpha=1, beta=4` (prior mean **0.20**). Family-scope probability **GATES emission** (drop child if `P(family) < tau=0.10`); exact-scope probability **REPLACES** the structural confidence. Stats learned from evaluated shadow+action predictions, applied **symmetrically to the real prediction and the shuffled control**, inert until history exists, env-disable via `INFRAGRAPH_CASCADE_GATING=0`. **Critical safety asymmetry:** the shadow/fold lane runs `drop=True`; the fail-CLOSED action lane runs `drop=False` (annotate `cascade_prob_family`, **never drop or alter structural confidence** — verdict must see the full blast radius), infragraph.py:841-844. Without this gate, enumerated blast radius over-predicts (~0.05–0.15 raw precision **by design**) and any fold/suppression gate reads garbage.

### 1.5 Prediction → mechanical verdict → negative control

- **Prediction** (`predict_action`) is the fail-CLOSED pre-remediation artifact committed OUTSIDE the LLM, plan_hash-keyed. Returns `eligible=False` when the target is **not in the graph** — remediation lane fails CLOSED (infragraph.py:836-838).
- **Mechanical verdict** (`action_verdict`, infragraph.py:917-977) is the **sole** verdict author (the acting LLM never adjudicates its own outcome). Three-way dominance **deviation > partial > match** over the observed set AFTER two exclusions: (a) the action's **own target_host self-alerts** (a reboot making the rebooted host alert is expected), (b) **coincidental cross-site** alerts. `_host_site()` (infragraph.py:905-914) maps only dc1→nl, dc2/02→gr; VPS/unknown → None, and an alert is cross-site-excluded **only when BOTH sites are known AND differ** — unknown/None-site hosts are **never** excluded (fails toward "surprise" = safe). **deviation never auto-resolves.**
- **Negative control** (`shuffled_control`, infragraph.py:749-809) is a **genuine degree-preserving shuffle**: bucket live edges by rel_type, `rng.shuffle` the target list within each bucket (deterministic per-day seed `_utcnow()[:10]`), rebuild the reverse-dep structure, run the **identical** depth-capped BFS, emit `(host,rule)` from shuffled `expected_alerts`. Scored `control_tp/control_fp = score_prediction(control, actual)` on exact `(host,rule)` pairs. **Falsifiability is a numeric gate:** `control_ratio = control_precision/precision`, and both `GATE_B2C` and `FOLD_GATE` require `control_ratio <= 0.5` — the real prediction must be **≥2× as precise as the shuffle** or the gate is judged to encode nothing (infragraph-eval.py:280-323).
- `rule_family()` (infragraph.py:461-488) is a versioned, deliberately-non-gameable coarse map (host-down / etcd / k8s-pod / rag / resource / backup / other) that family-granular scoring and the fold gate depend on — changing it invalidates learned cascade stats.

### 1.6 What TG's EstateGraphBuilder / ResolutionPolicy MUST replicate (checklist)

1. The full source→edge-type→confidence table (§1.1), sourced from the pve/netbox/librenms adapters (spec/008), plus L1/L2 learners.
2. Per-source-isolated ordered seeder with loud per-source error reporting (not all-or-nothing).
3. MAX-confidence ratchet on `(src,tgt,rel_type)`, valid_until refresh, single shared dynamics row, chaos-overrides-source — **decide emergent-max vs explicit-precedence**.
4. Mandatory entity resolution (normalize + resolve-or-typed-default) before every edge write; typed `entity_type` + `rel_type` enums.
5. Path-product confidence + shortest-then-highest per-node reduction + cycle guard + depth caps (3/2/cap 5); decide the dynamics-INNER-JOIN inclusion rule.
6. Per-edge `expected_alerts` + learned confidence + delay p50/p95; window `max(900, 2·p95)`.
7. Common-cause siblings at 0.6× confidence.
8. Laplace(1,4)/tau=0.10 cascade-probability gate, applied symmetrically to control, `drop=False` on the action lane.
9. 7d valid_until on live sources / open-ended on declared+tunnels / traversal freshness filter / per-source last-seed staleness metric; decay layer reporting-only.
10. Real degree-preserving shuffled control + `control_tp/control_fp` scoring + `control_ratio<=0.5` gate.
11. Criticality/tier catalog + `_P0_HOSTS_BASE` floor + DRIFT detection, kept OUT of graph-degree derivation, stored where daily regeneration cannot overwrite operator edits.
12. Fail-CLOSED eligibility (target-not-in-graph ⇒ eligible=False); treat empty blast radius as "advisory-absent / no graph data," never a valid prediction.

### 1.7 Where the original can be improved (do NOT port faithfully)

1. **Provenance-misattribution bug:** the MAX-merge UPDATE never updates the `source` column (infragraph.py:238-242), so a NetBox edge (0.85) later strengthened by LibreNMS (0.90) stores conf 0.90 but provenance still "netbox," and `traverse()` surfaces the wrong `d.source`. **Fix:** store the source of the *winning* confidence, or keep a per-source contribution map.
2. **No source-vs-source reconciliation.** Conflicting attributes/edges from two sources are simply superimposed and left to confidence — nothing flags disagreement. TG should emit an explicit reconciliation/drift event when sources contradict on the same resolved entity.
3. **`StrictHostKeyChecking=no` live-placement SSH** (seed:198) violates TG INV-02 — replace with a capability-scoped, host-key-pinned ingest adapter.
4. **Single-PVE-cluster assumption** (one `pvesh` returns all guests, seed:196) — generalize to N org clusters / non-Proxmox hypervisors.
5. **Confidence constants are scattered hard-coded floats** — centralize as named per-source/per-rel_type policy rows.
6. **Incident edges learned from fixed-900s co-occurrence alone** manufacture false-positive edges and floor recall (infragraph-eval.py:19-23) — require a topological prior (a real runs_on/routes_via edge) before co-occurrence may strengthen an edge; keep the 0.75 cap as a tested invariant.
7. **Never gate on raw structural precision (~0.05–0.15 by design)** and never ship `GATE_B2C` (0.95 precision @ 0.8 confidence) as an aspirational target — real cascade confidence tops out ~0.70, making it **structurally unsatisfiable**; only reversible fold/dedup at 0.80 precision, guarded by the never-auto floor, tolerates the stochastic ceiling.
8. **Declared-edge table lives inside the regenerated `host-blast-radius.md`** (hand-edits get wiped, refresh:210-319) — store operator-declared edges where daily regeneration cannot overwrite them.
9. **Bi-temporal decay shipped dark (reporting-only)** — either wire it into scoring or delete it; do not ship a dead capability.

---

## 2. PORT-FIDELITY FINDINGS (ranked, most safety-relevant first)

Each item flagged **[MR]** is a recommended TG fix/merge-request.

**P0 — foundational / directly unsafe**

1. **[PARTIAL — estate MRs + !77]** ~~Estate graph builder entirely absent; prediction gate wired to an EMPTY graph.~~ The multi-source causal graph (`core/estate`: model + `Build` + MAX-ratchet + path-product blast radius + siblings + degree-preserving control) is built and wired into `cmd/worker` (`estate.Build`, `PredictionEligible`/`BlastRadiusWide` computed over it). **[SUBSTANTIALLY FIXED]** All three concrete topology readers now exist and are worker-wired: `netbox.EstateSource` (!77, VM placement → `runs_on`), `librenms.EstateSource` (!78, `dependency_parent_hostname` → `depends_on`), and `pve.EstateSource` (!79, cluster resources → `runs_on`, the 0.95 source-of-truth). Each is seeded when its endpoint is declared (`TG_NETBOX_URL` / `TG_LIBRENMS_DEPLOYMENTS` / `TG_PVE_URL`), per-source-isolated with errors surfaced — so a configured deployment has a NON-empty, multi-source, MAX-ratcheted blast radius and a real prediction; PredictionEligible + BlastRadiusWide + the negative control are all LIVE. The **operator-declared edge source** (!80) closes the administrator-defined-topology requirement: `estate.DeclaredSource` + `ParseDeclared` load an operator-maintained edge file (`TG_ESTATE_DECLARED_FILE`) at SourceDeclared 0.85 — a LIVE source (PVE 0.95 / NetBox·LibreNMS 0.90) always out-ranks a declared edge on the same key via the MAX-ratchet, so "live devices state is the source of truth" holds by construction while declared fills gaps; a malformed declaration is rejected loudly, never seeding a phantom edge. The **learned tier** (!84) is now built: `estate.LearnedSource` turns repeated incident co-occurrence (≥ LearnedMinObservations) into `depends_on` edges at `LearnedConfidence` (hard-capped 0.75 — below every live source and the 0.80 suppression cutoff, so a heuristic edge only enriches prediction, never outranks truth or suppresses). Fed from an operator-exported co-occurrence file (`TG_ESTATE_LEARNED_FILE`) until the outcome-labelled memory loop feeds it automatically. The **tunnel tier** (!85) completes the source model: `estate.TunnelSource` emits `routes_via` edges at SourceTunnel 1.0 (the top confidence — a tunnel is ground truth) from a declared tunnel file (`TG_ESTATE_TUNNEL_FILE`), placing a cross-site VPS in its firewall's blast radius. **All six confidence tiers are now built** (tunnel 1.0 > pve 0.95 > netbox/librenms 0.90 > declared 0.85 > learned ≤0.75) AND they now COMBINE coherently: cross-type entity reconciliation (!86) makes the blast-radius walk name-canonical, so the same machine seen by NetBox (`physical_host`), PVE (`pve_node`), and LibreNMS (`host`) merges its edge sets into ONE blast radius instead of three disconnected typed twins; a domain-qualified endpoint also resolves to its bare form. The automatic outcome-labelled feed (!87) is now built: `core/learn.CoOccurrenceLearner` turns the OBSERVED alert stream into co-occurrence counts (earlier host = root, later = consequent, within a cascade window) — the "outcome-labelled memory" dimension realized in READ-ONLY mode, no action required — and snapshots them into an `estate.LearnedSource`. Determinism: every timestamp comes from the observation, not a wall clock. Remaining: wiring the learner to the live ingest stream + a periodic estate re-seed (a runtime integration; the learning mechanism itself is complete and oracle-tested). Until a source is configured the graph is empty and the gate correctly fails closed on eligibility (a vacuous prediction is never emitted).

2. **[MR] Fail-OPEN eligibility default.** `temporal/runner/activities.go:87-92` `eligible()` returns TRUE when the hook is nil, and `cmd/worker` never sets `PredictionEligible`, so `HasPrediction=true` is fed to risk classification for **every** host against the empty graph. Predecessor fails CLOSED when the target is not in the graph (infragraph.py:836-838). Inverts the fail direction; latent now, live at Phase 2.

3. **[MR] Server-side op-class/reversibility is NOT derived — TG trusts the model's self-declared JSON.** `proposal/parse.go:60-79` → `activities.go:74-82` → `risk.GatedInput.OpClass` unchecked. A proposal `{op_class:'restart-service', reversible:true, op:'dropdb …'}` classifies AUTO. Only proxmox reboot/halt and kubectl delete/drain re-derive floor-class at the leaf; mkfs/dropdb/terraform-destroy/credential-revoke/config-overwrite have no backstop. Inverts the predecessor's adversarial "a plan cannot hide a mutation."

4. **[MR] Stateful-workload denylist dropped entirely.** No TG analog of `_STATEFUL_DENY_RE` (~30 classes: etcd/postgres/redis/statefulset/vault/kafka/…, classify-session-risk.py:375-381). A reversible `kubectl rollout-restart` of an etcd StatefulSet → AUTO (quorum/data loss).

5. **[MR] Known-transient suppression fires on bare `AlertRule` string equality.** `core/suppression/knownpattern.go:8-31` drops all four predecessor gates: confidence ≥0.7 floor, required transient keyword, 7d recency, and the governance-demotion (`analysis_only`→escalate) escape (tier1_suppression.py:363-409). **`spec/005/design.md:56` claims keyword+confidence gating the code does not implement.** Largest false-suppression risk in the port.

6. **[MR] Maintenance-window / chaos freeze omitted entirely.** No equivalent of `suppression-gates.sh` (used by ~30 scripts) anywhere in `core/`/`temporal/`. TG will spawn remediation sessions for the very alerts a declared maintenance window is expected to cause.

7. **[MR] Cross-site exclusion drift in `ComputeVerdict` weakens the fail-closed signal.** `core/verify/verdict.go:58-60` `if a.Site != pred.Site { continue }` excludes any alert whose site **differs OR is empty/unknown**. Predecessor excludes only when BOTH sites are known and differ; unknown/VPS/empty-site hosts are never excluded (conservative). The estate HAS such hosts (notrf01vps01/chzrh01vps01/txhou01vps01 with routes_via edges), so a genuine cascade to a VPS during a single-site action is silently swallowed as a match. Fail-safe direction inverted; the verdict.go doc's "never hide a cascade on a named host" claim is technically true but hides one on an unnamed empty-site host.

8. **[MR] R0 verdict-gate granularity lost in reconcile.** `core/reconcile/reconciler.go:66-73` demotes only `VerdictDeviation`; a **partial** verdict + auto band + confirmed-clear auto-closes to Done (predecessor demotes partial too, reconcile-completed-sessions.py:370-378). No pending/unevaluated state ⇒ a write-action can auto-close **before its blast-radius verdict is computed**; fail-closed-on-lookup-error is not representable in `FinishedSession`.

**P1 — masks incidents / weakens guarantees**

9. **[MR] Dedup dropped open-issue semantics.** `core/suppression/dedup.go:46-61` collapses ANY prior `(host,rule)` in-window regardless of prior outcome or whether the parent issue is still open (`TriageEntry` has no Outcome/IssueID). Predecessor dedups only against a prior `escalated` entry AND confirms the parent YT issue is still open (tier1_suppression.py:109-165). A genuine re-fire after the prior incident closed is silently suppressed.

10. **[MR] Re-running discovery silently DEMOTES live scheduled-reboot rows.** `core/suppression/discover.go:20-26` / `persist/scheduled_reboots.go:83` unconditionally force `Status=observing`, `ObservedCount=0`, overwrite. Predecessor's `ON CONFLICT` deliberately preserves status/observed_count/kill_switch (scheduled_reboots.py:271-280). A weekly sweep un-promotes every live schedule. Registry key also drops `cron_expr` (host,kind only) so two crons of the same kind collide.

11. **[MR] Promotion drops boot-timestamp dedup + accumulation.** `core/suppression/promote.go:35-44` recomputes `inWindow` from one call's slice, no dedup, no 10-cap, overwrites `ObservedCount`. Predecessor dedups by boot iso, caps at 10, accumulates across runs (scheduled_reboots.py:302-308). A single boot seen in overlapping journalctl lookbacks can promote to "live" on ONE boot, defeating observe-before-live.

12. **[MR] Reactive-vs-clean boot gate absent.** Predecessor registers an observing row only for CLEAN boots (reboot.target/systemd-reboot/syncing filesystems) and never for REACTIVE ones (oom/panic/watchdog/hung_task/emergency/self-heal/thermal, classify-reboot-alert.py:43-77). Without it, an OOM/self-heal reboot near a cron minute can be learned as "scheduled" and later suppress real incidents.

13. **[FIXED — MR !72]** ~~Negative control is not a real shuffle, is never scored, and has no ratio gate.~~ The committed control now uses `estate.ShuffledControl` (degree-preserving: out-degree + per-rel_type target multiset preserved, real topology destroyed, seeded blast-radius walk) when an estate is wired, via `InfragraphModel.controlHosts`; an unresolvable target yields an empty control. `ScoreControl(record, observed)` scores real vs control host-level TP/FP with `ComputeVerdict`'s exclusions; `ControlScore.Ratio()` = control_tp/real_tp and `Falsifiable()` gates it at `ControlRatioCeiling` (0.5). INV-22 is now behaviorally satisfied, not just shape-satisfied — oracle proves a real prediction separates from its control and a vacuous one does not. (The old flat count-only shuffle remains ONLY as the no-estate fallback.)

14. **[MR] Reconcile→escalation bridge unwired (orphaned-poll re-check).** Nothing outside tests calls `escalation.Controller.ScheduleReCheck`; `Reconcile` has no POLL_PAUSE/orphaned-poll branch. This is the IFRNLLEI01PRD-1536 fix (a 90→100% disk poll that went unanswered and silently worsened). REQ-206 trigger side is missing end-to-end.

15. **[FIXED — MRs !61, !76]** ~~Judge-liveness dropped the window lower bound (-2h lag); frontier cross-check not ported.~~ The `-2h` Lag lower bound was restored in !61 (excludes just-ended not-yet-judgeable sessions). **Frontier cross-check now ported** (!76): `core/governance/frontier_crosscheck.go` `FrontierCrossCheckMonitor` catches DRIFT (local judged but disagrees with an independent frontier re-judgment over the same rubric — liveness reads healthy) and confirmed DEATH (frontier scores sessions the local judge left `-1` — the exact 3-week dead-judge class no purely-local metric catches). Pure `Evaluate` decision behind an injected `PairSource`; oracle-tested; lockstep-bound to spec/004.

16. **[PARTIAL — MRs !70, !74]** Five classifier safety branches were DORMANT (NovelIncident, CriticalityTier, BlastRadiusWide, SilentCognitionGuard/AutoResolveMarked/Evidence, HasVerdict/Verdict handled in `classifier.go` but never populated by `activities.go`). **CriticalityTier** (!70): `Deps.CriticalityTier(host)` from an operator-declared P0-host set (`TG_CRITICALITY_TIER_HOSTS`) ceilings a P0 host at AUTO_NOTICE. **BlastRadiusWide** (!74): `Deps.BlastRadiusWide(host)` computes the host's estate blast-radius width against an operator threshold (`TG_BLAST_RADIUS_WIDE_THRESHOLD`, default 8) and ceilings a wide cascade at AUTO_NOTICE; empty estate ⇒ no host wide (fail-safe), goes live as topology seeds. **NovelIncident** (!81): `Deps.PriorIncidents(host, alert_rule)` with positively-established-novelty semantics — a class forces a poll only when its prior count is KNOWN and zero; an unknown count never fires (no false poll from a missing store). `AlertRule` is now threaded into ClassifyInput. All fail safe when unwired. **SilentCognitionGuard** (!82): the guard is always active (INV-11); ClassifyActivity binds the proposal's cited evidence ids against the orchestrator-captured tool results (threaded through InvestigateResult) — a citation that binds nothing (hallucinated id, failed tool, off-target result) strips the AUTO-RESOLVE and polls. **All five dormant branches are now wired** except HasVerdict/Verdict, which is post-execution and belongs to Phase 2 (mutation OFF ⇒ no verdict yet).

**P2 — floor completeness / dropped mechanisms / correctness**

17. **[MR] Missing irreversible floor slugs** (safety.go:63-67 has only 12): wipefs/shred/blkdiscard/dd-to-/dev, vgremove/lvremove/pvremove, zfs-rollback/zpool-offline, drop-table/truncate-table, docker volume/system/network prune, network-catastrophic verbs, code-deploy/repo-write. Unknown-op fails closed only when `Reversible==Irreversible` — a model-declared bool — so an unlisted destructive op labeled `reversible=true` reaches AUTO.

18. **jailbreak floor slug is dead** (safety.go:66) — no detector feeds it; the inline prompt-injection screen (classify-session-risk.py:784-808) is unported.

19. **[SUBSTANTIALLY FIXED — MRs !68, !83]** ~~Cron window symmetric + same-day-only + minimal parser.~~ Cross-midnight evaluation restored (!68: fires checked on the alert's day + adjacent days). The parser now handles the FULL crontab grammar (!83): `*`, single values, ranges, steps `*/s` and `a-b/s`, comma-lists, day-of-month + month, and cron's DOM-or-DOW day semantics (Sunday 0 or 7) — a weekday-range/monthly reboot cron matches, malformed fields fail open. Remaining (deferred, needs the chain assembled): the reboot-rule allowlist as data, the dark-launch arm-switch (`TIER1_SCHED_REBOOT_ENABLED` default off), and renew-on-match.

20. **[PARTIAL — estate MRs]** ~~Per-edge `expected_alerts` collapsed to one flat `DefaultRules`; siblings omitted.~~ The estate graph carries per-edge `ExpectedAlerts`; `Predict` adds each impact's own expected alerts (falling back to `DefaultRules` only when an edge names none) AND folds common-cause `Siblings` — so `ComputeVerdict`'s "partial" branch now has real per-(host,rule) content. **Laplace-smoothed edge confidence now built** (!90): `estate.LaplaceConfidence(hits, trials)` = capped `(hits+1)/(trials+2)` — the learned tier is now BASE-RATE-AWARE, so a dependent that follows a primary 5/5 times outranks one that follows 5/50; the co-occurrence learner records per-host trial counts (`PrimaryTrials`) to drive it, falling back to the count-only ramp when trials are unknown. STILL open: the dynamic verification window (a prediction-precision refinement, not a correctness gap).

21. **[PARTIAL — MR !71]** Conservative-remediation carve guards. **Self-protected control-plane restart (`_SELF_PROTECTED_RESTART_RE`) now ported**: `safety.IsRestartClass` + a config-declared `Deps.SelfProtectedService` (`TG_SELF_PROTECTED_SERVICES`) veto a restart of the platform's own services to POLL_PAUSE. STILL open: destructive-sibling co-occurrence (`_CONS_BLOCKERS`) and guest-loop/2+-reboot quorum — both keyed on a MULTI-command plan blob, which TG's per-action model (one argv per decision) largely dissolves; revisit if/when a compound-action surface appears. Note TG never had the coarse-high→carve-down mechanism (it fails closed by default), so the carve BLOCKERS are the only faithful part to port.

22. **[FIXED — MR !69]** ~~Category/severity risk defaults dropped as a band driver~~ — `safety.HighRiskCategory` restores the predecessor's `HIGH_RISK_CATEGORIES` set; `Classify` now forces POLL_PAUSE for maintenance/security-incident/deployment (safe-direction clamp, step 2), threaded from the normalized `category` label via the runner. spec/001 + spec/012.

23. **Provenance-misattribution bug (§1.7-1)** — flag as **fix-on-port**, not faithful replication.

24. **Circuit breaker not ported** despite CONSTITUTION.md:130,134 promising "named, observable circuit breakers with persisted state" — model-gateway/judge/RAG calls have no bounded-failure fallback (the very outage class behind the dead-judge incidents).

25. **[FIXED — MR !73]** ~~Active-memory (Phase 3) suppression stage is dangling documentation.~~ `core/suppression/activememory.go` implements `ActiveMemoryStage` + typed `SuppressRule` (host/rule `path.Match` globs + operator reason), porting the predecessor's `openclaw_memory` `suppress:<reason>` triage-rules: critical/unknown never suppressed (defense-in-depth), a malformed glob fails open, no match fails open. Registered under T-005-5, lockstep-bound, oracle-tested.

26. **[FIXED — MR !75]** ~~`infragraph_prediction` migration + cascade_stats table do not exist.~~ Migration `0002_infragraph_prediction` creates the append-only `infragraph_prediction` spine (`(plan_hash, kind)` PK; immutable identity + `control_hosts` for INV-22; nullable verify-time `tp/fp/fn/control_tp/control_fp`; `schema_version` CHECK > 0 matching the registry) AND `infragraph_cascade_stats` (windowed `control_ratio` + `falsifiable`). Down pair + an offline pure-Go migration test (up/down pairing, the registered table + control/schema-version columns) guard it. The pgx store binding is now built (!92): `db.PredictionStore` implements `predict.PredictionStore` over the table (append-only `ON CONFLICT DO NOTHING`, sorted-jsonb sets, `control_hosts` persisted for INV-22), worker-wired when `TG_DB_DSN` is set (else the in-memory oracle twin). The set↔jsonb marshaling is unit-tested; the round-trip is a compose-only integration test (skipped without `TG_TEST_POSTGRES_DSN`).

**Faithful (worth preserving as the model)**

- `ComputeVerdict` three-way match/partial/deviation + target-self exclusion + deviation-never-auto (verdict.go:52-75) — clean port; the `gated`-unexported-field makes an ungated approval poll uncompilable.
- Prediction-gate spine: append-only first-wins commit, full-SHA-256 plan_hash (closes the predecessor's 16-hex collision surface), action_id threading, default-deny GatedProposal.
- Governance hash-chain + retention split made structural-by-construction (INV-19, `retention.go:60-70 ErrSpineTouched`) rather than a retrofitted chain over a mutable table — hold this up as the model for re-expressing the other dropped controls.
- `core/actuate` interceptor: single unexported chokepoint, fail-loud SelfTest, never-auto floor at the adapter, mutation ships OFF behind a proof-gated path.
- Three-band model with `BandPollPause=0` / `Reversibility=Irreversible` zero-values — fail-closed as a construction property.

---

## 3. TOP INSIGHTS & TRAPS (preserve or avoid)

0. **[BUILT — MR !88] The namesake control, the TERRITORY GATE, is ported** (`core/territory`, from the predecessor PreToolUse `territory-gate.py`): a mutating action in a high-stakes territory (k8s/network/edge/pve/native/docker) may proceed only once that territory's operating manual is acknowledged this session — the "grounding" the product is named for. Read-only is never gated; a confirmed infra write the gate cannot place fails CLOSED. Pure typed gate, oracle-tested; composes into the Phase-2 interceptor chain when mutation is earned.

1. **The #1 predecessor failure mode is "wired-but-disconnected dead capability"** (FAISS index never read, jailbreak detector never inline, bi-temporal decay reporting-only, intermediate rail dark). **TG is re-importing it right now** — the fail-closed PredictionGate is wired to an empty, builder-less graph. Lesson: wire the estate graph end-to-end or do not claim the capability. Do not ship an inert gate.

2. **Raw structural blast-radius over-predicts by design (~0.05–0.15 precision).** Never present or gate on raw precision — a PrecisionDrop alert on it is permanently firing or permanently inert. The operative unit is **fold-band, rule-family precision on the `cascade_prob_family ≥ 0.60` subset**.

3. **Calibrated cascade probabilities top out at ~0.70** ("no real infra cascade is 95% deterministic"), so the 0.95-precision-@-0.8-confidence auto-resolve gate is **structurally unsatisfiable**. The operator abandoned prediction-driven auto-resolution and moved to a **reversible fold/dedup gate at 0.80 precision guarded by the never-auto floor**. Only reversible dedup tolerates the stochastic ceiling. Porting the 0.95/0.8 gate as an aspiration re-imports a dead metric.

4. **The two safety asymmetries are load-bearing:** (a) cascade gating uses `drop=True` in the shadow/fold lane but **`drop=False` in the fail-closed action lane** — dropping a host there would flip a real cascade match→deviation; (b) the whole system **fails OPEN for the triage/advisory lane, fails CLOSED for the remediation lane.** Get either backwards and the safety contract inverts.

5. **Siblings (common-cause co-failure) is a distinct signal a pure who-depends-on-me walk misses.** The 2026-05-08 pattern (4 VMs flap, node stays silent) is exactly why it exists at 0.6× confidence. Omitting it makes every sibling co-failure a deviation → over-escalation that tanks the very match/auto-resolve rate the gate exists to raise.

6. **Learning edges from fixed-window co-occurrence alone is a trap.** Independent co-incident alerts manufacture false-positive edges (hurts precision) and recall is an explicit LOWER BOUND (infragraph-eval.py:19-23). Require a topological prior before time co-occurrence may strengthen an edge; keep learned edges capped at 0.75, structurally below the 0.80 suppression cutoff.

7. **Declared/hand-maintained truth goes stale and contradicts live** (canonical: n8n01 declared on pve01, live seeder corrected to pve04). Resolve by (confidence, freshness) with live-wins-over-stale + 7d TTL, NOT last-writer-wins. And the criticality catalog living inside a regenerated doc means hand-edits get wiped — a non-obvious coupling.

8. **A single self-referential liveness metric is insufficient** — the judge was dead for 3 weeks and only the independent frontier cross-check (sampling the judge-independent sessions table, so a judge writing zero rows is still caught) would have found it. Shipping judge-death detection without a second, model-independent opinion reintroduces that blind spot.

9. **Fail-safe direction is everything, and TG has quietly inverted it in three places:** cross-site exclusion (excludes empty-site → hides deviations), eligibility default (nil hook → true), and unknown-op fail-closed (depends on a model-declared bool). The predecessor's rule is uniform: **unknown ⇒ fail toward the conservative/escalate/never-auto side.**

10. **The predecessor's own retrofitted controls are cautionary:** the hash-chain was bolted on late (migration 021) over a mutable table and had a busy-timeout race that left the audit table empty; the fail-closed branch once exited before the audit write. TG's structural-by-construction approach (zero-value fail-closed, required-field audit output) is the correct antidote — apply it to every remaining port.

11. **Backtest evidence was retrodiction-contaminated on a tiny sample.** Make prediction-accuracy evidence forward-only (holdout/natural traffic), and state `n` + the honest confidence ceiling next to any accuracy claim.

12. **Reactive reboots are symptoms, never schedules.** Learning an OOM/panic/watchdog reboot as "scheduled" would let it suppress the next real incident — a genuine safety mechanism (the clean-vs-reactive gate) that TG dropped.

---

## 4. IMPROVEMENT IDEAS (concrete, ranked)

1. **Build the EstateGraphBuilder now, before Phase 2 mutation.** A Temporal-scheduled read-only activity that populates `graph_entities`/`graph_relationships`/`infragraph_dynamics` from the spec/008 pve/netbox/librenms adapters, reusing the exact source→edge-type→confidence table (§1.1) with the layered ResolutionPolicy (live 0.95 > librenms 0.90 > netbox 0.90/0.85 > declared 0.85 > mined ≤0.75). Wire its output into `cmd/worker/main.go` instead of the empty map. Single highest-value gap; everything else in section 1 hangs off it.

2. **Introduce a server-side ResolutionPolicy that derives op_class + reversibility + floor-membership from the graph-resolved action (target-type + op + params) and OVERRIDES the model-declared proposal fields** before they reach `GatedInput`. Closes the core adversarial bypass (finding P0-3) and gives findings P0-4 and P2-17 a real home. Tag each node's stateful-ness / territory-class / host-vs-guest identity so the policy can clamp stateful rollout/scale/reboot to POLL_PAUSE without regex.

3. **Add edge weight + per-edge `expected_alerts` to `DependencyGraph` and implement path-product confidence with shortest-then-highest per-node reduction, the siblings mechanism (0.6×), the dynamic window `max(900, 2·p95)`, and the Laplace(1,4)/tau=0.10 cascade gate applied symmetrically to the control with `drop=False` on the action lane.** These are the Phase-2 mechanisms the port dropped; without the gate TG re-inherits the -1118/-1119 precision collapse.

4. **Rebuild the negative control as a true degree-preserving graph shuffle, score `control_tp/control_fp` in `VerifyActivity` against the same observed set, and add an eval that fails when `control_ratio > 0.5`.** Makes INV-22 "populated" actually true and gives TG the predecessor's go/no-go falsifiability test. Alert on fold-band precision (`cascade_prob_family ≥ 0.60`), never raw structural precision; encode the ~0.70 confidence ceiling and the 0.80 reversible-fold bar as first-class constants.

5. **Fix the three inverted fail-safe directions:** (a) `ComputeVerdict` cross-site exclusion → exclude only when both sites are known and differ; (b) `eligible()` → fail closed (false when no prior/empty graph), treating an empty blast radius as "advisory-absent"; (c) route unknown-op fail-closed through the server-derived op-class, not the model bool. Low-effort, high safety payoff.

6. **Restore the suppression floors:** known-transient confidence ≥0.7 + transient-keyword + 7d recency + governance-demotion escape (make code match `design.md:56`); dedup open-issue semantics (add Outcome + IssueRef + injected fail-open open-issue checker); and port the maintenance/chaos freeze as an org-scoped policy node consulted before per-host resolution.

7. **Fix the scheduled-reboot regressions as one MR:** upsert that PRESERVES status/observed_count/kill_switch on re-discovery, registry key including `cron_expr`, boot-timestamp dedup + 10-cap + accumulate-across-runs in Promote, the reactive-vs-clean boot gate before registration, asymmetric `[fire-5m, fire+10m]` window with prev/next-fire evaluation, `valid_until` default (90d) + renew-on-match, and `journalctl --list-boots -o json` epoch-microseconds (never `--utc`, to avoid the CEST off-by-2h promotion bug).

8. **Wire the reconcile side:** extend `FinishedSession` to distinguish `{noPrediction, pending, matched, partial, deviation, unevaluated-past-window, lookupError}` with fail-closed zero-value; demote on partial + lookupError, skip on pending, auto-resolve only on matched; add the POLL_PAUSE/orphaned-poll branch that calls `ScheduleReCheck` (REQ-206); page the approver graph on any close-out demote (a silent To-Verify is invisible to an operator who ignores the queue); add age gating (min-idle / recent-24h / very-old-48h) so a first pass doesn't mass-transition a backlog.

