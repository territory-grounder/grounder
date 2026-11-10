## Goal
Attribute high CPU or memory on IOS or ASA to a specific process, traffic source, or cause before
reaching for a reload — the interrupt/process split and the per-process breakdown almost always name the
culprit directly, and a reload destroys that evidence along with the symptom. NOTE — tool-gated content:
TG has no Cisco CLI tool wired yet (the interactive-SSH transport is separate, not-yet-built work); this
runbook is knowledge-library material until a vendor-official, read-only surface lands. Every command
below is read-only diagnostic guidance.

## Required evidence
- IOS: `show processes cpu sorted [5sec|1min|5min]` — load sorted highest-first, with the first line's
  interrupt-percentage figure read SEPARATELY from any single process's share.
- IOS-XE: `show processes cpu platform sorted` — the underlying platform/Linux process view, for when the
  IOS process list alone doesn't explain the load.
- IOS: `show processes memory sorted` / `show memory statistics` — memory-holding processes by allocated
  bytes, for attributing a memory climb to a specific process rather than "the box."
- ASA: `show processes cpu-usage sorted [non-zero]` for the per-process breakdown, and `show cpu usage`
  first for the single rolled-up figure before drilling in.
- ASA: `show memory detail` and `show blocks` — buffer-block exhaustion (a specific block size chronically
  at or near zero) produces "the box is slow" symptoms independent of the CPU commands above.
- ASA: `show conn count` and `show local-host [detail]` — a connection-table flood or a single
  misbehaving/scanned host is one of the most common high-CPU root causes on a firewall and is invisible
  from the CPU commands alone.

## Decision rules
- Split interrupt from process load first: a high interrupt percentage with no single dominant process in
  the sorted list is a traffic-volume/punt problem (check what is hitting the control plane — ACL
  logging volume, a routing flap generating updates, an attack) — chasing a "high CPU process" here is
  the wrong hunt.
- A single named process dominating the sorted list points at that subsystem specifically — crypto/IPsec
  processing, routing-protocol churn, SNMP polling, logging — treat the process name as the lead, not
  background noise to scroll past.
- On ASA specifically, crypto-map-driven IPsec processing (as opposed to VTI/route-based tunnels) is a
  known, named cause of elevated CPU under load — if the process list points at crypto and any legacy
  crypto-map bindings remain alongside newer VTI tunnels, that overlap is the first thing to check.
- Don't reload as a first move: a reload clears the exact process and connection-table state that would
  show root cause, along with the symptom, and on a redundant-path device causes its own outage window.
  Attribute first; reload only as a last resort after attribution, or as a scoped, notified action.
- Distinguish a slow climb that plateaus (normal — a cache or table filling at steady state) from a climb
  that keeps going with no plateau (a leak candidate) — a single memory reading is never enough to tell
  them apart; the call needs at least two observations over time.
- A connection-count or per-host flood finding (`show conn count`, `show local-host`) changes the response
  entirely — this is a security/capacity incident (an internal host to quarantine, or a DoS pattern to
  filter), not a device-tuning problem, and should route accordingly rather than being treated as "the
  box needs more headroom."

## Verification
- `show processes cpu sorted` (or the ASA equivalent) re-read after the fix shows the previously-dominant
  process back to baseline — not just an overall percentage that happened to be lower on one sample.
- If a leak was suspected, memory is confirmed stable (not still climbing) across a second observation
  window after the fix, not just a lower number taken once.
- Whatever named the root cause — a process, a flooding host, an interrupt source — is recorded; "CPU is
  fine now" with no attribution is a symptom note, not a closed triage.

## Doc basis
- Cisco: Use the Show Processes Command —
  https://www.cisco.com/c/en/us/support/docs/ios-nx-os-software/ios-software-releases-120-mainline/15102-showproc-cpu.html
  (`show processes cpu sorted`, interrupt-percentage interpretation).
- Cisco: Troubleshooting High CPU Utilization (Catalyst 3750 family) —
  https://www.cisco.com/c/en/us/td/docs/switches/lan/catalyst3750/software/troubleshooting/cpu_util.html
- Cisco: Monitor and Troubleshoot ASA Performance Issues —
  https://www.cisco.com/c/en/us/support/docs/security/asa-5500-x-series-next-generation-firewalls/113185-asaperformance.html
  (`show processes cpu-usage`, `show memory detail`, `show local-host` for DoS attribution).
