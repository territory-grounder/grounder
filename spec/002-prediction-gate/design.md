<!-- spec/002 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/002 — Design: fail-closed prediction gate + mechanical verdict

How the requirements in `requirements.md` are realized on the Go / Temporal / PostgreSQL stack. Where
this design and the code disagree, the code is the bug and this document is the intent. This lane is the
remediation lane: absent a committed, action-bound prediction, it denies.

## Components

- **`core/predict.PredictionGate`** (Phase 2) — computes the `plan_hash`-keyed machine consequence
  prediction from the infragraph dependency model, entirely outside the LLM, and commits it to the
  append-only `infragraph_prediction` store. When an estate graph (`core/estate`) is wired, the predicted
  cascade is drawn from its path-product blast radius plus — ONLY for a common-cause incident (a whole-host
  availability/connectivity fault, classified from the alert rule by `predict.SiblingsEligible`) — the
  target's shared-parent siblings, using each edge's expected alerts. A resource- or service-local fault
  predicts blast radius only, so a leaf guest's local alert no longer names its co-tenants as false positives;
  the negative control mirrors the same siblings gate so the comparison stays the same shape. A target that
  does not resolve in the graph yields an EMPTY prediction, so the
  remediation lane fails closed on eligibility rather than predicting a vacuous cascade. A misconfigured model
  with NEITHER an estate graph nor a flat fallback graph also yields an empty prediction — fail closed, never
  a nil-graph panic. The model reads the
  graph through an optional `EstateProvider` (an atomically-refreshable `estate.Holder`), so a runtime
  topology refresh takes effect without rebuilding the gate; the fixed `Estate` field is used when no provider
  is wired. It is the only constructor of a **`GatedProposal`**: a
  typed value that carries the committed prediction, its `plan_hash`, and the bound `action_id`.
- **`core/verify.Verifier`** (Phase 2) — the deterministic `computeVerdict(pred, observed)` that diffs
  observed alerts against the committed prediction and returns one `safety.Verdict`
  (`match` / `partial` / `deviation`). It is the sole writer of the verdict; the model and session
  database roles hold no UPDATE or DELETE grant on the prediction or verdict tables.
- **`core/manifest.ActionManifest`** (Phase 0/1, already built) — the immutable content-hashed binding
  spine. `action_id = SHA-256(canonicalJSON(Action))` is computed once and threaded unchanged through
  predict → approve → execute → verify; each Temporal activity calls `Assert(expectedActionID)` on
  receipt. A mutated Action derives a different id and fails the assertion closed. A DURABLE store reads a
  manifest back through `Rehydrate(action_id, action, …)`, which re-derives and verifies the content hash and
  returns a *sealed* manifest — the unexported seal flag cannot be set by a cross-package struct literal, so a
  hand-built manifest would fail `Assert` (`!sealed`) on every persisted row and the INV-07 re-assertion would
  never actually run; `Rehydrate` performs that check itself. The manifest is the
  single immutable record for the whole lifecycle: it binds the `Provenance` the action was born from (the
  triggering incident, the context snapshot, the model, the compiled-prompt hash — frozen once the first
  stage is recorded) and carries an **append-only `Stages` chain**. Each `Stage` (`predicted → approved →
  executed → verified`) is stamped with the action_id and a SHA-256 of its payload (the committed
  prediction, the approval choice, the ACTUAL tool calls, the observed postconditions/verdict). `Record`
  refuses out-of-order or unsealed appends; `VerifyChain` re-derives the action_id and asserts every stage
  binds it — so a prediction, approval or verdict for some *other* action can never be passed off as this
  one's (the structural fix for the predecessor's "committed prediction not bound to the executed action").

## Temporal realization of ordering (REQ-101, REQ-102, REQ-102b)

The remediation workflow declares its activities in a fixed order:
`PredictActivity → ApprovalActivity → ExecuteActivity → VerifyActivity`. `ApprovalActivity.BuildPoll`
takes a `GatedProposal` parameter — a type only `PredictionGate` can produce — so an approval poll for a
proposal with no committed prediction does not type-check. This replaces the predecessor's runtime
"default-deny if no prediction row" guard (walkable via a second grammar) with a compile-time
impossibility (INV-06/INV-07). Each activity re-derives `action_id` from the manifest it receives and
aborts fail-closed on any mismatch, so a proposal mutated after prediction re-enters the gate as a
child workflow keyed on the new id.

