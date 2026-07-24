<!-- spec/017 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/017 — Threat model: Actuation Regime Engine (STRIDE slice)

Per-feature threat slice for the `regime.Engine` and its first lane, the AWX-job actuator. The system-wide
model is [`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes the engine's own trust
boundary and is the security half of the spec's definition-of-done.

**Trust boundary.** The engine sits ABOVE the spec/013 actuation interceptor and BELOW the operator's regime
configuration. Its inputs are a typed `Target`, the operator-declared regime rules, the operator-declared
AWX template-allowlist (`job_template_id → { op_class, extra_vars_schema }`), and the read-only pull from
AWX; its output is a typed `Lane` (or a fail-closed refusal) whose effect leaf the interceptor drives, plus
the immutable `regime_resolution` / `regime_actuation` / `deferred_verdict` rows appended to the
tamper-evident ledger. The assets are (1) the ability to launch an AWX job template that mutates a
production target and (2) the AWX API token. Adversaries of interest: (a) a compromised model output that
tries to launch an arbitrary or non-sanctioned template, smuggle a command through variables, or pick a
permissive lane; (b) an async gap where a job is launched but its outcome is never verified; (c) an attacker
attempting to exfiltrate the AWX token; (d) a poisoned regime rule or allowlist that routes a target down a
weaker channel; (e) a double-fire that actuates twice.

**The deliberate posture (threat-modelled honestly).** An actuator that can trigger AWX job templates is
powerful — but a job template is a NARROWER effect channel than raw SSH, not a wider one: the argv-equivalent
is a fixed template id plus typed, schema-validated `extra_vars` (REQ-1705), with no free-form command
string, so the model cannot express an arbitrary shell command through this lane. It is safe ONLY because
(1) the policy engine authorizes WHICH template/op-class (REQ-1704) and the template-allowlist bounds the set
to sanctioned, reviewed, idempotent job templates; (2) the credential engine authenticates (REQ-1706) and
the mechanical never-auto floor + mode chokepoint still gate every launch — a mutating template is OFF (mode
Shadow) until the owner-present flip (REQ-1707); (3) the launch is a prediction whose effect is verified by a
deferred-verify channel, never assumed at launch (REQ-1709/1710); and (4) the lane adds no gate and bypasses
none (REQ-1702). A selected lane grants no authority the platform beneath it did not already grant.

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A compromised model output names an arbitrary or attacker-chosen job template id to run an unsanctioned playbook | The actuator launches ONLY a template on the operator-declared allowlist and only when the policy engine's verdict for the bound op-class is non-deny; a non-allowlisted or policy-denied template refuses | REQ-1704, INV-06/INV-21 |
| **Tampering** | Free-form or hostile input is smuggled through `extra_vars` to escape the template into an arbitrary command (a shell escape) | The launch body is the template id plus typed `extra_vars` validated against the operator-declared per-template schema; an unknown key is rejected; no free-form command string is ever passed — the argv-equivalent is the template plus its typed variables | REQ-1705, INV-02 |
| **Tampering** | A poisoned regime rule or allowlist routes a target down a weaker/wrong channel or to a wrong regime | Regime resolution matches the shared estate object-model with deterministic most-specific-wins; one regime per target; an unknown regime refuses or takes the operator default, and an ambiguous (multi-regime) target fails closed; every resolution and allowlist edit is appended to the ledger | REQ-1700, REQ-1701, REQ-1703, REQ-1715, REQ-1716, INV-09/INV-19 |
| **Repudiation** | A lane selection, a job launch, or a deferred verdict is denied later | Exactly one immutable `regime_resolution`, `regime_actuation`, and `deferred_verdict` row — each a required output — appended to the hash-chained governance ledger; the runtime role holds no UPDATE/DELETE on these append-only tables | REQ-1712, REQ-1715, INV-07/INV-19 |
| **Information disclosure** | The AWX API token is exfiltrated at rest (config/ledger/export) or leaked into an audit row | The token is a `core/config.SecretRef` resolved at runtime through the sealed store — no plaintext in config, the ledger, or an exportable artifact; `regime_actuation` carries only non-secret launch metadata (never the token or a secret `extra_var` value); gitleaks CI backs the no-literal-secret rule | REQ-1708, REQ-1715, INV-13 |
| **Denial of service** | An AWX job is launched but its outcome is never observed (the async gap), so a failed mutation is silently trusted or a class graduates on unverified runs | The launch returns a job handle and the engine polls to a terminal AWX status through the deferred-verify channel; a launched-but-unverified job is `pending-verification` and counts as no clean run; a job that does not reach terminal within the operator bound is `unverified` and never graduates | REQ-1709, REQ-1710, REQ-1711, INV-10/INV-11 |
| **Denial of service** | A retry, re-poll, or redelivery launches a second job for the same action, double-actuating | Each launch is bound to its `action_id` on the manifest lifecycle chain; a second launch for an `action_id` that already carries a live or terminal job is refused | REQ-1712, INV-07 |
| **Elevation of privilege** | The knowledge/discovery lane is used as an effect channel to run a playbook outside the governed path | The knowledge lane is READ-ONLY, launches nothing, and uses a read-only token distinct from any launch-capable token; a surfaced runbook re-enters only as a proposal subject to the full interceptor chain | REQ-1708, REQ-1713, REQ-1714, INV-21 |
| **Elevation of privilege** | A lane is used to reach an effect without traversing the floor, policy, credential, or mode gates | Every lane's actuator is unexported and reachable only through the spec/013 interceptor `Do`; a standing regime-composition check fails on any exported effect path that skips the chain; the mode chokepoint remains the sole actuation keystone and every mutating lane stays OFF (mode Shadow) until the owner-present flip | REQ-1702, REQ-1707, INV-09/INV-21 |

**Adversarial acceptance (boundary tests, Phase 4).** Fuzz the regime rules and the template-allowlist with
every permutation and assert the fail-closed property (REQ-1701) holds under unknown and ambiguous regimes;
assert the AWX-job actuator refuses every non-allowlisted or policy-denied template and rejects every unknown
`extra_vars` key (REQ-1704/1705); assert no launch fires while the mode is Shadow and no lane reaches an
effect around the interceptor (REQ-1702/1707); assert a launched-but-unverified job never counts as a clean
run and a second launch for an `action_id` refuses (REQ-1711/1712); assert no `regime_actuation` row carries
a plaintext token. These drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md) §3.1.
