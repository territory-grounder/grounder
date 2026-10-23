## Storage appliance triage (this estate's dedicated storage is Synology DSM NAS units; PVE-node ZFS and k8s volumes are their own lanes and never route here)

This skill sharpens WHICH fault and the SHAPE of the fix on a storage-appliance incident. It does NOT
decide whether an action runs and it does NOT lower the risk band — the band is machine-enforced and
every proposal still goes to the gate at the band the classifier assigns. Nothing below is a licence to
auto-act; treat every proposal as poll-by-default and let the machinery decide.

On an appliance there is NO reversible verb: nothing here is a unit to restart or a guest to start, and
the estate declares no storage op-class. The honest terminal outcomes are a reasoned stand-down (the
alert is stale/normal-state) or an escalation that NAMES the failed member — proposing an action is the
error. The fault lives in ONE of three planes — name the plane:

CAPACITY plane (a volume filling). Read the storage-health table before concluding: each volume reports
its own percent AGAINST ITS OWN warn threshold. What matters is TRAJECTORY, not the threshold crossing —
a volume that has sat at 87% for weeks is a capacity plan, not an incident; one that gained ten points
overnight has a producer to name (the eventlog often shows which share or service). Space on an
appliance is never TG's to reclaim: no grow verb applies to a NAS volume and deleting data is a human
decision — escalate with the numbers (volume, percent, size, what changed) rather than a bare "disk
full".

ARRAY plane (RAID/pool/disk state). The state sensors are the evidence: on these MIBs a state sensor
reads 1 when normal — any other value is the fault detail, and the sensor's own name says WHERE (Disk N
= a member drive; Storage Pool N / Volume N = the array above it). A degraded pool with all disks normal
usually means a rebuild in progress — say so and escalate calmly with the member states quoted; a failed
DISK is a hands-on-hardware event — escalate naming the slot, never anything else. NEVER propose
rebooting the appliance to clear an array or sensor alarm: a reboot interrupts rebuilds and scrubs,
risks the array, and clears only the messenger.

APPLIANCE plane (the NAS itself unreachable or its system sensors alarming). An appliance-down is
storage-plane, never a guest fault: there is no start verb for hardware, and hosts that mount its shares
are the blast radius to check (the estate graph knows who depends on it) — name them in the escalation
so the human sees the reach. A temperature/fan/PSU sensor over limit is environmental: quote the sensor
and its value, hands-off, escalate.

DEFAULT POSTURE is read-only diagnosis + human approval — and on this platform the diagnosis IS the
deliverable: the failed member named, the trajectory quantified, the blast radius listed.

NEVER auto (human poll, always): rebooting or shutting down the appliance; any volume/pool/disk
operation (create/expand/repair/eject/scrub); package or service operations on the NAS; anything that
touches the data the appliance holds.
