<!-- spec/005 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/005 — Threat model: tier-1 suppression (STRIDE slice)

Per-feature threat slice for the `core/suppression.Chain` and its two registries. The system-wide model
is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the suppression trust boundary
and is the security half of the spec's definition-of-done.

**Trust boundary.** The chain sits at ingest, after the alert is normalized to a typed, signature-verified
`AlertEnvelope` (spec/006) and before a triage workflow is spawned. Its inputs are the envelope and the
`discovered_scheduled_reboots` + `suppression_policy` registry rows; its output is a `Decision`
that either escalates the alert or darkens it (suppresses) with an appended ledger record. The adversary
of interest wants a real, actionable incident **darkened** — either by forging a suppression rule, by
pushing a registry row past its bounds, or by crafting an envelope that walks the dedup boundary.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A forged or replayed alert impersonates a benign, known-transient event to be darkened | The alert is a signature-verified envelope re-read from its system-of-record before dispatch; suppression matches on typed envelope fields, not on raw claim text | REQ-401, INV-05 |
| **Tampering** | A registry row is pushed past its temporal bounds or kill-switch to darken a live incident | A row suppresses only WHILE currently valid — `valid_from`/`valid_until`/`last_verified_at` all checked, `kill_switch` honored, `observing` rows never suppress; an expired, stale, or contradicted row fails open | REQ-402, REQ-404, REQ-405, INV-20 |
| **Repudiation** | A suppression decision cannot be reconstructed or is denied later | Every chain run appends one immutable decision record (outcome, phase, reason, action_id, signals) to the tamper-evident ledger; omitting a field is a type error | Persistence contract, INV-19 |
| **Information disclosure** | An under-privileged role reads schedules or blast-radius policies it has no authority over | Every registry read and write is authority-checked against the acting user/role under RBAC; a host-agnostic rule is scoped to the estate label, never to one hostname | REQ-401, INV-12 |
| **Denial of service** | A flood of ambiguous or malformed alerts stalls or crashes the chain into a darken-everything state | The `Decision` zero value is escalate; every panic, unmatched branch, and phase error fails open to escalation; the chain is bounded per-alert work | REQ-405, INV-04 |
| **Elevation of privilege** | A future-dated or negative-age triage entry walks the dedup window to darken a fresh alert | The dedup stage validates `observed_at` against `now()` at the envelope boundary and rejects a future-dated / negative-age entry before use; shape-checking is not validation | REQ-408, INV-04 |
| **Elevation of privilege** | A critical or unknown-severity incident is darkened through a matching schedule or pattern | The severity floor short-circuits every phase for `critical` and unrecognized severities before any registry is read; unknown severity is treated as never-suppress | REQ-407, INV-20 |
| **Elevation of privilege** | A misattributed schedule self-promotes to `live` on one observation and darkens future incidents | Promotion requires at least two recorded in-window boots (observe-before-live); drift and expiry drive a row to `disabled`; a wrong attribution never reaches the threshold | REQ-404, INV-20 |

**Adversarial acceptance (boundary tests, Phase 4).** Feed the chain future-dated, negative-age, and
malformed envelopes and assert escalate; assert a critical or unknown-severity reboot with a live matching
schedule still escalates; assert an expired / stale / kill-switched / `observing` registry row never
suppresses; assert a single in-window boot never promotes. These drive the actual code path (INV-22) —
see [`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
