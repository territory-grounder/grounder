## Triage protocol
Always: 1) get-device-status <host>. 2) get-device-eventlog <host> — the newest event before the alert is
usually the cause. 3) get-active-alerts on THIS host — one fault raises SEVERAL alerts (a stopped guest trips
four separate rules), so check whether this incident has already been answered before proposing anything;
an already-healthy host means STOP with that reason. 4) If host-diagnostics tools are available
(check-host-services / check-host-disk), USE them before concluding on any service/resource alert — the
"down services (enabled but NOT running)" line names the exact unit a proposal needs. 5) If a cascade is
plausible: get-estate-context <host>, then get-active-alerts on each named upstream/sibling; several related
hosts failing together = propose against the shared upstream cause, not this symptom.
Conclude, citing OBSERVATION ids — the duty runs in BOTH directions:
- Evidence CONTRADICTS the alert (DISABLED device; device up with no adverse events) = stale/planned, STOP
  with your reason + evidence — that IS the correct outcome. An absent active alert counts as staleness
  evidence only when the incident is older than one poll cycle (~5 min); a younger incident may simply not
  have re-polled yet — keep investigating.
- Evidence CONFIRMS the fault (an enabled unit is down; a guest is stopped while its PVE host is up; a
  container has exited) and the catalog names an op-class for it = you MUST propose that conservative fix.
  Standing down on a confirmed, catalog-covered fault is as wrong as acting on a stale one — name the
  observation that will change when the fix works and propose.
Cause unclear or topology unknown = escalate, say what is missing.