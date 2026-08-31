<!-- spec/016 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/016 — Threat model: Credential / Identity Engine (STRIDE slice)

Per-feature threat slice for the `credential.Engine`. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the engine's own trust boundary and
is the security half of the spec's definition-of-done.

**Trust boundary.** The engine sits AFTER the spec/015 policy engine returns a non-deny verdict and feeds
the spec/013 actuation interceptor's execute step with a resolved identity (REQ-1604). Its inputs are a
typed `Target`, the operator-declared resolver config, and the read-only pull from each configured
`CredentialSource`; its output is a required-field `CredentialBundle` (secrets as `SecretRef` only) or a
typed `ErrUnresolved`, plus an immutable `credential_resolution` / `credential_sync_run` row appended to
the tamper-evident ledger. The credential-store contents (which identity reaches which host) and the sync
sources (external platforms TG imports from) are the assets. Adversaries of interest: (a) a poisoned or
compromised sync source injecting a hostile identity or mapping; (b) an attacker attempting to exfiltrate a
credential at rest or in transit; (c) a manipulated resolution that binds a wrong or elevated identity to a
target; (d) a partially-synced or empty store that could fail OPEN to a default identity.

**The deliberate posture (threat-modelled honestly).** The engine imports credentials from platforms the
operator already runs (AWX / Ansible / Semaphore / OpenBao / Vault / LDAP / OIDC) rather than hand-entering
them — so a synced source is inside the resolution trust path. This is safe ONLY because (1) every synced
value is stored as a `SecretRef` reference, never a secret value (INV-13), so a store read discloses no
credential; (2) each source is read-only and re-reads its system-of-record by id (INV-05), so a mutated
webhook / cached copy is never trusted; (3) resolution fails closed (REQ-1602) so a poisoned source that
drops or garbles a mapping yields refuse, never a default identity; and (4) the constitutional mechanical
never-auto floor (INV-09) and the policy engine's authorization both remain in force beneath this engine —
a resolved identity is necessary, never sufficient, for actuation.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A poisoned or spoofed sync source injects a hostile host→identity mapping so TG authenticates to an attacker-controlled endpoint or with an attacker-chosen identity | Each source is read-only and re-reads its canonical object from its system-of-record by id (never a pushed/cached claim); the source is admitted only when its connector adapter is compiled in and registered; cross-source precedence records the winning + shadowed sources so an injected low-precedence mapping cannot silently win | REQ-1607, REQ-1609, REQ-1617, INV-05, INV-17 |
| **Spoofing** | An approver-plane (LDAP/OIDC) source is used to forge a machine host identity, or a machine source forges an approver | The two-plane router binds each source to exactly one plane and refuses a cross-plane write, so a credential-plane sync can never populate `approve_by` and an approver-plane sync can never populate a host bundle | REQ-1611, REQ-1614, INV-13 |
| **Tampering** | An operator resolver rule or a synced mapping is altered so a wrong or elevated identity binds to a target | Resolution matches the shared estate object-model with deterministic most-specific-wins precedence; an equal-specificity or precedence-ambiguous conflict fails closed rather than choosing arbitrarily; every resolution is appended to the ledger with its matched rule / winning source | REQ-1605, REQ-1606, REQ-1609, REQ-1617, INV-09/INV-19 |
| **Repudiation** | A resolution decision or a sync run is denied later | Exactly one immutable `credential_resolution` row per resolution and one `credential_sync_run` row per `Sync()`, each a required output appended to the hash-chained governance ledger; the runtime role holds no UPDATE/DELETE on these append-only tables | REQ-1615, REQ-1617, INV-19 |
| **Information disclosure** | A credential is exfiltrated at rest (store/config/export) or in transit | Every credential value is a `SecretRef` reference resolved at runtime through the sealed #27 store — no plaintext value in config, the store, logs, an audit row, or any exportable artifact; the Vault/OpenBao read and the OIDC client-credentials mint (`client_id`/`client_secret` are `SecretRef`s) are over the backend's authenticated channel with VERIFIED TLS (no `InsecureSkipVerify`); gitleaks CI backs the no-literal-secret rule | REQ-1603, REQ-1613, REQ-1619, REQ-1617, INV-13 |
| **Information disclosure** | An audit row or console surface leaks a secret value alongside the identity metadata | The `credential_resolution` row carries only non-secret identity metadata (`user`, `connection_scheme`, `port`, and the `SecretRef` STRING) — never a secret value; a standing check fails on any plaintext secret in a row | REQ-1617, INV-13/INV-19 |
| **Denial of service** | An unreachable Vault/OpenBao or OIDC token endpoint, a denied read/grant, an untrusted server cert, or an expired lease/token blocks resolution | The Vault/OpenBao read and the OIDC client-credentials mint fail closed with no default or blank credential (a cached OIDC token is re-minted before `expires_in`, never served stale on a denied re-mint); a failed `SecretRef` resolution refuses the target rather than falling open; a failed or partial sync leaves the prior converged state intact | REQ-1602, REQ-1613, REQ-1619, INV-09 |
| **Elevation of privilege** | A partially-synced or empty store fails OPEN so a target resolves to a default / last-used identity with broader reach than intended | The resolver's zero value is refuse; an unmatched, ambiguous, or unresolvable path returns `ErrUnresolved` under any partially-synced or empty store state; there is no default-identity fallback anywhere in the path | REQ-1602, REQ-1610, INV-09 |
| **Elevation of privilege** | The AWX/Ansible/Semaphore effect channel is used to run a mutating playbook outside the governed path | Read-only fact-gathering is a Phase-1-safe sensor; governed playbook / job-template actuation routes through the spec/015 mode chokepoint and the constitutional never-auto floor; the primary scope stays credential/identity resolution + sync | REQ-1616, INV-09/INV-21 |

**Adversarial acceptance (boundary tests, Phase 4).** Fuzz the resolver config and the synced source set
with every permutation and assert the fail-closed property (REQ-1602) holds under empty, partial, and
conflicting stores; assert no resolution ever returns a plaintext secret or a fabricated identity for an
uncovered target; assert a poisoned source cannot cross planes (REQ-1611) and cannot win precedence it was
not granted (REQ-1609); assert a Vault/OpenBao unreachable/denied/expired read refuses rather than blanks;
assert the ledger row is present for every resolution and sync. These drive the actual code path (INV-22) —
see [`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md) §3.1.
