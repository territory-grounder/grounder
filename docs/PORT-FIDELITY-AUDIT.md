> **NOT A WORK QUEUE — reference only.** The one authoritative queue is
> [`BOARD.md`](BOARD.md): work that, top-down. Anything below that reads as a priority, a ranking, a
> "first step" or a "do this next" is a RECORD OF WHAT WAS THOUGHT AT THE TIME and does not steer.
> The complete inventory of open work is YouTrack `project: TG #Unresolved` — the board is the ranked
> queue, not the inventory, and its silence is not closure.
>
> *Why this banner exists: on 2026-08-02 seventeen files in this repo told a reader what to do next. One
> of them had listed "Wire `VoteAdmission`" as a named GATE; that file was later quarantined for being
> unreadable and the gate went with it, leaving the approval control enforced by nothing from July to
> August (TG-254). A second authority is how a finding gets lost.*

> **SCOPE NOTE (added 2026-08-01).** This ledger is CLOSED **within its 8 audited subsystems**
> (estate-graph, suppression, classifier, screening, breakers, …). It is not a statement about the
> predecessor as a whole: retrieval, context assembly, the knowledge-base write-back loop, the skills
> layer and episodic memory were never in its scope — retrieval appears once, as finding #24's missing
> circuit breaker. Mechanisms outside this scope are tracked in
> [`PREDECESSOR-MECHANISM-INVENTORY.md`](PREDECESSOR-MECHANISM-INVENTORY.md), which found several
> ABSENT items. "26/26 adjudicated" means 26 of 26 *findings*, not 26 of 26 *mechanisms*.

####################################################################################################
RE-SCORE 2026-07-31 (adversarially verified; the initiation-gate ledger)
####################################################################################################

Re-score of all 26 findings at post-recovery HEAD (2026-07-31): six parallel scoring passes over
code + git history, every improved status independently re-verified by an adversarial default-refute
pass (zero refutations). The per-finding dated tags in §2 carry the evidence; this section is the
ledger and its consequences.

