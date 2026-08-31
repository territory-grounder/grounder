<!-- spec/004 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/004 — Threat model: governance auto-demote + judge-death detection (STRIDE slice)

Per-feature threat slice for the governance-metrics worker and the judge-liveness monitor. The
system-wide model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes this
family's own trust boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** Both controls are internal Temporal Schedules that read close-out rows,
session-outcome tables, and the policy store, and write demotion policy rows plus liveness facts to the
immutable audit spine. The adversaries of interest are (1) a **dead or manipulated judge** trying to
appear alive so its silence never pages, and (2) a caller or storm trying to flip a real
remediation-eligible tuple into analysis-only, or to erase the record that a demotion happened.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A dead judge certifies itself alive by reading its own output tail | The judged-fraction denominator is drawn ONLY from session-outcome tables the judge holds no write grant on; a judge that stops scoring drives the fraction down and trips the warning | REQ-305, INV-15/INV-22 |
| **Tampering** | A demotion policy row is edited to un-demote a genuine repeat-offender | Demotion state is an append-only, `valid_until`-bounded org-global policy row; the read path treats an expired or absent row as un-demoted, and every change is recorded on the hash-chained audit spine | REQ-304, INV-19 |
| **Repudiation** | A demotion or judge-death event cannot be reconstructed or is denied later | Every governance decision and liveness fact is appended to the tamper-evident hash-chained audit spine as a required output of the decision function | Persistence contract, INV-19 |
| **Information disclosure** | An under-privileged role reads offender tuples or judged transcripts it has no authority over | Every governance, liveness, and transcript read is authority-checked against the acting user/role under RBAC; the judged fraction aggregates org-global facts only | REQ-305, INV-12 |
| **Denial of service** | An alert storm inflates recurrence counts and mass-demotes real remediation into analysis-only | Recurrence is counted per-incident best-outcome (not per event), known-transients are excluded, and every demotion auto-expires in 30 days — reversible by construction | REQ-302/REQ-303/REQ-304, INV-22 |
| **Elevation of privilege** | A right-to-erasure purge of operational transcripts also erases the governance audit trail | Retention is split: governance decisions and liveness facts live on the immutable audit spine, never in the purgeable transcript store, so a TTL purge cannot remove an audit-spine record | Persistence contract, paradigm-rule 5, INV-19 |

**Adversarial acceptance (boundary tests, Phase 4).** Zero out the judge's writes and assert the
fraction falls and REQ-306 fires; replay a storm of one incident and assert the recurrence count stays
at one; attempt an UPDATE on a demotion row from the session role and assert the grant is absent; purge
the operational transcripts and assert the audit-spine decision survives. These drive the actual code path
(INV-22) — see [`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