## The mechanical verdict (REQ-103, REQ-104)

`computeVerdict` is a pure function over the predicted `(host, rule)` set and the observed alert set,
re-expressing the predecessor `infragraph.action_verdict` logic under the typed spine:

1. Exclude the action's own target-host alerts — a rebooted host alerting is the expected direct effect,
   not a cascade surprise. A surprise host is otherwise ALWAYS a deviation: the verdict does NOT trust an
   ingest-supplied `Site` label to downgrade a surprise cascade to a match. The label is a free-form ingest
   slug any deployment may stamp, so trusting it fails OPEN — a real cascade to a host carrying a *third*
   site label would be silently swallowed as background noise. The predecessor avoids this by deriving site
   from the HOST IDENTITY (a closed nl/gr vocabulary; every other host is site-less and never excluded); TG
   has no such host→site vocabulary (config-not-code), so it fails CLOSED — a surprise host surfaces as a
   deviation regardless of any label it carries. (A precise coincidental-cross-site filter would key on an
   estate-derived / operator-configured site vocabulary, restoring the noise-filtering without the fail-open.)
2. `surprise` = an observed alert on a host the prediction never named → **`deviation`**.
3. Else `host-level` = an observed alert on a predicted host but with an unpredicted rule → **`partial`**.
4. Else → **`match`** (includes the quiet case where a healthy remediation fires no cascade).

A `deviation` means the world diverged from the model. Per REQ-104 it can never auto-resolve regardless
of band or confidence; the reconciler routes it to POLL_PAUSE and the approver graph. The
`match`/`partial`/`deviation` verdict is post-execution; the pre-execution prediction commitment
(REQ-101) is the gate. The two are distinct decisions and are recorded on distinct rows.

### Typed verdict detail (REQ-103a)

The verifier exposes two entry points backed by ONE decision. `ComputeVerdictDetail(pred, observed)` is the
single-pass author: it walks the observed alerts once, collecting the **surprise hosts** (deviation triggers)
and the **rule mismatches** — a predicted host carrying an unpredicted rule (partial triggers) — into a typed
`VerdictDetail{Verdict, SurpriseHosts, Mismatches}`, then DERIVES the verdict from that breakdown (deviation
dominates partial dominates match). `ComputeVerdict(pred, observed)` is the enum-only projection —
`ComputeVerdictDetail(pred, observed).Verdict` — so the verdict decision lives in exactly one place (INV-10)
and is byte-identical to the pre-detail implementation for every input; a battery oracle asserts
`ComputeVerdictDetail(...).Verdict == ComputeVerdict(...)` across match/partial/deviation/exclusion/fail-closed
cases. The breakdown slices are deduplicated and sorted, so the detail is deterministic for a given
`(prediction, observation)`. Verify-time callers (`core/falsify.Scorer`) consume the typed detail instead of
re-diffing the prediction against the observation to rediscover which hosts surprised. A third entry point,
`ComputeVerdictDetailWithBaseline(pred, observed, baseline)`, takes a TEMPORAL baseline — the estate's active
alerts captured just BEFORE the action executed — and excludes any observed alert already present in it (keyed
host+rule) from the surprise/mismatch breakdown: an alert that fired BEFORE the action cannot be its cascade, so
only alerts that APPEARED since it can trigger a deviation (TG-148). The interceptor's post-execution verify uses
this with a pre-execute snapshot, so a pre-existing UNRELATED estate alert (the post-state Observe is estate-wide)
no longer false-DEVIATES — which would otherwise demote the op-class and trip the breaker on a SUCCESSFUL heal. A
nil/empty baseline reproduces `ComputeVerdictDetail` exactly (fail-safe: widens what counts as a surprise, never
hides a real cascade — a slow cascade not yet visible at the immediate pass is caught by the settle-window
reconcile). — the falsifiability
confusion matrix stays with `ScoreControl`, which applies a cross-site noise filter the verdict deliberately
does NOT, so the two remain distinct measurements rather than a duplication.

## Analysis-only mode (REQ-105)

