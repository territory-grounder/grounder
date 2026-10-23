<!-- spec/008 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/008 — Threat model: the day-1 connector fleet (STRIDE slice)

Per-surface threat slice for the loadable connector modules. The system-wide model is
[`docs/THREAT-MODEL.md`](../../docs/THREAT-MODEL.md); this file scopes each module surface's own trust
boundary and is the security half of this spec's definition-of-done. Single-org: one estate, no
`tenant_id`; isolation controls below are least-privilege per adapter/operation.

**Trust boundary.** Every connector is a signed, capability-scoped module behind an `adapters/`
interface; a disabled or unregistered module has **no execution path** (INV-17). Ingest and tracker
modules cross an *inbound* boundary — an external system's payload is a claim, never a fact, until
grammar-validated (INV-04) and re-read by id from its system-of-record (INV-05). Actuation and notifier
modules cross an *outbound* boundary — the deterministic orchestrator owns the effect channel and the
model is untrusted (INV-08); actuation is reachable only through the single `Execute(ctx, ActionManifest)`
chokepoint (INV-21). The adversary of interest is a compromised external system, a manipulated LLM
trying to reach an effect, and a forged approval trying to launder an action into AUTO.

## Ingest surface (REQ-801..803)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A forged alert payload impersonates a real monitoring source to trigger a session | The ingest module authenticates the source and validates every field against an explicit grammar before admission | REQ-801/802/803, INV-04 |
| **Tampering** | A payload asserts a false host/entity to misdirect remediation | The canonical entity is re-read by id from its system-of-record (NetBox / the source API) before dispatch — the payload is a claim | REQ-810, INV-05 |
| **Repudiation** | An admitted alert cannot be traced to its raw input | Each admitted event carries its normalized canonical shape and emits a traced `triage.requested` signal | REQ-802 |
| **Information disclosure** | Ingest credentials (API tokens, LAPI keys) leak | Credentials are configuration held outside the module blob; the module reads them from the config store, never from a workflow export | REQ-801, INV-13 |
| **Denial of service** | An alert storm floods the classifier and inflates spend | In-code dedup, flap, and burst admission run before any model dispatch; correlation folds firing→resolved to one incident | REQ-801/802/803 |
| **Elevation of privilege** | A duplicated ingest implementation drifts one site into an ungoverned path | Exactly one implementation per source type driven by config (two config rows, one module); a CI parity test enforces it | REQ-801, INV-18 |

## Tracker surface (REQ-804..805)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | A forged issue transition starts an unauthorized session | The tracker authenticates against the tracker's own credential and drives state through the authenticated REST contract | REQ-804 |
| **Tampering** | The correlation key is swapped to misattribute a session | The issue id is the single correlation key across the session lifecycle, re-read from the tracker | REQ-804, INV-05 |
| **Repudiation** | The terminal disposition is denied later | The tracker posts one terminal audit comment as the session sink | REQ-804 |
| **Elevation of privilege** | A reference tracker swap opens an ungoverned side path | Reference trackers satisfy the identical trigger/correlation/state/sink contract behind one interface; a swap is config, not a fork | REQ-805, INV-18 |

## Notifier + approval surface (REQ-806..809)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | An unauthenticated sender casts an approval vote | The sender is authenticated against the approver set before a vote is accepted | REQ-806/808/809 |
| **Tampering** | A vote is misattributed to the wrong pending decision | Each vote is bound to a specific `decision_id` and routed via a Temporal signal to exactly the owning workflow | REQ-806/808, INV-12 |
| **Repudiation** | An approval cannot be reconstructed | Votes append to the tamper-evident governance ledger | REQ-806, INV-19 |
| **Information disclosure** | Credentials or PII leak into a posted channel body | Credential and PII redaction is a channel-agnostic obligation every notifier inherits | REQ-806/809 |
| **Elevation of privilege** | A page channel carries a command that executes | The Twilio page carries no command-executing content and is one channel, not the effect path | REQ-807, INV-08 |

## CMDB surface (REQ-810)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Tampering** | A stale or spoofed entity misdirects an action | NetBox is the authoritative re-read-by-id source every ingested payload passes through before dispatch | REQ-810, INV-05 |
| **Information disclosure** | CMDB reads expose more than an operation needs | Least-privilege scoping per adapter/operation | REQ-810, INV-12 |

