<!-- spec/006 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/006 — Threat model: interface contracts (STRIDE slice)

Per-feature threat slice for the interface boundary — the authenticated HTTP router, the
`triage.requested` ingest event, the governed persistence contracts, and the generated wire contracts.
The system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the
boundary this spec owns and is the security half of its definition-of-done.

**Trust boundary.** This is the outermost boundary of Territory Grounder: it separates the untrusted
external estate (alert providers, the operator console, replay callers) from the governed spine. The
predecessor exported roughly 25 unauthenticated webhooks and hand-maintained parallel contracts; the
adversary of interest is an unauthenticated or under-privileged caller trying to trigger a receiver, replay
a signed request, resume a mutating session with injected input, read rows it has no authority over, or slip
past a stale contract.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | An unauthenticated caller triggers a stats, replay, or ingest endpoint | Every route is registered on the mandatory auth router; a caller is authenticated (mTLS or per-source HMAC) before the handler runs, and a route declared `auth=none` panics at boot rather than serving open | REQ-501, INV-01 |
| **Tampering** | A signed request body is altered in flight, or the wire contract drifts from the code | The HMAC covers the raw body with timestamp and nonce, so an altered body fails verification; the `openapi.yaml`/`asyncapi.yaml`/JSON Schemas are generated from the canonical model and CI fails on drift, an uncovered path, or a hand-written count | REQ-501, REQ-501b, INV-01/INV-15 |
| **Repudiation** | A classification decision cannot be reconstructed or is denied later | Exactly one required-field `session_risk_audit` row per classification is appended to the tamper-evident hash-chained governance ledger | REQ-503, INV-19 |
| **Information disclosure** | A caller reads replay, stats, or audit rows it has no authority over | Unauthorized reads are unresolvable under an RBAC authority check plus a NOT NULL foreign key; an unknown id and an unauthorized id both return not-found, revealing nothing about the row | REQ-504, INV-12 |
| **Denial of service** | A replayed or oversized request, or a flood of duplicate alerts, exhausts the boundary | The nonce store plus bounded timestamp window reject replays; the body reader is size-limited and rejects an oversized payload; ingest runs `dedup → flap → burst → correlate` in code before publishing | REQ-501, REQ-502, INV-01 |
| **Elevation of privilege** | A "session-replay" resumes a mutating session with attacker-supplied input, skipping the gate | Replay mints a new Temporal workflow from an immutable read-only `ContextSnapshot` and re-runs the full gate from zero; there is no privileged resume-with-prompt primitive | REQ-501, INV-01 (H-01/P0-2) |
| **Elevation of privilege** | A reader silently mis-decodes a row written by a newer schema, or a bare re-trigger re-enters the pipeline unauthenticated | Readers reject a future `schema_version` with `SchemaVersionError`; a requeued escalation re-enters the pipeline only as an authenticated internal Temporal signal keyed by `session_id` | REQ-505, REQ-507, INV-15/INV-16 |

**Adversarial acceptance (boundary tests, Phase 4).** Drive the router with unauthenticated, replayed,
stale-timestamp, tampered-body, and oversized requests and assert rejection before body-parse; assert a
route with `auth=none` cannot register; assert an unauthorized replay id returns not-found; assert a
future-versioned row raises `SchemaVersionError`; assert the generated contract fails CI on an uncovered
path or missing provenance. These drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
