<!-- spec/019 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/019 — Threat model: Scheduling awareness & the maintenance-window seam (STRIDE slice)

Per-feature threat slice for the read-only Cronicle scheduling connector and the `core/schedule` seam. The
system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the connector's own
trust boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The connector reads the estate scheduler's events read-only over the scheduler's REST API
and derives an ephemeral `core/schedule.Calendar`; its output is the `MaintenanceWindow(ctx, target, now)`
answer the actuation path may consult to DEFER a change. Its inputs are the operator-declared connector config
(base URL + a sealed API-key `SecretRef`) and the scheduler's event set (recurrence, timezone, notes). The
scheduler is an external system TG imports a signal from; the asset is the correctness of the "is this a
sanctioned time to act?" answer. Adversaries of interest: (a) an unreachable or hostile scheduler that could
make TG believe it is safe to act; (b) an attacker attempting to read the API key at rest or in transit;
(c) a poisoned event that widens a maintenance window or forges a target scope.

**The deliberate posture (threat-modelled honestly).** The maintenance-window signal is advisory to a chain
that already fails closed: a resolved "in-window" is NEVER sufficient for actuation — the mechanical never-auto
floor (INV-09), the mode chokepoint, and the policy engine all remain in force beneath it. The one place the
signal is load-bearing is the FAIL-SAFE direction: when the schedule is unreadable the seam reports OUTSIDE the
window, so a hostile or dead scheduler can only make TG MORE conservative, never less.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A spoofed/MITM scheduler serves a forged wide-open maintenance window so TG believes any time is sanctioned | The connector talks only to the operator-declared base URL over TLS (optional private CA); the signal is advisory beneath the never-auto floor + mode chokepoint (in-window is never sufficient to actuate); an unrecognised directive kind is skipped, never a guessed window | REQ-1901, REQ-1908, INV-09 |
| **Tampering** | A poisoned event widens a window, forges a target scope, or sets an always-open recurrence | Windows are derived from the event's OWN recurrence + timezone with a duration capped at 24h (an over-long window is skipped-with-record, not treated as always-open); a change-freeze denies over any overlapping maintenance window (deny-overrides); target scope defaults to the event's own target | REQ-1901, REQ-1904, REQ-1905 |
| **Repudiation** | A deferral decision cannot be explained later | `MaintenanceWindow` returns a stable non-secret reason string (unreadable / freeze / outside / in-window with the source event id), which the actuation interceptor records as a refusal in the tamper-evident ledger when it consults the seam | REQ-1903, REQ-1906, INV-19 |
| **Information disclosure** | The scheduler API key is exfiltrated at rest, in transit, or via logs | The key is a `SecretRef` (env:/file:/store:) resolved at request time, cached in memory, sent only as the `X-API-Key` header, and never placed in a URL, log line, error message, or the `Calendar.Note`; no plaintext key in config or any exportable artifact; gitleaks CI backs the no-literal-secret rule | REQ-1907, INV-13 |
| **Denial of service** | An unreachable, slow, or paginating-forever scheduler blocks or hangs the evaluation | Each round-trip is timeout-bounded; pagination is page-capped; a body read is size-limited; any transport/status/decode/non-zero-code failure returns an unreadable `Calendar` promptly, which the seam answers conservatively | REQ-1900, REQ-1903, INV-09 |
| **Elevation of privilege** | The read-only connector is used as an effect channel, or an unreadable schedule fails OPEN to "safe to actuate" | The connector is native `net/http` only — no subprocess, no execute path, actuates nothing (INV-02); the seam's conservative default is OUTSIDE-the-window, so an unreadable/ambiguous/frozen state defers rather than permits (INV-09); a nil guard is inert (window-gating is opt-in, no path when unconfigured, INV-17) | REQ-1903, REQ-1908, INV-02/INV-09/INV-17 |
