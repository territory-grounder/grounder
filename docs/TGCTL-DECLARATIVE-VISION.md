# tgctl & the Declarative Control Plane — configure TG the way TG configures the world

> **Status: GOVERNED NORTH-STAR (vision, not a task-bearing spec).** A declaratively-reconciled,
> API-first control plane for TG *itself* is a destination, not a near build. It depends on
> prerequisites that are not yet complete (see § 10). This document is the honest capture of that
> destination and its locked design stance so the shape cannot drift. It **graduates to
> `spec/022-declarative-control-plane` once its prerequisites are met** (`spec/021` is reserved for
> federation); until then it bears no `tasks.json`, no acceptance oracles, and no build obligation.

*Provenance tags follow the house three-layer model (see `00-README.md`): **[F]** foundation
inherited from the predecessor, **[R]** the single-organization product reframe, **[O]** the audit
hardening / invariants. This is a new **[R+O]** thesis layered strictly on top of the inviolable core
in `CONSTITUTION.md`; nothing here relaxes that core. It unifies two owner-approved ideas — a
kubectl-style `tgctl` CLI and a k8s-shaped controller/worker topology — into one thesis.*

---

## 1. Thesis — "configure TG the way TG configures the world"

TG's job is to reconcile an *estate* toward a desired, governed state: it senses, predicts, acts, is
surprised, and updates — an outer control loop kept honest by an error channel the model does not
author. The thesis of this document is to turn that same discipline **inward**: TG should be
**configured the way it configures the world** — declaratively, reconciled, API-first — rather than
poked imperatively.

Two owner-approved ideas unify into one:

- **(a) a kubectl-style `tgctl` declarative CLI** — you describe the *desired* state of TG's own
  governance (`tgctl apply -f policy.yaml`), and a reconciler converges TG toward it, instead of
  clicking through a console or mutating a database by hand.
- **(b) a k8s-shaped controller/worker topology** — TG runs as a control plane (schedulers,
  reconcilers, durable state) fronting a horizontally-scalable worker fleet, the way Kubernetes
  splits api-server/etcd/scheduler from kubelets.

The predecessor is **imperative**: an operator (or an n8n flow) triggers actions and hand-edits
config; the web console is likewise a surface of buttons that mutate state. TG should be
**declarative + reconciled + API-first**: desired state is data, reconcilers own convergence, and
every human/tool/agent surface is a client of one authenticated API. This is the same inversion
Kubernetes made over hand-run `docker` commands — applied to a governed-autonomy control plane.

---

## 2. Why it fits TG unusually well

This is not a pattern grafted onto a hostile substrate. TG is *already* built the way a declarative
control plane needs its subject to be built, on **both** axes:

- **TG's own configuration is already config-as-data.** Policy is `policy_ruleset` rows (rules-as-data,
  boot-loaded, deny-overrides); the operating mode is `policy_mode`; the skill store is loadable prose,
  not Go literals; credential bindings are per-target resolution records; the actuation **regime is
  config-not-code** (spec/017 REQ-1700); and the knowledge file is loadable. The loadable-not-hardcoded
  disposition ("model READS = loadable; code that TOUCHES the estate = compiled") is *already* the
  project's north star for prose. A declarative control plane is the natural home for exactly this
  class of data — it is what `spec`/`status` resources *are*.
- **TG already manages its ESTATE declaratively.** The GitOps-MR actuation regime doesn't SSH a
  change onto a box; it opens a reviewed pull request against a desired-state manifest and lets a
  reconciler converge the estate. TG already reasons in desired-state, drift, and reconciliation terms
  for the world it governs.

So this thesis is **dogfooding**: TG should configure itself with the same declarative, reviewed,
reconciled machinery it uses to configure the estate. A governed-autonomy plane whose *own* config is
declarative + PR-reviewed + reconciled + audited is the gold standard of trust: the plane that tells
you "no" is itself governed by a plane that tells *it* "no." Trust in TG's actions is only as strong
as trust in TG's configuration, and hand-mutated config undermines the former.

