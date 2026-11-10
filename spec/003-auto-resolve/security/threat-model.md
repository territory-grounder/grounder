<!-- spec/003 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/003 — Threat model: per-incident auto-resolve and escalation requeue (STRIDE slice)

Per-feature threat slice for the band-aware `Reconciler` and the `escalation_queue` requeue lane. The
system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the close-out
and requeue trust boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The reconciler runs after a session finishes, deciding whether an incident closes,
stays open, or requeues. Its inputs are a typed `SessionSnapshot` (band from spec/001, mechanical verdict
from spec/002, orchestrator-captured `ToolResult` evidence); its output is a required-field `CloseOut`
appended to the tamper-evident ledger, plus an optional authenticated re-check signal. The adversary of
interest is a compromised agent (or a forged re-trigger) trying to launder a failed remediation into a
closed ticket, or to re-drive a session with attacker-chosen input.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A forged re-trigger re-drives a session or re-opens an incident without authorization | A queued re-check re-enters the gated pipeline only as an authenticated internal Temporal signal keyed by `session_id`; there is no unauthenticated re-trigger primitive | REQ-207, INV-01 |
| **Tampering** | The acting agent rewrites `resolution_type` to mark a failed remediation `auto_resolved` | The close-out `resolution_type` is written by the reconciler activity from the mechanical verdict; the model has no write path, and a `deviation` / `partial` verdict is demoted to To Verify, not closed | REQ-203, INV-11 |
| **Repudiation** | A close-out decision cannot be reconstructed or is denied later | Exactly one immutable close-out record per incident, hash-chained into the governance ledger; every field is a required output of the decision function | Persistence contract, INV-19 |
| **Information disclosure** | An under-privileged role reads an escalation queue or outcome rollup it has no authority over | Every session, close-out, outcome, and queue read is authority-checked against the acting user/role under RBAC, keyed by `session_id`; the acting model has no read path to another operator's session | REQ-205, INV-12 |
| **Denial of service** | An alert storm inflates the auto-resolve denominator or drives unbounded re-escalation | Outcomes roll up per-incident best-outcome (count incidents, not events); the requeue is bounded by a per-incident unanswered-poll cap that stands the lane down to a human | REQ-205, REQ-208, INV-12 |
| **Elevation of privilege** | A session closes an incident on the agent's asserted success without a verified clear | A close to Done admits only on an orchestrator-captured `ToolResult` or an independent post-condition check confirming the alert cleared; unconfirmed clears leave the incident open | REQ-201, INV-11 |

**Adversarial acceptance (boundary tests, Phase 4).** Feed the reconciler an AUTO session whose recovery
lacks bound evidence and assert the incident stays open; feed a `deviation` verdict and assert To Verify,
not Done; deliver a re-check over an unauthenticated path and assert rejection before the gated pipeline;
replay a single incident as an event flood and assert the auto-resolve denominator counts it once. These
drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
