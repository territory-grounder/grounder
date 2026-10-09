## Goal
Triage judgment for storage incidents — NAS, pools, network block/file serving — where the defining
fact is that storage faults CASCADE upward into consumers and storage "fixes" can destroy what they
mean to save. NOTE — tool-gated content: TG has no storage-vendor read tools wired; this runbook is
knowledge-library material until a vendor-official, read-only surface lands. `check-host-disk` covers
guest filesystems; the vendor layer below it does not exist as a TG tool yet.

## Required evidence
- The host's OWN filesystem and pool answer (df, pool status) — never only the vendor panel's number:
  management UIs lag and aggregate differently, and the divergence can be tens of gigabytes during
  background tasks.
- Export/target state and per-consumer connection health for whatever the storage serves.
- The replication factor and placement of any distributed storage involved — BEFORE any action that
  removes a node or replica from service.
- Consumer-side symptoms correlated in time: workload evictions, database fsync latency, control-plane
  restarts.
- `get-estate-context <host>` for who depends on this storage; `get-incident-history <host>` for
  recurring capacity patterns.

## Decision rules
- Never propose a reboot to "clear" a storage alert: storage restarts cascade — initiator timeouts,
  write-log corruption, dependent control-plane restarts. The fix must address the cause without a
  restart unless the remediation itself requires one, stated as such.
- Grade capacity alerts by TRAJECTORY, not threshold: usage plus growth rate gives days-until-full;
  a 90% filesystem growing 0.1%/day and one growing 5%/day are different incidents.
- Distrust single-copy assumptions: read the ACTUAL replication factor from the system, not from what
  the system is named — "replicated storage" running replication-off means every removal is data loss.
  Any drain/removal proposal must cite the observed factor and placement.
- When compute-layer symptoms (evictions, fsync stalls, apiserver churn) co-occur with a storage
  alert, the storage is upstream until proven otherwise — fix ordering follows the dependency
  direction.
- Thin-provisioning makes free-space arithmetic lie in both directions; reconcile the pool-level and
  filesystem-level numbers before concluding either.

## Verification
- Capacity claims cite both the host-level read and the trend window used for the forecast.
- Any replica/node-removal proposal in the record carries the observed replication factor and
  placement facts.
- After remediation, consumer-side symptoms are re-checked and shown quiet — a storage fix is not
  verified by the storage layer alone.