Analysis-only is an org-global, RBAC-gated policy row (the reframe of the predecessor host-local
`INFRAGRAPH_DISABLED=1` sentinel). While it is active, `PredictActivity` still writes the
prediction and `VerifyActivity` still writes a shadow verdict for evaluation, but `ApprovalActivity`
does not block on the prediction — the lane degrades to the fail-open advisory posture. The mechanical
never-auto floor and the deviation-never-auto rule are not affected by this mode: analysis-only can make
the gate advisory, never make an irreversible or deviating action auto-resolve.

## Falsifiability (negative control)

Every committed prediction carries a real DEGREE-PRESERVING negative control: when an estate graph is
wired the control host set is `estate.ShuffledControl(target, depth, plan_hash, includeSiblings)` (each
source keeps its out-degree and each rel_type keeps its target multiset, but the real who-depends-on-what
topology is destroyed), so the control has the same graph SHAPE as the real prediction — beating it is a
genuine signal, not an artifact of naming the right NUMBER of hosts. `includeSiblings` mirrors the real
prediction's common-cause siblings gate, so when a prediction adds siblings its control walks the shuffled
graph's siblings too and the two stay the same shape (an asymmetric control would rig the comparison). An unresolvable target yields an empty
control (mirroring the empty prediction). With no estate wired a flat-graph count-only fallback is used
(oracle/legacy path only). The append-only `MemPredictionStore` is CONCURRENCY-SAFE (a mutex guards its
map/slice) — it is shared across the worker's concurrent gate activities, and without the lock concurrent
`Commit` races the index map and can double-index a plan_hash.

At verify time `ScoreControl(record, observed)` computes host-level true/false positives for BOTH the real
prediction and its control, applying the same target-host exclusion as `ComputeVerdict`. The
falsifiability test (INV-22) is `ControlScore.Falsifiable()` — `control_tp / real_tp` (real floored at 1)
must be `<= ControlRatioCeiling` (0.5): a prediction whose control captures at least half as many true
cascades named the right shape but not the right hosts, so it is not a trustworthy causal claim and must
not be leaned on to auto-resolve. INV-22 property tests assert the control columns exist and are populated
on every prediction row AND that the scorer positively separates a real prediction from its control.

## Verify-time falsifiability writeback (measurement, mutation-independent)

`ScoreControl` and `ComputeVerdict` are the deterministic scorers; `core/falsify.Scorer` is their production
caller — the piece the Phase-2 readiness review found missing (the chain had zero production callers, so the
grounding scorecard's `SignalRatio` was degenerate). It is MEASUREMENT ONLY and does not depend on mutation
being ON: a prediction is committed BEFORE any action (by the gate) and scored AFTER a post-incident
observation window elapses. Each pass reads the committed-but-unscored predictions whose window has passed
(`tp IS NULL AND committed_at < now-window`), observes the LIVE post-incident alerts through the same
read-only surface the interceptor's verifier uses, and (1) writes the confusion matrix
(`tp/fp/fn/control_tp/control_fp`) back onto the prediction row — the SOLE verify-time write, the immutable
prediction identity is never touched; (2) persists the mechanical verdict (INV-10 — a `deviation` is
never-auto by construction, `AutoResolvable` is false); (3) accumulates one windowed `infragraph_cascade_stats`
row (INV-22 over-prediction gating). The writeback runs on the READ-ONLY / propose path (it scores, it never
actuates), so `SignalRatio` / precision / recall in the grounding scorecard read REAL scored predictions. The
observation is a best-effort window snapshot of live active alerts: a cascade that has already cleared reads
as a quiet post-state (a `match` with `real_tp=0`), which is honest — the falsifiability signal is carried by
the incidents whose cascade is still observable when the window elapses.

## Persistence & audit

Every prediction and every verdict is one immutable append-only row stamped
`schema_version`, carrying the bound `action_id`, and chained into the tamper-evident governance ledger
(INV-19). Reads of predictions and verdicts are authority-checked against the acting user/role under RBAC (INV-12).

## Out of scope

The three-band classification that consumes the verdict is spec/001. The per-incident auto-resolve /
escalation-requeue reconciler that acts on a `match` or stands down on a `deviation` is spec/003. The
Phase-C blast-radius suppression proposal lane that turns a repeatedly-predicted cascade into a declared
fold rule is spec/005. Ledger chaining mechanics are spec/006.
