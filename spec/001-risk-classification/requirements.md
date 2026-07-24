<!-- spec/001 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/001 — Three-band risk classification

**Owning behavior family:** BEH-1 (see [`docs/GOVERNED-BEHAVIORS.md`](../../docs/GOVERNED-BEHAVIORS.md)).
**Constitution / invariants:** INV-06, INV-07, INV-09, INV-10, INV-11.
**Phase:** classifier behavior lands in Phase 2; the mechanical fail-closed primitives land in Phase 0
(`core/safety`). **Status:** Approved.

The `RiskClassifier` is a typed, deterministic admission gate that writes exactly one
`session_risk_audit` row per classification and emits one of three autonomy bands —
**AUTO / AUTO_NOTICE / POLL_PAUSE**. Its zero value and every error path fail closed to POLL_PAUSE by
construction. This document is the requirement source of record; the design is in `design.md`, the
runnable acceptance oracles are in `acceptance/`, and the engineering tasks are in `tasks.json`.

## Requirements

- **REQ-001** — [F] spec/001 · [R] paradigm-rule 2.
  WHEN a session is admitted for a low-risk or reversible-and-prediction-eligible action that is not on
  an organization-designated criticality-tier host and whose predicted blast-radius is below the
  configured threshold, the classifier SHALL emit band **AUTO** and mark the proposal for `[AUTO-RESOLVE]`.

- **REQ-002** — [F] spec/001 · [R] paradigm-rule 3.
  WHEN the action is reversible-mixed on a criticality-tier host, or the predicted blast-radius is wide,
  the classifier SHALL emit band **AUTO_NOTICE**, proceed with `[AUTO-RESOLVE]`, and set
  `notify_required = true` so the on-call group receives an out-of-band veto notice in parallel.

- **REQ-003** — [F] spec/001 · [O] INV-09.
  WHEN the action is high-risk, irreversible, unpredicted, a verification deviation, or a
  novel-incident class (`ood:novel-incident`), the classifier SHALL emit band **POLL_PAUSE**, mark the
  proposal `[POLL]`, hold on durable pause/resume state, notify the approver graph, and SHALL NOT
  proceed on timeout.
  The **novelty signature** SHALL be `(incident subject host, canonical rule family)`, where the incident
  subject host is the ingest-validated alerted device (`env.Host`) — the SAME identity the confirmed-clean
  novelty writeback records and the retrieval/precedent plane queries. The LLM-expressed action target
  (which may name the guest or its hypervisor for the same fault) SHALL be consulted only as a LEGACY
  compatibility key, so a precedent under either key de-novels the incident; the subject key is the one
  that transfers across proposals. An incident is novel only when EVERY consulted key is a known-zero
  count; an unknown count (no knowledge store) SHALL be treated as NOT novel (novelty-unknown ⇒ do not
  fire), never inventing a poll from missing data.

- **REQ-004** — [F] spec/001 · [O] INV-09/INV-10 · [R] paradigm-rule 8.
  The classifier SHALL apply the inviolable mechanical NEVER-auto floor as a non-configurable
  precondition that clamps the band to **POLL_PAUSE** — regardless of confidence, organization policy, or any
  flag — for any action in the irreversible class (`mkfs`, `dropdb`, `zpool`/`zfs destroy`,
  `terraform`/`tofu destroy`, `kubectl delete`/`drain`, credential-revoke, reboot/halt, config-file
  overwrite, criticality-tier reboot, confirmed jailbreak). An unrecognized mutation SHALL be treated
  as never-auto (unknown action-class implies the never-auto ceiling), never as safe by omission.

- **REQ-005** — RETIRED. [R] paradigm-rule 4/7.
  The predecessor's "byte-identical legacy output while the autonomy sentinel is absent" behavior does
  not exist in Territory Grounder; autonomy is an org-global RBAC-gated audited feature-flag whose off-state is
  the non-autonomous read-only baseline, not a legacy code path.

- **REQ-006** — [F] spec/001 · [O] INV-06/INV-09.
  IF the model output is unparseable, non-manifest-expressible, or ambiguous, the classifier SHALL fail
  closed to **POLL_PAUSE** as its only defined behavior — the `Band` enum zero value is the
  most-restrictive band, so any error, panic, or unmatched path is fail-closed by construction — and
  the output SHALL NOT be routed through a looser fallback grammar.

- **REQ-007** — [F] spec/001.
  WHEN an incident class has no learned prior (no prediction-eligible history for this
  `(alert_rule, host)`), the classifier SHALL fail closed to **POLL_PAUSE**.

