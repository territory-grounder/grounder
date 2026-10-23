## Goal
Decide whether a single Proxmox guest (a VM or an LXC) that is down or unreachable is a GUEST-lifecycle
fault — the one Proxmox plane where a reversible action is a candidate — or a consequence of a node,
storage, or cluster fault that a guest action would only paper over. This is the actionable plane: a
stopped guest whose node and backing storage are both healthy can be a reversible `qm start` / `pct start`
candidate. But the ordering is strict, because the most common error is restarting a guest that is only
"down" because its HOST is down — which does nothing, or worse, races the host's own recovery.

## Required evidence
- The guest's own state: `qm status <vmid>` (VM) or `pct status <vmid>` (LXC) on its node — `running` /
  `stopped` and, for a VM, whether it is paused or its config points at missing storage.
- The NODE's health FIRST: is the node up, quorate, and not wedged? A guest cannot be triaged in isolation —
  confirm the host is serving before proposing any guest action (see the node/storage and cluster runbooks
  for the host-plane checks).
- The backing storage: `pvesm status` and the guest's disk location — a guest that will not START is very
  often a storage fault (a degraded/again-unavailable pool, an NFS/iSCSI target that vanished), not a guest
  fault. Restarting into gone storage just fails again and buries the cause.
- WHY it stopped: the task log (`/var/log/pve/tasks/…` or `qm`/`pct` recent tasks) and the guest console —
  an internal crash, an OOM, a failed boot, or an operator/scheduled stop each imply a different response.
- On a host that just recovered: whether the guest is `onboot: 1` and simply has not been reached yet by
  the node's ordered autostart, versus a guest that autostart tried and failed.

## Decision rules
- **A stopped guest whose NODE is UP and whose STORAGE is AVAILABLE, with a named reason it stopped, is a
  reversible start candidate** — the band is still machine-enforced (poll by default), and the prediction is
  concrete: status `running`, and the guest's own service checks (pings/HTTP) resume. Never propose a blind
  start: name why it stopped first, or a spec/boot fault is hidden by the restart.
- **A guest that is "down" only because its NODE is down is a NODE-plane fault** — do not propose a guest
  action. The guest returns with the node; a guest start against a down host is a no-op that muddies the
  timeline.
- **A guest that FAILS to start is storage-first** — check the backing pool/target before a second start
  attempt. A start that fails identically twice is evidence about storage, not the guest.
- **On a freshly recovered host, prefer letting the node's ordered autostart finish** before manually
  starting an `onboot: 1` guest it simply has not reached yet — a manual start is for a guest autostart
  tried and left down, not for one still in the queue.

## Verification
- After a start, the guest reports `running` AND its own service checks recover (the alert that opened the
  incident clears at the source, not just the hypervisor's status line) — a guest `running` whose service
  is still down means the fault was inside the guest, not its lifecycle.
- The reason it stopped is named in the record, so the start is a remediation with a cause, not a reflex —
  and if the same guest stops again shortly after, that recurrence is the real signal (a crash loop, a
  storage flap) and escalates rather than restarts a third time.
- No host-plane action was taken to achieve the guest recovery — the node was confirmed healthy, not made
  healthy.

## Never do
- NEVER act on the HOST to recover a guest (reboot/reset/power-cycle/migrate-all the node) — that is the
  node plane's never-auto floor and endangers every other guest the node carries.
- NEVER `qm`/`pct` a reboot as one action to "fix" a guest that needs power-cycling — a stop then a start
  are two separately-reviewed proposals; there is no single reboot op-class, by design.
- NEVER restart a stateful guest (a database, etcd, a message store) to clear a symptom without confirming
  it is safe — a restart can lose in-flight state; that is a human decision.

## Hand-off shape
When the fault does not cleanly sit in the GUEST plane with a healthy node AND available storage, stop and
name what a human needs — the node/storage/cluster fact that must be addressed first. The reversible guest
start is earned only after the host is confirmed serving; otherwise the correct output is a precise
escalation, not an action.
