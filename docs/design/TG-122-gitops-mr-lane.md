> **NOT A WORK QUEUE — reference / design-review only.** The one authoritative queue is
> [`../BOARD.md`](../BOARD.md); the complete inventory of open work is YouTrack `project: TG #Unresolved`.
> Nothing below steers work by itself. This document is a **DESIGN for owner review**; it authorizes
> **NO code** and flips **NO mutation**.

<!-- TG-122 — DESIGN DOCUMENT (SDD narrative layer, pre-spec).
     Provenance tags: [F] foundation / [R] product reframe / [O] audit overlay.
     Grounded against the SHIPPED spec/017 engine (TG-110) and the real target estate, 2026-08-13.
     Supersedes the architecture of docs/GITOPS-REGIME-ACTUATOR-SPEC.md (2026-07-18) where noted in §10. -->

# TG-122 — the GitOps-MR (`gitops-mr`) lane + the k8s-declarative effect lane

> **STATUS: DRAFT — AWAITING OWNER SIGN-OFF. NO ACTUATOR CODE UNTIL SIGNED.**
> Mutation stays **OFF** (mode Shadow) through this entire design. Nothing here opens a real merge
> request; it names *which channel* a future, separately-authorized change would take, and reuses the
> already-shipped deferred-verify back-half so we can *measure* that channel before we ever arm it. Even
> after the owner-present flip, the **first** real mutation estate-wide remains the Proxmox runtime
> canary — **never** a `gitops-mr` MR.

