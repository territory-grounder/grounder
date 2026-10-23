## Goal
After ANY recovery — a heal, a manual fix, an exercise — re-prove baseline layer by layer before
calling the incident closed. "The alert cleared" is one layer's opinion; the fault touched several,
and the un-revalidated layer is where the next incident is already growing.

## Required evidence
- The fault's blast radius, which defines the checklist: every layer the fault touched gets its own
  named check with its own instrument. Typical ladder: connectivity/paths, the service itself,
  dependent workloads, cluster/quorum state, protection-system state, suppression state, monitor
  state.
- `get-device-status <host>` and `check-host-services <host>` for the healed target and each dependent
  named by `get-estate-context <host>`.
- `get-active-alerts` — for the target AND its dependents, after allowing the monitor its polling
  cycle.
- The post-state observation the fix's own prediction named (the verifier's axis) — necessary, but not
  the whole ladder.

## Decision rules
- One check per layer, each with its own instrument: a tunnel can be up while a session table is
  poisoned; a container can run while its service crash-loops. No layer's health is inferred from
  another's.
- Restart counts are an instrument: a recovered workload that has restarted repeatedly since recovery
  is not recovered — it is crash-looping slowly. Check the count, not just the current state.
- Clean up the incident's OWN residue: maintenance/exercise flags, suppression windows, protection-
  system entries (blocks/shuns) accrued during the fault. A leftover suppression flag masks the next
  real incident; that check belongs to closing this one.
- Lingering-alert patience, both directions: an alert may take a polling cycle or two to clear after a
  genuine fix — and a "clear" monitor may lag a genuine regression. Wait out one cycle before
  believing either; if it still fires, the condition is real or the alert is stale — decide which,
  with evidence, before acknowledging anything.
- Stuck-state discrimination: a dependent stuck half-recovered (session established but no traffic,
  peer in a connecting state) means the layer BELOW it did not fully recover — descend, do not
  re-kick the dependent.
- The incident closes when every layer's check passes; a partial pass is a finding, not a footnote.

## Verification
- The record shows the per-layer checklist with each check's instrument and result — including the
  suppression-cleanup and protection-state rows.
- Re-checks after the polling window are timestamped, so "waited a cycle" is visible, not asserted.
- The next similar incident's triage can retrieve this record via `get-incident-history` — closure
  includes landing the outcome where precedent lookup will find it.
