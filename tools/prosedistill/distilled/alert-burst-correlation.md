## Goal
When several hosts alert at once, treat the set as ONE event until the evidence says otherwise. Triage
the shared cause, not N symptoms — and escalate once, not N times. Per-host triage of a burst produces
N shallow diagnoses of one upstream fault.

## Required evidence
- The full `get-active-alerts` snapshot: every host in the burst, each alert's rule and first-seen time.
- `get-estate-context <host>` for EACH burst member — the shared parents (hypervisor, switch, uplink,
  power domain) are the correlation candidates.
- `get-device-status` on each shared-parent candidate — is the common upstream itself down or degraded?
- Per-host findings gathered WITHOUT individually escalating any member.

## Decision rules
- Two or more hosts alerting within one poll cycle → open one MASTER incident summarizing the burst, and
  attach each host's investigation as a linked child. The master owns the correlation question; the
  children own per-host evidence.
- Correlate through topology, not through similarity of symptoms: a shared parent that is down or
  degraded makes the burst that parent's fault — diagnose and propose against the parent, and say the
  children are expected collateral.
- No shared topology found → the "burst" is coincidence. Split it into independent incidents and state
  explicitly that correlation was tested and refuted — a silent split looks like a missed correlation.
- Escalate the MASTER exactly once, carrying the correlation verdict. Never escalate children — an
  escalation storm from one event buries the signal it carries.
- Keep child investigations lightweight until the correlation verdict lands: deep per-host work before
  the upstream check is usually thrown away.

## Verification
- The master names every child and states the correlation verdict with the topology evidence cited.
- If the upstream fix is right, the children clear WITHOUT per-host action: `get-active-alerts` empties
  for the burst set after the parent recovers.
- A child that survives the upstream fix was not that parent's collateral — reopen it as an independent
  incident and say why the correlation excluded it.
