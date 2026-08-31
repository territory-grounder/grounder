<!-- spec/011 — per-feature STRIDE slice. Provenance: [F]/[R]/[O] as elsewhere. -->

# spec/011 — Threat model: native Go agent loop

STRIDE slice for the agent loop. The controlling invariant is INV-08 (no model-produced token becomes
control flow, a command string, or a query fragment) plus INV-06 (one proposal grammar).

| Threat | Vector | Control | Ref |
|---|---|---|---|
| **Tampering / Elevation** | The model emits text intended to be executed as a command (`tool: "get-logs; rm -rf /"`) | A tool is dispatched only by an exact allowlist lookup against the registered tool set; an unknown name fails closed and is never executed — model text is never `exec`'d or interpolated | REQ-1001, INV-08 |
| **Elevation** | The model tries to invoke a mutating tool while mutation is off | `ToolSet.Register` refuses a non-read-only tool (`ErrWriteToolWithheld`); a write tool is structurally absent, so there is no code path to a mutating call | REQ-1003, INV-08 |
| **Spoofing (alternate grammar)** | The model emits a second, looser proposal grammar (markdown/sentinel) to bypass the gate | The proposal is emitted only through the single `ParseProposal` entry point; an unparseable/non-manifest-expressible output fails closed to a stop, with no fallback grammar | REQ-1005, INV-06 |
| **Repudiation / over-confidence** | The model self-authorizes with a high confidence it did not earn | Confidence is DATA, not authority: below the stop threshold the agent stops; below the escalate threshold it escalates to a human poll; the gate and classifier — not the marker — decide | REQ-1002 |
| **Denial of service** | The model loops without ever proposing, consuming budget | The loop is bounded by the poll-handoff and hard-halt cycle limits; an unbounded/looping agent is not reachable | REQ-1004 |
| **Tampering (effect channel)** | The agent produces a side effect on the estate directly | The agent only proposes; in Phase 0/1 it invokes no mutating tool and the deterministic orchestrator owns the effect channel | REQ-1006 |

## Adversarial acceptance

The acceptance oracles drive the real agent with a scripted model that attempts each vector above
(injection tool name, write tool, markdown-instead-of-directive, sub-threshold confidence, never-propose
loop) and assert the fail-closed outcome. No source-string assertion is used — every scenario drives the
actual `agent.Agent.Run` code path (INV-22).
