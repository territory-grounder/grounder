## Goal
Build a queue-level picture of everything currently firing BEFORE deep-diving any single alert: how many
distinct incidents, how old, what is flapping, what is stale. Deep triage of one symptom while the queue
holds its cause (or nine siblings) wastes the session on the wrong target.

## Required evidence
- `get-active-alerts` with no target — the full estate-wide queue, not one host's slice.
- For each distinct host in the queue: first-seen age and current severity of its alerts.
- For any alert suspected stale: `get-device-status <host>` — the live state, not the alert row.
- Recent fire/clear pairs for the same <alert-rule> on the same <host> (flap evidence).

## Decision rules
- Group alert rows into INCIDENTS first: one fault raises several alerts (a stopped guest can trip four
  separate rules on itself and its parent). Count and report incidents, never raw rows.
- An alert older than 4 hours with no state change since is a staleness suspect — but verify with
  `get-device-status` before dismissing: a stale-LOOKING alert on a genuinely down device is not stale.
  Dismiss only with the contradicting live observation cited.
- The same rule firing and clearing repeatedly within an hour is a FLAP. Do not triage each instance;
  the oscillation itself is the fault — investigate what is cycling.
- Alerts arriving together on several hosts that share an upstream are a BURST — hand the set to the
  burst-correlation procedure instead of walking them one by one.
- Work order: newest critical first, then the oldest unhandled item — an old warning nobody owns rots
  into an outage.

## Verification
- After an incident is handled, re-run `get-active-alerts`: every row that incident owned is gone or
  acknowledged, and the queue count fell by exactly that number.
- A row that survives when its incident was declared resolved is a finding, not noise — name it and
  return it to the queue as its own incident.