## Actuation surface (REQ-811..814)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Tampering** | An approved action is swapped for a different effect after approval | Actuation is bound to the immutable content-hashed `action_id`; a mutated action mints a new id and re-enters the gate | REQ-811, INV-07 |
| **Repudiation** | A mutating command has no undo record | One `execution_log` row per mutating command captures pre/post state and the exact rollback command | REQ-811 |
| **Information disclosure** | A shell or host-key bypass exfiltrates or man-in-the-middles | No module can express a shell; SSH is fixed argv / validated-JSON stdin; `StrictHostKeyChecking=no` is inexpressible | REQ-811, INV-02/INV-13 |
| **Elevation of privilege** | An irreversible op reaches the auto ceiling via confidence or a flag | kubectl delete/drain and Proxmox reboot/halt are clamped to the non-configurable never-auto floor; lifecycle ops are enable-flag-gated | REQ-812/813, INV-09 |
| **Elevation of privilege** | An unregistered or disabled tool still executes | An unregistered MCP tool has no execution path; every actuation traverses the single `Execute` chokepoint (a startup self-test fails boot if the guard is unwired) | REQ-814, INV-17/INV-21 |

## Model-provider surface (REQ-815)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Spoofing** | Model output impersonates control flow or a command string | Every model response enters only as untrusted, typed, delimited data — it never becomes control flow, a command string, or a query fragment | REQ-815, INV-08 |
| **Tampering** | A compromised provider returns a manipulated tool call | The response is parsed once against a typed schema by the single proposal parser; a bad parse fails closed | REQ-815, INV-06 |
| **Repudiation** | Token spend is fabricated or unattributable | Real-token usage is recorded per request to `llm_usage` with no fabrication | REQ-815 |
| **Denial of service** | A provider outage stalls the agent | The configured auto-fallback ladder advances on error or rate-limit across z.ai → DeepSeek → Mistral → Ollama → Anthropic/OpenAI | REQ-815 |

## Observability surface (REQ-816..820, REQ-823)

| STRIDE | Threat | Control | Requirement / invariant |
|---|---|---|---|
| **Repudiation** | A session's trajectory cannot be reconstructed after the fact | OTLP tracing is default-on to OpenObserve; per-session traces to Langfuse keyed by session id | REQ-818/819 |
| **Tampering** | A metric writer dies silently and the plane reads as healthy | Each series is freshness-stamped and an `absent()`-guarded staleness metric pages on a dead writer | REQ-816, INV-15 |
| **Denial of service** | The internal alert path itself is dark and no one is paged | Healthchecks.io is an external dead-man pinged on each heartbeat, independent of the internal alert path | REQ-820 |
| **Elevation of privilege** | A hand-edited dashboard hides a control's true state | Grafana dashboards are provisioned from version control; hand-edited panels are rejected as drift | REQ-817, INV-15 |
| **Spoofing** | An on-path adversary impersonates a syslog server to feed the agent forged device logs or harvest the read credential | The native in-process SSH client verifies the server host key against the operator-declared known_hosts file; an unknown or changed key refuses the handshake, and no known_hosts file refuses every read (fail closed — no trust-on-first-use, no insecure bypass) | REQ-823, INV-13 |
| **Tampering** | A model-chosen host or pattern argument steers the remote read into an arbitrary file or a flag | Strict host allowlist (no separator, no parent-directory reference, no leading dash), a path built from validated components only, and a fixed per-element POSIX-quoted argv the remote shell re-parses verbatim — no string-built command, no local shell, no subprocess | REQ-823, INV-02 |
| **Information disclosure** | The SSH private key leaks via a staged file, an error message, or a log line | The key is a secret REFERENCE resolved at read time and parsed in memory; it is never written to a filesystem path, and a key failure names the reference only, never material | REQ-823, INV-13 |
| **Elevation of privilege** | Returned log text (attacker-influenceable device output) becomes control flow | The log text is an untrusted observation bounded in bytes and lines; nothing in it is parsed into a command, query, or routing decision | REQ-823, INV-08 |

**Adversarial acceptance (boundary tests, Phase 4).** Feed each ingest module a spoofed/malformed
payload and assert grammar rejection + re-read-by-id; feed the notifier surface a forged vote and assert
sender-auth rejection + `decision_id` binding; drive the actuation chokepoint with an irreversible op
under maximal confidence and assert the never-auto clamp holds; disable a module and assert it has no
execution path; return a manipulated tool call from a provider and assert the fail-closed parse. These
drive the actual code path (INV-22) — see
[`docs/TESTING-AND-BENCHMARK.md`](../../docs/TESTING-AND-BENCHMARK.md).
