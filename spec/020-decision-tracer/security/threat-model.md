<!-- spec/020 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/020 — Threat model: Governed decision tracer (STRIDE slice)

Per-feature threat slice for the decision tracer — its per-step evidence tables, its detail read endpoint,
and its optional export lane. The system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md);
this file scopes the tracer's own trust boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The tracer sits ABOVE the decision path it observes and BELOW the operator viewing the
walk. Its inputs are the outputs of engines that already run (classify, agent loop, interceptor, policy,
credential, regime, verify); its outputs are the append-only per-step evidence rows (`agent_step`,
`interceptor_gate_verdict`), the widened decision-signal columns, the versioned ruleset record, and the
authenticated `GET /v1/sessions/{external_ref}` projection plus the step channel. The tracer authorizes
nothing, authenticates nothing, gates nothing, and mutates no target — it is READ-ONLY / OBSERVE-ONLY, off
every actuation chokepoint (REQ-2002).

**The defining disclosure surface (threat-modelled honestly).** The tracer's assembled trace is a RICHER
disclosure than any existing endpoint: it aggregates prompt content, credential *identities*, full ACL rule
text, and step-by-step reasoning into one readable surface — a superset of what the `/v1/sessions` summary
exposes. That is the whole point of the inspector, and it is also the primary risk. It is safe ONLY because
(1) every credential and secret-bearing field is a `SecretRef` reference or a scheme, NEVER a value
(REQ-2015, INV-13), and every agent thought / observation / tool-result is run through the Scrub / redaction
path BEFORE it reaches any trace table (REQ-2008, INV-08); (2) the surface is gated behind a distinct
elevated `trace-read` role, not the current read-only surface (REQ-2014, INV-01); (3) the per-step evidence
tables are append-only with the runtime role holding no UPDATE / DELETE (REQ-2016, INV-19); and (4) the
optional export lane carries the redacted LLM subset ONLY, never the governance fields, and is off by
default (REQ-2020). The tracer grants no authority the platform beneath it did not already grant — it grants
visibility, and visibility is bounded to references, schemes, and a redacted transcript.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Information disclosure** | A secret value (credential, token, key) leaks into a trace table, the detail DTO, or the export because the reasoning transcript or a tool-result carried it | Only `SecretRef` references and schemes are persisted or projected; agent thoughts / observations / tool-results pass through the Scrub / redaction path before write; the detail projection is asserted against a value-shaped secret denylist; gitleaks CI backs the no-literal-secret rule | REQ-2008, REQ-2015, INV-13/INV-08 |
| **Information disclosure** | A principal with only the ordinary read-only surface reads the richer trace (prompts + credential identities + full ACL text) | The detail endpoint, the step channel, and the console walk are gated behind a distinct elevated `trace-read` role separate from `AuthReadOnly`; a principal without `trace-read` is refused | REQ-2011, REQ-2014, INV-01 |
| **Tampering** | A persisted trace step (a gate verdict or an agent-step) is silently rewritten or deleted to hide what a decision did | `agent_step` and `interceptor_gate_verdict` are append-only; the migration REVOKEs UPDATE and DELETE from the runtime role; a persisted step cannot be mutated in place | REQ-2016, INV-19 |
| **Tampering** | An ingested agent thought or tool-result is treated as control flow and steers the decision path through the trace | Ingested thoughts / tool-results enter as DATA only; they are Scrubbed and persisted as evidence and never re-enter the decision path as control flow; the tracer is off every chokepoint | REQ-2002, REQ-2008, INV-08 |
| **Repudiation** | A gate verdict, an agent step, or a policy decision is denied later, or a decision cannot be tied to the ACL doc that produced it | One ordered immutable row per gate and per ReAct cycle keyed by both correlation keys; the policy decision carries bundle_version + matched_rules + reason; the versioned immutable ruleset record lets a past decision join the exact ACL document in force when it decided | REQ-2004, REQ-2007, REQ-2008, REQ-2018, INV-19 |
| **Elevation of privilege** | The tracer's emit or read path is used to reach an actuator, lift a floor, or return a value that gates actuation | The tracer sits off every chokepoint; every emit is a nil-safe side-write and the read endpoint is a projection; a standing check fails if any emit or read path reaches an actuator or returns a value that gates actuation; a decision behaves identically whether or not it is traced | REQ-2001, REQ-2002, INV-09/INV-22 |
| **Information disclosure** | The optional export lane ships governance fields or estate-specific data off the instance, or becomes a shadow system of record | The export lane is off by default; when enabled it carries the redacted LLM subset only (the generalizable layer), never the governance fields, never the estate-specific layer; it never becomes the system of record and never sits on the decision path; v1 shares nothing | REQ-2017, REQ-2020, INV-13 |
| **Denial of service** | A missing or slow trace sink stalls or fails the decision path | Every emit is a nil-safe side-write that degrades to a no-op when the sink is absent; the boundary it observes is unaffected; the tracer never blocks a decision | REQ-2001, INV-22 |

**Adversarial acceptance (boundary tests, Phase 4).** Assert the detail projection carries zero secret
values under a value-shaped denylist across every credential and secret-bearing field (REQ-2015); assert a
principal without `trace-read` is refused the detail endpoint, the step channel, and the console walk
(REQ-2014); assert the runtime role's UPDATE / DELETE on `agent_step` and `interceptor_gate_verdict` is
denied by the grants (REQ-2016); assert a refusing action leaves the refusing gate's row and no phantom pass
rows past it (REQ-2007); assert an agent thought written without Scrub is rejected (REQ-2008); assert a
replayed decision behaves identically with tracing on and off and no emit path reaches an actuator
(REQ-2002); and assert the export lane exports nothing while disabled and only the redacted LLM subset when
enabled (REQ-2020). These drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
