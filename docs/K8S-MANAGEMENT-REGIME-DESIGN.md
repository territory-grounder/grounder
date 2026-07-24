# Kubernetes management-regime — decision brief & design

**Status:** DRAFT — research complete, **not yet built**, awaiting owner sign-off.
**Date:** 2026-07-18.
**Owner directive:** *"we need to understand this first before we proceed building something… BUT also we
need to respect the industry's standards."* This doc answers the three questions asked (predecessor
mechanism, industry best practice, TG recommendation) so we can decide the design **before** any code.

Related: [`docs/CONSTITUTION.md`](CONSTITUTION.md), memory `management-regime-gap`,
[`docs/CONNECTOR-INVENTORY.md`](CONNECTOR-INVENTORY.md).

---

## TL;DR

The predecessor is **right and mature**: for a GitOps-managed cluster it treats
**OpenTofu → GitLab-MR → Atlantis** as the *only* write path and keeps its k8s automation **strictly
read-only + escalate-to-human**. The 2024–2026 industry consensus agrees, and adds one refinement the
predecessor blurs: split k8s operations into a **runtime lane** (pods/nodes/rollouts — safe to actuate
directly, self-healing, doesn't fight the reconciler) and a **declarative lane** (anything that is a field
in Git — must go through an MR). **TG today has the wrong default for a GitOps cluster**: its `kubernetes`
actuator would permit direct `apply/patch/scale`, and **there is no MR actuator at all**. Recommendation:
model *regime per target*, route declarative fixes through a **new GitLab-MR actuator** that reuses TG's
existing interceptor chain, and keep direct kubectl to the runtime lane only. Mutation stays OFF; this only
changes *which channel* a future mutation would take.

---

## 1. How does the predecessor handle k8s? (Q1)

**All of the above — three coordinated pieces, no single component:**

1. **CLAUDE-side instructions** — `openclaw/SOUL.md` §"Kubernetes Alert Handling" / "You have kubectl
   access" (lines 238–310).
2. **A dedicated Tier-1 skill** — `openclaw/skills/k8s-triage/k8s-triage.sh` (~1000 lines) + `SKILL.md`.
   Every cluster call goes through a `kctl()` wrapper (`site-config.sh:97`) and uses **only read verbs**
   (`get`/`describe`/`logs`/`top`) — a grep of the whole script finds **zero** `apply/delete/scale/patch/
   edit/cordon/drain/rollout/create`, **zero** `helm install/upgrade`, **zero** `tofu/atlantis apply`.
3. **A dedicated Tier-2 sub-agent** — `.claude/agents/k8s-diagnostician.md` (model haiku; mcpServers:
   kubernetes, netbox).

**What it does:** dedup/create a YouTrack issue → read-only investigation (nodes, pods, events, control-plane
deep-dive) → post findings as a YT comment → ACK the matching LibreNMS alert → **escalate to a human /
Claude-Code session** (`escalate-to-claude.sh` → n8n webhook). *It never executes a fix.*

**Is it GitOps-aware? Strongly — not blind.** The OpenTofu+Atlantis MR flow is the *only* sanctioned write
path, enforced in three places:
- `memory/feedback_k8s_strict_gitops.md` — *"ALL Kubernetes changes must go through OpenTofu + Atlantis MR
  flow — no kubectl apply, no helm install, no exceptions"* + a 9-step procedure (branch → edit `.tf` →
  `tofu fmt` → MR → Atlantis plan → `atlantis apply` → verify → merge → sync). Rationale: *"kubectl apply,
  helm install, or direct API changes cause drift that OpenTofu will overwrite… The user has been burned by
  drift before."*
- `.claude/agents/k8s-diagnostician.md:43` — *"Strict GitOps: ALL K8s changes via OpenTofu + Atlantis MR.
  Never kubectl apply."*
- `openclaw/SOUL.md:310` — *"NEVER use kubectl for write operations… K8s changes go through OpenTofu +
  Atlantis only."*

