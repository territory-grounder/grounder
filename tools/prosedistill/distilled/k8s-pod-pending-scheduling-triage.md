## Goal
Decide why a pod is stuck `Pending` — no node satisfies its constraints, versus the cluster is short of a
specific resource, versus a storage dependency is blocking it — from the scheduler's own stated reason
before touching node counts, requests, or taints. NOTE — tool-gated content: TG has no Kubernetes read tool
wired into the agent's own tool registry (`agent/tools.go`) — `modules/actuation/kubernetes` exists as a
declared-but-disabled reference actuation module (Phase 0/1, a `deniedRunner`, no execution path; see
`modules/bootstrap/bootstrap.go`); this runbook is knowledge-library material until a vendor-official,
read-only cluster surface is wired into the agent's own tools. Every command below is read-only diagnostic
guidance.

## Required evidence
- `kubectl get pods -n <namespace>` — `STATUS: Pending` and `AGE` (a pod pending for minutes-to-hours is a
  different signal than one seconds old).
- `kubectl describe pod <pod> -n <namespace>` — the `Events:` section's `Warning FailedScheduling` entry
  from `default-scheduler`, whose `Message` states the failed predicate in the scheduler's own words (for
  example `Insufficient cpu`, `node(s) had untolerated taint {...}`, `node(s) didn't match Pod's node
  affinity/selector`, `node(s) had volume node affinity conflict`).
- `kubectl get nodes -o wide` and `kubectl describe node <node>` for each candidate node — `Allocatable`
  versus `Allocated resources` (requests already committed), the `Taints:` field, and `Conditions:`.
- The pod's own `spec.nodeSelector`, `spec.affinity`, `spec.tolerations`, and container
  `resources.requests`, read directly against node labels, taints, and allocatable capacity — never
  inferred.
- For resource-exhaustion symptoms specifically: the `Allocated resources` section of `kubectl describe
  node` (per-resource requests already committed versus `Allocatable`) across every node the pod could
  land on.

## Decision rules
- Read the `FailedScheduling` `Message` verbatim before hypothesizing. The scheduler states which predicate
  failed, and each names a different fix: `Insufficient <resource>` is capacity, `node(s) had untolerated
  taint` is a taint/toleration mismatch, `node(s) didn't match Pod's node affinity/selector` is a label
  mismatch, `node(s) had volume node affinity conflict` is a storage-topology problem, not a compute one.
- `nodeSelector` and `requiredDuringSchedulingIgnoredDuringExecution` node affinity are HARD constraints —
  zero matching nodes means `Pending` indefinitely, not eventually. `preferredDuringSchedulingIgnoredDuringExecution`
  is soft and never blocks scheduling by itself, so a `Pending` pod whose only affinity rule is `preferred`
  has a different root cause than affinity.
- A `NoSchedule` taint on every viable node blocks NEW pods but does not evict what is already running
  there — don't conflate "nothing is running on it" with "nothing can run on it." Check `kubectl describe
  node` `Taints:` on the specific candidate nodes, not the cluster in aggregate. `NoExecute` is the effect
  that also evicts already-running pods without a matching toleration; `PreferNoSchedule` is a soft
  preference that never blocks scheduling outright.
- Insufficient-resource messages are about `requests`, not `limits` — a pod can stay `Pending` because its
  requested CPU/memory doesn't fit in any SINGLE node's `Allocatable` minus already-`Allocated`, even while
  the cluster's raw total capacity looks sufficient. The request has to fit on one node, not the fleet.
- A `node(s) had volume node affinity conflict` message means the PV the pod's PVC is already bound to is
  zone- or node-pinned somewhere no schedulable node can reach. This is a storage-layer finding — hand off
  to PVC/StorageClass triage instead of adding compute capacity that cannot fix it.
- A taint automatically applied by the node controller (for example on a node that is `NotReady`) produces
  the same `FailedScheduling` shape as an operator-authored taint — check whether the named node is
  actually healthy before assuming the taint itself is the thing to change.

## Verification
- `kubectl get pods -n <namespace>` shows the pod transitioned to `Running`, not merely re-`Pending` with a
  fresh `AGE`.
- `kubectl describe pod` no longer shows `FailedScheduling` as its most recent scheduling-related event —
  the newest relevant entry is `Normal Scheduled`, naming the node it landed on.
- If the fix was a taint/toleration or affinity edit, `kubectl describe node <node>` (or the pod's resolved
  `spec.nodeName`) confirms the specific field matches what was intended, not just "some node was picked."

## Doc basis
- Kubernetes: Assign Pods to Nodes —
  https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/
  (`nodeSelector`, node affinity required-vs-preferred semantics, Pending-forever behavior on a hard
  constraint with no match).
- Kubernetes: Taints and Tolerations —
  https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
  (`NoSchedule` / `PreferNoSchedule` / `NoExecute` effects, `kubectl taint` syntax, unmatched-taint Pending
  behavior, toleration matching logic).
- Kubernetes: Node-pressure Eviction —
  https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/
  (eviction signals and thresholds behind the resource-exhaustion node conditions that also drive
  scheduling pressure).