---

## 3. API-first architecture — the CLI is the easy 20%

It is tempting to read this vision as "ship a CLI." That is the easy 20% and the least valuable part.
A CLI is a thin ergonomic skin; the prize is the **resource model + reconcilers** underneath it — the
typed desired/observed state of the whole control plane and the controllers that converge it. Get the
resource model right and the CLI is a few days of surface; get it wrong and no CLI redeems it.

The discipline is **API-first**: there is **one** authenticated `/v1` API, and *every* surface is a
client of it. The console and `tgctl` are **both** clients of that single API — not two code paths
into the same database. TG is already most of the way here: the deployed console already consumes
`/v1` + SSE for its live surfaces (the v2 console is the live-wired one). `tgctl` is a *second client*
of the same typed API the console already uses, not a new privileged path. This is the `kubectl` ==
GUI == CI discipline (one typed api-server, many thin clients), which the prior art (§ 12) confirms is
mature and worth reusing wholesale.

---

## 4. The resource model — TG's "CRDs"

The heart of the work is a typed resource model with the k8s **spec/status** split: `spec` is
operator-declared *desired* state; `status` is controller-observed/-owned *actual* state. Roughly:

| Resource | Kind | `spec` (desired, operator-writable) | `status` (observed, controller-owned) |
|---|---|---|---|
| `AccessList` / `Policy` | config | the ordered, versioned/immutable ACL + policy rules (rules-as-data) | active version, load result, last-applied hash |
| `Mode` | config | desired operating mode (Shadow / Semi / Full) | effective mode, last preflight verdict, force-Shadow trips |
| `Skill` | config | a loadable skill/runbook/rubric artifact + its version | graduation standing, A/B eval scorecard, live/candidate |
| `CredentialBinding` | config | per-target credential resolution (native / AWX / LDAP / OpenBao) | resolve health, last-used, binding validity |
| `Regime` | config | actuation regime per target (native-SSH / AWX-job / GitOps-MR) | regime health, drift signal, last reconcile |
| `Connector` | config | an ingest/tracker/CMDB module binding + its settings | connector health, last-poll, schema-fidelity status |
| `Incident` | read-only | — | lifecycle state, band, requeue/auto-resolve, verify window |
| `Decision` / `Trace` | read-only | — | the full decision record (predict → gate → verdict → verify) |
| `LedgerEntry` | read-only | — | append-only governance-ledger record (immutable, tamper-evident) |

The **read-only resources make the tracer a first-class CLI citizen.** `tgctl describe decision <id>`
and `tgctl trace <session>` **are** the decision-tracer (spec/020) rendered on the command line: the
same durable decision record, surfaced as a resource you can `get`, `describe`, and `trace` exactly
like any k8s object. The tracer's archive is the read-side of this resource model; `tgctl` is one of
its viewers.

---

## 5. Two safety boundaries (the load-bearing part)

A declarative control plane over a *governed-autonomy* plane is dangerous in exactly two ways, and
both are closed by design, not by convention.

### 5.1 The CLI is a CLIENT, never a side-door

`tgctl apply mode: Full` does **not** flip a database column. It submits the desired state to the
**same** authenticated `/v1` API, through the **same** gated RBAC, through the **same** preflight
`ModeController` that the console goes through. There is **no DB side-door and no chokepoint bypass**
(INV-01): the CLI is architecturally incapable of reaching the estate or the policy/mode chokepoint by
any path the console couldn't. Every actuation path is **authorized, audited, and graduated
identically** whether it originates from the console, `tgctl`, CI, or the agent itself — one
chokepoint, many non-privileged clients. The CLI's convenience must never become an authority the
console never had. This is a *security property*, and § 12 flags it as one of TG's few genuine
differentiators precisely because the read-only analogues never had to solve it.

### 5.2 DECLARED vs EARNED — you cannot `apply` your way to `auto`

This is the sharpest line in the document. Two kinds of state must never be conflated:

- **Declared state is `spec`.** ACL rules, the desired operating mode, connector settings, a skill's
  desired version — these are configuration an authorized operator *declares* and the reconciler
  converges toward.
- **Earned trust is `status`, and `status` ONLY.** Op-class graduation, verified-outcome counts, a
  skill's A/B standing — these are **conferred by the controller from demonstrated behavior**, never
  written by an operator. **You cannot `apply` your way to `auto`.** An operator can declare *desired
  mode = Full*, but an *op-class* only reaches auto by earning it through verified (`match`) outcomes
  on the graduation ladder — and no manifest, no `apply`, no CLI flag can forge that. Graduation is
  earned, not declared.

This is **idiomatic**, which is exactly why it is credible rather than exotic: in Kubernetes,
`status` is controller-owned and users do not write it. TG inherits that discipline and gives it teeth
— the reconciler's *authority* is self-conferred through demonstrated behavior, and the CLI is
structurally unable to counterfeit it. Per § 12 this **declared-vs-earned** split has **no observed
prior art** and is TG's single most defensible novelty.

---

## 6. Declarative + imperative hybrid — like kubectl, not despite it

Declarative is not a religion. `kubectl` is declarative for config (`apply -f`) **and** keeps a set of
imperative verbs for one-shot actions (`exec`, `rollout`, `drain`, `cordon`) that do not sensibly
reduce to desired-state objects. TG's plane is the same shape:

- **Configuration is declarative — `apply`.** ACLs, mode, skills, regimes, connectors, credential
  bindings: desired state, reconciled.
- **Actions are imperative verbs.** *Approve a pending decision*, *replay/re-judge an incident*,
  *inject a fault into a guinea-pig LXC*, *re-run an eval* — these are events, not desired states.
  They are the `kubectl exec` / `rollout` / `drain` analogues and should be **imperative verbs**, not
  contorted into YAML. Approving *this specific pending decision now* is not a fact about desired
  world-state; forcing it into a declarative object would be a category error.

The design rule: **do not force verbs into YAML.** Config `apply`s; actions are verbs. A mature
control plane draws exactly this line, and TG should too.

---

## 7. Reconciliation, drift, and GitOps-for-TG-itself

The reconcilers are the substance. A `spec` change is a *desired* state; a controller watches, diffs
desired against observed, and converges — continuously, not once at apply time. That immediately
raises the **drift** question, and drift needs a declared **source of truth**: when the Git manifest,
the live DB, and the console disagree about the desired ACL, *which one wins*? That must be an explicit
decision, not an accident of whoever wrote last.

TG already reasons about exactly this problem — **for the estate.** The regime-per-target design
exists because directly poking a box that is under GitOps management creates drift against its
manifest; the recommendation there is regime-per-target with a GitOps-MR actuator so the manifest
stays the source of truth. **This is the same drift problem, now turned on TG itself.** "GitOps for
TG's own config" means TG's `spec` resources live in a reviewed Git repo, changes land as pull
requests, and a reconciler converges the running plane toward the merged manifest — the estate
discipline TG already advocates, applied reflexively to the governing plane. The payoff of § 2's
dogfooding is realized here: TG's own config becomes reviewed, versioned, and drift-corrected the way
TG insists the estate's config should be.

---

## 8. The k8s-shaped scaling topology — ~70% already there

The second idea (k8s-shaped controller/worker topology) sounds like a large build. It is not, because
**Temporal + Postgres already give TG the Kubernetes split for free.** TG is roughly 70% of the way to
a k8s-shaped topology today without adding a bespoke coordination layer:

| Kubernetes | Territory Grounder (today) |
|---|---|
| api-server + scheduler (control plane) | Temporal server (frontend gateway + matching + scheduling) |
| etcd (durable, consistent cluster state) | Postgres — Temporal persistence **and** TG's own consistent state |
| kubelet / node worker fleet (horizontally scalable) | the `cmd/worker` fleet — stateless, horizontally scalable |
| scheduler → node binding / work distribution | Temporal **task queues** (scheduling + distribution) |
| controllers / operators (reconcile loops) | TG reconcilers (to build) over the § 4 resource model |
| leader election for single-active controllers | ArgoCD-style controller **sharding** (design-around, below) |

