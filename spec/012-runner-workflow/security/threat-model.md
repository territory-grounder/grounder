<!-- spec/012 — per-feature STRIDE slice. Provenance: [F]/[R]/[O] as elsewhere. -->

# spec/012 — Threat model: read-only Runner Temporal workflow

STRIDE slice for the Runner. The controlling invariants are INV-21 (the control-flow contains no OS
execution), INV-09 (mutation off), and INV-07 (action identity binding).

| Threat | Vector | Control | Ref |
|---|---|---|---|
| **Elevation** | The orchestrator itself executes an estate mutation, bypassing the gate | The workflow body is control flow only; every side effect is an activity, and the execute/verify activities are no-op while mutation is off — the Runner stops at propose | REQ-1101/1102, INV-09/21 |
| **Elevation (OS)** | A workflow performs an OS command directly | No activity in the read-only pipeline executes an OS command; the only inline computation is a deterministic hash | REQ-1102, INV-21 |
| **Tampering (action substitution)** | The action is changed between derivation and the gate, so a prediction is bound to a different action | The `action_id` is threaded and asserted equal to the gate's sealed `ActionManifest` id; a mismatch fails closed and re-enters the gate | REQ-1103, INV-07 |
| **Repudiation** | A session's decision cannot be reconstructed | The classify activity appends one required-field `session_risk_audit` row to the hash-chained ledger for every classification (spec/006) | REQ-1101, INV-19 |
| **Denial of service** | The agent loops forever, never proposing | The agent loop is cycle-bounded (spec/011); the Runner short-circuits to a read-only stop when no proposal is produced | REQ-1104 |

## Adversarial acceptance

The acceptance oracle drives the real `RunnerWorkflow` in the Temporal in-process test env with in-memory
governed primitives, asserting the incident reaches a sealed gated proposal with `Mutated=false`, that
the threaded `action_id` equals the sealed manifest id, and that a no-proposal incident ends without an
action. No source-string assertion is used — the workflow's actual activities run (INV-22).
