---
name: k8s-control-plane-degradation-triage
class: runbook
version: 0.1.0-distilled
source: distill:https://kubernetes.io/docs/reference/using-api/health-checks/
description: Kubernetes control-plane triage — separate an apiserver reachability fault from an etcd quorum loss, verbose readyz names which
---

## Goal
Separate an apiserver-reachability problem from an etcd-quorum problem when the control plane itself is
degraded, since the two have different real causes and neither is fixed by restarting workloads. NOTE —
tool-gated content: TG has no Kubernetes read tool wired into the agent's own tool registry
(`agent/tools.go`) — `modules/actuation/kubernetes` exists as a declared-but-disabled reference actuation
module (Phase 0/1, a `deniedRunner`, no execution path; see `modules/bootstrap/bootstrap.go`); this runbook
is knowledge-library material until a vendor-official, read-only cluster surface is wired into the agent's
own tools. Every command below is read-only diagnostic guidance.

## Required evidence
- API server health: `kubectl get --raw='/readyz?verbose'` (and `/livez?verbose`) — the per-check
  breakdown, each line prefixed `[+]` or `[-]` naming a specific subsystem, including an `etcd` line. Run
  the equivalent directly with `curl -k https://<control-plane-host>:6443/readyz?verbose` when `kubectl`
  itself cannot complete a request.
- Control-plane component status: `kubectl get componentstatuses` is deprecated — use
  `kubectl get pods -n kube-system -l component=kube-apiserver` (and `component=kube-scheduler`,
  `component=kube-controller-manager`) instead, or, on the control-plane host itself when the API is
  unreachable, `crictl pods` / `crictl ps -a` plus `crictl logs <container-id>` against the static-pod
  containers, or the node's own `journalctl` / `/var/log/kube-apiserver.log` per its logging setup.
- etcd health: `etcdctl endpoint health` and `etcdctl endpoint status`, run with `--cacert=`, `--cert=`, and
  `--key=` pointed at the etcd server certificates (typically under `/etc/kubernetes/pki/etcd/`), evaluated
  PER MEMBER — plus `etcdctl member list` to confirm the expected member count and which are reachable.
- `kubectl get events -A --sort-by=.lastTimestamp` for a fast recent-history scan when the API server is at
  least partially responding.

## Decision rules
- `/readyz?verbose` names the failing check directly — an `etcd` line reading `[-]` while every other check
  reads `[+]` isolates the fault to etcd connectivity specifically, not the apiserver process itself. Read
  the per-check output before guessing which control-plane component owns the failure.
- etcd is quorum-based: with N members, the cluster tolerates ⌊(N-1)/2⌋ member failures and keeps serving.
  Losing quorum (a majority of members unreachable) means the cluster CANNOT accept writes — no new pods
  can be scheduled, no status updates persist — even though already-running workloads on healthy nodes keep
  executing, because they do not depend on the control plane to keep running.
- `etcdctl endpoint health` evaluated PER MEMBER (not once against a load-balanced endpoint) is what
  actually tells you which members are down versus which are fine — a single aggregate check can read
  healthy while a specific member that matters for quorum math is not.
- Elevated apiserver latency or timeouts with etcd itself reporting healthy points at a different cause
  (apiserver resource pressure, an admission webhook stalling, high request volume) — don't default to
  "etcd must be slow" just because the symptoms look control-plane-wide. The `/readyz?verbose` `etcd` line
  is the actual discriminator between the two.
- Never act on multiple etcd members simultaneously to "fix" degradation. The documented posture is to
  replace or recover failed members ONE AT A TIME and confirm cluster stability between each — simultaneous
  action on multiple members is what turns a tolerable failure into a lost quorum.
- A production etcd topology is documented as an odd number of members (five is the common production
  size) specifically so failure tolerance has a clean majority. An even-member or single-member "cluster"
  discovered during triage is itself a finding to escalate, not just the immediate symptom to work around.

## Verification
- `kubectl get --raw='/readyz?verbose'` shows every check `[+]ok`, including `etcd` — not just the specific
  check that was previously failing.
- `etcdctl endpoint health` returns healthy for EVERY member, not just a quorum-sufficient subset, before
  calling the incident closed — a cluster that recovered quorum with one member still down is degraded, not
  healed.
- A representative write actually lands: a trivial `kubectl` create or patch (or re-observing that a
  previously stuck pod now schedules) confirms the API server is accepting and persisting changes again,
  not merely answering reads.

## Doc basis
- Kubernetes: Kubernetes API Health Endpoints —
  https://kubernetes.io/docs/reference/using-api/health-checks/
  (`/livez`, `/readyz`, the `?verbose` and `?exclude=` query parameters, per-check semantics).
- Kubernetes: Operating etcd clusters for Kubernetes —
  https://kubernetes.io/docs/tasks/administer-cluster/configure-upgrade-etcd/
  (`etcdctl endpoint health`/`endpoint status` syntax, quorum and failure-tolerance guidance, replacing
  failed members one at a time, the five-member production recommendation).
- Kubernetes: Debugging Kubernetes Nodes with crictl —
  https://kubernetes.io/docs/tasks/debug/debug-cluster/crictl/
  (`crictl pods`/`ps`/`logs` for inspecting static control-plane containers when the API itself is
  unreachable).
- Kubernetes: Troubleshooting Clusters —
  https://kubernetes.io/docs/tasks/debug/debug-cluster/
  (control-plane component log locations, `kubectl get nodes`/`describe node`, `kubectl cluster-info dump`).
