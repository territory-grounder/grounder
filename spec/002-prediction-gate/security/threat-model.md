<!-- spec/002 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/002 — Threat model: fail-closed prediction gate + mechanical verdict (STRIDE slice)

Per-feature threat slice for the remediation lane's `PredictionGate` / `Verifier` pair. The system-wide
model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the gate's own trust
boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The gate sits inside the remediation Temporal workflow, between a classified
proposal and its execution. Its inputs are a typed `GatedProposal` (constructible only by
`PredictionGate`) bound to a content-hashed `ActionManifest`; its outputs are one append-only prediction
row and, post-execution, one verifier-authored verdict row, both chained into the tamper-evident ledger.
The lane fails **CLOSED**: absent a committed, action-bound prediction, the approval poll is denied. The
adversary of interest is a compromised or manipulated LLM trying to execute an unpredicted action, to
author its own `match` verdict, or to swap the action after the prediction is committed.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | Model output poses as a committed prediction to pass the gate | `BuildApprovalPoll` accepts only a `GatedProposal` constructible solely by `PredictionGate`; there is no path from raw model text to a "prediction exists" | REQ-102, INV-06 |
| **Tampering** | The action is swapped for a different one after the prediction is committed | Every stage re-derives and asserts the content-hashed `action_id`; a mutated Action mints a new id, fails the assertion closed, and re-enters the gate as a child workflow | REQ-102b, INV-07 |
| **Repudiation** | A prediction or verdict cannot be reconstructed / is denied later | One immutable append-only prediction row and one verifier-authored verdict row per action, each hash-chained into the governance ledger, stamped `schema_version` | Persistence contract, INV-19 |
| **Information disclosure** | An under-privileged role reads predictions, cascades, or verdicts it has no authority over | Every prediction and verdict read is authority-checked against the acting user/role under RBAC; a bare id grants no read | Cross-cutting rule, INV-12 |
| **Denial of service** | A flood of unpredicted proposals stalls the gate into an open state | Default-deny by construction — no committed prediction means the poll is denied; the gate does no unbounded work and the remediation lane's zero value is fail-closed | REQ-102, INV-09/INV-10 |
| **Elevation of privilege** | The acting model writes a `match` verdict to auto-resolve an action that actually deviated | The verdict is written only by `computeVerdict`; the model and session database roles hold no UPDATE or DELETE grant on the prediction or verdict tables | REQ-103, INV-10 |
| **Elevation of privilege** | A `deviation` is laundered into an auto-resolve via a high band or confidence score | A `deviation` verdict never auto-resolves regardless of band or confidence and routes to POLL_PAUSE and the approver graph — the mechanical rule is non-configurable | REQ-104, INV-10 |
| **Elevation of privilege** | Analysis-only mode is abused to skip gating a real remediation | Analysis-only is an org-global RBAC-gated policy row that only makes the lane advisory (record without block); it cannot lift the never-auto floor or the deviation-never-auto rule | REQ-105, INV-07/INV-10 |

**Adversarial acceptance (boundary tests, Phase 4).** Attempt to build an approval poll from a
non-`GatedProposal` and assert it cannot be constructed; feed a post-prediction action mutation and
assert the `action_id` assertion aborts closed; grant the session role and attempt a verdict-column
write and assert it is rejected; feed an observed alert on an unnamed host and assert the verdict is
`deviation` and no auto-resolve follows. These drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md) §3.1.