**The ledger** (updated 2026-07-31 after TG-223/TG-224 and TG-220 landed)**:** fixed **18** (#1 #2 #3
#4 #6 #7 #8 #9 #13 #14 #16 #17 #18 #20 #22 #23 #25 #26) · partial **8** (#5 #10 #11 #12 #15 #19 #21
#24) · open **0** · na-by-design **0**. `outcome_cost` is **theoretical on all 26** — no residual
carries a measured live cost today. #16/#17 moved PARTIAL→FIXED under the owner BUILD rulings below;
#20 moved PARTIAL→FIXED when TG-220 landed the learned falsifiability window. The 2026-07-31 re-score
scored these partial; these lines are post-re-score corrections as gate work closes, never revisions
of the scoring pass. Each §2 tag carries the commit + oracle evidence.

**Ledger movement since:** **2026-07-31 (TG-219)** — #19 #11 #12 #10 #5 re-scored PARTIAL→**FIXED**
(the suppression-learning chain assembled with production callers, shipped with its unlearning half).
**FINAL LEDGER 2026-07-31 — the initiation gate is CLOSED.** fixed **25** · owner-waived
N/A-by-design **1** (#21) · open **0**. All 26 findings adjudicated. Gate work TG-219..224 all
LANDED (TG-221 #24 model-path breaker, TG-222 #15 governance monitors, both merged 2026-07-31);
#21 waived with an auto-reopen tripwire (compound-plan surface). No finding remains unaddressed.

Versus the frozen 2026-07-16 tags: **9 newly closed** (#1 #2 #3 #4 #6 #7 #9 #18 #23); the six other
frozen-FIXED confirmed holding (#8 #13 #14 #22 #25 #26 — #13/#25/#26 materially deepened since the
freeze); **two downgrades** (#15 FIXED→PARTIAL, #19 SUBSTANTIALLY-FIXED→PARTIAL) are status-honesty
corrections under the conservative unexercised-path rule, not code regressions — the code the frozen
doc credited exists; it has no production caller.

**This ledger is now the CLOSED checklist for the head-to-head initiation gate**
(`tools/shadowbench/PRE-REGISTRATION.md` §6; owner doctrine, docs/BOARD.md workstream D): every
finding must end **fixed**, **na-by-design**, or **owner-waived**; no additions without owner
sign-off. The 11 partials as re-scored resolved through exactly four gate work items plus an owner
waiver list (three of which — TG-223, TG-224, TG-220 — have since landed; see the rulings below):

**Gate work items (issues filed; must land — or be owner-waived — before initiation):**

- **TG-219 — LANDED 2026-07-31.** The observe→verify→promote chain with production callers, ScheduleKey carrying schedule identity (#10), boot-reason gating at registration (#12), and the demotion escape consulted before any learned suppression (#5) — learning shipped WITH unlearning per the owner ruling. Dark-launched OFF (TG_SUPPRESSION_LEARN_ENABLED), fail-safe without a boot-reason signal; per-worker in-memory storage and deferred durable verify are the follow-up slice (TG-225). Original finding text: Promote,
  RegisterObserving, TwoPhaseVerifier, and the boot-clean registration gate have zero production
  callers; TG runs only operator-declared schedules forced straight to LIVE. Head-to-head
  mechanism: the predecessor suppresses undeclared-but-regular reboots that TG escalates — a
  straight suppression-recall delta in TG's disfavor. Assembling the chain as-is would also revive
  two dormant hazards: the suppression-registry regKey is still `(host,kind)` so a shifted cron
  inherits promotion state (#10), and registration never checks boot reason (#12).
- **TG-220 — learned falsifiability window (#20). LANDED 2026-07-31 — FIXED, not pinned.** TG scored
  predictions in a fixed 10m window (`TG_FALSIFIABILITY_WINDOW`) where the predecessor used
  `max(900s, 2×p95)` learned per edge, so cascades slower than 10m adjudicated as misses in TG and
  hits in the predecessor. Because that bias sits in the comparison's OWN instrument, it was FIXED
  rather than declared as a constant in the frozen decision rule: the window is now learned per edge
  from observed cascade latency in TG's durable ingest ledger, bounded by an operator-visible floor
  (900s) and cap. See finding #20 for the implementation and spec/002 REQ-110 for the law. The INV-22
  degree-preserving control is the tripwire that the wider window recovers causal signal rather than
  ambient noise, and an oracle asserts `control_ratio` does not rise as the window widens.
- **TG-221 — breaker on the production model path (#24).** The persisted breaker is live for the
  mutation and cost lanes, but production model calls go through `adapters/model.Gateway` with no
  breaker (the guarded litellm module is unexercised). A gateway flap during the comparison window
  degrades judge cron and skill-gen unbounded where the predecessor's ladder contained it;
  invisible in healthy operation.
- **TG-222 — arm the governance monitors (#15).** Judge-liveness and frontier cross-check are
  code-complete with no constructor, no caller, and schedule workflows that are defined nowhere.
  Moves nothing while the judge is healthy; silently invalidates the comparison window if the
  judge dies — the exact 3-week dead-judge class the finding exists for, still undetectable.

**OWNER RULINGS 2026-07-31 (the waiver list is CLOSED).** The four partials outside TG-219..222
were adjudicated by the owner on the principle the charter states: *port the logic, not the code* —
a predecessor control is owed only where its attack surface exists in TG, and fidelity theater is
rejected. Rulings:

- **#5 — FOLDED INTO TG-219 (not waived, not standalone).** Dead today because nothing writes
  demotion rows — but TG-219 makes TG *learn* suppression patterns, and learning without
  unlearning is a one-way ratchet: the first bad lesson would suppress real incidents forever.
  The demotion escape ships as part of TG-219's done-shape. Learning and unlearning land together
  or neither lands.
- **#16 — BUILD (TG-223). DONE 2026-07-31 (ca3294b).** The finding's own deferral rationale ("mutation OFF ⇒ no verdict to
  classify on") has EXPIRED: mutation is Semi-auto/ON, verdicts are written on the execute path,
  and post-C4 they are meaningful (baselined, forecast/action-split). Wiring prior-verdict into
  classification extends TG's own "a deviation never auto-resolves again" one step earlier, is
  small, and fails toward caution.
- **#17 — BUILD (TG-224). DONE 2026-07-31 (c25cfe4).** Triple-shielded today (registry-only argv ⇒ unregistered verbs cannot
  execute; floor slugs; server-derived destructiveness), so this is insurance, not exposure — but
  it is ~an hour of regex plus tests, and the deriver must already know these verbs on the day a
  network or deploy op-class is first registered.
- **#21 — WAIVED, N/A-BY-DESIGN.** Compound-plan co-occurrence (destructive siblings, 2+-reboot
  quorum) detects a hazard in an input TG cannot represent: single Action, argv-only, no `sh -c`,
  per-argv backstop (INV-02). Building detection for an unrepresentable input is fidelity theater.
  **TRIPWIRE:** if a compound/multi-command plan surface is ever introduced, this finding REOPENS
  automatically and must be built before that surface ships.

With these rulings the gate distance was exactly: TG-219 (incl. #5), TG-220, TG-221, TG-222, TG-223
(#16), TG-224 (#17). No finding remains unadjudicated. **TG-223, TG-224 and TG-220 have since
LANDED** (all spec-bound; the safety pair carries owner-approved law-change trailers; TG-220 closes
finding #20 with the learned falsifiability window), so the remaining gate distance is
**TG-219 (incl. #5), TG-221, TG-222** — three work items.

**2026-07-31 — TG-219 LANDED.** Findings #19, #11, #12, #10 and #5 are re-scored FIXED above (each
tag names the mechanism and the real-path oracle). Remaining gate distance: **TG-220, TG-221,
TG-222, TG-223, TG-224** — 5 issues.

**WAIVER CANDIDATES (superseded by the rulings above; retained for the record)** — the partials the re-score judged inert or deliberately
deferred (every partial NOT covered by TG-219..222; each needs an explicit owner waiver, or a fix,
to close the gate):

- **#5** — the governance-demotion 4th suppression gate is dead code, and inert: nothing writes
  demotion rows, and the three restored floors already gate declared patterns.
- **#16** — HasVerdict/Verdict is post-execution and Phase-2 by the finding's own text (mutation
  OFF ⇒ no verdict exists to classify on); the other four branches are live.
- **#17** — the remaining network-catastrophic and code-deploy/repo-write verbs bite only if such
  an op is ever proposed; ~30 floor slugs plus server-derived destructiveness cover the rest.
- **#21** — destructive-sibling co-occurrence and the 2+-reboot quorum need a compound
  multi-command plan surface that does not exist (single-Action proposals, argv-only, no `sh -c`,
  per-argv destructive backstop).

**Comparison-ready, with caveats, not comparison-clean:** every fail-direction inversion and
trust-the-model bypass the audit ranked P0 is closed and exercised; zero findings are open
outright; every residual is enumerable with a known bias direction rather than an unbounded
unknown. What remains is either measurable in the results (TG-219's recall delta, which the
head-to-head will attribute correctly) or covered by a manual judge-liveness and gateway-health
check during the window (TG-222/TG-221, whose automated equivalents are dead today).
TG-220 is no longer on this list and was NOT pinned by procedure: a bias that sits inside the
comparison's own measuring instrument cannot be declared away in the decision rule, so the window is
now learned rather than constant (finding #20).

---

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

1. **[FIXED 2026-07-31 — both remaining items closed: learner wired to the live ingest stream (540e59e) + periodic estate re-seed folding learned edges (a2e24b4, `TG_ESTATE_REFRESH_INTERVAL` default 5m); exercised live at 369 nodes (115791c), SiteOf verdict scoping on the seeded graph (d5e052b); was PARTIAL — estate MRs + !77]** ~~Estate graph builder entirely absent; prediction gate wired to an EMPTY graph.~~ The multi-source causal graph (`core/estate`: model + `Build` + MAX-ratchet + path-product blast radius + siblings + degree-preserving control) is built and wired into `cmd/worker` (`estate.Build`, `PredictionEligible`/`BlastRadiusWide` computed over it). **[SUBSTANTIALLY FIXED]** All three concrete topology readers now exist and are worker-wired: `netbox.EstateSource` (!77, VM placement → `runs_on`), `librenms.EstateSource` (!78, `dependency_parent_hostname` → `depends_on`), and `pve.EstateSource` (!79, cluster resources → `runs_on`, the 0.95 source-of-truth). Each is seeded when its endpoint is declared (`TG_NETBOX_URL` / `TG_LIBRENMS_DEPLOYMENTS` / `TG_PVE_URL`), per-source-isolated with errors surfaced — so a configured deployment has a NON-empty, multi-source, MAX-ratcheted blast radius and a real prediction; PredictionEligible + BlastRadiusWide + the negative control are all LIVE. The **operator-declared edge source** (!80) closes the administrator-defined-topology requirement: `estate.DeclaredSource` + `ParseDeclared` load an operator-maintained edge file (`TG_ESTATE_DECLARED_FILE`) at SourceDeclared 0.85 — a LIVE source (PVE 0.95 / NetBox·LibreNMS 0.90) always out-ranks a declared edge on the same key via the MAX-ratchet, so "live devices state is the source of truth" holds by construction while declared fills gaps; a malformed declaration is rejected loudly, never seeding a phantom edge. The **learned tier** (!84) is now built: `estate.LearnedSource` turns repeated incident co-occurrence (≥ LearnedMinObservations) into `depends_on` edges at `LearnedConfidence` (hard-capped 0.75 — below every live source and the 0.80 suppression cutoff, so a heuristic edge only enriches prediction, never outranks truth or suppresses). Fed from an operator-exported co-occurrence file (`TG_ESTATE_LEARNED_FILE`) until the outcome-labelled memory loop feeds it automatically. The **tunnel tier** (!85) completes the source model: `estate.TunnelSource` emits `routes_via` edges at SourceTunnel 1.0 (the top confidence — a tunnel is ground truth) from a declared tunnel file (`TG_ESTATE_TUNNEL_FILE`), placing a cross-site VPS in its firewall's blast radius. **All six confidence tiers are now built** (tunnel 1.0 > pve 0.95 > netbox/librenms 0.90 > declared 0.85 > learned ≤0.75) AND they now COMBINE coherently: cross-type entity reconciliation (!86) makes the blast-radius walk name-canonical, so the same machine seen by NetBox (`physical_host`), PVE (`pve_node`), and LibreNMS (`host`) merges its edge sets into ONE blast radius instead of three disconnected typed twins; a domain-qualified endpoint also resolves to its bare form. The automatic outcome-labelled feed (!87) is now built: `core/learn.CoOccurrenceLearner` turns the OBSERVED alert stream into co-occurrence counts (earlier host = root, later = consequent, within a cascade window) — the "outcome-labelled memory" dimension realized in READ-ONLY mode, no action required — and snapshots them into an `estate.LearnedSource`. Determinism: every timestamp comes from the observation, not a wall clock. Remaining: wiring the learner to the live ingest stream + a periodic estate re-seed (a runtime integration; the learning mechanism itself is complete and oracle-tested). Until a source is configured the graph is empty and the gate correctly fails closed on eligibility (a vacuous prediction is never emitted).

2. **[FIXED 2026-07-31 — `eligible()` fails CLOSED on a nil hook and is wired to estate-graph membership (`estateHolder.Graph().Resolve`), running on every classification against the live 369-node graph; commit 089d65a]** ~~Fail-OPEN eligibility default.~~ `temporal/runner/activities.go:87-92` `eligible()` returns TRUE when the hook is nil, and `cmd/worker` never sets `PredictionEligible`, so `HasPrediction=true` is fed to risk classification for **every** host against the empty graph. Predecessor fails CLOSED when the target is not in the graph (infragraph.py:836-838). Inverts the fail direction; latent now, live at Phase 2.

3. **[FIXED 2026-07-31 — the named bypass is closed at three layers: destructiveness server-derived from the ACTUAL op string (`destructiveOpRE`, 844d23c + ab8c91b), re-derived again at the interceptor chokepoint, and the executed argv is built ONLY from the op-class schema registry (`sealedArgv`; unregistered class ⇒ nil argv, tier floored to Approve) — the model's free-text op never executes]** ~~Server-side op-class/reversibility is NOT derived — TG trusts the model's self-declared JSON.~~ `proposal/parse.go:60-79` → `activities.go:74-82` → `risk.GatedInput.OpClass` unchecked. A proposal `{op_class:'restart-service', reversible:true, op:'dropdb …'}` classifies AUTO. Only proxmox reboot/halt and kubectl delete/drain re-derive floor-class at the leaf; mkfs/dropdb/terraform-destroy/credential-revoke/config-overwrite have no backstop. Inverts the predecessor's adversarial "a plan cannot hide a mutation."

4. **[FIXED 2026-07-31 — `statefulDenyRE` restored (~35 classes, 2723a42) and made effective on this estate's real unbroken hostnames by removing the word-boundary anchors that left the port inert (300fef3); a reversible stateful mutation is downgraded to ReversibleMixed so the poll clamp is reachable — the etcd rollout-restart example now polls]** ~~Stateful-workload denylist dropped entirely.~~ No TG analog of `_STATEFUL_DENY_RE` (~30 classes: etcd/postgres/redis/statefulset/vault/kafka/…, classify-session-risk.py:375-381). A reversible `kubectl rollout-restart` of an etcd StatefulSet → AUTO (quorum/data loss).

5. **[FIXED 2026-07-31 (TG-219, per the owner ruling that learning ships WITH unlearning) — the fourth gate is now live: `core/governance.Demoter.EvaluateEvidence` writes the analysis-only row from PROOF (a learned suppression whose two-phase boot verify failed), `DemotionLookup` is consulted by phase SR before any LEARNED row may suppress (an unreadable state also refuses), the misfiring row is demoted in-path and its evidence cleared, and cmd/worker schedules the demote pass (`TG_SUPPRESSION_DEMOTE_INTERVAL`, default 1h). Scope note: the consult guards the LEARNED lane, which is the lane TG-219 creates and the only one that can produce a self-authored bad lesson; the known-transient stage's declared patterns stay operator-authored]** ~~Known-transient suppression fires on bare `AlertRule` string equality.~~ `core/suppression/knownpattern.go:8-31` drops all four predecessor gates: confidence ≥0.7 floor, required transient keyword, 7d recency, and the governance-demotion (`analysis_only`→escalate) escape (tier1_suppression.py:363-409). **`spec/005/design.md:56` claims keyword+confidence gating the code does not implement.** Largest false-suppression risk in the port.

6. **[FIXED 2026-07-31 — `FreezeGate` (scope host/rule/estate, `TG_SUPPRESSION_FREEZE_FILE`) consulted BEFORE the severity floor and run as the Runner's FIRST gate, so no remediation session spawns for a frozen alert (aac76ad, same-day-as-freeze); malformed/inverted rows fail toward investigating. Residue: windows load at worker start, so an ad-hoc immediate freeze needs a file edit + restart — pre-declarable, fail direction safe]** ~~Maintenance-window / chaos freeze omitted entirely.~~ No equivalent of `suppression-gates.sh` (used by ~30 scripts) anywhere in `core/`/`temporal/`. TG will spawn remediation sessions for the very alerts a declared maintenance window is expected to cause.

7. **[FIXED 2026-07-31 — fixed in three stages: the fail-open self-reported-site line deleted (1de24b4/29173f3, 2026-07-16), then the predecessor's `_host_site()` mechanic restored on estate-derived terms in the 07-30 adjudication repair (a2e57e5): `ComputeVerdictDetailScoped` excludes ONLY when BOTH sites are estate-known AND differ, unknown/empty-site hosts are never excluded, `SiteAuthority` wired from `estate.Graph.SiteOf` at all three verdict authors (interceptor, asyncverify, falsify scorer), plus REQ-108 rule-family matching — the conservative posture this finding demanded]** ~~Cross-site exclusion drift in `ComputeVerdict` weakens the fail-closed signal.~~ `core/verify/verdict.go:58-60` `if a.Site != pred.Site { continue }` excludes any alert whose site **differs OR is empty/unknown**. Predecessor excludes only when BOTH sites are known and differ; unknown/VPS/empty-site hosts are never excluded (conservative). The estate HAS such hosts (notrf01vps01/chzrh01vps01/txhou01vps01 with routes_via edges), so a genuine cascade to a VPS during a single-site action is silently swallowed as a match. Fail-safe direction inverted; the verdict.go doc's "never hide a cascade on a named host" claim is technically true but hides one on an unnamed empty-site host.

8. **[FIXED — re-checked 2026-07-30; re-verified 2026-07-31 — still holding at post-recovery HEAD: `core/reconcile/reconciler.go` last touched by 8df4a9c, the 07-30/31 recovery did not regress it; all four oracles present]** ~~R0 verdict-gate granularity lost in reconcile.~~ Fixed by same-day-as-freeze + later work this frozen snapshot never absorbed: `core/reconcile/reconciler.go` now routes EVERY non-match verdict — partial included — to To Verify, never Done (a093aa9, 2026-07-16), never auto-closes a POLL_PAUSE band even on a confirmed clear (human-owned; no silent "human_resolved" with no human), and HOLDS an **executed** AUTO session that lacks a clean MATCH verdict (`Executed && !(HasVerdict && Verdict==match)` ⇒ To Verify, 8df4a9c). That hold IS the missing pending/unevaluated state: the workflow sets `HasVerdict = exec.Verdict != ""` (temporal/runner/workflow.go, writeback-fresh-verdict), so an async verdict not yet landed — or a verdict the verifier could not produce (lookup gap/unverifiable) — leaves `HasVerdict=false` and the executed write-action fails CLOSED to To Verify instead of auto-closing before its blast-radius verdict. Oracles: `TestReconcilePartialNeverAutoCloses` / `TestReconcileExecutedAutoHoldsForVerdict` / `TestReconcileDeviationNeverCloses` / `TestReconcilePollBandNotAutoClosed` (core/reconcile/reconcile_test.go). Still open (tracked in §4-8, deliberately not re-litigated here): the richer typed outcome enum, close-out-demote paging, and age gating.

**P1 — masks incidents / weakens guarantees**

9. **[FIXED 2026-07-31 — `TriageEntry` carries Suppressed + IssueRef; dedup anchors only on an escalated prior, and with the OpenIssue tracker check a re-fire after the parent incident CLOSED escalates as a genuine new incident (d858aa0, same-day-as-freeze); every degraded mode fails OPEN (an extra session, never a false suppression)]** ~~Dedup dropped open-issue semantics.~~ `core/suppression/dedup.go:46-61` collapses ANY prior `(host,rule)` in-window regardless of prior outcome or whether the parent issue is still open (`TriageEntry` has no Outcome/IssueID). Predecessor dedups only against a prior `escalated` entry AND confirms the parent YT issue is still open (tier1_suppression.py:109-165). A genuine re-fire after the prior incident closed is silently suppressed.

10. **[FIXED 2026-07-31 (TG-219) — the suppression-domain registry is now keyed by `ScheduleKey{Host, Kind, Cron}` (`core/suppression/discover.go`), matching the durable twins and the predecessor's uq_dsr_host_expr_kind, so a SHIFTED schedule is a new observing row instead of inheriting LIVE. Re-registration of the same signature still preserves status/observations/kill_switch. Real-path oracle: after the schedule moves, the first reboot at the new time is investigated and only its OWN two occurrences promote it]** ~~Re-running discovery silently DEMOTES live scheduled-reboot rows.~~ `core/suppression/discover.go:20-26` / `persist/scheduled_reboots.go:83` unconditionally force `Status=observing`, `ObservedCount=0`, overwrite. Predecessor's `ON CONFLICT` deliberately preserves status/observed_count/kill_switch (scheduled_reboots.py:271-280). A weekly sweep un-promotes every live schedule. Registry key also drops `cron_expr` (host,kind only) so two crons of the same kind collide.

11. **[FIXED 2026-07-31 (TG-219) — Promote now has a PRODUCTION caller: `suppression.Learner.Observe` runs it on the live ingest path (temporal/runner.LiveSuppressGate.afterDecide) for every reboot-class alert the chain did not suppress, so the accumulate/dedup/10-cap/threshold-2 mechanism is exercised in production and not only by tests. Oracles drive the real gate: the first two nightly reboots are investigated, the third is suppressed]** ~~Promotion drops boot-timestamp dedup + accumulation.~~ `core/suppression/promote.go:35-44` recomputes `inWindow` from one call's slice, no dedup, no 10-cap, overwrites `ObservedCount`. Predecessor dedups by boot iso, caps at 10, accumulates across runs (scheduled_reboots.py:302-308). A single boot seen in overlapping journalctl lookbacks can promote to "live" on ONE boot, defeating observe-before-live.

12. **[FIXED 2026-07-31 (TG-219) — the classifier is now consulted at REGISTRATION time: `Learner.Observe` gates on `TwoPhaseVerifier.Confirm` BEFORE recording anything, so a reactive (or unknown-reason) boot is never recorded as evidence and can neither register a pattern nor contribute to a later promotion. Real-path oracle: four regular OOM reboots at the same minute each night produce zero live learned schedules]** ~~Reactive-vs-clean boot gate absent.~~ Predecessor registers an observing row only for CLEAN boots (reboot.target/systemd-reboot/syncing filesystems) and never for REACTIVE ones (oom/panic/watchdog/hung_task/emergency/self-heal/thermal, classify-reboot-alert.py:43-77). Without it, an OOM/self-heal reboot near a cron minute can be learned as "scheduled" and later suppress real incidents.

13. **[FIXED — MR !72; re-verified 2026-07-31 — materially deepened since the freeze: control committed on every gated prediction, and LIVE-scored by the verify-time falsifiability writeback ticker (5fe4b0c, 2026-07-18) with the 07-30 adjudication repair (a2e57e5: commit-time baselines, forecast/action split, estate-derived site scoping); `control_hosts` persisted for INV-22]** ~~Negative control is not a real shuffle, is never scored, and has no ratio gate.~~ The committed control now uses `estate.ShuffledControl` (degree-preserving: out-degree + per-rel_type target multiset preserved, real topology destroyed, seeded blast-radius walk) when an estate is wired, via `InfragraphModel.controlHosts`; an unresolvable target yields an empty control. `ScoreControl(record, observed)` scores real vs control host-level TP/FP with `ComputeVerdict`'s exclusions; `ControlScore.Ratio()` = control_tp/real_tp and `Falsifiable()` gates it at `ControlRatioCeiling` (0.5). INV-22 is now behaviorally satisfied, not just shape-satisfied — oracle proves a real prediction separates from its control and a vacuous one does not. (The old flat count-only shuffle remains ONLY as the no-estate fallback.)

14. **[FIXED — re-checked 2026-07-30; re-verified 2026-07-31 — full chain confirmed at HEAD (08a49eb + 46905d9), all six oracles present; caveat: the lane is constructed only when a durable store (`TG_DB_DSN`) exists, which is the live deployment posture]** ~~Reconcile→escalation bridge unwired (orphaned-poll re-check).~~ Wired end to end since 08a49eb (2026-07-16, the branch) + 46905d9 (2026-07-20, the worker wiring): the workflow flags an unanswered poll (`PollUnanswered` on a timeout/budget-exceeded vote, temporal/runner/workflow.go) → `reconcile.Reconcile` HAS the orphaned-poll branch (archives `poll_unanswered`, ticket stays To Verify, flags `ScheduleReCheck` with the attempt count) → `ReconcileActivity` hands off through the `Deps.ReCheckSchedule` seam (temporal/runner/reconcile.go) → `cmd/worker/main.go` wires that seam over `escalation.Controller.ScheduleReCheck` (per-incident cap `TG_ESCALATION_RECHECK_CAP`, delay `TG_ESCALATION_RECHECK_DELAY`) and the scheduled **FireDue cron** fires each due re-check against the LIVE condition — still-active re-escalates + pages, recovered defers, the cap stands down to a human, and an absent live-condition oracle fails SAFE to still-active. The IFRNLLEI01PRD-1536 class (an unanswered 90→100% disk poll silently worsening) now converges to a human; REQ-206's trigger side is live. Oracles: `TestReconcileOrphanedPollSchedulesReCheck` (core/reconcile) + `TestScheduleReCheckEnqueues` / `TestFireStillActiveReEscalates` / `TestFireRecoveredDefers` / `TestCapStandsDownToHuman` / `TestFireDuePerRowIsolation` (core/escalation/escalation_test.go).

15. **[PARTIAL 2026-07-31 — DOWNGRADED from FIXED (status honesty under the conservative unexercised-path rule, not a code regression): the port itself is code-complete (Lag lower bound + FrontierCrossCheckMonitor with predecessor-matching DRIFT/DEATH thresholds, f9a9cde), but the lane is dead — the only PairSource is the test fake, the monitors have no non-test constructor or caller, `temporal/governance/schedule.go` CreateSchedules is called by nothing and the workflow names it schedules are defined nowhere, and no cmd/ binary imports the packages. The 3-week dead-judge class is still undetectable on the live system — TG-222]** ~~Judge-liveness dropped the window lower bound (-2h lag); frontier cross-check not ported.~~ **[was FIXED — MRs !61, !76]** The `-2h` Lag lower bound was restored in !61 (excludes just-ended not-yet-judgeable sessions). **Frontier cross-check now ported** (!76): `core/governance/frontier_crosscheck.go` `FrontierCrossCheckMonitor` catches DRIFT (local judged but disagrees with an independent frontier re-judgment over the same rubric — liveness reads healthy) and confirmed DEATH (frontier scores sessions the local judge left `-1` — the exact 3-week dead-judge class no purely-local metric catches). Pure `Evaluate` decision behind an injected `PairSource`; oracle-tested; lockstep-bound to spec/004.

16. **[FIXED 2026-07-31 — TG-223 (ca3294b): the FIFTH dormant branch is wired. `ClassifyActivity` now sets HasVerdict/Verdict from the durable ACTUATION ledger (`core/db/prior_verdict_read.go` — action_execution + executed-only action_verdict via the interceptor_gate_verdict anti-join; prediction_verdict deliberately EXCLUDED per migration 0042, since 23 of 24 propose-path deviations grade a world model about an estate TG never touched). Relevance is two bounds: rule-FAMILY scoped through the one authority (`core/knowledge.CanonicalRule`, folded in Go never in SQL) and recency-bounded by `TG_PRIOR_VERDICT_WINDOW` (default 48h, the predecessor's own verdict-staleness bound; documented in deploy/.env.example + compose). FAIL TOWARD CAUTION: absent/unknown/unreadable ⇒ byte-identical to the pre-feature ladder, a read error neither fails open nor closed and is LOGGED, match/partial do not tighten (agreeing with the ladder's OutcomeUnverified mapping), nil seam/no DSN ⇒ inert. Evidence recorded on the audit row (`prior_verdict_key` = host|canonical-family, `prior_verdict`), REQ-014 style. ORACLES DRIVE THE REAL PATH: nine tests in `temporal/runner/prior_verdict_test.go` call the actual ClassifyActivity the worker registers and let it build the GatedInput; four mutation controls EXECUTED RED — (A) restoring the shipped HasVerdict-never-set state → 5 RED, (B) dropping the family fold for string equality → the sibling spelling stops tightening, (C) failing the read closed → a DB hiccup would poll the fleet, (D) dropping the evidence signal → the ruling without the reading. spec/001 REQ-015 + T-001-15 + 3 godog scenarios; lockstep re-stamped. was PARTIAL — MRs !70, !74]** Five classifier safety branches were DORMANT (NovelIncident, CriticalityTier, BlastRadiusWide, SilentCognitionGuard/AutoResolveMarked/Evidence, HasVerdict/Verdict handled in `classifier.go` but never populated by `activities.go`). **CriticalityTier** (!70): `Deps.CriticalityTier(host)` from an operator-declared P0-host set (`TG_CRITICALITY_TIER_HOSTS`) ceilings a P0 host at AUTO_NOTICE. **BlastRadiusWide** (!74): `Deps.BlastRadiusWide(host)` computes the host's estate blast-radius width against an operator threshold (`TG_BLAST_RADIUS_WIDE_THRESHOLD`, default 8) and ceilings a wide cascade at AUTO_NOTICE; empty estate ⇒ no host wide (fail-safe), goes live as topology seeds. **NovelIncident** (!81): `Deps.PriorIncidents(host, alert_rule)` with positively-established-novelty semantics — a class forces a poll only when its prior count is KNOWN and zero; an unknown count never fires (no false poll from a missing store). `AlertRule` is now threaded into ClassifyInput. All fail safe when unwired. **SilentCognitionGuard** (!82): the guard is always active (INV-11); ClassifyActivity binds the proposal's cited evidence ids against the orchestrator-captured tool results (threaded through InvestigateResult) — a citation that binds nothing (hallucinated id, failed tool, off-target result) strips the AUTO-RESOLVE and polls. **All five dormant branches are now wired** except HasVerdict/Verdict, which is post-execution and belongs to Phase 2 (mutation OFF ⇒ no verdict yet).

**P2 — floor completeness / dropped mechanisms / correctness**

17. **[FIXED 2026-07-31 — TG-224 (c25cfe4): the two remaining predecessor categories are ported into BOTH halves of the derivation. `destructiveOpRE` gains network-catastrophic (write erase / erase startup-config|nvram|flash / no ip routing / default interface / clear configure all / no ip route / no access-list / no spanning-tree / no switchport trunk / ip link delete / brctl del* / ovs-vsctl del-* / nft flush ruleset / iptables -F|-X) and code-deploy·repo-write (the predecessor's gh|glab pr|mr merge, release create and api -X DELETE branches verbatim, plus the FLAG-LEVEL git shapes its coarse `git-write` match could not express — force-push, ref delete, reset --hard, clean -fdx, update-ref -d, filter-repo — plus deploy-key revoke and pipeline/environment/runner destruction, CLI-anchored so rationale prose cannot trip them); the never-auto floor gains 24 matching slugs, the route a verb takes when it arrives as a DECLARED op_class. TWO EXCLUSIONS DECLARED AND ASSERTED as non-matches: bare `reload` (collides with `systemctl reload`; the predecessor recorded the same decision) and the prose-plausible `no interface`/`no vlan`/`no router` (the pattern is fed the proposal's RATIONALE), carried as slugs instead. Every new pattern has a named oracle case (46 verbs across `core/safety/destructive_test.go`), and the INTENT is pinned through the REAL mechanism in `core/policy/destructive_family_floor_test.go`: `Ladder.GraduatedVerdict` over the live registry plus a deliberately TWO-SIDED substitution arm showing that an auto-eligible tier DOES graduate a network-teardown class, so the explicit floor decision is visibly the only thing between a registered `write-erase` and autonomy. Four mutation controls EXECUTED RED, including one that reds in BOTH packages. NO behavior change today by construction (registry-only argv ⇒ these verbs cannot execute); one oracle corrected the code (`git clean -fdx` ended mid-flag and the wrapper's trailing \b rejected the alternation — the REQ-012 anchor defect, caught before shipping inert). spec/001 REQ-016 + T-001-16; lockstep re-stamped. was PARTIAL]** ~~Missing irreversible floor slugs~~ (safety.go:63-67 has only 12): wipefs/shred/blkdiscard/dd-to-/dev, vgremove/lvremove/pvremove, zfs-rollback/zpool-offline, drop-table/truncate-table, docker volume/system/network prune, network-catastrophic verbs, code-deploy/repo-write. Unknown-op fails closed only when `Reversible==Irreversible` — a model-declared bool — so an unlisted destructive op labeled `reversible=true` reaches AUTO.

18. **[FIXED 2026-07-31 — `core/screen` ports the predecessor's 5-category detector (the package doc names this finding as the gap it closes) and feeds it BOTH directions: outbound, every classification sets Jailbreak over rationale+approval_choice and the classifier forces POLL_PAUSE('jailbreak-detected') BEFORE every other branch, so the floor slug is now fed; inbound, poisoned precedent snippets are skipped and seed blocks neutralized-and-flagged; zero-width/NFKC/homoglyph normalization defeats disguises (ef31d2f, d096a5a, 41b6018)]** ~~jailbreak floor slug is dead~~ (safety.go:66) — no detector feeds it; the inline prompt-injection screen (classify-session-risk.py:784-808) is unported.

19. **[FIXED 2026-07-31 (TG-219) — the chain is ASSEMBLED with production callers and all three deferred items land with it: the reboot-rule allowlist is DATA (`core/suppression/rebootrules.go`, `TG_SUPPRESSION_REBOOT_RULES` replaces the compiled default set), the dark-launch arm-switch is `TG_SUPPRESSION_LEARN_ENABLED` (default OFF — the learned lane lands dark exactly as `TIER1_SCHED_REBOOT_ENABLED` did), and renew-on-match is wired into phase SR (`ScheduleRegistry.RenewOnMatch`, only for the row that actually suppressed, never touching promotion state). Declared schedules keep registering LIVE by design — an operator declaration IS the authorization; observe-before-live now governs the LEARNED lane beside it]** ~~Cron window symmetric + same-day-only + minimal parser.~~ **[was SUBSTANTIALLY FIXED — MRs !68, !83]** Cross-midnight evaluation restored (!68: fires checked on the alert's day + adjacent days). The parser now handles the FULL crontab grammar (!83): `*`, single values, ranges, steps `*/s` and `a-b/s`, comma-lists, day-of-month + month, and cron's DOM-or-DOW day semantics (Sunday 0 or 7) — a weekday-range/monthly reboot cron matches, malformed fields fail open. Remaining (deferred, needs the chain assembled): the reboot-rule allowlist as data, the dark-launch arm-switch (`TIER1_SCHED_REBOOT_ENABLED` default off), and renew-on-match.

20. **[FIXED 2026-07-31 — the last item closed by TG-220: the dynamic verification window is ported. `core/falsify/window.go` computes `max(FLOOR=900s, 2 × p95 observed cascade latency)` per EDGE (keyed on the ordered primary→dependent host pair, the same identity `estate.CoOccurrence` uses), maxed over the edges a prediction claims and clamped to `TG_FALSIFIABILITY_WINDOW_MAX`; p95 comes from TG's OWN durable ledger (`core/db.CascadeLatencyStore` over `ingest_alert`, trailing 64 samples/edge — the ported `SAMPLE_CAP`), computed by deterministic Go (`falsify.Percentile`, the ported nearest-rank `_percentile`), never a model call. Wired into the live scorer in `cmd/worker`. Two DELIBERATE improvements on the original: an upper CAP (the predecessor has none — one 6-hour sample there yields a 12-hour window and the row never scores) and score-time rather than commit-time computation (no `window_seconds` column, and the newest evidence reaches a prediction still waiting). Fail-safe in every direction: unobserved edge / unreadable read / unwired seam ⇒ the 900s floor, which is LONGER than the retired 10m constant, so nothing here can manufacture a miss. Governed by spec/002 REQ-110 + T-002-220 with three acceptance scenarios; the 15-minute-cascade oracle and the INV-22 control-ratio oracle each carry a mutation control. `TG_FALSIFIABILITY_WINDOW` is RETIRED (ignored loudly at boot). Was PARTIAL — estate MRs]** ~~Per-edge `expected_alerts` collapsed to one flat `DefaultRules`; siblings omitted.~~ The estate graph carries per-edge `ExpectedAlerts`; `Predict` adds each impact's own expected alerts (falling back to `DefaultRules` only when an edge names none) AND folds common-cause `Siblings` — so `ComputeVerdict`'s "partial" branch now has real per-(host,rule) content. **Laplace-smoothed edge confidence now built** (!90): `estate.LaplaceConfidence(hits, trials)` = capped `(hits+1)/(trials+2)` — the learned tier is now BASE-RATE-AWARE, so a dependent that follows a primary 5/5 times outranks one that follows 5/50; the co-occurrence learner records per-host trial counts (`PrimaryTrials`) to drive it, falling back to the count-only ramp when trials are unknown. STILL open: the dynamic verification window (a prediction-precision refinement, not a correctness gap).

21. **[PARTIAL 2026-07-31 — the ported half is live and HARDENED post-freeze (restartClassRE expanded to every registry lifecycle op-class token + a live-registry oracle; server-derived at the activity so the model cannot declare its way out; 5425107 closed the A2 veto-coverage gap); still unported: `_CONS_BLOCKERS` destructive-sibling co-occurrence and the 2+-reboot quorum — but their precondition (a compound multi-command plan blob) still does not exist (single-Action proposals, argv-only, no `sh -c`, per-argv destructiveOpRE backstop). Standing note: no cross-action quorum for sequential single-guest reboots. WAIVER CANDIDATE; was PARTIAL — MR !71]** Conservative-remediation carve guards. **Self-protected control-plane restart (`_SELF_PROTECTED_RESTART_RE`) now ported**: `safety.IsRestartClass` + a config-declared `Deps.SelfProtectedService` (`TG_SELF_PROTECTED_SERVICES`) veto a restart of the platform's own services to POLL_PAUSE. STILL open: destructive-sibling co-occurrence (`_CONS_BLOCKERS`) and guest-loop/2+-reboot quorum — both keyed on a MULTI-command plan blob, which TG's per-action model (one argv per decision) largely dissolves; revisit if/when a compound-action surface appears. Note TG never had the coarse-high→carve-down mechanism (it fails closed by default), so the carve BLOCKERS are the only faithful part to port.

22. **[FIXED — MR !69; re-verified 2026-07-31 — intact end-to-end at HEAD (4bf4f40): the predecessor's exact category set {maintenance, security-incident, deployment}, POLL_PAUSE clamp, threaded from the normalized provider `category` label; absent/unknown category adds no clamp, matching the predecessor's fail direction]** ~~Category/severity risk defaults dropped as a band driver~~ — `safety.HighRiskCategory` restores the predecessor's `HIGH_RISK_CATEGORIES` set; `Classify` now forces POLL_PAUSE for maintenance/security-incident/deployment (safe-direction clamp, step 2), threaded from the normalized `category` label via the runner. spec/001 + spec/012.

23. **[FIXED 2026-07-31 — the fix-on-port was done: Upsert's MAX-ratchet stores the provenance of the WINNING confidence (`cur.Source = e.Source`, estate.go:205; the doc comment cites this audit's §1.7-1), oracle-tested on the finding's exact NetBox-then-LibreNMS scenario (b4a10df); landed with the estate MR chain but never re-tagged here]** **Provenance-misattribution bug (§1.7-1)** — flag as **fix-on-port**, not faithful replication.

24. **[PARTIAL 2026-07-31 — the breaker is BUILT (three-state, pgx-persisted, Prometheus-observable, citing CONSTITUTION.md:130; 8a03968) and LIVE for two lanes the finding did not name (mutation + cost); but the finding's named lanes are still unguarded on the EXERCISED path: production model calls go through `adapters/model.Gateway` with no breaker (the guarded per-rung litellm module has no production constructor — judge cron and skill-gen call the gateway directly), and RAG's deterministic lexical fallback is bounded per-call but not a named persisted breaker — TG-221]** ~~Circuit breaker not ported~~ despite CONSTITUTION.md:130,134 promising "named, observable circuit breakers with persisted state" — model-gateway/judge/RAG calls have no bounded-failure fallback (the very outage class behind the dead-judge incidents).

25. **[FIXED — MR !73; re-verified 2026-07-31 — deepened since the freeze from registered-and-tested to LIVE-WIRED: `TG_SUPPRESSION_RULES_FILE` loaded by the worker (additionally REFUSING a catch-all host=\* rule=\* rule) and the stage appended LAST in the spec/005 chain order (2368e46)]** ~~Active-memory (Phase 3) suppression stage is dangling documentation.~~ `core/suppression/activememory.go` implements `ActiveMemoryStage` + typed `SuppressRule` (host/rule `path.Match` globs + operator reason), porting the predecessor's `openclaw_memory` `suppress:<reason>` triage-rules: critical/unknown never suppressed (defense-in-depth), a malformed glob fails open, no match fails open. Registered under T-005-5, lockstep-bound, oracle-tested.

26. **[FIXED — MR !75 (+ !92 pgx binding); re-verified 2026-07-31 — EXCEEDS the frozen text: `infragraph_cascade_stats` is now WRITTEN, not just created — fed by the verify-time falsifiability scorer (5fe4b0c) plus the PredictionVerdictStore, and the 07-30 adjudication repair (a2e57e5) landed on this same spine]** ~~`infragraph_prediction` migration + cascade_stats table do not exist.~~ Migration `0002_infragraph_prediction` creates the append-only `infragraph_prediction` spine (`(plan_hash, kind)` PK; immutable identity + `control_hosts` for INV-22; nullable verify-time `tp/fp/fn/control_tp/control_fp`; `schema_version` CHECK > 0 matching the registry) AND `infragraph_cascade_stats` (windowed `control_ratio` + `falsifiable`). Down pair + an offline pure-Go migration test (up/down pairing, the registered table + control/schema-version columns) guard it. The pgx store binding is now built (!92): `db.PredictionStore` implements `predict.PredictionStore` over the table (append-only `ON CONFLICT DO NOTHING`, sorted-jsonb sets, `control_hosts` persisted for INV-22), worker-wired when `TG_DB_DSN` is set (else the in-memory oracle twin). The set↔jsonb marshaling is unit-tested; the round-trip is a compose-only integration test (skipped without `TG_TEST_POSTGRES_DSN`).

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

