---
name: k8s-pvc-storageclass-triage
class: runbook
version: 0.1.0-distilled
source: distill:https://kubernetes.io/docs/concepts/storage/persistent-volumes/
description: Kubernetes PVC and StorageClass binding triage — WaitForFirstConsumer is not broken, default-class ambiguity, access-mode and capacity matching
---

## Goal
Diagnose a PVC stuck `Pending`, or a pod stuck on volume binding, by reading the PV/PVC binding state and
the governing StorageClass — rather than guessing at raw capacity. NOTE — tool-gated content: TG has no
Kubernetes read tool wired into the agent's own tool registry (`agent/tools.go`) —
`modules/actuation/kubernetes` exists as a declared-but-disabled reference actuation module (Phase 0/1, a
`deniedRunner`, no execution path; see `modules/bootstrap/bootstrap.go`); this runbook is knowledge-library
material until a vendor-official, read-only cluster surface is wired into the agent's own tools. Every
command below is read-only diagnostic guidance.

## Required evidence
- `kubectl get pvc -n <namespace>` and `kubectl get pv` — the `STATUS` column (`Pending` / `Bound` /
  `Released` / `Failed`) and, for a PV, the `CLAIM` column showing what it is bound to, or blank.
- `kubectl describe pvc <pvc> -n <namespace>` — `StorageClass:`, `Status:`, `Volume:`, `Access Modes:`, and
  `Events:` (provisioning failures, including a provisioner's own error message, land here).
- `kubectl get storageclass` and `kubectl describe storageclass <name>` — `Provisioner:`,
  `VolumeBindingMode:`, `ReclaimPolicy:`, and which StorageClass (if any) carries the
  `storageclass.kubernetes.io/is-default-class: "true"` annotation.
- The PVC's own `spec.storageClassName`, `spec.accessModes`, and `spec.resources.requests.storage`, diffed
  directly against candidate PVs' `Capacity`, `Access Modes`, and `StorageClass:` (for static/pre-provisioned
  PVs), or against the named StorageClass (for dynamic provisioning).
- `kubectl describe pod <pod> -n <namespace>` when the symptom is a stuck POD rather than a visibly Pending
  PVC — a `FailedScheduling` citing "volume node affinity conflict," or a `FailedMount`/`FailedAttach`
  event, both surface here even when the PVC itself already reads `Bound`.

## Decision rules
- A PVC reading `Status: Pending` with `Volume:` blank and no controller error in `Events:` is not
  automatically a fault: with `volumeBindingMode: WaitForFirstConsumer` set on its StorageClass, binding and
  provisioning are DEFERRED by design until a pod that uses the PVC is actually scheduled. Check whether a
  consuming pod exists yet before treating this as broken.
- With `volumeBindingMode: Immediate` (the default when unset), a PVC that stays `Pending` past pod creation
  means no matching PV was found (static provisioning: no PV with sufficient capacity, matching access
  mode, and matching StorageClass) or dynamic provisioning failed — the PVC's `Events:` names which.
- No `storageClassName` on the PVC does NOT mean "any StorageClass." It means "the cluster's default
  StorageClass," and if no StorageClass carries the default annotation, the PVC provisions nothing and stays
  `Pending`. More than one StorageClass marked default is an equally broken, ambiguous state.
- Capacity and access-mode matching against a static PV pool is at-least, not approximate — a PVC
  requesting 100Gi will never bind to a pool of 50Gi PVs no matter how many exist, and a `ReadWriteOnce`-only
  PV will never satisfy a `ReadWriteMany` request. Read both fields on both sides before concluding "there's
  plenty of storage."
- `WaitForFirstConsumer` combined with a pod pinned via `spec.nodeName` (bypassing the scheduler) is a
  documented foot-gun: provisioning happens in the topology the SCHEDULER picks, and a hardcoded `nodeName`
  skips that step, so the volume can provision in the wrong zone/node and the pod then fails to mount it —
  use a `nodeSelector` instead of `nodeName` when a pod must land on a specific node under this binding
  mode.
- Once a PV is `Bound`, that binding is exclusive and does not move on its own. A PV stuck `Released` (its
  claim deleted, storage not yet reclaimed) needs an explicit, reclaim-policy-aware decision: `Retain` needs
  manual cleanup before the PV can be reused, while `Delete` should self-clear — don't expect a `Retain`
  volume to become `Available` again by itself.

## Verification
- `kubectl get pvc -n <namespace>` shows `STATUS: Bound` with a populated `VOLUME` name, not just "no error
  currently showing."
- `kubectl describe pod` for the consuming pod shows no `FailedMount` / `FailedAttach` / volume-affinity-
  conflict event as its latest volume-related entry.
- The bound PV's `Capacity` and `Access Modes` (from `kubectl describe pv`) actually match what the workload
  needs — an under-sized or wrong-access-mode bind can still read `Bound` while being the wrong fix.

## Doc basis
- Kubernetes: Persistent Volumes —
  https://kubernetes.io/docs/concepts/storage/persistent-volumes/
  (PV phases Available/Bound/Released/Failed, PVC Pending semantics, the binding control loop, access
  modes, reclaim policy, the `kubernetes.io/pvc-protection` and `pv-protection` finalizers).
- Kubernetes: Storage Classes —
  https://kubernetes.io/docs/concepts/storage/storage-classes/
  (`provisioner`, `volumeBindingMode` Immediate vs WaitForFirstConsumer, the default-StorageClass
  annotation, the `nodeName`-bypasses-the-scheduler foot-gun with WaitForFirstConsumer).