This is the follow-up carved out of TG-110. Spec/017 (the Actuation Regime Engine) shipped and deployed
the CORE + the `awx-job` and `native-ssh` mutating lanes; it named **`gitops-mr`, `k8s-declarative`, and
`api`** as "the seam's future tenants … their connectors are their own later changes"
([`spec/017-actuation-regime-engine/design.md`](../../spec/017-actuation-regime-engine/design.md) §"Out of
scope"). TG-122 designs the first two of those three.

The headline of this design is that **most of what the 2026-07-18 draft
([`docs/GITOPS-REGIME-ACTUATOR-SPEC.md`](../GITOPS-REGIME-ACTUATOR-SPEC.md)) proposed to build from
scratch has since shipped as general machinery in `core/regime/`** — the global deferred-verify channel,
the async-lane refusal, the durable pending store. So the lane is now a **much smaller, mostly
non-protected-path build** that reuses that machinery rather than performing surgery on the protected
`core/actuate/interceptor.go`. §10 records exactly what this revises versus the prior draft.

---

## 0. Reading order and grounding (cite real seams)

Verified in-tree, 2026-08-13. These are the load-bearing facts the design stands on:

- **The regime seam is already wired and live-shaped.** `temporal/runner/activities.go`
  `ExecuteActivity` (`:2031`) routes a governed `actuate.Request` **either** through the direct
  native-ssh interceptor **or** through `RegimeEngine.SelectLane` / `LaneForRegime` → `LaneEffect.Apply`
  (`:2183-2213`). `effectKindRegime` (`:1563`) maps an op-class's *effect kind* to a regime. The manual
  rollback path (`temporal/runner/rollback_workflow.go:419-482`, TG-462) routes through the **same**
  `RegimeEngine` + `LaneEffect`. A new lane plugs in here with **no new dispatch code**.
- **The Lane abstraction + composition seam are built.** `core/regime/lane.go` — a `Lane` is
  `{ Regime() Regime; effectLeaf() actuation.Actuator }`; the effect leaf is **UNEXPORTED**, reachable
  only by `core/regime/effect.go` `LaneEffect.Apply`, which hands it to `actuate.Interceptor.Do` (the
  spec/013 chain). `nativeSSHLane` / `awxJobLane` / `proxmoxLane` are the templates; a not-yet-wired lane
  carries a `pendingActuator` that fails closed (`lane.go:177`, `ErrLaneNotWired`).
- **The closed regime enum already contains our two targets.** `core/regime/regime.go:47-51` —
  `RegimeGitOpsMR = "gitops-mr"` and `RegimeK8sDeclarative = "k8s-declarative"` are defined and `Valid()`.
- **The async-lane refusal is already structural.** `core/regime/effect.go:86` — `LaneEffect.Apply`
  refuses to drive a lane for which `returnsHandleNotOutcome(regime)` is true (a launch handle is a
  prediction, not a success). Today that predicate returns true **only** for `awx-job`
  (`regime.go:79`); `gitops-mr` must be added to it (a one-line, **non-protected** `core/regime` change).
- **The GLOBAL deferred-verify channel is SHIPPED** (spec/017 T-017-4 / T-017-8, both `completed`).
  `core/regime/asyncverify.go`: `AsyncVerify` with `Reserve` (pre-launch idempotency, REQ-1712),
  `BindHandle` (record the async handle, REQ-1709), a read-only `JobPoller` seam
  (`PollJob(ctx, jobID) (JobStatus, error)`), `GraduationSink.RecordDeferredVerdict`, a baseline anchored
  at `LaunchedAt` (REQ-1712a), and a **durable pgx pending store** that survives a worker restart
  (T-017-8). Its own doc comment (`asyncverify.go:69-73`) already names the tenants: *"every other lane's
  effect (awx-job job template, **gitops-mr merge request, k8s-declarative reconcile**, api) completes
  asynchronously and MUST be verified by this channel rather than trusted at launch."* **This design is
  the realization of that stated intent.**
- **The one missing piece is shared with awx-job.** `core/regime/effect.go:78` records that the
  deferred-verify **producer is unwired for every lane**: *"Reserve/BindHandle have no non-test callers,
  and `actuate.Outcome` carries no handle field, so the job id is discarded at this boundary."* So the
  gitops-mr sensor/verify half is, in large part, *"build the deferred-verify producer + a gitops-mr
  `JobPoller`"* — which **also unblocks awx-job's** deferred verify (slice 0, §9).
- **The AWX-job lane is the actuator template.** `modules/actuation/awxjob/awxjob.go` — an
  `actuation.Actuator` with a fixed argv verb (`LaunchVerb`), a typed `LaunchSpec` in stdin (never a
  command string), an operator `TemplateAllowlist`, an op-class confused-deputy cross-check, a
  credential-after-policy resolve, and a defense-in-depth mode-chokepoint re-guard at the leaf. The
  `gitops-mr` actuator mirrors this shape exactly.
- **The real target estate is 100% GitOps.** [`infrastructure/dc1/production/k8s/CLAUDE.md`](../../../../infrastructure/dc1/production/k8s/CLAUDE.md):
  OpenTofu manages all k8s resources via **Atlantis** (plan/apply on MR comments; `automerge:false`, one
  `k8s` project, apply fans out over the whole project); **Argo CD** auto-syncs 4 apps from
  `argocd-apps/` (push YAML to main). "**Never use `kubectl apply` directly**", "**Never run `tofu apply`
  locally**", `tofu fmt -recursive` enforced, state on GitLab TF HTTP backend. **The only sanctioned
  actuation into this cluster is a git MR that Atlantis/Argo consumes.**
- **The territory gate already knows this.** `core/territory` routes
  `tofu|terraform|argocd|cilium|kubectl|helm` → `TerritoryK8s` with the caveat *"OpenTofu/Atlantis only —
  no kubectl apply / helm install on managed resources"*, failing closed on an infra write it cannot
  place (per the prior draft's grounding, §0).
- **A read-only GitLab sensor already exists.** `modules/actorevidence/gitopsmr/gitopsmr.go` (spec/023,
  a **different** concern — attributing *who* merged a deploy) already speaks the estate GitLab REST API
  (`merged_by` / `merged_at` / `merge_commit_sha`, `/merge_requests/{iid}/diffs`). Its client shape is
  reusable for the actuator's sensor half. **Note the package-name reuse:** the new actuator is
  `modules/actuation/gitopsmr` (distinct import path); the capability slug is `gitops-mr` to match the
  regime.

---

## 1. What the GitOps-MR lane IS

The GitOps-MR lane is a **bidirectional VCS sensor + actuator**. For a GitOps-managed target the effect
channel is **Git, not the cluster API**:

- **Actuator half (the writer).** TG proposes an infrastructure change as a **merge request** to a target
  repo — the `gitops-mr` effect kind. It renders a *structured, single-field* edit onto a new `tg/`
  branch and opens an MR. It **stops there.** It never merges, never comments `atlantis apply`, never
  touches the cluster API.
- **Sensor half (the reader).** TG **senses the MR's lifecycle** — pipeline status, the Atlantis plan
  comment, approvals, merge, apply, and finally the cluster reconcile — and feeds that to the
  deterministic verifier to decide whether the effect actually happened.

**The load-bearing contrast with the direct lanes** (`awx-job` / `native-ssh` / `proxmox`): those apply
an **immediate mutation** and (for the synchronous ones) observe the result inline. The GitOps-MR effect
is a **PROPOSED, human/Atlantis-gated change**. When `Exec` returns, the estate is **untouched** — the MR
merely exists, awaiting a human. This is why the lane is **asynchronous** and why it is refused on the
synchronous verify path until its deferred-verify channel is wired (§5). The lane's contribution is
**"through which channel"** — it authorizes nothing, authenticates nothing, lifts no floor. *A lane is a
channel, not a permission.*

**Why an MR is the correct — and safest — channel here.** A direct `kubectl`/`helm`/`tofu apply` on a
GitOps cluster **drifts** the desired state, is **auto-reverted** by the reconciler (Argo self-heal
reverts within ~5 s), **bypasses review**, and causes **split-brain** between two controllers. The MR
routes the declarative change through the platform's own reconciler, so TG's change is legitimate under
the four OpenGitOps principles (declarative · versioned · pulled · reconciled) by construction.

---

## 2. How it traverses the governed chain

The lane is an **effect leaf beneath the SAME spec/013 interceptor** every other lane uses. `LaneEffect.Apply`
hands the leaf to `actuate.Interceptor.Do`, which runs the wired chain unchanged (spec/017 REQ-1702):

```
admission → never-auto floor → policy authorize → credential authenticate → mode chokepoint → EXECUTE → verify
```

For this lane, **"EXECUTE" = open/update the MR**, not "mutate the cluster." Each gate's meaning:

| Gate | For the gitops-mr lane |
|---|---|
| **admission** (INV-12) | If the op-class bands to POLL_PAUSE, opening the MR itself waits on TG's **own approval vote** (`POST /v1/vote`, the durable Temporal vote-wait). This is the **first** of two human gates (§6b). |
| **never-auto floor** (INV-09) | Re-derived from the op-class **and** the proposed change's semantic op-class: a floor-class change (destroy/delete/drop/prune) is refused **even though a git edit is technically revertable** — a plan cannot launder a floored mutation across the MR lane. |
| **policy authorize** (INV-05, spec/015) | The op-class bound to the target repo + change-class must be non-deny. |
| **credential authenticate** (INV-13, spec/016) | The **api-scoped GitLab PAT** is resolved as a `config.SecretRef` **after** the policy verdict and **before** the write. Never a literal; per-site (NL vs GR). A resolved token is necessary, never sufficient. |
| **mode chokepoint** (`safety.MayActuate`) | **Where it bites — see below.** |
| **EXECUTE** | Open the MR: exactly two REST calls, then STOP (§3). |
| **verify** | The synchronous path is **refused** for this async lane; verification is **deferred** (§5). |

### 2a. Where the mode chokepoint bites — Shadow opens NOTHING

**Recommendation: in Shadow, the lane opens no MR at all — not even a draft.** The interceptor refuses at
the mode chokepoint **before** `Exec`, exactly as it does for `awx-job`; and as defense in depth the
actuator re-guards the mode at its own leaf (mirroring `awxjob.go:196-203`). Rationale:

- **A draft MR is still a repo write.** It creates a branch, a commit, and an MR object; it triggers the
  target repo's pipeline and notifies reviewers. That is an **external effect**, and Shadow's contract is
  **zero external effect**. Opening a draft in Shadow would quietly break the one invariant the mode
  chokepoint exists to hold.
- **The valuable "preview" behavior belongs in the console/ledger, not the repo.** The lane can render
  the would-open diff and record it as a **shadow proposal** (read-only, no repo write, ledger-audited),
  giving the operator the exact change TG *would* propose — without writing anything to GitLab.

Opening a draft MR in Shadow is offered as an **owner open question** (§8 Q2), but the recommendation is
against it. This keeps the mode chokepoint's meaning crisp and the lane structurally identical to every
other lane at Shadow.

### 2b. How it fails closed

Four independent fail-closed properties, all already coded or a one-line addition:

1. **Async-lane refusal (structural, the primary guard).** `returnsHandleNotOutcome(RegimeGitOpsMR)` must
   return true, so `LaneEffect.Apply` (`effect.go:86`) **refuses to adjudicate the lane on the synchronous
   verify path** — a governed refusal (`Refused=true`, `Executed=false`, nil error), not a bypass. The
   lane becomes actuatable **only once the deferred-verify producer is built** (§5). Injecting a
   gitops-mr actuator does **not** arm it; the missing integration cannot be activated by environment
   alone (the exact hardening `effect.go:78-90` applied to awx-job).
2. **Unwired lane refuses.** With no injected actuator the lane carries the `pendingActuator`
   (`lane.go:177`) → `ErrLaneNotWired`.
3. **Ambiguous / unresolved target refuses.** `SelectLane` / `LaneForRegime` fail closed on an unknown,
   ambiguous, or resolved-but-unwired regime (`engine.go:75-102`) — never a guessed lane.
4. **Mode chokepoint at Shadow refuses** before `Exec`, and again at the leaf.

Plus lane-specific pre-write refusals (§3): a non-allowlisted repo, an op-class/repo mismatch, a
secret-value in the patch, a competitor MR on the same paths, or an edit that resolves to ≠1 field.

---

## 3. The op-classes, the effect, and MR-body derivation

### 3a. What routes to `gitops-mr` vs `k8s-declarative`

Two regime slugs exist (`regime.go`), and they are **not redundant**:

- **`gitops-mr` is the CHANNEL** — the provider-general VCS/MR actuator (open an MR, sense its
  lifecycle). It is what actually *does* the work.
- **`k8s-declarative` is a REGIME / effect-kind** — a target whose declarative source is a Kubernetes
  manifest or Helm value under Git. **For this estate (100% GitOps), a `k8s-declarative` change is
  realized THROUGH the `gitops-mr` channel** — its lane *is* the gitops-mr actuator, parametrized with
  k8s-aware renderers (§4). The `k8s-declarative` slug is retained distinct so a **non-GitOps**
  declarative-k8s cluster could later bind it to a *different* lane (a server-side-apply / `kubectl diff`
  channel) — but that lane is **out of scope** here and unneeded for this estate.

So the op-class → channel map for this estate:

| Op-class (examples) | Regime / kind | Channel |
|---|---|---|
| Helm/HelmRelease `values`, `replicas`, image/tag, resources, env/ConfigMap, node-pool size, any `.tf`/manifest field on a managed target | `k8s-declarative` (and any `gitops-mr`-regime target) | **`gitops-mr`** — open MR, Atlantis plan→apply / Argo reconcile |
| pod delete/restart, `rollout restart`, node cordon/uncordon | *runtime* | **Direct** via the (kept) `kubernetes` actuator — self-healing, doesn't fight the reconciler |
| `kubectl delete pvc/pv/ns/secret`, `apply --prune`, `helm uninstall/rollback`, `tofu/terraform destroy`, node **drain** | any | **Escalate** — already on the never-auto floor; human-only |

Routing uses the **existing** seam: `effectKindRegime` (activities.go:1563) gains a case mapping the
declarative-k8s effect kind → `RegimeGitOpsMR` (or the target's regime rule resolves to `gitops-mr` via
`SelectLane`). This adds the **op-class → effect-kind** data in the op-class catalog (spec/028) and one
`switch` case; no new dispatch logic.

### 3b. How the MR body is DERIVED — argv-only / registry-gated / templated diff, NO free-form model text

The MR content is **not** model-authored file bytes. It is derived, exactly mirroring how `awx-job`'s
effect is *template id + typed `extra_vars`, never a command string*:

- **The plan-as-data is a typed `ProposeSpec` in the request stdin** (the argv is a single fixed verb, so
  there is no command string to interpolate — the `awxjob.LaunchSpec` pattern):

  ```go
  // modules/actuation/gitopsmr — sketch, NOT implementation
  const Capability  = "gitops-mr"          // matches regime.RegimeGitOpsMR
  const ProposeVerb = "gitops-mr-open"     // the fixed argv[0]; the plan travels as JSON stdin

  type ProposeSpec struct {
      RepoID    string      `json:"repo_id"`    // MUST be on the operator RepoAllowlist
      OpClass   string      `json:"op_class"`   // cross-checked vs RepoPolicy.OpClass (confused-deputy)
      Edits     []FieldEdit `json:"edits"`      // typed field edits — NO free-form file content
      Rationale string      `json:"rationale"`  // templated MR prose; a pre-render guard rejects secrets
  }
  type FieldEdit struct {
      FieldRuleID string `json:"field_rule_id"` // selects ONE closed FieldRule on the repo policy
      NewValue    string `json:"new_value"`     // the scalar (replicas=3, image tag) — typed, validated
  }
  ```

- **The diff is produced by a structured, file-type-dispatched renderer**, never regex/`sed`:
  - `*.tf` and `helm_release { set { … } }` → **`hclwrite`** (token-level: `ParseConfig →
    FirstMatchingBlock → SetAttributeValue → Bytes`; preserves comments/order/indent; helm `set.name`
    doubly-escaped `a\\.b\\.c`).
  - `argocd-apps/**` manifests and `values.yaml(.tpl)` → **`kyaml` RNode** (`Lookup`/`FieldSetter`;
    avoid `sigs.k8s.io/yaml`, which drops comments).
  - **Diff-minimality is the review contract:** the renderer is allowlisted to in-place-updatable fields
    (`replicas`, `image`, values); a `ForceNew`/`-/+` replace routes to a higher-approval regime, not
    this lane. An edit that resolves to **≠ exactly one field** is **refused** (fail-closed, same
    direction as the registry).
- **The operator `RepoAllowlist` is the registry gate** (the `awxjob.TemplateAllowlist` analogue):

  ```go
  type RepoPolicy struct {
      BaseURL      string            // per-site GitLab base (NL vs GR) — never hardcoded
      ProjectPath  string            // e.g. infrastructure/dc1/production
      TargetBranch string            // e.g. main
      BranchPrefix string            // reserved TG prefix, e.g. "tg/"
      TokenRef     config.SecretRef  // api-scoped PAT, sealed; never a literal
      OpClass      string            // the op-class this repo+change-class is authorized for
      FieldRules   []FieldRule       // the CLOSED set of locate rules the renderer may target
  }
  type RepoAllowlist map[string]RepoPolicy   // keyed by repo id
  ```

  A repo absent from it is not writable; an edit naming a `FieldRuleID` not on the policy is refused.
  So the model chooses **which allowlisted op-class, which target, which in-schema new-value** — **never
  free-form repo bytes.** This is the same "not a shell escape" posture the awx-job lane has for its
  template + typed vars.

### 3c. Target repo + branch + credential (grounded)

Resolved by the prior draft's local scan (§12) and confirmed against the estate:

- **Repos:** per-site infra repos, each its own git repo on a per-site GitLab —
  `infrastructure/dc1/production` (NL, on the NL per-site GitLab) and
  `infrastructure/dc2/production` (GR, on a **separate** per-site GitLab instance). The k8s config is the
  Atlantis **`k8s` project** (dir `k8s`), `k8s/namespaces/<ns>/*.tf` + `k8s/_core/<comp>/*.tf` with
  `helm_release` + `values.yaml.tpl`; Argo app-of-apps under `k8s/argocd-apps/`. **Two instances ⇒
  `BaseURL` and `TokenRef` are per-repo config, never hardcoded.**
- **Branch:** target `main`; reserved TG prefix `tg/` (so the sensor never fights `renovate/*` or Atlantis
  dirs).
- **Credential:** an **`api`-scoped** GitLab PAT per site, sealed as a `config.SecretRef` (env:/file:),
  **never** embedded in a remote URL or committed. **`write_repository` scope is insufficient** (Git-over-
  HTTP only, no REST) and must be rejected; **`CI_JOB_TOKEN` is forbidden** (its recursion guard
  suppresses the MR pipeline). The bot holds **Developer** (push + open-MR), **not** merge — which *is*
  the Free-CE anti-self-merge (§6b).

---

## 4. The k8s declarative effect lane

A k8s change flows **without any direct cluster write**:

```
TG renders a declarative edit           (helm_release.set in *.tf via hclwrite,
   │                                      OR an argocd-apps/*.yaml field via kyaml)
   ▼
open MR to the k8s project (tg/ branch)  ← the gitops-mr actuator: exactly two REST calls, then STOP
   ▼
Atlantis autoplans on MR-open, posts the plan comment      (TG SENSES this; never comments `atlantis apply`)
   ▼
human Maintainer reviews the plan, comments `atlantis apply`, merges   (Atlantis automerge:false, one k8s project)
   ▼                                          — OR, for argocd-apps/, a human merges and Argo auto-syncs
Atlantis apply / Argo sync reconciles the cluster
   ▼
TG SENSES reconcile-convergence → deferred verdict (§5)
```

This **honors every estate rule**: never `kubectl apply` (TG opens an MR, the reconciler owns the
apiserver); never `tofu apply` locally (Atlantis applies on the human's comment); `tofu fmt -recursive`
runs in the pipeline the MR triggers. **The direct `kubernetes` actuator
(`modules/actuation/kubernetes/kubernetes.go`) is demoted** to the runtime lane only: its declarative
verbs (`apply`/`patch`/`scale`) on a gitops-managed target route to `gitops-mr`; `rollout restart` / pod
delete / cordon-uncordon stay direct (self-healing, they don't fight the reconciler). The floor already
re-derives destructiveness server-side, so a mislabeled plan cannot slip a declarative write through as
"runtime."

**Two hard k8s-specific refusals** (fail-closed, from the industry research):
- **Controller-owned fields are off-limits.** If an HPA targets the workload, `replicas` is runtime-owned
  → **refuse the MR**; respect `ignoreDifferences` paths. Never author `atlantis.yaml` / Argo `AppProject`
  policy files.
- **Secret VALUES never enter Git.** Under OpenBao + External Secrets, only *references* live in Git — an
  MR editing a decoded value either leaks a literal secret or is a no-op. A **pre-render guard** scans the
  patch for decoded values and hard-fails; only reference/plumbing edits (`remoteRef`, `SecretStore`,
  `refreshInterval`) are MR-safe.

---

## 5. The sensor half + VERIFY — reusing the shipped deferred-verify channel

This is the design's central architectural choice, and it is **the documented intent of the shipped
code** (`asyncverify.go:69-73` names gitops-mr and k8s-declarative as tenants of this exact channel).

**The gitops-mr lane is an ASYNC lane exactly like awx-job.** After `Exec` opens the MR, the estate is
untouched; the real effect (merge → apply/sync → reconcile) lands minutes-to-days later, human-gated. So:

1. **`Exec` returns the MR handle** (`"<repoID>!<iid>"`) as the async job handle in
   `actuation.Result.Stdout` — the same shape `awxjob.Exec` returns its job id (`awxjob.go:242-248`). The
   actuator declares **no success** at open time.
2. **The deferred-verify channel drives it.** `core/regime/asyncverify.go` — `Reserve` claims the
   `action_id` pre-launch (idempotency, REQ-1712 — a retry/redelivery never double-opens an MR),
   `BindHandle` records the MR handle (REQ-1709), and a poll loop calls a **gitops-mr `JobPoller`** to
   terminal, then computes the spec/002 verdict against the `LaunchedAt` baseline (REQ-1712a) and feeds
   the graduation ladder via `RecordDeferredVerdict` (REQ-1710). A launch that never reaches terminal
   within the operator bound is `unverified` and **never counts as a clean run** (REQ-1711).
3. **A gitops-mr `JobPoller` maps the MR lifecycle → `regime.JobStatus`** (the AWX client satisfies the
   same seam via `GET /api/v2/jobs/{id}/`; the gitops-mr client satisfies it via the estate GitLab REST +
   the reconcile signal):

   ```go
   // implements regime.JobPoller — READ-ONLY (the composition check forbids an Exec here)
   func (p *GitOpsMRPoller) PollJob(ctx context.Context, jobID string) (regime.JobStatus, error) {
       // read: MR state (open/merged/closed) + pipeline + Atlantis plan/apply comment + reconcile signal
       //   opened / plan-posted / merged / applied-not-yet-reconciled  → JobRunning   (non-terminal → stays pending-verification)
       //   RECONCILED and live-observed at the predicted state          → JobSuccessful (terminal → adjudicate vs prediction)
       //   closed-unmerged | plan Error | apply failed                  → JobFailed    (terminal → never a clean run)
       // a transient read error returns (err) → the deferred verify stays pending and retries (never fabricates terminal)
   }
   ```

**What "the effect happened" means for an async, human-gated apply.** It is **reconcile-convergence**,
observed on the live cluster — **not** "MR merged", **not** "Atlantis apply returned 0" (the estate doc
is explicit: *"Apply complete ≠ the live cluster changed"*). So `JobSuccessful` is emitted **only** when
the deferred observer re-reads live state (or a trusted reconcile signal) and sees convergence to the
committed prediction. Everything before that — open, plan, merge, apply-return — is `JobRunning`, and the
action sits `pending-verification`.

**How the workflow models the delay.** No new primitive: the **existing durable pgx pending store**
(T-017-8, survives worker restarts) + the poll loop + the per-op-class **verification bound** (config-not-
code) already provide "park until settled, then adjudicate, or time out to `unverified`." The gitops-mr
verification bound is simply **long** (hours/days) where awx-job's is short. The prior draft's bespoke
`SettleAndVerifyActivity` + `settleFor` timer + `core/verify/proposal.go` are **subsumed** by this shipped
machinery (§10).

**The sensor is partly built.** `modules/actorevidence/gitopsmr` already reads the estate GitLab MR REST
(state, `merged_by`, `merged_at`, diffs); the poller reuses that client shape. A webhook sensor (real-time)
plus a low-frequency reconcile poll (safety net) is the recommended ingest (owner knob, §8).

**Multi-actor no-fight contract** (Atlantis/Renovate/Argo are co-actors on one Git source of truth) — a
**hard pre-write gate** in the actuator: `ListChangeRequests` + diff-intersect the path-set; a competitor
MR touching the same paths → **back off / refuse** (no split-brain); reserve the `tg/` prefix; recognize
`renovate/*` / Atlantis dirs / Argo paths; re-verify the base SHA at push time and abort-on-change (never
force); a **closed/reverted MR is a durable negative signal** keyed `(target, change)` → no auto-re-propose
(avoid the "immortal Renovate PR").

---

## 6. Safety properties — mapped to the mission guardrails

**(a) An MR effect is INHERENTLY REVERSIBLE.** Its inverse is a revert:
- **Before merge** the MR is a pure proposal — the inverse is *close the MR + delete the `tg/` branch*
  (recorded in the `ExecRecorder` forward/inverse log). Never a counter-mutation.
- **After merge** the inverse is a **fresh governed `git-revert` MR**, itself traversing the full chain.
  This pairs with **TG-82** (commit-confirmed / auto-revert: the deferred-verify terminal *is* the confirm
  window — a deviating or non-converging deferred verdict arms the revert-MR) and **TG-462** (operator
  manual rollback over `InvertsActionID`, which **already routes through the same `RegimeEngine` +
  `LaneEffect`** in `rollback_workflow.go:419`). In the op-class catalog a gitops-mr propose is
  `TierLowReversible` (`core/actuate/opschema`), and a revert-MR is its `RollbackArgv`. → **INV-09/INV-10**.

**(b) It is DOUBLY GATED — TG can never unilaterally change the cluster.**
- **Gate 1 — TG's own chain:** mode chokepoint (Shadow ⇒ no MR) + policy + the admission vote (POLL_PAUSE
  bands wait on `POST /v1/vote` before the MR is even opened).
- **Gate 2 — the human/Atlantis review:** on Free CE, `main` is protected, the bot holds **Developer**
  (push/MR, not merge), Atlantis `automerge:false`; a human **Maintainer** reads the plan comment,
  comments `atlantis apply`, and merges. Even an **AUTO-banded** op — where gate 1 is automatic — still
  hits gate 2, so TG can only ever *open* an MR, never merge or apply. → **INV-21** (single chokepoint) +
  the estate's structural anti-self-merge.

**(c) NEVER direct-applies.** The actuator makes **exactly two REST calls** (atomic branch+commit via
`actions[]`, then create-MR) and stops — never the Files API, never `atlantis apply`, never merge, never
`kubectl`/`helm`/`tofu apply`. → the estate's "never kubectl apply / never tofu apply locally" + the
`core/territory` `TerritoryK8s` caveat.

**(d) Fail-closed** — the four properties of §2b (async-lane refusal, unwired-lane refusal, ambiguous-target
refusal, mode-chokepoint refusal) plus the pre-write refusals (non-allowlisted repo, op-class mismatch,
secret-value guard, competitor-MR back-off, ≠1-field). → **INV-09/INV-10/INV-21**.

**(e) It is DOUBLY AUDITABLE.** The append-only ledger records `regime_resolution` + `regime_actuation` +
`deferred_verdict` (no secret value; `tg_runtime` holds no UPDATE/DELETE — REQ-1715 / T-017-6), **and** the
MR itself is an independent, human-readable audit trail (the diff, the reviewers, the Atlantis plan
comment, the merge + apply record). → **INV-19**.

---

## 7. Incremental slices — smallest-safe-first

Every slice ships **DARK** (inert under mutation-OFF) with oracle/fake coverage. The `gitops-mr` regime
being unconfigured is treated as a **config accident, not a control** — so each slice is structurally inert
(fail-closed) independent of configuration.

| Slice | Builds | DoD (oracle) | Protected-path / eval posture |
|---|---|---|---|
| **0. Deferred-verify PRODUCER seam** *(shared prerequisite — also unblocks awx-job)* | Add an async-handle field to `actuate.Outcome`; wire `ExecuteActivity` to `Reserve` (pre-launch) + `BindHandle` (post-launch) + register the poll loop against `AsyncVerify`. Fixes `effect.go:78` ("no non-test callers"). | An async launch's handle is durably bound; the poll loop drives it to terminal; `RecordDeferredVerdict` feeds graduation; a second launch for the same `action_id` is refused (REQ-1712). | ⚠ **PROTECTED** — `actuate.Outcome` is in `core/actuate/`. Needs `@ncpjfuzl` + spec/017 lockstep restamp. **Smallest possible protected touch** (one field + wiring), and it is **shared with awx-job**. |
| **1. `gitops-mr` lane skeleton + Shadow-only, no repo write** | `returnsHandleNotOutcome(gitops-mr)=true` (`core/regime/regime.go`); `NewGitOpsMRLane` + `WithGitOpsMRActuator` (`core/regime/lane.go`, mirror `awxJobLane`); `modules/actuation/gitopsmr` — the `Actuator` (New/Capability/ReadOnly/Exec), `RepoAllowlist`, `ProposeSpec`, the hclwrite+kyaml renderers, the two-REST-call writer behind an injected fake `Doer`. Ships **DISABLED**. | Fake `Doer` proves **exactly two** REST calls (atomic branch+commit via `actions[]`, then MR); MR opened-**not**-merged; refuses under gate-off; refuses non-allowlisted repo / op-class mismatch / secret-value in patch / renderer resolving ≠1 field; comments+order preserved. Doubly inert (Shadow **and** async-refusal). | **NOT protected** (`core/regime/`, `modules/actuation/gitopsmr/`). No agent-reasoning-surface change ⇒ eval gate not strictly triggered, but run tg-code-reviewer + green pipeline + `go test -race`. |
| **2. Sensor + gitops-mr `JobPoller` (the deferred verify)** | GitLab sensor (reuse `actorevidence/gitopsmr` client shape) reading MR/pipeline/Atlantis-plan/merge/apply + a reconcile signal; `GitOpsMRPoller` implementing `regime.JobPoller` (MR-lifecycle→`JobStatus`); wire it into `AsyncVerify` via slice 0's producer. | open→merged→applied→**reconciled** = `JobSuccessful` **only** on observed convergence; closed-unmerged / plan-error / apply-failed = `JobFailed`; never-converges-in-bound = `unverified` (REQ-1711); verdict baselined at `LaunchedAt` (REQ-1712a); double-poll idempotent. | **NOT protected** (`core/regime/` async wiring + `modules/`). Run the tests DSN-gated where they touch the pending store. |
| **3. k8s op-classes + declarative→gitops-mr routing + kubernetes-actuator demotion** | Add the declarative-k8s effect kind to the op-class catalog (spec/028 / `core/actuate/opschema`); `effectKindRegime` case → `RegimeGitOpsMR`; demote `modules/actuation/kubernetes` declarative verbs (route `apply`/`patch`/`scale` on a managed target → gitops-mr; keep runtime verbs direct). | A gitops-managed k8s declarative op resolves to the gitops-mr lane; a runtime op stays direct; HPA-owned `replicas` refused; the never-auto floor still re-derives destructiveness. | ⚠ **PARTIALLY PROTECTED** — `core/actuate/opschema/` (the effect-kind enum) needs `@ncpjfuzl` + spec restamp. `effectKindRegime` (`temporal/runner/`) and `modules/actuation/kubernetes/` are **not** protected. |
| **4. Arm live behind the mode chokepoint — SCRATCH repo first (owner-present)** | Provision the api-scoped PAT (`SecretRef`), a `RepoAllowlist` for a **scratch** repo, the Free-CE anti-self-merge on it. Escalate the mode for the scratch target only; prove the whole chain e2e. | On the scratch repo: open MR → human merge → Atlantis apply → reconcile → deferred `JobSuccessful` → graduation credit; and a deviating apply → `JobFailed` → breaker trip → revert-MR. The **real** infra repo enters the allowlist only after scratch proof, and the first estate-wide mutation stays the Proxmox canary. | **No new protected code** (config + provisioning + owner-present flip). Eval gate + tg-eval-runner before the flip (eval gates deploys). |

**Deferred to a later epic / v2 (explicitly out of TG-122's minimal-lane scope):**
- **The pre-commit plan-gate** (`atlantis plan`/`tofu plan` == committed prediction *before* merge — the
  prior draft's genuinely strong idea). It is **additive** to the deferred verify, not a replacement, and
  requires a `PlanActuator` type-assertion inside `core/actuate/interceptor.go` (⚠ protected). Valuable
  but not needed for a correct, measured lane.
- **The autonomy toggle** (assisted/autonomous — `core/risk`, ⚠ protected), the **blast-radius deploy
  reasoner** (`core/deploy` + `modules/vcs/tfplan`, lighting up `infragraph_prediction=0`), and the
  **approval-plane chatops bridge** (`adapters/approval` + a GitLab channel). Each is its own epic; the
  2026-07-18 draft bundled five briefs — TG-122 is scoped to **the two lanes** only.

---

## 8. Open questions for the owner

The decisions only the owner can make (top five first):

1. **Which repos are in scope + the bot identity's blast radius?** An `api`-scoped Developer PAT on the
   infra group can open MRs across *all* infra repos on that instance. Scope to a single repo / the `k8s`
   project, or a group token for the whole GitOps group (new repos auto-covered)? One sealed PAT **per
   GitLab instance** (NL vs GR). (Grounded default: per-repo `api` PAT, Developer role, Free CE.)
2. **Shadow behavior: open a draft MR, or open nothing?** *Recommendation: open **nothing** — a draft MR
   is still a repo write (branch+commit+pipeline+reviewer notice), which breaks Shadow's zero-external-
   effect contract. Render the would-open diff as a read-only console/ledger shadow proposal instead.*
3. **Is a merged-but-unapplied MR "actuated"?** *Recommendation: **NO.** "The effect happened" =
   reconcile-convergence observed on the live cluster, not merge and not apply-return (the estate doc:
   "Apply complete ≠ the live cluster changed"). The deferred verdict stays `pending` until convergence.*
4. **Interaction with Atlantis's own approval / apply.** Confirm TG **opens-and-STOPS** and a human
   comments `atlantis apply` + merges (grounded in `atlantis.yaml` `automerge:false`, one `k8s` project —
   a bare `atlantis apply` fans out over the whole project). TG never posts `atlantis apply`.
5. **Confirm the `k8s-declarative` → `gitops-mr` binding for this estate**, and that the
   **kubernetes-actuator demotion** (declarative verbs → MR, runtime verbs stay direct) is wanted now.

Further knobs (lower-stakes, config-not-code):
6. **Does the async-lane deferred-verify model supersede the prior draft's ★ blocking `open_ok`/gate-4c
   decision?** *Recommendation: **YES** — the async-lane refusal (REQ-1718) + the shipped `asyncverify`
   channel is the honest fail-closed posture; no synchronous "proposal-open" verdict is ever credited to
   graduation, so gate-4c never needs a special case. This dissolves the 2026-07-18 blocking question.*
7. **Sensor ingest:** webhook (real-time; needs public ingress + a secret) + a low-frequency reconcile
   poll (safety net). *Recommendation: both, webhook primary.*
8. **Verification-bound windows per op-class** (`gitops-mr-apply` window is hours/days) — owner-set.
9. **Capability slug** is `gitops-mr` (matches the regime); the actuator package is
   `modules/actuation/gitopsmr` (distinct from the existing `modules/actorevidence/gitopsmr` sensor).
   Confirm the slug.
10. **GR is a separate GitLab instance** — confirm v1 scopes to **NL** and GR is a later per-site
    provisioning step.

---

## 9. Protected-path / spec determination (for the review checklist)

The protected-path CI gate (`scripts/lint-protected-paths.sh:45`) fires on:
`core/safety/ · core/actuate/ · core/policy/ · core/risk/ · core/predict/ · core/verify/ · core/breaker/
· core/territory/` (+ constitution/CI/CODEOWNERS/AGENTS/CLAUDE docs). A change there needs an owner
`Law-Change:` approval trailer (`@ncpjfuzl`). Separately, `/spec/` is **CODEOWNED** (`@ncpjfuzl` merge
approval) though it was dropped from the CI *trailer* gate on 2026-07-30; and the **spec-code lockstep
gate (spec/007)** requires a spec-prose edit in the same MR as any governed code change.

**`core/regime/` is NOT a protected path.** This is the crux that makes TG-122 tractable:

| Slice | Touches protected `core/actuate`/etc.? | Needs `@ncpjfuzl` + spec lockstep? |
|---|---|---|
| **0. producer seam** | **YES** — one async-handle field on `actuate.Outcome` (`core/actuate/`) | **YES** — smallest possible touch; restamp spec/017 (REQ for the producer). Shared with awx-job. |
| **1. lane skeleton** | No (`core/regime/`, `modules/actuation/gitopsmr/`) | Spec/017 restamp for the new lane REQ (lockstep); **no** protected-paths trailer. |
| **2. sensor + poller** | No (`core/regime/` async wiring, `modules/`) | Spec/017 restamp (lockstep); no trailer. |
| **3. op-classes + demotion** | **PARTIAL** — the effect-kind enum in `core/actuate/opschema/` | **YES** for the opschema enum (trailer + spec/028 restamp); `effectKindRegime` + `modules/actuation/kubernetes/` are not protected. |
| **4. arm live** | No (config/provisioning) | Eval gate before the flip; no code trailer. |

**Net:** the actuator, its renderers, the lane wiring, the sensor, and the poller are **all
non-protected** (`core/regime/` + `modules/`). Only **two small, well-bounded protected touches** remain
— the async-handle field on `actuate.Outcome` (slice 0, shared with awx-job) and the effect-kind enum in
`core/actuate/opschema` (slice 3) — versus the prior draft's **two full `Interceptor.Do` surgeries**
(plan-gate 4d + the deferred-verify `Settle` split) plus a new `core/verify/proposal.go` and a
`core/safety` `VerdictPending`. New spec requirements (REQ-17xx) for the two lanes belong in **spec/017**;
the new op-classes belong in the **spec/028** catalog; the commit-confirmed pairing is **spec/029**
(TG-82). All spec edits are `@ncpjfuzl`-CODEOWNED and travel in lockstep with their code.

---

## 10. What this KEEPS vs REVISES from the 2026-07-18 draft

The prior draft ([`docs/GITOPS-REGIME-ACTUATOR-SPEC.md`](../GITOPS-REGIME-ACTUATOR-SPEC.md)) was written
**before spec/017 shipped**. Much of what it proposed to build now exists as general machinery. This
design re-grounds against the shipped code.

**KEPT (still correct, grounded, adopted):**
- **Propose-never-apply, exactly two REST calls** (atomic branch+commit via `actions[]`, then create-MR;
  never the Files API, never `atlantis apply`, never merge).
- **Structured file editors** (hclwrite for `.tf`/`helm_release.set`; kyaml for `argocd-apps`; reject
  regex/`sed`; diff-minimality as the review contract; `ForceNew` routes to higher approval).
- **The multi-actor no-fight contract** (sense-before-write path-intersect; reserve `tg/`; recognize
  `renovate/*`/Atlantis/Argo; abort-on-base-change, never force; closed-MR = durable negative signal).
- **Secret VALUES never enter Git** (only `SecretRef`s; a pre-render guard hard-fails a decoded value).
- **The Free-CE anti-self-merge model** (protected `main`, bot = Developer not merge, Atlantis
  `automerge:false`, human Maintainer applies+merges) and the **token requirements** (`api`-scoped, not
  `write_repository`, not `CI_JOB_TOKEN`, sealed `SecretRef`, per-site).
- **The regime-registry / config-not-code / fail-closed-on-unplaceable model** — now **realized** as
  `core/regime` `Engine` + resolver (the draft's §2 is shipped).

**REVISED / SUPERSEDED (because the machinery shipped):**
- **The bespoke async back-half → the shipped `core/regime/asyncverify.go`.** The draft proposed building
  `ProposalVerdict` (`core/verify/proposal.go`), splitting `Interceptor.Do` into a new
  `Interceptor.Settle`, a `DeferralSink`, a `VerdictPending` in `core/safety`, a `SettleAndVerifyActivity`
  + `settleFor` timer, and a durable breaker — **all protected-path.** All of it is **subsumed** by the
  shipped `AsyncVerify` (`Reserve`/`BindHandle`/`JobPoller`/`RecordDeferredVerdict`) + the durable pgx
  pending store (T-017-4/8) + the async-lane refusal (`returnsHandleNotOutcome`, T-017-R) + baseline-at-
  `LaunchedAt` (REQ-1712a). The gitops-mr lane **reuses** this; it does **not** re-derive it. This is the
  single biggest reduction in scope and protected-path surface.
- **The ★ blocking "does `open_ok` satisfy gate 4c?" decision → DISSOLVED.** Modeling the lane as async
  (refused on the synchronous path until the deferred-verify producer is wired) is the honest fail-closed
  posture the shipped spec already mandates (REQ-1718). No synchronous proposal verdict is credited to
  graduation, so gate-4c needs no new reasoning. (Recorded as owner Q6 for explicit sign-off that this
  supersedes the prior blocking question.)
- **The plan-gate (4d), autonomy toggle, blast-radius reasoner, and approval-plane bridge → DEFERRED to
  separate epics.** The draft bundled five briefs into one architecture; TG-122 is scoped to **the two
  lanes**. The plan-gate is retained as a *v2 enhancement* (a stronger pre-commit prediction check), not a
  prerequisite for a correct, measured lane.
- **`k8s-declarative` clarified as a regime realized THROUGH the `gitops-mr` channel** for this GitOps
  estate (the draft treated the k8s MR actuator and the regime somewhat interchangeably). The distinct
  slug is retained for a future non-GitOps server-side-apply lane, explicitly out of scope here.

**NEW (not in the prior draft):**
- The **shared deferred-verify producer prerequisite** (slice 0): `actuate.Outcome` carries no handle
  field and `Reserve`/`BindHandle` have no non-test callers (`effect.go:78`), so **awx-job's own deferred
  verify is also unwired** — building the producer serves both lanes. This dependency did not exist when
  the draft was written (the channel it depends on had not shipped).
- The **sensor is partly pre-built**: `modules/actorevidence/gitopsmr` already speaks the estate GitLab MR
  REST API; the poller reuses its client shape.

---

*End of DRAFT. This document authorizes NO code and flips NO mutation. It re-grounds the 2026-07-18 draft
against the shipped spec/017 engine, reducing the two remaining lanes to a mostly non-protected-path build
that reuses the global deferred-verify channel. Owner sign-off on §8 (Q1-Q5, and the Q6 supersession) is
the gate to open a spec/017 restamp and begin slice 0.*
