## Alert-class playbooks (all fixes are advisory proposals; mutation is OFF)
FIRST, before any playbook: get-active-alerts on THIS host. One fault raises SEVERAL alerts here (a stopped
guest trips four separate rules), so another session may already have answered this incident. If the host is
already healthy, or a sibling alert for the same fault is already being acted on, the correct outcome is to
STOP with that reason — not to re-propose a fix for something already fixed. Measured: 3.4 triage runs per
device-down incident, most of them re-answering a settled question.
DISABLED device or evidence-contradicted (stale) alert -> no action: stop with your cited reason.
Confirmed fault with a catalog op-class -> PROPOSE it; stopping on a confirmed, coverable fault is the
symmetric error to acting on a stale one (measured: the eval corpus's action-warranted incidents were being
stood down at 100% while the same faults healed fine in production).
Guest down, its PVE host up -> start-guest for that ONE guest. Multiple guests of one host down -> the
host is the fault, escalate.
Latency correlated across a path -> upstream link problem, never restart an endpoint; escalate.
Port admin-down = intentional; rising error counters = physical (SFP/cable) -> escalate, do not bounce.
Disk filling -> read the "can the root filesystem be GROWN?" line in check-host-disk. On a LOOPBACK-backed
rootfs (/dev/loopN, which is every LXC guest here) disk-grow CANNOT work — proposing it is an error, not a
heal. THERE IS NO prune/trim/vacuum op-class: the correct outcome is to STOP and name the consuming path
(from du / journal) so a human knows where to look. Do not substitute the nearest available op-class for the
one you actually need.
Memory -> restart the offending unit, but NEVER a stateful DB/etcd/redis/prometheus unit -> human poll.
Sustained load -> escalate for capacity.
Flapping = link/power/thermal, a restart will NOT fix it -> escalate. Environmental (temp/PSU/fan) -> escalate.