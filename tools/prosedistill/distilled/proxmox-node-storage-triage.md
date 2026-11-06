## Goal
Decide whether a Proxmox VE node's disk/storage alert is a SURVIVABLE degraded-redundancy event the node is
still serving through, or a real loss of service — and, above all, keep the triage READ-ONLY on the node
itself. The defining failure mode of this plane is not the hardware: it is a well-intentioned remote "revive"
of a wedged controller that converts a degraded-but-serving hypervisor into a full outage of every guest it
carries. A single-disk failure in a ZFS boot mirror is a SEV-you-can-schedule; a hung host is a SEV-1 with
every guest down. NOTE — plane-gated content: node, storage, and cluster-plane actions on a hypervisor are
never TG's to take. TG's only Proxmox write competence is the guest LIFECYCLE (start/stop a single VM/LXC
whose node and backing storage are healthy); everything below is read-only diagnostic guidance whose
conclusion is a human hand-off with the physical or cluster-level facts named.

## Required evidence
- `zpool status -v <pool>` (usually `rpool`) — the vdev tree and per-device state (`ONLINE` / `DEGRADED` /
  `FAULTED` / `REMOVED`), and critically the `errors:` line. `errors: No known data errors` on a DEGRADED
  mirror means ZFS absorbed the fault and the data is intact — the node is serving.
- `journalctl -k --since -2h | grep -iE 'nvme|ata|scsi|I/O error|medium error|timeout'` — the kernel's own
  device story: I/O timeouts, a controller reset, an FLR (function-level reset) that gives up, and the point
  where the kernel disabled the device. The disable event is when ZFS marks the vdev `REMOVED`.
- `pvecm status` and `pvecm nodes` — quorum and the corosync ring, to confirm the node still holds cluster
  membership (a storage fault and a quorum fault are different planes; do not conflate them).
- `qm list` / `pct list` on the node — the guests it carries, to size the blast radius of ANY node-level
  action BEFORE considering one. A downed node takes its guests with it as a consequence.
- `proxmox-boot-tool status` (or `efibootmgr -v` plus the ESP contents) — which mirror members actually
  carry a synced boot loader. This is the latent trap below; read it while the node is still up, not after.
- `lspci -k` for any address-pinned passthrough (`vfio-pci` `driver_override`) — the second latent trap.

## Decision rules
- A DEGRADED mirror with zero data errors is a REPLACE-THE-DRIVE event, not a revive-the-controller event.
  The correct disposition is: escalate for physical replacement + resilver, keep the node serving, and do
  NOT attempt any of the "software" recovery reflexes below.
- A `FAULTED` or `REMOVED` vdev on a NON-redundant pool (single disk, or a mirror already down to one member)
  is a real service-risk: the node is one more fault from an outage. Still not a node-action for TG — it is
  an urgent human escalation with the redundancy state named.
- Distinguish the DEVICE plane (a disk/controller) from the NODE plane (the host) from the CLUSTER plane
  (quorum/corosync). A disk event that has NOT taken the host down is a device event; treat the host as
  healthy and do not touch it.

## Verification
The disposition of this plane is a correct escalation, so its verification is that the node stayed served and
the redundancy was restored the right way — not that an action succeeded:
- After a drive replacement + resilver, `zpool status` shows the vdev `ONLINE` and a completed `scan:
  resilvered` line with `0` errors; the pool is redundant again.
- Throughout the degrade, the node's guests kept running (`qm`/`pct list` unchanged, guest service checks
  green) — confirming the degrade was survivable and no host-level action was taken.
- Both mirror members now carry a current, synced boot loader (`proxmox-boot-tool status`), and passthrough
  is pinned by device identity, so the NEXT disk failure is a scheduled swap, not an hours-long boot failure.
- If instead the host DID go down, the verification is the timeline read-back: the outage began at the moment
  of a node-level action, not at the moment of the disk fault — the signature this runbook exists to prevent.

## Never do (the lesson this runbook exists for)
- NEVER attempt a remote "revive" of a wedged storage controller — detaching/re-scanning it via sysfs
  (`echo 1 > .../remove`, a PCIe secondary-bus reset, `nvme reset`, a driver rebind). On a live hypervisor
  this can hang the host with no panic and no clean shutdown, taking every guest offline. The observed
  outcome of exactly this action was an 11-hour full outage grown from a survivable single-disk degrade.
- NEVER reboot, power-cycle, fence, or migrate-all a node from triage to "clear" a storage fault — the
  reboot is where the two latent traps below detonate, and a node that was serving stops serving.
- NEVER run a destructive pool op (`zpool destroy`/`offline` the last member, `zfs destroy`, a disk wipe)
  or clear a corosync/quorum device — these are irreversible or estate-partitioning and are on the
  never-auto floor.

## The latent traps that make recovery long (check them while the node is UP)
A single-disk failure is minutes to schedule; the outages that run for HOURS are made by configuration that
was invisible until hardware was removed:
- **Boot loader on only one mirror member.** If the ESP was never synced across the mirror and the member
  that dies is the one carrying the loader, the node falls back to a stale ESP and can boot an ancient kernel
  whose modules (e.g. the bonding driver) no longer exist on disk — so the node comes up with no network.
  Verify both members carry a current loader NOW; it is a five-second read that prevents an hours-long boot
  failure later.
- **Address-pinned PCI passthrough after renumbering.** A `driver_override=vfio-pci` pinned to a PCI address
  can land on the WRONG hardware once a device is physically pulled and the bus renumbers — hiding a healthy
  rpool disk or a bonded NIC port from its driver. Pin passthrough by device identity, not bus address, and
  audit it before any planned disk replacement.

## Hand-off shape
State the plane (device / node / cluster), the redundancy status in ZFS's own words (`DEGRADED, 0 errors`
vs `FAULTED, non-redundant`), the guests at risk, and the specific physical or cluster action a human must
take (replace-and-resilver, sync the ESP, correct the passthrough pin). Name what must NOT be attempted
remotely. The value of this triage is a correct escalation that keeps a serving node serving — not an action.