- **REQ-008** — [F] spec/001 · [O] INV-11.
  WHEN the `silent_cognition_guard` policy is active, the classifier SHALL strip any `[AUTO-RESOLVE]`
  marker whose response lacks a bound post-state evidence block and downgrade the session to a poll,
  where evidence means one or more orchestrator-captured `ToolResult` IDs checked for provenance,
  recency, success, and target-relevance — a bare fenced block SHALL be rejected.

- **REQ-009** — [F] spec/001 · [R] paradigm-rule 2/8.
  WHEN a `(host, op_class)` matches the deployment-declared **canary allowlist**, the classifier SHALL
  force **POLL_PAUSE** regardless of the action's otherwise-auto-eligible reversibility/risk, so the FIRST
  staged mutations require a human vote (never AUTO). The pin is **safe-direction only** — it can raise an
  otherwise-AUTO action to a poll but SHALL never lower a poll — and runs AFTER the inviolable mechanical
  floors (REQ-004 et al., which record the more fundamental reason when they also apply). The allowlist is
  operator-declared config (config-not-code — no hostnames/op-classes in the binary); when unconfigured the
  classifier behaves identically to one without this rule (inert by default).

- **REQ-011** — [O] INV-09/INV-19 · consumes spec/023 REQ-2301/2304/2305/2310 (the actor-attribution
  dispositions made classifier-visible, canary-pin style).
  WHEN the attribute step resolves an actor-attribution disposition of `stand-down-coordinate`,
  `security-escalate`, or escalate (an unmapped disposition or a non-suspicious contradiction), the
  classifier SHALL force **POLL_PAUSE** — respectively `actor-attributed-authorized`,
  `actor-attributed-suspicious` (also recording `security_escalation` in the decision signals), or
  `actor-attribution-escalate`. The three inputs are set by the attribute activity from typed,
  reader-captured evidence — never from model narrative — and are **safe-direction only**: they can raise
  an otherwise-AUTO action to a poll but SHALL never lower one, run AFTER the inviolable mechanical floors
  (which record the more fundamental reason when they also apply) and BEFORE the auto-eligible branches,
  and an `unattributable` attribution (or absent evidence) sets none of them so the classification is
  byte-identical to the pre-feature ladder (spec/023 REQ-2303). They SHALL NOT raise autonomy, lift the
  never-auto floor, or bypass the mode chokepoint (spec/023 REQ-2305).

- **REQ-010** — [O] INV-08/INV-11/INV-19 · design-wisdom #13 (SK 6.3 prompt-filter secret/PII removal).
  Before untrusted seed text (an alert narrative, an entry ticket, a CMDB record) reaches the model OR is
  written to any log or the governance ledger, the input screen SHALL redact high-confidence
  secret / credential shapes — a bearer or basic Authorization token, a labeled
  `password`/`token`/`api_key`/`secret`/`access_key`/`client_secret` value, a provider-prefixed key
  (GitLab `glpat-`, GitHub `gh*_`, AWS `AKIA*`), a PEM private-key block, basic-auth userinfo embedded in a
  URL, and a long high-entropy hex or base64 run in a key/secret/token context — replacing each with a
  deterministic `[REDACTED:<kind>]` marker. The redaction SHALL be deterministic (no model call) and
  conservative: a benign alert body carrying only hostnames, addresses, rule names, and numbers SHALL pass
  through unredacted, and a value that merely looks numeric SHALL NOT be redacted. WHEN the screen redacts
  a secret it SHALL flag the redaction on the same neutralize-and-flag channel the injection screen uses, so
  the caller emits the redacted (never the raw) text; redacting a secret SHALL NOT by itself force
  POLL_PAUSE (a leaked credential is a hygiene failure, not a jailbreak).

## Persistence contract

Exactly one immutable `session_risk_audit` row per classification, carrying
`risk_level`, `band`, `auto_approved`, `auto_proceed_on_timeout`, `notify_required`, `signals_json`,
`operator_override`, the `plan_hash` that joins to the prediction gate (spec/002), and the canonical
`action_id` (INV-07). The row is a required output of the decision function — omitting a field is a Go
type error — and is appended to the tamper-evident governance ledger (INV-19). See
[`docs/DATA-MODEL.md`](../../docs/DATA-MODEL.md).

## Band-aware audit invariant

A standing check SHALL FAIL if any `auto_approved` row is outside `{AUTO, AUTO_NOTICE}` or carries a
floor signal (`irreversible:*`, `criticality:reboot`, `deviation`). An `[AUTO-RESOLVE]` is valid only
for the exact `action_id` / `plan_hash` it was classified against (INV-07).
