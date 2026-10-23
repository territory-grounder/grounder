---
name: k8s-node-notready-triage
class: runbook
version: 0.1.0-distilled
source: distill:https://kubernetes.io/docs/concepts/architecture/nodes/
description: Kubernetes node NotReady and kubelet triage — Unknown versus False, pressure conditions, cordon before any drain or reboot
---

## Goal
Establish whether a `NotReady` node is a kubelet/connectivity reporting problem or a real resource-exhaustion
condition before draining or rebooting it — draining an unreachable node cannot evict cleanly, and
rebooting a node that is actually fine but network-partitioned from the control plane just adds a second
fault on top of the first. NOTE — tool-gated content: TG has no Kubernetes read tool wired into the agent's
own tool registry (`agent/tools.go`) — `modules/actuation/kubernetes` exists as a declared-but-disabled
reference actuation module (Phase 0/1, a `deniedRunner`, no execution path; see
`modules/bootstrap/bootstrap.go`); this runbook is knowledge-library material until a vendor-official,
read-only cluster surface is wired into the agent's own tools. Every command below is read-only diagnostic
guidance.

## Required evidence
- `kubectl get nodes` — the `STATUS` column (`Ready` / `NotReady` / `Unknown`) and how long it has held
  that value.
- `kubectl describe node <node>` — the `Conditions:` table (`Ready`, `MemoryPressure`, `DiskPressure`,
  `PIDPressure`, `NetworkUnavailable`) with `Status`/`Reason`/`Message` per condition, and the `Taints:`
  field for an auto-applied `node.kubernetes.io/not-ready` or pressure taint.
- `kubectl get events --field-selector involvedObject.name=<node>` (or the `Events:` section of `describe`)
  for the node controller's own transition messages.
- The kubelet's own logs on the node: `journalctl -u kubelet` where direct/SSH access exists, or —
  specifically for when it doesn't — `kubectl debug node/<node> -it --image=<image>`, which drops a
  debugging pod onto the node with its root filesystem mounted at `/host`, letting you read
  `/host/var/log/kubelet.log` (and `/host/var/log/containerd.log`, `/host/var/log/syslog`) from there.
- `kubectl get pods -A -o wide --field-selector spec.nodeName=<node>` — what is actually scheduled there,
  to size blast radius before taking any action.

## Decision rules
- `Ready: Unknown` (not `False`) means the node controller stopped RECEIVING heartbeats — status updates or
  the node's `Lease` object in `kube-node-lease` — which is a reporting/connectivity fault, not necessarily
  a resource fault. `Ready: False` with a stated `Reason` is the kubelet's own self-report of being
  unhealthy, a different class of problem.
- The node controller applies `node.kubernetes.io/not-ready` automatically once a node is unreachable past
  `node-monitor-grace-period`, and eventually triggers API-initiated eviction of the node's pods if it stays
  unreachable — this is autonomous cluster behavior already in flight by the time an operator looks, not
  something to reproduce or second-guess manually.
- Check `MemoryPressure`/`DiskPressure`/`PIDPressure` independently of `Ready` — a node can be `Ready: True`
  and still evicting pods under pressure. Conversely, `Ready: False` with every pressure condition `False`
  points away from resource exhaustion and toward the kubelet process or its network path (crashed, an
  expired certificate, the API server unreachable from that node).
- `kubectl describe node` conditions and events are the CONTROL PLANE's view, current only as of the last
  successful heartbeat — a node that is actually fine but network-partitioned FROM the control plane looks
  identical from this evidence alone to a node that is genuinely down. The kubelet's own log (via `kubectl
  debug node` or direct access) is the only way to distinguish "kubelet is fine but can't reach the API
  server" from "kubelet itself is wedged or crashed."
- Never drain or reboot before sizing what is currently scheduled there and its storage dependencies — a
  single-replica workload, or one backed by a node-pinned volume, turns a routine drain into an outage
  rather than a rolling replacement. `kubectl cordon <node>` (block new scheduling, touch nothing already
  running) is the safe first step ahead of any drain/reboot decision.

## Verification
- `kubectl get nodes` shows the node back to `Ready: True`, and `kubectl describe node`'s transition
  `Reason`/`Message` names what actually recovered (kubelet restarted, network path restored) — not just a
  status flip with no explanation on record.
- The auto-applied `node.kubernetes.io/not-ready` (and any pressure) taint is gone from `kubectl describe
  node` `Taints:`.
- Pods that were evicted or left pending during the outage are confirmed rescheduled and `Running` —
  elsewhere or back on the recovered node — not just "the node looks fine now."

## Doc basis
- Kubernetes: Nodes —
  https://kubernetes.io/docs/concepts/architecture/nodes/
  (node `Conditions`, node-controller heartbeats via status updates and the `Lease` object,
  `node-monitor-grace-period`, the auto-applied `node.kubernetes.io/not-ready` taint, `kubectl cordon`).
- Kubernetes: Node-pressure Eviction —
  https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/
  (`MemoryPressure`/`DiskPressure`/`PIDPressure` signals and the thresholds that set them).
- Kubernetes: Debugging Kubernetes Nodes With kubectl —
  https://kubernetes.io/docs/tasks/debug/debug-cluster/kubectl-node-debug/
  (`kubectl debug node/<node>`, the `/host` filesystem mount, reading kubelet/runtime logs without direct
  SSH access).
