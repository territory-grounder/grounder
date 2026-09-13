<!-- spec/015 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/015 — Threat model: operator-managed policy engine (STRIDE slice)

Per-feature threat slice for the `policy.Engine`. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the engine's own trust boundary
and is the security half of the spec's definition-of-done.

**Trust boundary.** The engine sits AFTER the spec/001 classifier and the spec/002 prediction gate and
feeds the spec/013 actuation interceptor's actuation branch. Its inputs are a typed `EvalInput` (the
sealed `ActionManifest`, risk band, op-class, host, territory, confidence, active mode) plus the
operator-managed rule DATA; its output is a required-field `PolicyDecision` appended to the
tamper-evident ledger. Two adversaries of interest: (a) a compromised or manipulated LLM trying to reach
an `auto` verdict for an action that should pause, and (b) a lower-privileged or spoofed operator/voter
trying to weaken policy or approve an action they have no authority over.

**The deliberate departure (threat-modelled honestly).** By product invariant (REQ-1517,
paradigm-rule 4), the engine WARNS but does NOT block a permissive operator policy — an allow-all is
reachable behind a red double-confirmation, and the `conservative` deny template is removable
(REQ-1513). This is a deliberate departure from imposing a second hard never-auto floor above the
constitution. It is safe ONLY because it removes just the operator's OWN engine-layer denies: the
**constitutional mechanical floor (INV-09)** — enforced beneath the engine at the classifier (spec/001
REQ-004) and the actuation adapter (spec/013 REQ-1203) — is NOT one of the removable templates and no
mode, template, `band_mode force`, or allow-all lifts it. Removing the operator floor still leaves
`mkfs` / `dropdb` / `zfs destroy` / reboot / `kubectl delete` clamped to POLL_PAUSE. The residual risk
the operator owns is a permissive reversible-action posture, not an irreversible one; the ledger records
every such change with its actor (REQ-1518) so the paranoia-dial position is always auditable.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A non-member principal votes on a pending decision to approve an action they lack authority over | `/v1/vote` admits a vote ONLY when the principal is a member of the decision's `approve_by` set, resolved against the local registry or the federated provider and checked by `decision_id` under the acting principal's authority | REQ-1516, INV-12, INV-13 |
| **Spoofing** | A federated LDAP/OIDC assertion is forged to impersonate an approver group | Federated principals resolve through the configured provider behind the same interface as local ones; authority is re-checked at vote time, never trusted from the claim alone | REQ-1516, INV-13 |
| **Tampering** | A permissive `auto` rule is ordered above an operator deny to shadow the deny | The fixed Rego module applies **deny-overrides** — any matching deny wins under any rule permutation, asserted as a property (policy-composition invariant) | REQ-1504, INV-09 |
| **Tampering** | Operator "rules as data" carry an injection that alters evaluator logic | Rules enter ONLY as `input.rules` data to a FIXED, audited Rego module; operators never author Rego, so there is no path from rule data to evaluator code | REQ-1503, INV-06 |
| **Tampering** | The mechanical floor is lifted by `band_mode force` or an allow-all policy | `force` and allow-all apply only above the constitutional floor; `safety.IsNeverAuto` / `IsDestructiveOp` clamp beneath the engine regardless of policy; a `force` transition is double-warned and audited | REQ-1510, REQ-1513, INV-09 |
| **Repudiation** | A policy decision or a mode change is denied later | Exactly one immutable `policy_decision` row per evaluation and one mode-transition record per change, each a required output appended to the hash-chained governance ledger; the runtime role has no UPDATE/DELETE | REQ-1502, REQ-1518, INV-19 |
| **Information disclosure** | An under-privileged role reads rules, criticality tiers, or approver sets it has no authority over | Every policy read and edit is authority-checked against the acting user/role under RBAC; the acting model has no read path to policy state | Cross-cutting rule, INV-12 |
| **Denial of service** | A flood of matching actions auto-executes past a safe rate | The rule `rate_limit` governor clamps over-rate actions to `approve`; the mode chokepoint permits actuation only in Semi-auto/Full-auto and the deviation breaker / `/halt` force the mode to Shadow, and the independent never-auto deny-floor remains a distinct layer beneath it (optional host-side sudoers hardening MAY back it, but TG never uses a per-command forced-command key) | REQ-1508, REQ-1520, INV-09/INV-21 |
| **Elevation of privilege** | An irreversible or below-confidence op reaches `auto` via a permissive rule, graduation, or `band_mode force` | The constitutional floor clamps beneath the engine; below-`min_confidence` clamps to `approve`; graduation promotes only on N verified clean runs and demotes on the first `deviation`; `auto` executes only through predict→execute→verify→breaker | REQ-1507, REQ-1514, REQ-1515, INV-09/INV-10 |
| **Elevation of privilege** | Retiring `TG_MUTATION_ENABLED` accidentally defaults actuation ON | `ModeShadow` is the enum zero value and an absent/unreadable mode fails closed to Shadow with the gate off — the retirement preserves the fail-closed zero-value property by construction | REQ-1519, REQ-1520, INV-09 |

**Adversarial acceptance (boundary tests, Phase 4).** Fuzz the rule data with every permutation and
assert deny-overrides holds; assert no `force`, allow-all, or graduation state lifts a constitutional
floor op above POLL_PAUSE; assert a non-member vote is rejected and a forged federated claim does not
authorize; assert a default/unknown mode leaves the mechanical gate off; assert the breaker and `/halt`
force the gate off from every mode. These drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md) §3.1.
