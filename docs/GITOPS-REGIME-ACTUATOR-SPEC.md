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

<!-- Territory Grounder — DESIGN SPEC (SDD narrative layer).
     Provenance tags: [F] foundation / [R] product reframe / [O] audit overlay.
     Parent design: docs/K8S-MANAGEMENT-REGIME-DESIGN.md (owner-approved regime model; see the
     management-regime-gap memory note for its substance). This spec REFINES that brief into one
     buildable architecture. -->

# GITOPS-REGIME-ACTUATOR-SPEC.md — the GitOps management-regime actuator (`gitopsmr`) + the autonomous GitOps-SRE loop

> **STATUS: DRAFT — AWAITING OWNER SIGN-OFF. NO CODE UNTIL SIGNED.**
> Mutation stays **OFF** through this entire spec. Nothing here flips a mutation; it changes only
> *which channel* a future, separately-authorized mutation would take, and lights up the currently-dark
> predict→verify→score back-half so we can *measure* that channel before we ever arm it. The first real
> mutation remains the **Proxmox runtime canary** (task #23), **never** a `gitopsmr` MR.
>
> This spec synthesizes five distilled design briefs — the GitOps/MR regime actuator, the
> async-verify chokepoint, the VCS sensor+actuator, the blast-radius reasoner, and the approval
> plane — into ONE coherent architecture. Each brief carried its own open questions; those are
> consolidated in **§10**. The single **blocking** decision (does an `open_ok` proposal satisfy gate
> 4c under INV-10?) is called out in §4 and §10.

---

## 0. Reading order and grounding

This spec is written against the **real** TG code, not a greenfield sketch. The load-bearing seams,
verified in-tree:

- `core/actuate/interceptor.go` — the single wired-by-construction chokepoint `Interceptor.Do`
  (INV-21). The gates run in order: SelfTest → nil-manifest → GuardMutation (1) → poll-band-needs-
  approval (1b) → **never-auto floor at the adapter** (2, `:192`) → structure/ungated (3) → evidence
  (4) → **territory gate** (4b, `:213`) → **verifiability gate** (4c, `:224`, `if r.Observe == nil {
  refuse }`) → **Exec** (5, `:228`, the chokepoint) → ExecRecorder (5a, `:237`) → StageExecuted (5b) →
  **ComputeVerdict** (6, `:251-252`) → VerifyChain → VerdictSink.Commit (7) → audit (8) → tripBreaker
  (8b, `:281`).
- `core/verify/verdict.go` — `ComputeVerdict(pred, observed)`, the SOLE mechanical verdict writer
  (INV-10). Pure, provenance-blind over `[]verify.ObservedAlert`; a surprise host is always a
  deviation (fail-closed).
- `core/estate/estate.go` — `Graph.BlastRadius(target, maxDepth)` (edges INTO a node = who-depends-on-
  me), `Siblings`, `Parents`, `Resolve` (name → most-specific typed node). The change-impact engine,
  reused unchanged.
- `core/safety/safety.go` — `IsNeverAuto`, `IsDestructiveOp`, `IsStatefulWorkload`, the `Verdict`
  enum + `ValidVerdict`, and the `MutationGate` (starts disabled; only `EnableMutation` + a green
  `SelfTest` prover can flip it).
- `core/httpapi/vote.go` — `POST /v1/vote` (AuthSession only) → `VoteSignaler.SignalVote(externalRef,
  actionID, approve, voter)`; the browser voter is the server-derived session principal.
- `temporal/runner/workflow.go` — the deterministic `RunnerWorkflow`; the vote-wait primitive
  (`GetSignalChannel(VoteSignalName)` `:301` + durable `NewTimer(VoteWait=24h)` `:306` + `Selector`
  `:310-316`, INV-12 misbound guard `maxMisbound=64` `:320`); the execute/verify tail `:403-422`.
- `adapters/notifier/notifier.go` — the existing `Notifier{SourceType, Notify, ResolveVote(raw
  []byte)}`. Structurally insufficient for signed callbacks (no HTTP-header access) — see §7.
- `adapters/actuation/actuation.go` — the `Actuator{Capability, ReadOnly, Exec}` contract (INV-02, no
  shell).
- `core/territory/territory.go` — the `Gate.Permit`; K8s classifier already routes
  `tofu|terraform|argocd|cilium|kubectl|helm` → `TerritoryK8s` with caveat *"OpenTofu/Atlantis only —
  no kubectl apply / helm install on managed resources"*; `Permit` fails **closed** on a confirmed
  infra write it cannot place.

Parent brief: the owner-approved **K8S management-regime design** (predecessor = read-only + escalate,
strict-GitOps; industry = runtime-lane-direct vs declarative-lane-MR-only; TG recommendation =
regime-per-target + build the `gitopsmr` MR actuator + demote the k8s actuator's declarative verbs).

---

## 1. OVERVIEW — the autonomous GitOps-SRE loop as ONE architecture

For a **GitOps-managed** target the effect channel is **Git, not the cluster API**. A direct
`kubectl`/`helm` write drifts the cluster, is auto-reverted by the reconciler, bypasses review, and
causes split-brain (two controllers). So the loop routes the *declarative* effect through an MR and
lets the platform's own reconciler (Atlantis in v1, ArgoCD in v2) apply it. The complete loop:

```
 alert / deploy-intent
        │
        ▼
 ┌───────────────┐   ┌────────────────────┐   ┌──────────────────────┐
 │ 2 REGIME       │   │ 6 BLAST-RADIUS      │   │  RunnerWorkflow       │
 │   REGISTRY     │──▶│   REASONER          │──▶│  (deterministic)      │
 │ target→plane,  │   │ change→estate nodes │   │  investigate→classify │
 │ regime,repo,   │   │ →BlastRadius+Sibs   │   │  →gate(commit pred)   │
 │ path, autonomy │   │ =committed Predict  │   └──────────┬───────────┘
 └───────────────┘   └────────────────────┘              │
        │                                                  ▼
        │  render change (hclwrite/kyaml)      ┌────────────────────────────┐
        └─────────────────────────────────────▶│ 4 CHOKEPOINT Interceptor.Do│
                                               │  gate 2 floor / 4b territory│
   3 VCS ACTUATOR (gitopsmr) opens MR ◀────────│  4c verifiability           │
   = Commits.CreateCommit(branch+actions[])    │  4d PLAN-GATE (new)         │
     then MergeRequests.CreateMergeRequest     │  Exec = open MR             │
                                               └────────────┬───────────────┘
        Atlantis autoplans on MR-open,                      │
        posts plan comment                                  ▼
              │                            ┌──────────────────────────────────┐
              ▼                            │ AUTONOMY TOGGLE (§5)              │
   3 VCS SENSOR reads plan + CI status ───▶│  assisted → 7 APPROVAL PLANE     │
   (reassemble Atlantis comments,          │            (human merges MR)     │
    atlantis/plan commit status)           │  autonomous → plan-gate IS the   │
              │                            │   reviewer (reversible/non-crit)  │
              ▼                            └───────────────┬──────────────────┘
      PLAN-GATE compares realized                          │ human merge (or, later, gated auto-merge)
      change-set ⊆ committed prediction                    ▼
              │                                    Atlantis apply on merge
              ▼                                            │
   proposal verdict {open_ok|open_failed} ◀─── Exec-return │  (minutes → days, human-gated)
   (NEVER "merged"; success = awaiting human)              ▼
              │                              6 DEFERRED VERIFY (Temporal timer/signal)
              └─────────────────────────────▶ re-observe live state → ComputeVerdict
                                              → terminal MATCH/PARTIAL/DEVIATION (INV-10)
                                                            │
                                                            ▼
                                              6 SCORE — GET /v1/grounding, kind='deploy'
                                              SignalRatio vs shuffled control (INV-22)
```

**The five subsystems and how they compose:**

1. **Regime registry** (§2, `core/regime`) — config-not-code. Resolves each target to a *plane*
   (`atlantis-tofu | argocd-app | runtime | unmanaged`), a *regime* (`gitops-mr | runtime-direct |
   escalate`), a repo/ref/path, a typed `FieldAddress`, an api-scoped token ref, and an autonomy
   level. Resolved **at plan time from the estate/territory model, never from the alert**. Fails
   **closed** on an unplaceable `(target,field)` — mirroring `territory.Permit`.
2. **VCS sensor + actuator** (§3, `adapters/vcs` + `modules/ingest/gitlab` + `modules/actuation/
   gitopsmr`) — a provider-abstracted contract (GitLab v1). The sensor reads MR/plan/CI state
   inbound; the `gitopsmr` actuator opens the MR outbound (exactly two REST calls, propose-never-
   apply). Both traverse the existing rails: sensor on the ingest surface, actuator behind
   `Interceptor.Do`.
3. **Chokepoint evolution** (§4, `core/actuate/interceptor.go`) — the one hard architectural change:
   a **PlanActuator** type-assertion (gate 4d) between `:226` and `:228`, and a **deferred-verify**
   split of `Do` at `:251`. Preserves INV-21 (single chokepoint), INV-10 (verifier is sole writer),
   INV-09 (floor).
4. **Autonomy toggle** (§5, `core/risk`) — assisted (POLL_PAUSE → approval plane) vs autonomous
   (plan-gate substitutes for the human vote, scoped to reversible/non-critical). Always-escalate
   cases enumerated.
5. **Blast-radius reasoner** (§6, `core/deploy` + `modules/vcs/tfplan`) + **approval plane / chatops
   bridge** (§7, `adapters/approval` + `modules/approval/gitlab`) — light up the dark InfraGraph
   back-half (`infragraph_prediction=0` today) by treating a plan as a Prediction, and wire the
   already-built vote-spine to where humans already are (the MR itself in v1).

The loop is a **strict superset** of today's remediation loop: `ComputeVerdict` is reused verbatim in
*three* roles (plan-time diff, deferred settle-verify, and the existing synchronous verify), fed a
different `[]verify.ObservedAlert` source each time because it is provenance-blind.

---

## 2. REGIME REGISTRY (config-not-code)

A new package `core/regime` — a **console-native, config-not-code** loader modelled on the existing
criticality-tier / canary-pin / proxmox-allowlist loaders (LAW-clamped resolver, #27). It is **read**
by the plan-time router; it holds no per-request state.

### 2.1 The row

Per-target rows resolve, most-specific-selector-wins:

| field | type | meaning |
|---|---|---|
| `Selector` | glob/typed | matches an estate node (site/cluster/namespace/name), most-specific wins |
| `Plane` | enum | `atlantis-tofu` \| `argocd-app` \| `runtime` \| `unmanaged` |
| `Regime` | enum | `gitops-mr` \| `runtime-direct` \| `escalate` |
| `Repo` / `Ref` / `Path` | string | the Git target: project path, tracked branch/`targetRevision`, file path |
| `Reconciler` | enum | `atlantis` \| `argocd` \| `none` |
| `FieldAddress` | typed | a **locate rule** — how to find the field in the file (see §3.4) |
| `ChangeMechanism` | enum | `hcl-attr` \| `helm-release-set` \| `kyaml-manifest` \| `kyaml-values` |
| `TokenRef` | `config.SecretRef` | the `api`-scoped credential (env:/file:, never literal) |
| `AutonomyLevel` | enum | `assisted` \| `autonomous` (see §5); floor-clamped |

### 2.2 Resolution contract

```
Registry.Resolve(manifest.Action) → (Target, Regime, error)
```

- Resolved **from the estate/territory model at plan time, NEVER from the alert** — the alert names a
  symptom; the *target node* determines its management regime.
- **Most-specific selector wins** (a namespace row beats a cluster row beats a site row).
- **Fail closed** (mirror `territory.go:113`, the `Permit` BLOCK arm): a declarative `(target,field)` the registry cannot place
  into a `FieldRule` is **refused** — `Resolve` returns the same shape of block as `territory.Permit`'s
  *"confirmed infrastructure write the territory gate cannot place — fail closed."*
- The router composes **after** the territory gate (4b): the K8s territory manual must already be
  acknowledged this session before a regime is even consulted.

### 2.3 Provenance — auto-discover vs config

- **`argocd-app` rows** are read-only auto-discoverable from Application CRs (`repoURL`, `path`,
  `targetRevision`, `syncPolicy`) under mutation-OFF — surfaced as **PROPOSED** rows an operator
  confirms, **never silently authoritative**.
- **`atlantis-tofu` rows** are only *partially* discoverable: `atlantis.yaml` gives projects/dirs, but
  auth + approver identity + `apply_requirements` live server-side in `repos.yaml` which TG cannot read
  → **Atlantis targets require explicit operator config** in v1.
- Recommendation: **explicit operator config in v1**; auto-discovery only as proposed rows.

---

## 3. VCS SENSOR + ACTUATOR COMPONENT

Not a new surface. The component is: (a) an **ingest** module for the sensor half, (b) an **actuation**
module for the write half, (c) one provider-agnostic value package `adapters/vcs`, and (d) two net-new
orchestration seams (a continuation-signal router on the ingest front door, and a VCS saga workflow).

### 3.1 The provider-abstracted contract

`adapters/vcs/vcs.go` owns the **normalized enums** because no two providers agree:

```
CRState   = open | merged | closed        // GitLab 'merged' is first-class; GitHub/Gitea derive it
Mergeable = yes | conflict | blocked | unknown
CIState   = ...
WebhookKind = ...
Directive = deny | approve | investigate   // tri-state (see §7)
```

Each provider adapter **folds its native vocab into TG's enums**, mirroring `tracker.State` folding in
`modules/tracker/github-issues`. **Trap:** reading only `state` on GitHub/Gitea silently misclassifies
every *merged* PR as *closed* — the fold table must compute merged as `state==closed && merged==true`.

Interfaces: `Sensor` (read), `WebhookNormalizer` (webhook → envelope), `Writer` (branch/MR/comment/
merge). v1 ships **GitLab + Atlantis** only; GitHub/Gitea/Argo are drop-in later impls (new fold tables
+ a `status` / `argocd app wait` reader).

### 3.2 The `gitopsmr` actuator — propose, never apply

`modules/actuation/gitopsmr/gitopsmr.go` implements `adapters/actuation.Actuator`:

- `Capability() = "gitopsmr"` (the VCS brief uses `"vcs.gitopsmr"`; **pick one slug and register it
  once** — see §10).
- `ReadOnly()` gated by the process `safety.MutationGate` (true until the flip) — **the whole
  `modules/actuation/proxmox` template**: injected fake GitLab `Doer` for tests, `WithMutation(gate,
  tokenRef, allowedRepos)`, and `ExecRecorder`.
- **`Exec` opens the MR — exactly TWO native-Go REST calls, then STOP:**
  1. `Commits.CreateCommit(StartBranch=<new tg/ branch>, Actions[])` — atomically creates the branch
     **and** the multi-file commit in **one** call.
  2. `MergeRequests.CreateMergeRequest`.
  - **NEVER** the Repository Files API (one-file / non-atomic per the official docs), **NEVER**
    `atlantis apply`, **NEVER** merge. Atlantis autoplans on MR-open and posts the plan; apply is a
    human comment (or, later, a gated merge — see §5/§8).
- **`ExecRecorder.ExecLog`** — the inverse is **MR-state-dependent** (§3.6).

Precedent: the actuator is **HTTP, not an OS command** — `modules/actuation/proxmox` (native-HTTP),
not the ssh argv actuator. It still traverses the full `Interceptor.Do` gauntlet (INV-21).

### 3.3 Structured file editors (dispatched by file type)

Regex/`sed` editing is **rejected outright**. `render_hcl.go` + `render_yaml.go` sit behind a
`FieldRule`-dispatched `Render(FieldRule, oldTree, newValue) → CommitAction`:

- **`*.tf` → hclwrite** (`ParseConfig → FirstMatchingBlock → GetAttribute → SetAttributeValue →
  Bytes`; token-level, preserves comments/order/indent).
- **`*.yaml` manifests + helm `values.yaml` → kyaml RNode** (`Lookup`/`LookupCreate` + `FieldSetter` +
  `ElementMatcher` to pick the right container). **Avoid `sigs.k8s.io/yaml`** (JSON round-trip drops
  comments / reorders).
- **`helm_release` `set` inside `.tf` → hclwrite with DOUBLY-ESCAPED `set.name`** (`a\\.b\\.c`; a plain
  YAML path targets the wrong key).
- **Diff-minimality IS the review contract**: allowlist to in-place-updatable fields
  (`replicas`/`image`). **ForceNew** fields that plan as `-/+` route to a higher-approval regime, not
  v1.

### 3.4 `FieldAddress` / `FieldRule`

A typed locate rule carried on the registry row: `{file, blockType, blockLabels, attrPath, listMatch}`.
The renderer dispatches on `ChangeMechanism`. An address the renderer cannot resolve to exactly one
field → **refuse** (same fail-closed direction as the registry).

### 3.5 Registration

- **Sensor** on the TG **ingest** surface: `modules/ingest/gitlab` implements `adapters/ingest.Ingester`
  (`SourceType()="gitlab"`, `Normalize` → `vcs.WebhookNormalizer`). Registered
  `RegisterConfiguredIngest(gitlab, enabled=true)` — **live in Phase 0/1** (read is always safe).
- **Actuator** on the **actuation** surface: `gitopsmr` added to `actuationModules` via
  `RegisterActuationDisabled` — **ships DISABLED**, inert until the flip.
- **The sensor MUST NOT import the writer.** They are two non-colliding registry rows (INV-18,
  one-impl-per-(surface,sourceType)).

### 3.6 Inverse (MR-state-dependent)

- **At open time** `ExecLog` records `forward=[open-mr, repo, branch, iid]` /
  `rollback=[close-mr, repo, iid]` — an unmerged MR is a **pure proposal** with a trivial undo (close
  MR + delete branch). **Never a counter-mutation.**
- **After human merge** the inverse is a **FRESH governed `git-revert` MR** (a separate `RevertMR`
  capability), itself human-gated through the full chokepoint — never an auto-executed compensator.

### 3.7 The GitLab token — hard requirements

- **MUST be `api`-scoped.** Prefer a **group access token** (one credential across the GitOps group;
  new repos auto-covered) **or** a named **service account** (stable, human-readable MR author).
- **`write_repository` is INSUFFICIENT** (Git-over-HTTP only, no REST auth) and **MUST be rejected by
  the registry.**
- **MUST NOT be `CI_JOB_TOKEN`** (the recursion guard silently suppresses the MR pipeline).
- The target repo needs `merge_request_event` rules **directly** in `.gitlab-ci.yml` (per-job `rules:`
  or top-level `workflow:`), **not only via `include:`** — otherwise the MR pipeline silently won't run.

### 3.8 Structural anti-self-merge — server-side, none TG-authored

Gates compose **in series**, and TG authors **none** of the policy:

```
TG never-auto floor (adapter, gate 2)
  → TG territory gate (4b)
  → TG regime router (§4, semantic op-class re-check)
  → GitLab SERVER-SIDE anti-self-merge  ← the crux
  → reconciler CI + admission (free)
```

The crux is `author_approval=false` server-side: **the token identity is the MR author**, so if the
author could approve their own MR the whole human gate collapses. One-time per-repo provisioning (an
operator runbook, §9): protected branches excluding the bot from `allowed_to_merge`; approvals with
`author_approval=false` + `disable_committers_approval=true` + `disable_overriding=true`; approval
rules naming ≥1 human approver (**Premium/Ultimate only** — confirm tier, §10); "Pipelines must
succeed" + "All threads resolved" merge checks.

---

## 4. CHOKEPOINT EVOLUTION — the one hard architectural change

Two additive, opt-in-by-assertion mechanisms so the single chokepoint stays single (INV-21) and
`ComputeVerdict` stays the sole verdict writer (INV-10). Both are **type-asserted exactly like the
existing `ExecRecorder`** (`interceptor.go:237`) — no new mandatory field on `Actuator`.

### 4.A The PLAN-GATE (gate 4d), between `:226` and `:228`

**New optional capability**, asserted like `ExecRecorder`:

```go
// core/actuate/interceptor.go (near the ExecRecorder type, ~:99)
type PlanActuator interface {
    Plan(ctx context.Context, argv []string, stdin []byte) (PlanArtifact, error)
}
type PlanArtifact struct {
    Changes []verify.ObservedAlert // realized change-set projected into estate-canonical (Host,Rule,Site)
    Empty   bool
}
```

**New `Request` fields** (`:51`): `RequirePlan bool`, `DeferVerify bool` — threaded from
`ExecuteActivity`.

**Insert gate 4d between the verifiability gate (4c, `:224-226`) and the Exec chokepoint (`:228`):**

```
// 4d. PLAN-GATE — a pre-commit dry-run diff against the committed prediction.
if r.RequirePlan {
    pa, ok := i.actuator.(PlanActuator)
    if !ok { return refuse("plan required but actuator cannot dry-run") }   // fail closed
    art, err := pa.Plan(ctx, r.Argv, r.Stdin)
    if err != nil { return refuse("plan failed: " + err.Error()) }
    planVerdict := verify.ComputeVerdict(r.Prediction, art.Changes)          // REUSED verbatim
    i.record("plan:"+string(planVerdict), actionID, "plan-gate diff", true)  // WITHHELD — never Commit'd
    if planVerdict == safety.VerdictDeviation {
        return refuse("plan-gate deviation — realized change touches a host outside the committed blast radius")
    }
}
```

- Placed **after** gate 2 (floor) and gate 4b (territory) so **no dry-run I/O runs for a floored or
  blocked op**.
- A plan-time **deviation** (a planned change to a host outside `PredictedHosts` = surprise
  blast-radius, caught **pre-commit**) is a **recorded refusal** via the existing `refuse` closure
  (`:171`) — **never a Go error** (INV-19).
- The plan verdict is **ledger-recorded (withheld) but NEVER `Commit`'d to the VerdictSink** — the
  INV-10 single-writer, one-verdict-per-`action_id` contract is untouched. Only the *deferred effect
  verdict* (§4.B) writes to the sink.
- **Regime-router semantic re-check (exclusion #4):** before rendering, the regime router computes the
  **semantic op-class of the PROPOSED change** and runs `safety.IsNeverAuto` on **that** — so a
  floor-class mutation cannot be laundered as a "revertable git edit". This runs alongside the existing
  gate-2 floor, which already re-derives destructiveness from the actual command
  (`IsDestructiveOp`).

### 4.B DEFERRED VERIFY — split `Do` at `:251`

A `gitopsmr` `Exec` returns when the MR is **OPEN**. The real effect (merge → apply/sync →
convergence) happens **minutes-to-days later, human-gated**. So the synchronous cascade verdict is
**uncomputable at Exec-return** and would degenerate to a hollow `match` (the exact "verifier theater"
gate 4c forbids). We split verification into two epistemically-different verdicts.

**(a) OPEN-time synchronous PROPOSAL check** — a **new** `ProposalVerdict` domain in a **new sibling
file** `core/verify/proposal.go` (do **NOT** overload `safety.Verdict`):

```
ProposalVerdict = open_ok | open_failed
open_ok  ⟺  MR opened-not-merged
         ∧ merge_request_event pipeline running/passed
         ∧ approvals_left > 0
         ∧ NOT merged        // success is "awaiting human", NEVER "merged"
```

**(b) DELAYED out-of-band EFFECT verdict** — the **existing** `verify.ComputeVerdict` re-run against
**independently re-observed live state** days later, keyed by `action_id`. `ComputeVerdict` is
**unchanged and reused** ("Apply complete" ≠ live convergence).

**The split at `interceptor.go:251`:**

- The **synchronous half keeps `:227-246`** (Exec `:228` = open the MR, ExecLog `:237` = record the
  close-MR inverse, StageExecuted `:246` = record the MR ref).
- For a deferred actuator: **SKIP the synchronous terminal `:251-252`** (`observed := r.Observe(ctx);
  verdict := ComputeVerdict(...)`). Instead compute + persist the `ProposalVerdict`, write an
  `awaiting_effect` state, and **enqueue the delayed job keyed on `action_id`**.
- The tail `:251-284` (observe → ComputeVerdict → StageVerified → VerifyChain → VerdictSink.Commit →
  audit → breaker) **lifts into a new verify-only `Interceptor.Settle(ctx, SettleRequest{...})`**,
  invoked later by a **new Temporal `SettleAndVerifyActivity`**. `Settle` contains **NO
  `i.actuator.Exec`** — execute-at-most-once holds.
- **Breaker**: for the deferred path the `MutationBreaker` trips on `ProposalVerdict == open_failed`
  (there is no open-time cascade deviation to trip on), and later on the delayed `VerdictDeviation`
  from `Settle`.

**Satisfying gate 4c without fail-open theater.** Gate 4c today refuses `if r.Observe == nil`. For a
deferred actuator we **do not** supply a nil/empty observer (that would be the exact verifier-theater
the gate forbids). The naive shape — *"if `r.DeferVerify` is true, skip the `Observe==nil` refusal"* —
is **REJECTED**: `DeferVerify` is a caller-set `Request` bool, and letting a bool route around 4c is the
precise **"a flag lifts the gate"** anti-pattern the gate-2 floor comment forbids (`interceptor.go:186-191`,
*"No flag lifts this"*). A mis-set `DeferVerify=true` with nothing wired would then execute (open the MR)
with **no** verification ever — fail-OPEN.

So the mechanical rule is: `DeferVerify` alone **NEVER** satisfies 4c. The interceptor must carry a
**wired, non-nil deferral collaborator** — a new `DeferralSink` (settle-enqueue) field checked exactly
like `SelfTest` checks `ledger`/`gate`/`actuator`. Gate 4c becomes:

```
// 4c (deferred variant)
if r.DeferVerify {
    if i.deferral == nil { return refuse("deferred-verify requested but no settle sink wired — cannot verify ⇒ will not execute") } // fail closed, same direction as the nil-observer refusal
} else if r.Observe == nil {
    return refuse("unverifiable — no post-execution observer wired")
}
```

`SelfTest` is likewise extended so a boot that enables any `DeferVerify`-capable actuator **without** the
`DeferralSink` fails loud at preflight. Thus 4c is satisfied by a **mandatory** pair — `ProposalOutcome()`
at open-time **AND** a wired settle path proven non-nil — and a deferred actuator with no wired
delayed-verify path is **refused**, same fail direction as the original nil-observer refusal.

### 4.C How the invariants are preserved (diff-level)

- **INV-21 (single chokepoint):** both gate 4d and the deferred split live **inside `Interceptor.Do`**
  / its extracted `Settle`. No second execution path is created; `Settle` cannot `Exec`. The actuator
  stays unexported.
- **INV-10 (verifier is sole verdict writer, one per `action_id`):** `ComputeVerdict` is reused
  verbatim in all three roles. The **plan verdict is withheld** (never `Commit`'d). The **proposal
  verdict** is a **separate domain** (`core/verify/proposal.go`), not a `safety.Verdict`, and is not
  written to the `VerdictSink`. The single durable `VerdictSink.Commit` fires **exactly once**, later,
  from `Settle`.
- **INV-09 (never-auto floor):** gate 2 is untouched; the regime router adds a **redundant** semantic-
  op-class `IsNeverAuto` check **before** rendering. A floored op is refused at gate 2 with **zero**
  `Plan()` calls (the plan-gate sits after the floor).
- **Pending state carries NO schema change:** `safety.VerdictPending` is added but **excluded from
  `ValidVerdict`** and non-`AutoResolvable`, so `VerdictStore.Commit` **rejects** it and the
  reconciler treats it as deviation-or-invalid (fail-closed). The durable single `INSERT ... ON
  CONFLICT (action_id) DO NOTHING` just fires later at settlement.
- **Reconciler holds automatically:** `core/reconcile/reconciler.go:116` already holds an executed
  action lacking a clean `match` verdict (`if s.Executed && !(s.HasVerdict && s.Verdict==Match) { hold
  → To Verify }`). **No code change** — the delayed effect-verdict flips `HasVerdict` later; the
  orphaned-poll re-check (`ScheduleReCheck`) naturally drives the merge/apply poll.
- **Durable breaker is a BLOCKING prerequisite:** swap `breaker.NewMemStore()` (`cmd/worker/
  main.go:999`) → the **pgx `breaker.Store`** so a deferred deviation trip (fired hours later, possibly
  across a worker restart) survives and `gate.Disable` is not lost.
- **Timer is authoritative, reconcile-signal is early-wake ONLY:** a `settleFor(opClass)` durable timer
  guarantees every pending resolves; the `ReconcileSignalName="reconcile-observed"` signal carries
  **only** the sealed `action_id` as a trigger — the verdict is **always** computed from trusted
  `Deps.PostStateObserve`, never from signal-carried data (INV-10). Misbound action_ids counted-only,
  `maxMisbound=64` (mirrors the vote-wait guard).

### 4.D Naming collision to avoid

The existing `RecordPending`/`ResolvePending`/`pending_decision` machinery is the **console POLL_PAUSE
approvals projection (REQ-519)** — a *different* concept. The verdict-pending state gets **NO new
table** and **MUST NOT** reuse that machinery.

---

## 5. AUTONOMY TOGGLE

The toggle **never touches the vote path** and **never manufactures `Approved=true`**. It is a
per-`(op-class × management-regime)` config value flowing through the LAW-clamped resolver (#27), and
it can only **TIGHTEN**, never loosen, the fail-closed floors.

- **ASSISTED (default):** one **RAISE-ONLY** classifier clamp forcing `POLL_PAUSE`. The human approves
  via the existing vote loop — in the declarative regime, **that vote IS the MR approval / merge** (§7).
  Added as `GatedInput.AssistedMode` + a clamp in `core/risk/classifier.go` after the mechanical
  floors, before the AUTO branches: `if in.AssistedMode { return poll(d, "assisted-mode-human-approval-
  required") }`. Most-restrictive-first ordering intact.
- **AUTONOMOUS:** **no** clamp — the classifier's own `AUTO`/`AUTO_NOTICE` band stands and self-applies,
  but `RequirePlan=true` makes the **plan-gate the MANDATORY reviewer** replacing the human notify. The
  toggle never lifts a POLL_PAUSE to AUTO; it only lets an *already-AUTO* band self-apply through a
  green plan-gate.
- **`POLL_PAUSE` stays human-only, always.** The rejected Design-1 variant ("plan-gate substitutes for
  the human vote on a would-be-POLL_PAUSE action") erodes INV-12 + the 24h deny-by-default and needs
  its own separate sign-off (§10, blocking-ish).

**Reversible/non-critical scoping is enforced TWICE:**
1. **Mechanically** by the classifier's most-restrictive-first ordering — an action reaches
   `AUTO`/`AUTO_NOTICE` only if it is not-floor, reversible, not server-destructive, not stateful, not
   a self-protected restart, has a committed prediction, and has no deviation; a P0/criticality host
   **ceilings at `AUTO_NOTICE`**.
2. **Redundantly** by `AutonomyMode` returning `false` for floor/irreversible/critical ops; the
   interceptor gate-2 floor + territory gate backstop a mis-scoped grant.

**ALWAYS-escalate (never autonomous, regardless of toggle):**
- Any never-auto floor op-class (destroy/delete/drop/reboot/…), including one re-derived from the
  actual command or the **proposed change's semantic op-class**.
- Secret **VALUE** changes (§8) — escalate, never an MR edit.
- A **ForceNew** plan (`-/+` replace) — routes to a higher-approval regime, not v1.
- Any target the regime registry cannot place (fail-closed).
- Any op on a stateful workload (`IsStatefulWorkload`), a self-protected control-plane restart, or a
  criticality-P0 host.
- **Empty-graph inertness — the ACTUAL guarantee is mutation-OFF, not the empty graph.** The empty
  graph escalates only **multi-host blast radius**: a plan touching a resource whose blast radius the
  empty graph never named reads as a surprise → deviation → escalate. But a **single-target reversible
  change** produces **no non-target plan row**, so `ComputeVerdict` finds no surprise host → MATCH → the
  plan-gate **PASSES**, and an AUTO-band single-target change would self-apply on that basis alone. So do
  **not** rely on the empty graph for safety: the hard, unconditional inertness is **gate-1 mutation-OFF**
  (`interceptor.go:177`), which refuses every mutation before any plan or exec runs. The empty graph only
  weakens the plan-gate's *blast-radius* discrimination — which is exactly why **autonomy MUST be gated
  on real estate data landing first** (ties to the infragraph-audit backlog), independently of the
  mutation flip.

**Plan-gate and deferred-verify are PEERS**, each primary for a different actuator class:
`ssh-restart` has no meaningful dry-run → skips gate 4d, relies on deferred-verify (short settle
window); `gitopsmr`/tofu → plan-gate is native + mandatory (`tofu plan` **IS** the dry-run) with a long
window / reconcile signal when Atlantis apply lands.

---

## 6. BLAST-RADIUS REASONER — light up the dark predict→verify→score back-half

Today `infragraph_prediction=0` in prod: the reasoning layer (predict→control→verify→score) is
unexercised. This subsystem applies the **existing** InfraGraph spine to a **commit+push/plan** instead
of an alert — new packages `core/deploy` (reasoner) + `modules/vcs/tfplan` (plan ingest); everything
downstream of a `Prediction` is **reuse**.

### 6.1 THE ONE INVARIANT — never collapse two epistemically-different inputs

- **IaC plan** (`tofu show -json` `resource_changes`) = **HIGH-confidence** authoritative provider
  delta. Gate-worthy; can block an autonomous apply.
- **App-code impact set** (changed paths → service-map + code-graph reverse-reachability) =
  **LOW-confidence** static hypothesis (~12% edge under-approximation; structurally blind to dynamic
  dispatch/DI/feature-flags/SQL-strings/cross-service+queue edges/DB migrations). **Advisory ONLY**;
  MUST NOT auto-gate an apply.
- Bands are **stored + rendered separately and labelled, never averaged.** v1 ships the **IaC path
  only**; the app-code band is designed but **deferred to v2**.

### 6.2 Plan → estate node (the critical correctness trap)

A tofu address (`module.x.kubernetes_manifest.y`) is the **provider handle, NOT the InfraGraph key**
`(Type, canonName(Name))`. A string-join off the address silently matches **ZERO** nodes. So
per-provider **extractors** (`modules/vcs/tfplan/extractors.go`) read `change.after`/`before` to
recover the real entity name (`kubernetes_manifest → after.manifest.metadata.{ns,name,kind}`;
`helm_release → name+namespace`; `proxmox_vm_qemu → name/vmid/target_node`; `netbox_*`/`dns → name`)
then call **`estate.Graph.Resolve`**. An **unresolved** name → **`UnknownBlastRadius` escalation
signal, NEVER scored zero.**

### 6.3 PREDICT = set-fold (reuse `core/estate`)

For every changed node, union **`estate.BlastRadius`** (edges INTO the node = who-depends-on-me,
exactly right for who-breaks-if-I-destroy-this) + **`Siblings`**, then seed the **changed nodes
themselves PLUS that union** into the **existing `PredictedHosts` set** of the **same `verify.Prediction`
struct** — this is what makes the whole affected set "named". The action's scalar `TargetHost`
(`verdict.go:23`, consumed at `:72`) is left **empty** for a multi-resource plan (an empty `TargetHost`
excludes nothing extra and widens nothing — the fail direction stays safe, see the `verdict.go:68`
note) or set to the plan's single primary resource. Expected post-state rides the **existing
`PredictedRules` map** as an assertion channel (`RuleKey(host, "state:replicas=3")`) — so
`ComputeVerdict` is **reused VERBATIM** (no struct change, no edit to `verdict.go:72`): a
changed/alerting node the blast radius never named is a **surprise host → deviation** (`verdict.go:83-84`),
a predicted node carrying an unpredicted rule → **partial** (`verdict.go:86-87`) — exactly the §6.4
semantics. (Note the discarded alternative: putting the affected set into the *TargetHost exclusion*
would EXCLUDE those hosts, so an unpredicted alert on an affected node could never surface as PARTIAL —
it must go in `PredictedHosts`, not the exclusion.) **`ShuffledControl` seeded from `plan_hash`** gives
the degree-preserving null → INV-22 falsifiability for free.

Risk tier is a **pure function of `change.actions`**: `[]`/read → t0; `[create]` → t1 additive;
`[update]` → t2 in-place; `[create,delete]` → t3 create-before-destroy (no availability gap);
`[delete,create]` → t4 destroy-then-create; `[delete]`/`[forget]` → t4. High-blast predicate =
*actions contains `delete`*. **Action ORDER is semantic** (t3 is materially safer than t4 and must be
tiered separately). `replace_paths` + `action_reason` are the explainability payload into the
`ActionManifest` / approval rationale.

### 6.4 Deploy verdict semantics (re-authored for intended-set)

`core/deploy/verdict.go` **wraps** `core/verify` (the DEVIATION-never-auto floor reused verbatim):
- **MATCH** = every affected node's observed post-state == predicted AND no alert on a node outside the
  predicted set.
- **PARTIAL** = affected node reached predicted state but fired an unpredicted transient alert.
- **DEVIATION** = affected node whose post-state diverged, OR a changed/alerting node the blast radius
  never named.
The whole-affected-set-intended semantics are expressed by seeding the affected set into
`pred.PredictedHosts` (NOT the target-host exclusion — see §6.3), so an unpredicted alert on an
affected node surfaces as PARTIAL and a never-named node surfaces as DEVIATION, verbatim.

### 6.5 TWO gates at two times

- **(a) PLAN-GATE, synchronous, BEFORE apply** (INV-21 pre-exec chokepoint = §4.A): at `plan.posted`
  compare the realized change-set (from `$SHOWFILE`) vs the committed prediction → **REFUSE/ESCALATE**
  on a surprise t4 delete/replace, any resource outside the committed change-set, `UnknownBlastRadius`,
  or `BlastRadiusWide(≥ TG_BLAST_RADIUS_WIDE_THRESHOLD, default 8)` → POLL_PAUSE; **PROCEED only if
  realized ⊆ predicted with no surprise delete/replace/unknown.** This is `ComputeVerdict`'s
  surprise-host-is-DEVIATION floor applied at plan time.
- **(b) DEFERRED VERIFY, async, AFTER reconcile** (= §4.B): monitor keys on **reconcile-settled state**
  (k8s rollout status / tofu state diff / health checks), **NOT apply-return**. Deploy Observer window
  `TG_DEPLOY_FALSIFIABILITY_WINDOW` (default 10m, tunable per regime) is longer than remediation and
  anchored on **reconcile-complete**.

### 6.6 Commit + score

Commit as a **new prediction kind='deploy'** alongside remediation kind='action', via **migration
`0015_prediction_kind_deploy` (`ALTER TYPE prediction_kind ADD VALUE 'deploy'`, enum-value-add only)**.
NB: the `prediction_kind` enum is defined in **`0002_infragraph_prediction.up.sql`** (existing values
`'action'`,`'cascade'`) — NOT in `0003` (which is `audit_spine`); the next free migration index is
`0015` (0001–0014 are taken). `DeployGate` mirrors `PredictionGate.Commit`: persist append-only **BEFORE**
apply, seal an `ActionManifest` binding `plan_hash + prediction_hash`, default-deny. `plan_hash` derives
from **canonical plan content** (`sha256(normalize(resource_changes) sorted by address, stripping
volatile `after_unknown`/timestamps)`) — **not** the remediation lane's `sha256(externalRef+action_id)`.

kind='deploy' rows feed the **same `GET /v1/grounding` GroundingScorecard** (`SignalRatio =
AvgRealTP/max(AvgControlTP,1)`, Precision, Recall, FloorHolds), reported **per-kind** so
deploy-prediction quality is tracked separately from remediation. **Everything runs SHADOW /
measurement-only with mutation OFF**: predictions committed before apply, scored after reconcile, no TG
write access needed. That measured `SignalRatio` is **itself a flip prerequisite** and closes the
`infragraph_prediction=0` gap. An untrusted (`SignalRatio ≤ 1`) deploy reasoner **cannot arm the
plan-gate to block** — plan-gate arming to block is explicitly **v2**.

### 6.7 Known gaps (do not paper over)

- **k8s deploy blast-radius is BLIND today:** no `EdgeSource` emits
  namespace→workload→node / helm-release→resources / service→backing-host edges — only VM/LXC/proxmox
  placement is modeled. A pure-k8s deployment resolves to `UnknownBlastRadius` + escalate (never scored
  zero). Building a k8s/GitOps topology `EdgeSource` is greenfield → **v2**. **v1 works today for
  tofu-managed VM/LXC/proxmox/netbox resources the estate already knows.**
- Only 9 of 13 predecessor node types are ported — confirm no deployment-relevant type was among the 4
  dropped before relying on the type spine (§10).

---

## 7. APPROVAL PLANE + CHATOPS BRIDGE

**Authority stays in TG.** The Temporal vote-wait + hash-chained ledger is the single source of truth.
Every external channel is a **bridge / UX only**: it renders the proposal outbound and relays an
authenticated, `action_id`-bound vote inbound. It **never** holds authority, releases/revokes an action,
writes the ledger, or adds await/timeout logic.

### 7.1 One injection seam

Every external vote terminates at the **existing** signal:
`SignalWorkflow(WorkflowID(external_ref), "approval-vote", VoteSignal{Approve, Voter, ActionID})` via
`temporalVotes.SignalVote` (`cmd/grounder/deps.go`). **Do NOT invent a second signal name/shape.** This
is why the callback MUST supply a matching `action_id` — `workflow.go:320` counts a mismatch as
*misbound* and never releases.

### 7.2 Why the existing notifier is insufficient — and the new package

`adapters/notifier.Notifier.ResolveVote(ctx, raw []byte)` has **no HTTP-header access**, so Slack v0
HMAC / Jira `X-Hub-Signature` / YouTrack `X-YouTrack-Token` / GitLab `X-Gitlab-Token` are
**unverifiable**. So a **NEW `adapters/approval` package** (leave `adapters/notifier` untouched for
page/notice-only backends like twilio-sms):

```
SendApprovalRequest(Proposal) → Posted{Handle, Nonce}
VerifyAndDecodeVote(Inbound{Header, RawBody, ReceivedAt}) → DecodedVote
```

**STRICT two-stage inbound:** `VerifyAndDecodeVote` must (1) verify transport authenticity from the
**full envelope** and return `ErrUnauthentic` **BEFORE trusting ANY body field** (including sender),
then (2) decode + bind. `RawBody` must be the **exact received bytes** (HMAC is over raw bytes), never
re-marshalled.

### 7.3 Authenticated inbound route (none exists today)

Add `rt.Handle("/v1/approval/{source_type}/callback", auth.AuthIngestPush, d.approvalCallbackHandler)`
alongside the existing `/v1/ingest/{source_type}` front door. **Do NOT** route through `POST /v1/vote`
— that route is `AuthSession` (browser-cookie only); machine HMAC/mTLS cannot satisfy it. Provision each
channel a `Source` row with an `IngestToken` (layer-1 channel auth) like Alertmanager.

### 7.4 DecisionID → ActionID bridge (mandatory)

`notifier.Vote` carries `DecisionID (= external_ref)` but **no `action_id`**. The handler must recover
the sealed `action_id` **server-side** by `external_ref` from the `pending_decisions` projection (or an
`approval_bindings` row) before signaling — otherwise `workflow.go:320` silently treats every chat vote
as misbound and never releases. **Add `action_id` to `notifier.Vote`** (and the approval `DecodedVote`)
so the binding is explicit.

### 7.5 Four security gates (all required before a vote reaches the signal)

- **A) Transport authenticity** per-provider over the raw body (reuse `core/auth` Verifier discipline:
  HMAC/token + timestamp window + nonce store + constant-time + fail-closed + secret-refs).
- **B) `action_id` binding (INV-12) + nonce echo**, plus (for YouTrack/Jira) a transition-target =
  `Approved`/`Denied` confirmation via `changedFields`/`changelog`.
- **C) Idempotency** CAS `PENDING → DECIDED` on an `approval_bindings` dedupe key (`core/db/
  approval_bindings.go`, migration `0016` — `0009` is already `skill_store`; DSN-gated round-trip test per the pgx-fake memo).
- **D) Who-approved** captured as `voter = sender@source_type` → `RecordVoteActivity` ledger (INV-19).

### 7.6 v1 = GitLab, and it composes perfectly with §3

The estate is GitOps (OpenTofu→Atlantis), so **the MR is BOTH the approval artifact AND the change
artifact.** The human approves the exact plan/diff (`Proposal.PlanDiff`); **MR approval permissions ARE
the human-authz gate**; `X-Gitlab-Token` is the simplest-correct Gate-A; and it composes with the
`gitopsmr` actuator — **the actuator opens the MR, approval IS the MR approval, merge IS the release** —
avoiding split-brain/drift. Every other provider (Slack/Mattermost/Matrix/YouTrack/Jira) is one
renderer + one Gate-A verifier through the same abstraction, added later.

### 7.7 Tri-state Directive + autonomy-toggle middle setting

`Directive{Deny, Approve, Investigate}` ports the predecessor's valuable 3rd option (resume with deeper
RCA, do NOT act). **v1 decodes `Investigate` but folds it to `Deny` + directive-log** until the runner
resume-with-RCA path is wired; the enum is reserved so no backend is re-authored later. The
autonomy-toggle **middle setting** (`require-approval-via-channel-X`) routes a POLL_PAUSE to the
configured channel; it can only tighten the floors, never loosen them (`approval.route.<op_class>.
<regime> = <source_type>`, LAW-clamped).

Deny-by-default is already guaranteed by `VoteWait=24h` + `maxMisbound=64`; the bridge adds **no**
timeout logic. **Single-enabled-notifier constraint:** the worker resolves exactly ONE enabled
`SurfaceNotifier` (`cmd/worker/main.go:1387`) — the approval plane must **FAIL LOUD** here, not silently
skip. A multi-channel resolver is a follow-on.

---

## 8. HARD RULES (fail closed)

1. **Secret VALUE changes → ESCALATE, NEVER a Git/MR edit.** Under OpenBao (system-of-record) + ESO
   (runtime inject) only **references** live in Git — so an MR editing a value either **leaks a literal
   secret** or is a **no-op**. A **pre-render guard** scans the patch for decoded values and hard-fails.
   Reference/plumbing edits (`remoteRef`, `SecretStore`, `refreshInterval`) **ARE** MR-safe.
2. **Never auto-`atlantis apply`**, and keep **ONE MR per project/dir.** A bare `atlantis apply` fans
   out over **all** planned projects; locking is per dir+workspace.
3. **Never self-merge** — enforced **server-side** (`author_approval=false`; §3.8), the crux because the
   token identity is the MR author.
4. **Floor-class semantic op escalates** even though a git edit is technically revertable — the regime
   router applies `safety.IsNeverAuto` to the **proposed change's op-class** so a plan cannot hide a
   mutation across the MR lane (§4.A).
5. **Controller-owned fields are off-limits.** If an HPA targets the workload, `replicas` is
   runtime-owned → **refuse the MR.** Respect `ignoreDifferences`/`RespectIgnoreDifferences` paths.
   **Never author `atlantis.yaml` / Argo `AppProject` policy files.**
6. **Multi-actor no-fight contract** (Atlantis/Renovate/Argo are co-actors on one Git source of truth):
   - **propose-never-apply** on GitOps targets;
   - **sense-before-write** is a HARD pre-actuation gate — `ListChangeRequests` + diff-intersect the
     path-set; a competitor MR touching the same paths → **back off / escalate to a PAUSE row**;
   - **reserve TG's own branch prefix `tg/`** and **recognize `renovate/*`, Atlantis dirs, Argo app
     paths** (don't fight the reconciler, don't collide);
   - **optimistic-concurrency abort-on-change** (re-verify base SHA + open-MR set at push time,
     abort+re-plan, **never force**);
   - **serialize at the platform's existing point** (Atlantis per-dir lock / merge queue) — don't build
     a bespoke lock;
   - a **closed/reverted MR = durable negative signal** → ignore/pin store keyed `(target,change)`, **no
     auto-re-propose** (avoid the Renovate "immortal PR");
   - **one visible intent surface + human gate until trusted.**
7. **ArgoCD (v2):** commit to the Application CR's **EXACT tracked `targetRevision`** (a wrong ref is a
   **silent no-op**); check `AppProject` `syncWindows` **deny-freeze** before proposing.

---

## 9. V1 SCOPE + BUILD SEQUENCE

**v1 = the GitLab + Atlantis lane, plan-gated, propose-only, mutation OFF throughout.** End-to-end
validation runs against a **SCRATCH repo**, **never the real k8s repo** until the owner provisions the
repo + bot token (§10). Every MR ships **DARK** (inert under mutation OFF) with oracle/fake coverage;
live validation is the staged canary at the Phase-2 flip — and even then the **first** real mutation is
the Proxmox runtime canary, **not** a `gitopsmr` MR.

Ordered smallest-safe-first. The **⚠ chokepoint** flag marks MRs that touch the governed, lockstep'd
`Interceptor.Do` — these get the most adversarial review and byte-identical golden-ledger tests.

| # | Builds | Files | Tests | Chokepoint? |
|---|---|---|---|---|
| **MR-1** | `VerdictPending` const + exclusion from `ValidVerdict` | `core/safety/safety.go` | `Commit(pending)→ErrInvalidVerdict`; `AutoResolvable(pending)==false`; classifier maps pending-input → POLL_PAUSE | no |
| **MR-2** | Durable breaker (BLOCKING prereq) — **BUILD** a pgx impl of the existing `core/breaker.Store` interface (only `MemStore` exists today) + its table migration (`0017_breaker_trips`), then `cmd/worker/main.go:999` swap `breaker.NewMemStore()` → the pgx store | trip persists across a simulated store reload; **DSN-gated round-trip on the REAL SQL** (pgx-fake memo) | no |
| **MR-3** | `ProposalVerdict` domain + open-time assertion helper | **new** `core/verify/proposal.go` | `open_ok` only when created+CI-present+`approvals_left>0`+not-merged; `open_failed` otherwise | no |
| **MR-4** | Regime registry | **new** `core/regime/regime.go` (+ console-native loader) | most-specific selector wins; unplaceable field fails closed; floor-class semantic op → escalate; HPA-owned `replicas` refused; `write_repository`-only token rejected | no |
| **MR-5** | `adapters/vcs` value types + TG-owned enums + Sensor/Writer/Normalizer interfaces | **new** `adapters/vcs/vcs.go` | enum-fold table tests (GitLab `state`→`CRState`, `detailed_merge_status`→`Mergeable`; GitHub merged-not-closed trap) | no |
| **MR-6** | GitLab **sensor** (ingest) + webhook normalizer + Atlantis plan reassembly/parser | `modules/ingest/gitlab/{gitlab,webhook,sensor,atlantis}.go`; `RegisterConfiguredIngest(gitlab, enabled)` | canned webhook + signature verify; split-comment reassembly; plan-text parse (`Plan:`/`No changes.`/`Error:`) | no |
| **MR-7** | Structured file editors | **new** `modules/actuation/gitopsmr/{render_hcl,render_yaml}.go` | single-line diff; comments/order preserved; doubly-escaped helm `set.name`; reject a change that would parse-break or exceed the intended field | no |
| **MR-8** | `gitopsmr` **actuator** (DISABLED) + writer + ExecRecorder | `modules/actuation/gitopsmr/{gitopsmr,writer}.go`; `bootstrap.go` `RegisterActuationDisabled` | fake GitLab `Doer`: **exactly two** REST calls; atomic branch+multi-file commit via `actions[]` (never Files API); MR opened-not-merged; refuses under gate-off; ExecLog forward/inverse | no |
| **MR-9 ⚠** | **PLAN-GATE (gate 4d)** + `PlanActuator`/`PlanArtifact` + `Request.RequirePlan` | `core/actuate/interceptor.go` | oracle plan==prediction → passes to Exec; surprise-host plan → **recorded refusal** (assert ledger row, assert **NO** VerdictSink write); `RequirePlan` + no `PlanActuator` → refuse; floor op → refused at gate 2 with **ZERO** `Plan()` calls | **YES** |
| **MR-10 ⚠** | **Deferred-verify split** of `Do` at `:251` + new `Interceptor.Settle` + `Request.DeferVerify` | `core/actuate/interceptor.go` | `DeferVerify=false` **byte-identical** to today (golden ledger); `DeferVerify=true` returns `Executed`+`VerdictPending`, **no Commit/no StageVerified**; `Settle` rebuilds executed→verified, `Commit` fires **once**, `Settle` Exec-count==0; deferred deviation trips breaker→`gate.Disable`; fail-closed when no delayed-verify path | **YES** |
| **MR-11** | `SettleAndVerifyActivity` + `settleFor(opClass)` + `ReconcileSignalName` + register.go | `temporal/runner/{workflow.go:418→420, activities.go, register.go}` under `GetVersion("settle-verify")` | `register_test.go` covers the new activity; env with `settle>0` parks then resolves the durable verdict; `settleFor` determinism; **replay test** against a pre-change history | no (workflow) |
| **MR-12** | Reconcile signal (early-wake) + `PlanSignaler`/`ReconcileSignaler` + `SignalReconcile` caller | `temporal/runner`, `cmd/grounder/deps.go` | signal wakes early; misbound `action_id` counted-only; verdict computed from `PostStateObserve` even when the signal carries junk | no |
| **MR-13** | Blast-radius reasoner: tofu plan parser + per-provider extractors | **new** `modules/vcs/tfplan/{plan,parse,extractors}.go` | create/update/delete/`[delete,create]`/`[create,delete]`/forget/no-op tiers; `replace_paths`/`action_reason`; golden extractor fixtures; `UnknownBlastRadius` on unresolved | no |
| **MR-14** | `DeployModel.PredictPlan` (set-valued, seeds `PredictedHosts`) + `DeployGate` + migration `0015` + deploy verdict | **new** `core/deploy/{predict,gate,plangate,verdict}.go`; `core/db/migrations/0015_prediction_kind_deploy.up.sql` (`ADD VALUE 'deploy'`; NB enum is in `0002`) | union/dedup; empty-change fail-closed; control-set determinism; commit-before-apply ordering; append-only first-wins; all three deploy verdicts + DEVIATION-never-auto floor; plan-gate subset/surprise/wide table | no |
| **MR-15** | Deploy Observer (reconcile-settled) + Scorer cron wiring + config | `modules/vcs/observe.go`; `cmd/worker/main.go` (`TG_DEPLOY_FALSIFIABILITY_WINDOW`, `TG_BLAST_RADIUS_WIDE_THRESHOLD`) | reconcile-window anchoring; quiet-state = real observation not nil; armed-only-when-wired inert path | no |
| **MR-16** | Autonomy toggle: `LoadAutonomyScope` + `AutonomyMode` predicate + `GatedInput.AssistedMode` + classifier clamp | `core/risk/{input,classifier,policy}.go`; `temporal/runner` threads `RequirePlan`/`DeferVerify`/`AssistedMode` | malformed scope → boot hard-fail; default→assisted→POLL_PAUSE; autonomous reversible/non-critical keeps AUTO + sets `RequirePlan`; autonomous floor op still POLL_PAUSE (clamp can't lower a band); glob-scoping table | no |
| **MR-17** | Approval plane: `adapters/approval` + GitLab channel + `approval_bindings` + callback route | **new** `adapters/approval/approval.go`, `modules/approval/gitlab/{gitlab,verify}.go`, `core/db/approval_bindings.go` (+ migration `0016`), `core/httpapi/approval_callback.go`, route + resolver wiring; add `action_id` to `notifier.Vote` | interface-obligation oracle (redact-before-send, two-stage verify, `ErrUnauthentic`-before-any-field); wrong-token/replay/wrong-action/nonce reject; unauthentic→401, closed-window→409, happy→202; **DSN-gated** binding round-trip | no |
| **MR-18** | Per-actuator `PlanActuator` impls + `planToObserved` projectors | `adapters/actuation/*` (proxmox lab-validated first, then gitopsmr) | namespace projection round-trips through `estate.Graph.Resolve`; a plan touching an unresolved host → deviation | ⚠ touches actuator plan path |
| **MR-19** | Operator runbook (docs) — per-repo anti-self-merge provisioning + token/service-account identity | `docs/` runbook | n/a (docs) | no |
| **MR-20** | Demote the kubernetes actuator's declarative verbs | `modules/actuation/kubernetes/kubernetes.go` | for a gitops-managed target route `apply`/`patch`/`scale` → `gitopsmr`; keep `rollout restart`/`pod delete`/`cordon-uncordon` direct | no |

`go.mod` additions land with the first MR that needs them: `gitlab.com/gitlab-org/api/client-go` (the
maintained fork; **`xanzy/go-gitlab` is deprecated**), `github.com/hashicorp/hcl/v2/hclwrite`,
`sigs.k8s.io/kustomize/kyaml`.

**Dependency notes:** MR-1/2/3 are prerequisites for the chokepoint MRs. MR-9 and MR-10 are the only
two that modify `Interceptor.Do` and should be reviewed together but **land separately** (4d before the
split). MR-20 is a *policy demotion only* — the full native-client rebuild of the kubernetes actuator
(K8S design §3) is a **separate, larger item, out of v1's critical path.** The kubernetes deploy
blast-radius `EdgeSource` and the app-code advisory band are **v2**.

---

## 10. OPEN DECISIONS FOR THE OWNER

**★ THE ONE BLOCKING DECISION (sign off BEFORE any code):**
Is an **`open_ok` proposal** — with the effect cascade verdict still **PENDING** — an acceptable
"verified execution" under **INV-10 / gate 4c** ("never execute what you cannot verify")?
*Recommendation = **YES**, provided (a) the deferred effect-verdict path is **MANDATORY** (fail-closed
if absent) and (b) the reconciler keeps refusing close-out on an executed action lacking a clean match.*
Without this sign-off, `gitopsmr` cannot pass gate 4c as currently written, and supplying a nil/empty
observer would be the exact **fail-open verifier theater** the gate forbids.

**A second, near-blocking decision:** Does **autonomous mode EVER auto-apply a would-be-`POLL_PAUSE`
action** by treating the plan-gate as the reviewer? *Synthesis recommendation = **NO** — the toggle
never manufactures `Approved=true`; auto-apply is ONLY the classifier's own AUTO/AUTO_NOTICE band + a
green plan-gate.* A "YES" is a materially larger trust decision touching INV-12 + the 24h
deny-by-default and needs its own sign-off.

**Provisioning / identity:**
1. **Which repo?** Confirm the OpenTofu/GitLab deploy repo TG opens MRs against (project path, target
   branch), whether Atlantis autoplan is enabled, and whether it runs a `terraform show -json` step.
   **v1 uses a SCRATCH repo until this is given.**
2. **Bot identity + token:** a **group access token** (one credential across the GitOps group, new
   repos auto-covered) vs a named **service account** (stable, human-readable MR author). Both **MUST be
   `api`-scoped**; the chosen identity is what `author_approval=false` keys off. Owner provisions it as
   `config.SecretRef` (env:/file:), never literal. Confirm the reserved branch prefix (`tg/` proposed).
3. **GitLab tier:** approval **rules** (naming required human approvers) require **Premium/Ultimate**.
   If the instance is Free, anti-self-merge must lean on protected-branch merge-access exclusion alone.
4. **CI precondition:** confirm the target repo carries `merge_request_event` rules **directly** in
   `.gitlab-ci.yml`, not only via `include:`.

**Design knobs:**
5. **Webhook vs polling** for the sensor: webhook (real-time, needs public ingress + secret, misses
   downtime events) vs a low-frequency reconcile poll (firewall-friendly, self-healing, laggy).
   *Recommendation = webhook primary + a low-frequency reconcile poll as a safety net.*
6. **Webhook signature mode** for GitLab: modern `webhook-signature` HMAC vs legacy plaintext
   `X-Gitlab-Token`. *v1 supports both; owner sets policy.*
7. **Apply channel** for the saga's apply step: post an `atlantis apply` comment vs `Writer.Merge` and
   let Atlantis apply-on-merge (depends on the repo's Atlantis workflow config).
8. **`settleFor` per-op-class window durations** (ssh-restart, proxmox-boot, gitopsmr-apply,
   k8s-rollout) + `TG_DEPLOY_FALSIFIABILITY_WINDOW` (default 10m) + `TG_BLAST_RADIUS_WIDE_THRESHOLD`
   (default 8) — config-not-code, owner-set values.
9. **Capability slug** collision: the GitOps brief uses `Capability()="gitopsmr"`, the VCS brief uses
   `"vcs.gitopsmr"`. **Pick one and register it once.** (This spec assumes `gitopsmr` unless overridden.)
10. **Reconcile-signal authorization:** same attacker-reachability as the vote signal. Confirm it stays
    `action_id`-bound + `maxMisbound=64` + **observation-only** (never verdict-carrying), and decide WHO
    may emit it (Atlantis webhook? LibreNMS poller?).
11. **`Investigate` directive:** build the resume-with-deeper-RCA path now, or accept the v1 fold to
    `Deny` + directive-log (enum already reserved)?
12. **Node-type coverage:** confirm no deployment-relevant type (k8s workload/pod/service subtype) was
    among the 4 dropped predecessor node types before relying on the type spine.
13. **v1↔v2 line:** confirm v1 ships only the kubernetes-actuator **demotion** policy (route declarative
    verbs → `gitopsmr`) and that the full native-client rebuild + k8s topology `EdgeSource` + app-code
    advisory band + plan-gate-arming-to-block are tracked as separate **v2** items.

**Coordination:** a parallel AFK session shares this git tree (memory: `parallel-session-shared-tree-
race`) and sibling worktree branches carried some notifier backends + constitution-honesty work.
**Check for existing `gitopsmr`/`adapters/vcs`/`adapters/approval` branches and dedup before building.**

---

## 11. ADVERSARIAL REVIEW CORRECTIONS (2026-07-18)

An adversarial pass cross-checked every code claim against the real files. Corrections already folded
into the text above:

- **§6.3/§6.4 correctness + self-contradiction (FIXED):** the draft simultaneously claimed
  `ComputeVerdict` is "reused verbatim / unchanged" **and** specified replacing the scalar `TargetHost`
  with a new `TargetHosts` *set* at `verdict.go:72` — a struct-and-function change, so not verbatim. Worse,
  seeding the affected set as the *exclusion* would have **swallowed** an unpredicted alert on an affected
  node (it can never become PARTIAL), directly contradicting §6.4's PARTIAL definition. Fixed to seed the
  affected set into the **existing `PredictedHosts`** (leaving `TargetHost` empty/primary), which is
  genuinely verbatim reuse and yields §6.4's MATCH/PARTIAL/DEVIATION semantics correctly.
- **§2.2 wrong file:line (FIXED):** the fail-closed mirror was cited as `territory.go:47` (a caveat
  string); the real `Permit` BLOCK arm is `territory.go:113`.
- **Migration collisions (FIXED):** MR-14 named `0003_prediction_kind_deploy` (0003 is `audit_spine`; the
  `prediction_kind` enum is in `0002`) and MR-17 named `0009` (already `skill_store`). Highest existing is
  `0014`. Reassigned deploy→`0015`, approval_bindings→`0016`, breaker→`0017`.
- **MR-2 understated (FIXED):** "swap `NewMemStore()` → pgx `breaker.Store`" — no pgx store exists (only
  the `Store` interface + `MemStore`); MR-2 must **build** the pgx impl + its table migration.
- **Gate 4c chokepoint hardening (FIXED):** the deferred path must NOT be lifted by the caller-set
  `DeferVerify` bool alone (the "a flag lifts the gate" anti-pattern gate-2 forbids). 4c now requires a
  wired non-nil `DeferralSink`, checked in `SelfTest`, fail-closed if absent.
- **§5 empty-graph overstatement (FIXED):** the empty graph escalates only *multi-host* blast radius; a
  single-target reversible change passes the plan-gate. The unconditional inertness is **gate-1
  mutation-OFF**; autonomy must additionally be gated on populated estate.

**Residual risks the owner must decide (not fixable in-spec):**

1. **`open_ok` CI-liveness race (§4.B a).** `open_ok` requires the `merge_request_event` pipeline
   "running/passed" — but at synchronous MR-open the pipeline may not have registered yet (GitLab enqueues
   it async). The CI condition realistically belongs to the **sensor/deferred** read, not the open-time
   synchronous check. Decide: relax `open_ok` to "MR created + not merged + approvals_left>0", and move the
   CI-present assertion into the sensor poll.
2. **Durable-settle ownership ambiguity (§4.B vs §4.C/MR-11).** §4.B says `Do` "enqueues the delayed job
   keyed on action_id"; §4.C/MR-11 make the **RunnerWorkflow** own a durable `settleFor` timer +
   `SettleAndVerifyActivity`. These are two different durability mechanisms. For INV-21 (interceptor grows
   no second scheduler) the workflow should own it and `Do` should only return `Executed+Pending` + write
   `awaiting_effect`. Confirm the interceptor does **not** own an enqueue.
3. **Cross-half state threading (§4.B split).** `execRecErr` is computed in the synchronous half
   (`interceptor.go:246`) but consumed by the chain-integrity check (`:257`) that lifts into `Settle`. The
   spec hand-waves this as "Settle rebuilds executed→verified" — the plumbing (carry `execRecErr` + the
   manifest handle into `awaiting_effect`) needs an explicit design in MR-10, and `VerifyChain()` must NOT
   be called between open and settle (the chain is legitimately incomplete then).
4. **MR-10 without MR-11 leaves pending unresolvable.** A `DeferVerify=true` action admitted after MR-10
   but before MR-11/12 has no settle activity to resolve it. Inert under mutation-OFF, but the sequence
   should state that the deferred path is non-functional until MR-11 lands.

---

## 12. PROVISIONING — DISCOVERED (2026-07-18, owner-directed local scan)

Resolves §10 provisioning decisions 1–3 from a scan of `/home/tg/gitlab/` + the predecessor +
the owner's confirmation "GitLab is Free CE".

**§10.1 Target repo(s) — FOUND.** The k8s OpenTofu lives in **per-site infrastructure repos**, each its OWN
git repo on a per-site GitLab instance:
- `infrastructure/dc1/production` → `https://gitlab.example.net/infrastructure/dc1/production` (NL)
- `infrastructure/dc2/production` → `https://gr-gitlab.example.net/infrastructure/dc2/production` (GR — a **SEPARATE** GitLab instance)
- (+ `infrastructure/common/production` on NL for shared infra.)

The k8s config is the Atlantis **`k8s` project** (dir `k8s`), structured as `k8s/namespaces/<ns>/*.tf` +
`k8s/_core/<comp>/*.tf`, using `helm_release` + templated `values.yaml.tpl` (via `templatefile()`); ArgoCD
app-of-apps under `k8s/argocd-apps/` (the v2 plane). → **Two GitLab instances confirm the VCS actuator's
`base_url` must be per-target config, never hardcoded (§3.1).** FieldAddress targets: a `.tf`
`helm_release`/`set` block (hclwrite) OR a `values.yaml.tpl` (kyaml) — both covered by §3.3.

**§10.2 Bot identity — SAME AS PREDECESSOR (with a hygiene fix).** The infra repos authenticate with a
`glpat-` GitLab **PAT embedded (`oauth2:<pat>@`) in the git remote URL** — the existing mechanism. TG uses the
SAME kind of credential (an **`api`-scoped** GitLab PAT) but **sealed as a `config.SecretRef` (env:/file:),
NEVER embedded in a URL or committed** — a deliberate improvement (a PAT in `.git/config` is process-readable
and leaks through remotes). The bot user gets **Developer** role on the target repo: create-branch + push +
open-MR, but **cannot merge protected `main`** — which IS the Free-CE anti-self-merge (§10.3). Per-site: a
separate PAT per GitLab instance (NL vs GR).

**§10.3 GitLab tier = Free CE → anti-self-merge WITHOUT approval rules (SUPERSEDES §3.8).** Free CE has **no
Merge Request Approval *rules*** (those are Premium/Ultimate). So §3.8's approval-rule mechanism is
unavailable; the enforceable Free-CE model is: **protected `main`, merge-access limited to a Maintainer+
human**, the TG bot holding **Developer** (push/MR, not merge), plus Atlantis **`automerge: false`**
(confirmed in `infrastructure/dc1/production/atlantis.yaml`). A human Maintainer reads the Atlantis plan
comment, comments `atlantis apply`, and merges. Predecessor discipline, made structural by role. (CODEOWNERS
*required-approval* is also Premium — do not rely on it on Free CE.)

**Atlantis reality (confirmed, `atlantis.yaml`):** `automerge:false`, `parallel_apply:false`, ONE `k8s`
project (dir `k8s`), autoplan on `**/*.tf` + `*.tfvars` + `**/*.tpl`, apply over the **whole `k8s` project**.
→ CONFIRMS §8: TG opens the MR and STOPS (never `atlantis apply` — it fans out over the whole project); one
MR per project/dir.

**Still open (needs owner):** the §10 ★ blocking decision (open_ok / gate-4c) + the design knobs
(webhook-vs-poll, apply-channel, settle windows). v1 still validates against a **SCRATCH repo** before ever
touching `infrastructure/dc1/production`.

---

*End of DRAFT. This spec is the synthesis of five design briefs against the real TG code seams, plus §12
provisioning discovered from a local scan. It authorizes NO code and flips NO mutation. Owner sign-off on the
blocking decision (§10 ★) is the gate to begin MR-1.*
