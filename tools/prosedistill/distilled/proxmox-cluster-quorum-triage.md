## Goal
Separate three Proxmox cluster-plane conditions that look alike from the alert but demand opposite
responses: a BENIGN member-count alarm after the cluster changed size, a pmxcfs (the `/etc/pve` cluster
filesystem) WEDGE that masquerades as CPU saturation, and a REAL quorum/corosync fault. The expensive
mistake here is rebooting or power-cycling a node to "clear" a wedge when a no-guest-impact software path
exists — a reboot takes every guest on that node down and risks the boot-time traps a hypervisor carries.
NOTE — plane-gated content: cluster and node actions on a hypervisor are never TG's; TG's only Proxmox
write competence is single-guest lifecycle. Everything below is read-only diagnosis whose output is a
precise human hand-off — and, for the wedge case, the exact no-impact recovery a human can run.

## Required evidence
- `pvecm status` and `pvecm nodes` — `Quorate: Yes/No`, expected vs. found votes, and each member's ring
  state. This is the ground truth for whether quorum actually exists.
- `corosync-cfgtool -s` — the ring/link status per node. A `check_cororings`-style monitor parses exactly
  this and asserts the connected-member count equals a configured expectation.
- The load-versus-CPU signature: `uptime` (load average) against `vmstat 1 3` (CPU idle %, iowait %). A
  load average of dozens-to-hundreds while the CPU sits ~idle with ~0% iowait is NOT saturation — it is
  many tasks in **D-state** (uninterruptible sleep), the pmxcfs-wedge fingerprint.
- `ps -eo stat,wchan:32,cmd | grep ' D'` — count management processes (`pvesh`/`pvestatd`/`qm`/`pct`/
  `pveproxy`) stuck in D-state, especially with a `wchan` against the cluster filesystem. A rising count is
  the wedge canary.
- `zpool status` / `df` on the node — to CONFIRM the wedge is not a storage fault. In the canonical wedge
  the pool was 2% used and corosync was quorate; the fault was the FUSE filesystem alone.

## Decision rules
- **Member-count alarm after a size change is a THRESHOLD-config alarm, not a cluster fault.** A monitor
  reporting "expected N connected nodes but found M" right after a node was added or removed is stale
  configuration (the expected-count is hard-coded per node's service). The fix is the monitoring parameter,
  not any cluster action — and it is an operator/monitoring change, never a hypervisor action.
- **High load + idle CPU + D-state management procs against the cluster FS = a pmxcfs WEDGE, not CPU
  pressure.** Do not read it as `NodeSaturation`; do not add or resize CPU; do not reboot as a reflex. The
  guests on the node go down WITH the wedged host, so the priority is the no-impact recovery below, not a
  power-cycle.
- **Genuine quorum loss / a down ring is real and dangerous.** Never propose an action that could
  split-brain or trigger a fence. Read cluster membership, name the lost votes/ring, and escalate — a
  cluster-safety mechanism is a human decision, always.

## Verification
- For a wedge, the recovery a human runs is `systemctl restart pve-cluster` FIRST — tearing down the FUSE
  mount returns EIO to the blocked syscalls and releases the D-state pile-up INSTANTLY, with no guest
  impact — then `systemctl reset-failed pvestatd && systemctl restart pvestatd`. Verify: the D-state
  management-process count drops to zero, pmxcfs responds (`pvesh get /cluster/resources` returns
  promptly), and the node's guests were never interrupted.
- Restarting `pvestatd` ALONE does NOT clear a wedge — it re-enters D-state against the still-hung
  filesystem. The `pve-cluster` restart is the load-bearing step; confirm it was the one taken.
- For a member-count alarm, verify the alert clears after the monitoring expectation is corrected AND that
  `pvecm status` reports the full membership Quorate — the cluster was healthy all along.

## Never do
- NEVER reboot, power-cycle, or fence a node to clear a pmxcfs wedge — the no-impact `pve-cluster` restart
  exists, and a reboot both drops every guest and exposes the node's boot-time traps.
- NEVER `kill -9` a D-state process to "unstick" it — D-state ignores SIGKILL; only unwedging the
  filesystem (or a reboot) reclaims it, and trying wastes the window.
- NEVER take any action that could split-brain the cluster or trip fencing.

## Hand-off shape
Name which of the three conditions it is (member-count-config / pmxcfs-wedge / real-quorum-fault), the
evidence that distinguishes it (the load-vs-CPU-vs-D-state triad rules out saturation; quorate status
rules in/out a real fault), the guests at risk on the affected node, and — for a wedge — the exact
no-impact recovery step. The value is a correct classification that keeps a node's guests up, not an action.