It even has **deep drift knowledge**: `atlantis apply -p k8s` reconciles the *whole* project (not just the
MR's file); stale Renovate branches can *revert* merged bumps; "Apply complete" ≠ the live cluster changed
(must kubectl-verify). Engines: **Atlantis (OpenTofu — infra/helm) + Argo CD (apps)**; Flux is not used.
Argo drift is itself alerted on (`ArgocdAppOutOfSync` / `ArgocdAppDegraded`) and fed back into triage.

**Adequacy verdict.** The *policy* is correct and matches best practice. The **gap is enforcement**: it is
prompt/discipline-level, not a hard technical block — `openclaw/exec-approvals.json:41` allowlists a broad
`kubectl *` (write verbs included), and `k8s-diagnostician.md:113` even *claims* "the tool allowlist
excludes these commands" — but it does not. Adequate as a diagnose-and-escalate loop; it never closes the
remediation loop autonomously and relies on a human to run the MR flow. **TG's opportunity: make the correct
behaviour structural, not disciplinary.**

## 2. Industry best practice (Q2)

The 2024–2026 market **segments remediation by change type** — there is no single uniform rule, and *that
segmentation is the load-bearing insight*:

- **Declarative changes (a field under version control) → PR/MR-based, never direct.** The agent emits a
  commit/PR to the Git source of truth; CI + policy admission (OPA/Gatekeeper/Kyverno) gate it; the
  reconciler (Argo/Flux) applies it. Standard for replicas, image/tag, resources, env/ConfigMaps, Helm
  values, node-pool size. Direct `kubectl edit/apply/scale` on a managed field is an **anti-pattern**:
  with self-heal on, Argo reverts it within ~5 s (the fix is *lost*); with self-heal off, you get
  **split-brain** drift. *"If it's not in Git, it doesn't exist."*
- **Runtime / ephemeral ops → safe to actuate directly, even under GitOps.** Restart/delete a pod (the
  controller re-derives it — Kubernetes self-healing, not a GitOps violation), `rollout restart`, and
  **cordon/drain/uncordon a node** (schedulability is not a Git-reconciled field). This is the accepted
  automated node-remediation path.
- **Default entry posture → read-only diagnosis + human approval,** climbing a maturity ladder:
  read-only insights → advised actions → approval-based remediation → guardrailed autonomy. Production
  changes/rollbacks require approval. (Cleric = strictly read-only + approval; Datadog Bits AI = fixes as
  PRs; Shoreline/Komodor/PagerDuty = auto-execute *runtime* ops with guardrails; DZone agentic operator =
  PR-based declarative behind an OPA gate.)
- **Break-glass on a managed resource:** `suspend` the reconciler → act → backfill Git → resume. Never race
  the self-heal loop. Respect `ignoreDifferences` fields (HPA-owned replicas, mutating webhooks) — don't
  author changes to controller-owned fields.

Consensus one-liners: *"GitOps is complementary, not competing — agents write proposed changes back to Git
for review"* (Cast AI); *"Git remains the control plane — AI does not bypass Git"* (Unite.AI); the four
**OpenGitOps** principles (declarative · versioned/immutable · pulled automatically · continuously
reconciled) are exactly why a direct mutation is illegitimate — a change not expressed declaratively and
versioned in the source is, by definition, not part of the managed desired state.

## 3. What TG already has (verified against the code)

**Reuse — the hard parts exist and are green:**
- **Single pre-execution chokepoint** — `core/actuate/interceptor.go` (`Interceptor.Do`): mutation-gate →
  never-auto floor → committed-prediction gate → evidence gate → **territory gate** → verifiability →
  execute → verify → ledger. (INV-21.)
- **Never-auto floor already covers the destructive k8s/IaC verbs** — `core/safety/safety.go:70` lists
  `tofu-destroy`, `terraform-destroy`, `kubectl-delete`, `kubectl-drain`; the self-report regex (`:178–181`)
  catches `kubectl delete pvc/pv/ns/secret`, `helm uninstall/delete/rollback`, and `apply --prune`.
- **Territory gate already knows k8s = GitOps** — `core/territory/territory.go:38`:
  *"OpenTofu/Atlantis only — no kubectl apply / helm install on managed resources"*; the verb regex
  (`:64`) already maps `tofu|terraform|argocd|cilium` into the k8s territory.
- **`ExecRecorder` inverse/rollback hook** (`core/actuate/interceptor.go:99`) and a **`MutationBreaker`**.
- Mutation ships **OFF** (`safety.MutationGate` default false).

**The gaps:**
- **No MR actuator.** `modules/actuation/` = `kubernetes` · `ssh` · `proxmox` · `mcp` — *all direct-effect*.
  `go.mod` has **no GitLab client**. `gitlab-mcp`/`opentofu`/`tfmcp` are listed as *planned* in
  `docs/CONNECTOR-INVENTORY.md:83` only. **The GitOps remediation channel does not exist yet.**
- **The k8s actuator has the wrong default for a GitOps cluster.** `modules/actuation/kubernetes/
  kubernetes.go` builds argv that *permits* `apply/patch/rollout/scale` as **direct** verbs (`:61–63`);
  only `apply --prune`/`delete`/`drain`/`helm uninstall|rollback` are floor-clamped. For the owner's
  GitOps cluster, a direct `apply/patch/scale` is exactly the auto-reverted / split-brain write the
  research condemns.
- **The k8s actuator is also still subprocess-shaped** (`kubectl`/`helm` argv) and hard-wired
  `ReadOnly() == true` — it hasn't even been migrated to the `WithMutation(gate, …)` pattern that
  proxmox/ssh now use. So it needs a native-client rebuild *and* the regime split (bigger than the audit's
  "distroless-broken" finding alone).

## 4. Recommendation

### A. Model management regime as a per-*target* property (resolved at plan time, from the estate/territory model — never from the alert)

- `gitops-managed` — declarative source in the OpenTofu/GitLab repo, applied by Atlantis; apps by Argo CD.
  **The owner's cluster.**
- `runtime` — pods, node schedulability, rollouts (controller-owned / ephemeral).
- `unmanaged` — no Git source (rare here; treat as direct-with-approval, or escalate).

This is **config-not-code**, exactly like the existing criticality-tier / canary-pin config layers — a
regime registry declaring, per cluster/namespace/resource-class, the regime + change-channel + which
op-classes are direct / propose-MR / escalate.

### B. Route each op by (regime × op-class)

| Operation | Regime | Channel |
|---|---|---|
| pod delete/restart, `rollout restart`, cordon/uncordon a node | runtime | **Direct** via the (rebuilt, native) `kubernetes` actuator — self-healing; doesn't fight Atlantis/Argo |
| replicas, image/tag, resources, env/ConfigMap, Helm/HelmRelease values, node-pool size, any `.tf`/manifest field | gitops-managed | **MR** — commit to the OpenTofu/GitLab repo, open MR, let Atlantis plan→apply / Argo reconcile |
| `kubectl delete pvc/pv/ns/secret`, `apply --prune`, `helm uninstall/rollback`, `tofu/terraform destroy`, node **drain** | any | **Escalate** — already on the never-auto floor; human-only |

*`drain` stays on the never-auto floor for control-plane/stateful nodes (the predecessor flags SeaweedFS
single-replica data-loss risk); only consider lifting for stateless worker nodes, under approval.*

### C. Build the missing MR actuator (`modules/actuation/gitopsmr`)

A new `adapters/actuation.Actuator` that:
1. renders the `.tf`/manifest edit onto a branch and opens a **GitLab MR** via a **native go-gitlab client**
   (distroless-safe — no `sh`, per the no-exec constraint), returning the MR URL;
2. plugs into the **same** `Interceptor.Do` chain — the MR body carries the `ActionManifest`/`action_id`,
   evidence, and committed prediction; the territory gate (k8s grounding ack) and evidence gate apply
   unchanged;
3. implements `ExecRecorder` so the compensating inverse is the **`git revert` of the MR** (matches
   *"the cleanest GitOps rollback is a git revert"*);
4. treats **merge/apply as human-gated by default** — GitLab branch protection + required review *is* the
   approval vote. So even with mutation ON, TG's write is "open MR", never "apply". Atlantis `apply` and
   Argo sync remain the reconciler's authority. TG proposes into Git; the reconciler owns the apiserver.

### D. Demote the k8s actuator's declarative verbs

Route `apply/patch/scale` to the MR actuator for a `gitops-managed` target; keep only runtime verbs
(`rollout restart`, pod delete, cordon/uncordon) as direct. One policy change + regime resolution — and the
chokepoint already re-derives destructiveness server-side, so a mislabeled plan can't slip a declarative
write through as "runtime".

**Why this satisfies both constraints the owner set.** It keeps every TG-caused change inside the four
OpenGitOps principles (declarative, versioned, pulled, reconciled) — *respecting the industry standard* —
and it makes structural exactly the discipline the predecessor already enforces by prompt — *reaching and
exceeding the predecessor* (its enforcement is advisory; TG's would be a hard chokepoint).

## 5. Open decisions for the owner (before any build)

1. **Regime source of truth.** Should the regime registry live in TG config (console-native, like
   criticality-tier), or be *derived* by pointing TG at the OpenTofu repo / Atlantis project map? (Recommend:
   explicit TG config first; auto-derive later.)
2. **MR target repo & auth.** Which GitLab repo/project holds the k8s `.tf` (the Atlantis project 7 "k8s"),
   and what identity should TG's MRs use (a dedicated bot token, sealed via the secret-ref store)? MRs must
   be authored so review/audit is meaningful.
3. **Runtime lane scope for v1.** Start with the *safest* runtime op only (e.g. `rollout restart` /
   pod delete), or include node cordon/uncordon from day one? (Recommend: rollout/pod first; nodes later.)
4. **Apps plane (Argo CD).** Do we also want a path for Argo-managed *apps* (edit the app-of-apps repo), or
   scope v1 to the OpenTofu/Atlantis infra plane only? (Recommend: infra plane first.)
5. **Sequencing vs. the canary.** The Proxmox LXC-start canary (a pure *runtime* op, no GitOps involved)
   remains the right *first* mutation. The k8s regime layer is a larger, separate build — confirm it comes
   *after* the canary is proven.

## 6. What I am NOT doing until sign-off

Per the directive, **no code for the regime layer, the MR actuator, or the k8s-actuator rebuild** until this
design is reviewed. This doc + the research are the "understand first" deliverable. Independent, unblocked
work continues in parallel (connector-fidelity fixes, the canary path), none of which touches k8s mutation.

## Sources

- OpenGitOps principles v1.0.0 — <https://github.com/open-gitops/documents/blob/main/PRINCIPLES.md>
- Argo CD auto-sync / self-heal (5 s revert) — <https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/>
- GitOps anti-patterns / kubectl-vs-GitOps / drift — <https://oneuptime.com/blog/post/2026-02-26-gitops-anti-patterns/view>,
  <https://oneuptime.com/blog/post/2026-02-26-how-to-handle-kubectl-edit-vs-gitops-conflicts/view>
- Agentic PR-based operator (OPA/Gatekeeper gate) — <https://dzone.com/articles/gitops-agentic-operator-kubernetes-auto-remediation>
- Platform patterns ("if it's not in Git it doesn't exist") — <https://platformengineering.org/blog/gitops-architecture-patterns-and-anti-patterns>
- Maturity ladder / approval norm — <https://rootly.com/ai-sre-guide>, <https://cast.ai/automation-academy/agentic-operations/>, <https://cleric.ai/resources/reports/the-state-of-ai-sre>
- Agentic SRE, Git-as-control-plane — <https://www.unite.ai/agentic-sre-how-self-healing-infrastructure-is-redefining-enterprise-aiops-in-2026/>
- Safe node drain (runtime lane) — <https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/>

**Predecessor evidence:** `openclaw/skills/k8s-triage/{k8s-triage.sh,SKILL.md}`, `openclaw/SOUL.md:238–310`,
`.claude/agents/k8s-diagnostician.md`, `memory/feedback_k8s_strict_gitops.md`,
`openclaw/exec-approvals.json:41` (in `/home/tg/gitlab/n8n/claude-gateway`).
**TG evidence:** `core/actuate/interceptor.go`, `core/safety/safety.go:70,178–181`,
`core/territory/territory.go:38,64`, `modules/actuation/kubernetes/kubernetes.go:61–63`,
`docs/CONNECTOR-INVENTORY.md:83`.