The load-bearing consequence: **do not rebuild a bespoke etcd/Raft.** HA-Postgres + HA-Temporal give
the control plane its consistent-state HA already; re-implementing consensus would be effort spent
re-earning a property TG already has.

**The one valuable near-ish increment is the WORKER-ROLE SPLIT.** Today the worker fleet is
homogeneous. Splitting it by role, on **separate Temporal task queues**, is both a scale lever and a
governance win:

- **sensor-workers** — ingest / investigate, **read-only**, **many**, run *near the estate*; and
- **actuator-workers** — execute effects, **few**, **governed**, **pinned**.

This separates **DECIDE from ACT at the topology level**: the read-only sensing fleet can scale out
broadly and cheaply while the mutating fleet stays small, pinned, and tightly governed — a
**blast-radius** control as much as a throughput one. And TG already owns the multi-worker safety
primitive this needs: the **cross-process mutation-breaker store** (#3, migration 0021) means a single
trip **force-Shadows every sibling worker**, so a fleet of actuator-workers already fails safe as a
group rather than per-process. The safety substrate for a multi-worker actuation fleet is in place;
the split is the increment that exploits it.

**Design-arounds to inherit from the prior art (§ 12):**

- **Temporal's shard count is fixed at cluster creation.** Don't let TG's own partitioning inherit
  that rigidity — make TG's partitioning **reconfigurable** rather than baked at bootstrap.
- **controller-runtime operators are single-active (leader election) and don't scale horizontally.**
  Get horizontal scale the way ArgoCD does — **controller sharding + separately-scaled stateless
  workers + a shared cache** — not by running one hot controller.

---

## 9. Honest priority caveat — scale is NOT the binding constraint

This document would be dishonest if it implied TG needs scale now. It does not. TG has run **~36
triages ever**, **0 actuations**, and the corpus is **empty until TG-125**. Throughput and
multi-controller HA are not the binding constraint by any measure of current load.

The binding constraint is **proven loops + graduated trust** — driving the canary and the skill-store
flywheel through their first full cycles so that *any* op-class earns auto and *any* artifact
graduates. Until that is proven, horizontal scale is solving a problem TG does not have.

So the honest priority is:

- **Multi-controller HA scaling = a north-star to CAPTURE, not a now-build.** Write the shape down (so
  it can't drift), then build it only when load demands it.
- **The worker-role split = the modest early increment** worth doing sooner — not because TG is
  overloaded, but because sensor/actuator separation is a *governance and blast-radius* win that
  happens to also be a scale lever, and because the mutation-breaker primitive already exists to make
  it safe.

Capture the apex; build the increment.

---

## 10. Dependency chain + phased roadmap

This is deliberately *not* on the near roadmap; it sits on prerequisites that are themselves
incomplete. The chain, in order:

1. **The versioned/immutable ruleset (spec/020 REQ-2018) — the near-term brick.** An ordered,
   versioned, immutable ACL is *literally the first `AccessList` resource*: a `spec` you can version,
   diff, and reconcile. This is the first real brick of the resource model and is already on the near
   roadmap.
2. **Resource-ify `/v1` (spec/status).** Give the existing config surfaces the typed spec/status shape
   of § 4, exposed through the one authenticated API.
3. **Native reconcilers.** Controllers that watch desired state and converge — continuously, not
   apply-time — with an explicit source-of-truth/drift decision (§ 7).
4. **The `tgctl` client.** The kubectl-style CLI as a *second thin client* of the same `/v1` API the
   console uses (§ 3) — the easy 20%, built last.
5. **The worker-role split (§ 8).** Sensor-workers vs actuator-workers on separate task queues.
6. **GitOps-for-TG (§ 7).** TG's own `spec` resources in a reviewed Git repo, reconciled by PR.
7. **HA control plane.** Only when load demands it (§ 9): HA-Postgres + HA-Temporal + ArgoCD-style
   controller sharding — no bespoke consensus.

**How it composes with the rest of the lattice:**

- **Decision-tracer (spec/020).** `tgctl trace <session>` / `tgctl describe decision <id>` *are* the
  tracer on the CLI; the tracer's durable archive is the read-side of the § 4 resource model (they are
  the same work seen from two ends).
- **Federation (spec/021).** A declarative plane lets a **sovereign instance bootstrap from
  manifests**, and lets imported, re-validated distillate land as **`Skill` resources** that re-enter
  the local graduation ladder — federation's "apply the imported wisdom" step *is* an `apply` of
  `Skill` specs under § 5.2's earned-trust rule.
- **The mode-config questionnaire.** Operating mode becomes a **gated resource** (`Mode`), so the
  owner-target of default-populated ACLs and a configurable, questionnaire-driven mode selection is
  expressed as declaring a `spec` — never as forging `status`.

Only with (1)–(3) in place does `tgctl` become buildable rather than aspirational.

---

## 11. Relationship to the mission

The mission is governed autonomy you can trust with **production**. Trust in what TG *does* is bounded
by trust in how TG is *configured*: a plane that hand-edits its own policy in a database is not one you
hand production. **Declarative + reviewed + reconciled own-config** is what makes the governing plane
itself auditable — the config that decides "no" is versioned, PR-reviewed, drift-corrected, and unable
to be forged past its own chokepoint. This is not a feature adjacent to the mission; it is the mission
turned reflexive. TG earns the right to govern an estate by being, itself, the most governed thing in
the building.

---

## 12. Prior art & differentiation

*This section summarizes the standalone prior-art report faithfully; it invents no products and makes
no claim the report did not support. Every product named traces to a cited source in that report.*

**The plumbing is MATURE and CROWDED — reuse it, do not reinvent it.** The full mechanism stack —
a kubectl-style CLI driving CRDs, a declarative spec/status resource model, reconcilers that self-heal
drift, and a CLI that is a thin zero-authority client of one typed API — is fully mainstream. It is
demonstrated across **kagent** (agent-as-CRD + Go controller; the tgctl *shape* is now table-stakes),
**K8sGPT** and **HolmesGPT** (agentic-SRE-as-CRD, but **read-only diagnosis** — no actuation),
**Sympozium** ("policy is CRDs, agents are Pods," governance-first — the nearest architectural
neighbor), **ArgoCD** (the CLI==GUI==one-API discipline *and* the controller-sharding scale answer),
**Crossplane** (proves spec/status + reconciler generalizes off-cluster to any foreign API),
**Temporal** (the frontend/worker/state split TG already inherits), **Keep** (`keep workflow apply -f`
against the same REST API as the GUI — the cleanest CLI-as-pure-client precedent, but no reconcile and
no autonomy config), and **Shoreline** (a genuine native `op` CLI, but its declarative/reconciled path
is Terraform and it scopes to remediation). Building `tgctl` as *differentiation-by-plumbing* would be
a mistake: **adopt the conventions, don't reinvent the control grammar.**

**TG's genuine, honest novelty is four things — and none of them is the CLI:**

1. **The declarative-control-plane pattern applied to a GOVERNED-AUTONOMY plane.** No shipped product
   unifies (a) a native kubectl-style CLI, (b) declarative config of the *governed-autonomy* plane
   itself (policy + mode + autonomy), (c) **native** continuous reconciliation of that config (not
   delegated to Terraform/Helm), and (d) first-class client of the exact authenticated `/v1` API the
   console uses. Kubiya has (b) via the wrong vehicle (HCL, no native reconcile); Keep has (a)+(d)
   with no (b); Shoreline has the native-CLI form but Terraform for the reconciled path. **The
   unification is the whitespace.**
2. **DECLARED-vs-EARNED (spec-config vs status-earned trust) — the sharpest, most defensible novelty.**
   Every analogue treats governance as *declared* config you `apply`. TG's model — configuration is
   `spec`, but earned trust (op-class graduation) is **`status`-only, so you cannot `apply` your way to
   auto** — has **no observed prior art** (Kubiya, Shoreline, and Sympozium all treat governance as
   declared config). It is a real conceptual contribution, and it maps cleanly onto the mature,
   idiomatic k8s discipline that `status` is controller-owned — which is exactly why it is credible
   rather than exotic.
3. **CLI-as-non-bypassing-client of a mode/policy CHOKEPOINT** (§ 5.1). The general "CLI == API
   client" pattern is mature, but TG's specific claim — the CLI is *architecturally incapable* of
   bypassing the authenticated policy/mode chokepoint, so every actuation path is
   authorized/audited/graduated identically — is a **security-property** differentiation in the
   agentic-*mutation* context, which the read-only analogues (K8sGPT / Holmes / Cleric) never had to
   solve.
4. **Governed MUTATION with a graduation ladder + multi-regime actuation over a sharded
   sensor/actuator fleet.** This exact combination is **unoccupied as one integrated product**: every
   k8s-native SRE agent that ships is read-only, and every mutating governed-agent system (Kubiya) is
   not k8s-native-CRD + native-reconcile.

**Nearest neighbors to watch:** **Kubiya** (governance-as-code, wrong vehicle, no earned-trust),
**Sympozium** (policy-as-CRD, governance-first, no trust ladder — watch closely), and **kagent**
(agent-as-CRD + controller, no governed-mutation core).

**IP posture.** The operator/controller pattern, `kubectl`, CRDs, Crossplane's off-cluster reconciler,
ArgoCD sharding, Temporal's frontend/worker split, and the spec/status idiom are all open-source prior
art — **reuse and cite, never claim** them. The only plausibly-novel subject matter is
**declared-vs-earned** and the **non-bypassing-client-of-a-graduation-chokepoint** security
architecture; even these are close to obvious combinations of known art and should be treated as
**defensive-publication / positioning material**, not a strong patent.

**Bottom line: a mature pattern in an unoccupied domain — reuse the plumbing, claim only the
governance.** Spend the invention budget on the governed-mutation control plane and the
declared-vs-earned trust ladder; spend *zero* on the CLI/operator/reconciler machinery.

---

> **This document graduates to `spec/022-declarative-control-plane` once the § 10 prerequisites are
> met.** At that point the § 4 resource model and § 5 safety boundaries become EARS requirements, the
> § 10 chain becomes the `tasks.json` DAG, § 5.1 (non-bypassing client) and § 8's blast-radius split
> become STRIDE entries and acceptance oracles, and § 5.2 (declared-vs-earned) becomes an invariant on
> the resource layer. `spec/021` remains reserved for federation.

---

## Appendix — provenance quick map

- **[F] foundation:** the predict→act→verify outer loop the thesis turns inward; the deterministic
  orchestrator owning the effect channel with an untrusted model (the chokepoint § 5.1 protects,
  INV-08); the op-class graduation ladder and verified-outcome semantics that § 5.2 makes `status`; the
  Temporal + Postgres control/worker/state spine (§ 8); the mutation-breaker store (#3, migration 0021).
- **[R] reframe:** TG-configures-itself-declaratively as a single-organization product posture; the
  console + `tgctl` as co-equal clients of one `/v1` API; the mode-config questionnaire as a gated
  resource; config-as-data across policy/mode/skill/credential/regime/connector.
- **[O] overlay:** the CLI-as-non-bypassing-client security property (INV-01, no side-door, no
  chokepoint bypass); declared-vs-earned as a resource-layer invariant (status is controller-owned,
  never operator-writable); the versioned/immutable ruleset (spec/020 REQ-2018) as the first
  `AccessList` resource; GitOps-for-TG's-own-config drift/source-of-truth discipline; the DECIDE-from-ACT
  topological separation of sensor- and actuator-workers as a blast-radius control.
