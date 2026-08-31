## Goal
When a whole site reads "down", decide between DOWN and ISOLATED — because when the telemetry path is
itself the failed component, normal monitoring structurally cannot tell you, and the wrong verdict
("needs physical access, no path in") has been written for a site that was alive and self-recovering.
NOTE — tool-gated content: the in-site observer and out-of-band signal path described here are estate
mechanisms TG reads about but does not operate; knowledge-library material.

## Required evidence
- What the alert path CAN see: which observations travel over the path that just failed? Every one of
  them is uninformative right now — list them as such, do not count them as evidence of death.
- Any in-site observer's report over an OUT-OF-BAND channel: a probe running inside the site, with a
  signal path that does not traverse the failed link (a secondary uplink, a message gateway).
- The two-sided probe that discriminates the verdicts: can the site reach its mesh/tunnel anchors, and
  can it reach the public internet? Mesh down + internet up = ISOLATED (alive, cut off); both down =
  DARK (hard-down, or the observer's own uplink died — still ambiguous, say so).
- `get-estate-context <site-host>` for which functions are site-local (keep running during isolation)
  versus cross-site (genuinely lost).

## Decision rules
- Unreachable-from-here is a claim about the PATH, not the site: before concluding site-down, name at
  least one observation that does not share the failed path. If none exists, the honest verdict is
  "unknown — telemetry path lost", not "down".
- Probe anchors are spread across DISTINCT devices, any-up semantics: one dead anchor host must not be
  able to fake an isolation verdict.
- Ride out transient blips with a confirmation window (rekeys and re-leases look like isolation for a
  minute or two); alert on confirmed state change, then re-notify periodically while it persists and
  send a recovery notice with the measured duration.
- A backup uplink that carries egress but cannot carry the mesh (carrier-grade NAT, no stable inbound
  endpoint) is a STRUCTURAL gap: during isolation the site is alive, self-monitoring, and unreachable
  for control — plan for that state in advance rather than discovering it mid-incident.
- Cause attribution (uplink flap, address re-lease, carrier event) ENRICHES the isolation verdict but
  never gates it — send the verdict when the probe confirms, attach the cause when the local logs
  yield it.
- The single-observer weakness is honest scope: if the in-site observer host dies, the mechanism is
  blind; its own liveness needs an independent heartbeat (see platform-self-healing).

## Verification
- The verdict in the record is one of ISOLATED / DARK / DOWN-CONFIRMED, each backed by the probe
  evidence that discriminates it — never "down" inferred solely from silence.
- Isolation begin/end and duration are recorded; the recovery notice matches the monitor's own
  re-convergence.
- The structural-gap statement (what cannot be reached during isolation, and why) exists BEFORE the
  next isolation — verified by finding it via `get-incident-history` when one recurs.
