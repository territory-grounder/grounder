<!-- spec/010 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/010 — Threat model: the operator console (STRIDE slice)

Per-feature threat slice for the `frontend/` operator console. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the console's own trust boundary
and is the security half of the spec's definition-of-done.

**Trust boundary.** The console is an untrusted client of the Go control-plane. It authenticates to the
API, renders the API's decisions, and issues intents back through the single generated OpenAPI client —
it holds **no** effect authority, decision logic, or safety verdict of its own. The security model is
therefore: the browser can be fully compromised without widening autonomy, because every band, verdict,
never-auto floor, and RBAC grant is decided and enforced server-side (INV-08, INV-09, INV-01). The
adversary of interest is a malicious client, a lower-privileged operator attempting privilege
escalation, or a tampered API response.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A caller forges identity or role to reach an operational control | Every console call is authenticated at the API; the RBAC role and on-call assignment are re-checked server-side on each mutating action; client-side gating is presentational only | REQ-605, REQ-603, INV-01 |
| **Tampering** | A manipulated browser or response widens autonomy or approves the wrong action | The console enforces no safety decision client-side; a control the API denies stays unavailable; approve/veto is bound to the exact `decision_id` / `action_id`, and a mutated Action re-enters the gate server-side | REQ-615, REQ-605, INV-07, INV-08 |
| **Tampering** | The ledger chain is altered but shown as intact | The chain-verification verdict is the server-side `LedgerVerifier`'s output, surfaced verbatim; the browser never recomputes the chain, so it cannot be tricked into rendering a forged "intact" verdict | REQ-608, INV-19 |
| **Repudiation** | An autonomy-control change cannot be attributed later | Autonomy-band and kill-switch changes write to the policy store through the API and are audited on change; no host-local file is a control path, so every change is an attributable, ledgered record | REQ-610, INV-21, INV-19 |
| **Information disclosure** | The console over-fetches or renders data the caller may not read | The API returns only what the authenticated role is authorized to see; the console renders the API response and adds no privileged read path; an authorization error resolves to the restrictive state | REQ-616, REQ-602, INV-01 |
| **Denial of service** | A dropped or flooded SSE stream leaves the console silently stale | On disconnect the console shows a disconnected indicator and reconnects with backoff; staleness is visible, never masked as live | REQ-613, INV-15 |
| **Elevation of privilege** | A non-approver or non-administrator reaches a gated control via the client | The control is absent for a caller without the role, and the server re-checks authority on every mutating call; the browser can never grant what the API denied | REQ-605, REQ-612, REQ-615, INV-01 |
| **Elevation of privilege** | A mutating control appears while the platform is read-only | WHILE `mutation_enabled = false` no mutating control is mounted; the operational surface exists only after the server reports mutation enabled and grants the role | REQ-602, REQ-603, INV-09 |

**Adversarial acceptance (boundary tests, Phase 4).** Drive the console against a hostile API stub:
assert a denied or errored response yields the restrictive state (no data, no control); assert a
non-approver / non-administrator session mounts no gated control and that a forged client-side "allowed"
flag is rejected by the server re-check; assert a forged ledger "intact" payload is not synthesized in
the browser; assert a kill-switch renders disabled only after a confirmed write. These drive the actual
code path (INV-22) — see [`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
