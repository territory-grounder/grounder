## Kubernetes triage (this estate runs STRICT GitOps)
This skill sharpens WHICH fault and the SHAPE of the fix on a k8s incident. It does NOT decide whether an
action runs and it does NOT lower the risk band — the band is machine-enforced and every proposal still
goes to the gate at the band the classifier assigns. Nothing below is a licence to auto-act; treat every
proposal as poll-by-default and let the machinery decide.

The runtime lane and the declarative lane are different, and the fix SHAPE depends on which one the fault is in.

RUNTIME / EPHEMERAL faults — the controller re-derives state, so a conservative reversible action may be the
right PROPOSAL (its band is still machine-set; do not assume it auto-runs):
  - a Pod NotReady / CrashLooping whose Deployment/StatefulSet spec is healthy: the object is ephemeral. Localize
    the cause (OOMKilled -> memory; ImagePullBackOff -> registry/tag; CrashLoop -> the container's own logs)
    before proposing. Deleting/restarting the pod lets the controller re-create it — self-healing, NOT a GitOps
    violation — but only propose it once you have named WHY it failed; a blind restart hides a spec fault.
  - a Node NotReady / unreachable / flapping: cordon/drain/uncordon act on SCHEDULABILITY, which is not a
    Git-reconciled field, so they are runtime ops — but a node reboot/power-cycle is the never-auto floor's, a
    human poll. Check kubelet/containerd and the node's own disk/memory pressure first.

DECLARATIVE faults — a field under version control (replicas, image tag, resource limits, a HelmRelease value):
  NEVER propose a direct kubectl/helm write. On this estate ALL such changes go through OpenTofu + Atlantis MR.
  The right proposal is a free-form op_class that names the MR-shaped change (e.g. bump-replicas-via-mr), with the
  undo_sketch being "revert the MR". A ReplicasMismatch or a PVC-filling that needs a manifest change is this lane.

DEFAULT POSTURE is read-only diagnosis + human approval. When the observations do not clearly place the fault in
the runtime lane, STOP and name what a human needs — do not reach for a declarative change disguised as a runtime one.

NEVER auto (human poll, always): StatefulSet/PV whose workload is stateful (etcd/postgres/*-db/redis/prometheus/
seaweedfs) — a restart can lose data; namespace/pvc/pv deletion; node reboot; anything the never-auto floor lists.