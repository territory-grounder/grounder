<!-- spec/007 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/007 — Threat model: content-aware spec↔code lockstep (STRIDE slice)

Per-feature threat slice for the lockstep gate. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the gate's own trust boundary and
is the security half of the spec's definition-of-done.

**Trust boundary.** The gate runs in continuous integration over the manifest `spec/.lockstep.lock` and
the governed source tree, and it authorizes re-stamps through the governance ledger. The adversary of
interest is a contributor (human or coding agent) trying to land governed safety-critical code that has
drifted from its owning spec, or trying to re-stamp the manifest without the authorized approval — thereby
weakening a control while the gate still reports green.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | An unauthorized actor forges a re-stamp to appear approved | A re-stamp is accepted only inside an RBAC-gated approval by a `spec-owner` role, and the acceptance is attributed to that role identity on the governance ledger | REQ-703, INV-19 |
| **Tampering** | Governed code is changed without updating its owning spec | The content-hash drift check fails CI on any mismatch; the comment-insensitive hash blocks laundering a semantic change behind cosmetic edits | REQ-702, REQ-704, INV-22 |
| **Repudiation** | A re-stamp is later denied or cannot be reconstructed | Each accepted re-stamp is an immutable record appended to the tamper-evident governance ledger with the actor role, the changed paths, and the owning spec | REQ-703, INV-19 |
| **Information disclosure** | The manifest leaks sensitive state | The manifest holds only file paths, owning-spec ids, and one-way SHA-256 hashes; it carries no secret or operational data, and the hash is non-reversible | REQ-701, INV-22 |
| **Denial of service** | A malformed manifest or unreadable governed file stalls or defeats CI | The check is a bounded, pure-stdlib pass over a fixed manifest; a malformed manifest or a missing governed file fails closed with a non-zero exit rather than passing | REQ-702, INV-22 |
| **Elevation of privilege** | A governed file is quietly dropped from the manifest to dodge the gate | The coverage invariant fails if any governed safety-critical file is absent from the hash-verified set — no governed file may be excluded | REQ-702, INV-22 |

**Adversarial acceptance (boundary tests, Phase 4).** Mutate an executable token in a governed file and
assert `lockstep --check` reports drift and exits non-zero; edit only comments or reformat and assert no
drift; drop a governed file from the manifest and assert the coverage invariant fails; edit the manifest
outside the approval flow and assert CI still fails with no ledger record. These drive the actual code
path (INV-22) — see [`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md) §3.
