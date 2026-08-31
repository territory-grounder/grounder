<!-- spec/001 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/001 — Threat model: three-band risk classification (STRIDE slice)

Per-feature threat slice for the `RiskClassifier`. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the classifier's own trust
boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The classifier sits after ingest validation (spec/006) and the prediction gate
(spec/002) and before approval/execution. Its inputs are a typed `GatedInput` (never raw model text);
its output is a required-field `Decision` appended to the tamper-evident ledger. The adversary of
interest is a compromised or manipulated LLM whose output is trying to reach the **AUTO** band for an
action that should pause.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | Model output impersonates a "safe" classification to win AUTO | The classifier consumes a typed `Proposal` parsed once by one grammar; there is no path from raw model text to a band | REQ-006, INV-06 |
| **Tampering** | An approved action is swapped for a different one after classification | `[AUTO-RESOLVE]` is bound to the exact `action_id` / `plan_hash`; a mutated Action mints a new id and re-enters the gate | REQ-004 note, INV-07 |
| **Repudiation** | A decision cannot be reconstructed / is denied later | Exactly one immutable `session_risk_audit` row per classification, hash-chained into the governance ledger; every field is a required output | Persistence contract, INV-19 |
| **Information disclosure** | An under-privileged role reads criticality tiers / signals it has no authority over | Every audit and policy read is authority-checked against the acting user/role under RBAC; the acting model has no read path to another operator's session state, keyed by `session_id` | Cross-cutting rule, INV-12 |
| **Denial of service** | A flood of ambiguous inputs stalls or crashes the classifier into an open state | Every error/panic/unmatched path fails closed to POLL_PAUSE (the `Band` zero value); the classifier is a pure function with no unbounded work | REQ-006, INV-09 |
| **Elevation of privilege** | An irreversible op reaches the AUTO ceiling via a confidence score or organization policy flag | The non-configurable mechanical never-auto floor clamps the band before any confidence/policy is read; unknown op-class also clamps | REQ-004, INV-09/INV-10 |
| **Elevation of privilege** | A fabricated evidence fence launders an unverified claim into AUTO | Evidence must be one or more orchestrator-captured `ToolResult` IDs checked for provenance/recency/success/relevance; a bare fenced block is rejected | REQ-008, INV-11 |

**Adversarial acceptance (boundary tests, Phase 4).** Feed the classifier ambiguous, unparseable, and
panic-inducing input and assert POLL_PAUSE; assert no confidence/flag lifts an irreversible op above the
floor; assert the acting model has no write path to the audit/verdict columns. These drive the actual
code path (INV-22) — see [`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md) §3.1
(Banding fail-closed).
