## Proxmox / PVE triage (this estate runs a PVE cluster; guests are VMs + LXC)
This skill sharpens WHICH fault and the SHAPE of the fix on a Proxmox incident. It does NOT decide whether an
action runs and it does NOT lower the risk band — the band is machine-enforced and every proposal still goes to
the gate at the band the classifier assigns. Nothing below is a licence to auto-act; treat every proposal as
poll-by-default and let the machinery decide.

The fault lives in ONE of four planes — NAME the plane before proposing, because the fix SHAPE and the floor differ.

GUEST plane — a single VM/LXC is down/unreachable but its PVE node is healthy. The guest is the ephemeral object:
  - A stopped/crashed guest whose node is UP and whose backing storage is AVAILABLE: a reversible `qm start` /
    `pct start` is a candidate PROPOSAL — its band is still machine-set, do not
    assume it auto-runs — but only once you have NAMED why it stopped (node memory pressure / an internal
    crash / a failed boot). A blind restart hides the cause. Predict the post-state: guest status `running` and
    the guest's own service checks (pings/HTTP) resume.
  - Confirm the node is healthy FIRST. A guest that is "down" only because its NODE is down is a NODE-plane fault;
    do not propose a guest action for it.

NODE plane — a PVE host is down / unreachable / flapping. NEVER act on the host from triage: never reset, reboot,
  power-cycle, migrate-all, or fence a node — that endangers every guest it carries and the cluster's quorum. A
  node reboot or power-cycle is on the never-auto floor — a human poll. Check cluster quorum and the peer nodes.
  A downed node takes its guests down as a CONSEQUENCE — do not propose guest restarts for those guests; they
  return with the node.

STORAGE plane — a guest will not start, IO errors, or a pool is degraded. A guest that fails to start is very
  often a STORAGE fault, not a guest fault. Check the backing store (ZFS pool health / DEGRADED, LVM, the NFS or
  iSCSI target's reachability) BEFORE proposing a restart — restarting a guest whose storage is gone just fails
  again and buries the real cause. Storage repair is diagnosis-first and a human poll; never propose a destructive
  pool op (pool/volume destroy, disk wipe).

CLUSTER plane — quorum loss / fencing / split-brain risk. Quorum and fencing are cluster-wide safety mechanisms;
  never propose an action that could split-brain or trigger a fence. This is a human poll, always.

DEFAULT POSTURE is read-only diagnosis + human approval. When the observations do not cleanly place the fault in
the GUEST plane WITH a healthy node AND available storage, STOP and name what a human needs — do not propose a
guest action to paper over a node, storage, or cluster fault.

NEVER auto (human poll, always): any host/node op (reset / reboot / power-cycle / stop / migrate-all / stonith);
any destructive storage op (pool or volume destroy, disk wipe); touching the host while a guest is the symptom;
anything the never-auto floor lists. Reversible and allowlist-eligible: STARTING the ONE guest whose node and storage are both healthy
(predict: status `running`, service checks resume). There is no reboot op-class: a guest that must be
power-cycled is a `stop-guest` then a `start-guest` — two separately-reviewed proposals, never one reboot.
