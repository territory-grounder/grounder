# Territory Grounder — Execution Plan (Phase 0 / Phase 1)

> **Note (ADR-0010 supersession).** This backlog predates the single-org reframe: every "multi-tenant
> / `tenant_id` + RLS / per-tenant" phrase below is **superseded by [ADR-0010](adr/0010-single-org-multi-user-not-multi-tenant.md)** —
> TG is **single-organization, multi-user** (no `tenant_id`, no cross-org RLS; the DML-only runtime role
> is the privilege boundary; the correlation key is `external_ref`). Read those phrases as their
> single-org equivalent. Phase 0 **and the whole Phase-1 typed spine are now implemented** (see
> `AGENTS.md § Project status`); the P0/P1 task numbering here is historical context, not the live queue.
> **Mutation posture is likewise superseded:** the §0 "Mutation is OFF" guardrail scopes *this plan* (P0/P1),
> not the live system — the Phase-2 flip has since been executed (2026-07-20). Actuation is now governed by
> the **mode chokepoint** (live mode owner-set to Full-auto; absent/zero/corrupt ⇒ Shadow, no actuate). The
> reconciled current state is [`docs/BACKLOG.md`](BACKLOG.md) § Verified state.

> The concrete, dependency-sequenced build backlog for the first two roadmap phases. This
> document turns the [O] roadmap (Phase 0 "secure, read-only foundation" + Phase 1 "typed spine
> and action binding") into ordered engineering tasks with explicit predecessors, acceptance
> criteria, and layer/provenance tags. It is deliberately scoped to the **non-mutating** part of
> the system: **autonomous mutation stays globally OFF for the entire span of this plan.**
>
> Companion documents: see **ARCHITECTURE.md** (component/service layout), **CONSTITUTION.md**
> (the inviolable mechanical safety core + the 3 autonomy bands), **DATA-MODEL.md** (Postgres
> schema, `tenant_id`+RLS, ActionManifest), **ARCHITECTURE.md** / **ARCHITECTURE.md** (the module system),
> **THREAT-MODEL.md** (INV-01..22 threat mapping), and **ROADMAP.md** (Phases 2–4, which this plan
> is the on-ramp to). Terminology is identical across all of them: **ActionManifest**, the five
> execution classes (**DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION /
> HUMAN_LED**), the three autonomy bands (**AUTO / AUTO_NOTICE / POLL_PAUSE**), the **inviolable
> mechanical safety core**, the **module system**, **Temporal**, the **LiteLLM model-gateway**,
> and **`tenant_id` + RLS**.

## Provenance tags

Every substantive task is tagged by the layer it derives from:
`[F]` foundation (the predecessor's inherited design) · `[R]` product reframe (multi-tenant /
de-solo / de-homelab) · `[O]` audit overlay (security + quality hardening). Source ids
(`INV-NN`, `spec/00x`, `paradigm-rule N`, `C-0x`/`H-0x`/`M-0x`, `P0-x`/`P1-x`) are cited inline so
provenance is auditable and the layering cannot be silently re-inverted.

---

## 0. Guardrails that hold for the whole plan

These are not tasks; they are the invariants every task below must respect.

- **Mutation is OFF.** A global `mutation_enabled=false` is compiled in and asserted at boot; the
  entire Phase-0/1 system is a **read-only investigation and audit** platform. Mutation is only
  turned on in Phase 2, and only after the boot preflight (auth + action-binding + verification
  self-test) is green. [O] INV-09, P0-5.
- **The mechanical safety core is present but idle-by-construction.** Even though nothing mutates,
  the never-auto floor, the two-lane fail model (advisory OPEN / remediation CLOSED), and the
  "model is untrusted, deterministic orchestrator owns the effect channel" rule are wired from
  task 1 so that mutation cannot be enabled onto an unsafe base. [F] founding principles · [R]
  paradigm-rule 8 · [O] S8-1, S8-preserve-meta.
- **Multi-tenant from the first migration.** Every table carries `tenant_id` and is isolated by
  Postgres row-level security; correlation keys are `(tenant_id, external_ref)`, never a bare
  `issue_id`. No adapter, query, credential, or retrieval ever crosses a tenant boundary. [R]
  paradigm-rule 1 · [O] INV-12.
- **No high-impact endpoints are ported.** `session-replay`, `chaos-start`/`recover`, `wal-healer`,
  the legacy ticket-trigger, and the OpenClaw mode paths **do not exist in the TG build** —
  re-engagement mints a fresh gated workflow, chaos/replay are internal Temporal signals on an
  elevated listener, never HTTP. [O] P0-2, INV-17, threat "privileged session hijack".
- **The predecessor stack is gone.** No `claude -p`/`claude -r` subprocess, no n8n engine, no
  Cronicle, no sentinel-file control mechanism. Orchestration is Go + Temporal; the agent is a
  native Go ReAct/tool-calling loop over the bundled LiteLLM model-gateway; controls are
  API/RBAC/config-driven and audited on change. [corrections: stack, no-Claude-Code, controls] ·
  [R] paradigm-rules 4, 7.

---

## 1. Dependency overview

```
P0-1 repo skeleton
   ├─ P0-2 Go router + MANDATORY auth middleware ─────────────┐
   ├─ P0-3 Postgres: pgx+sqlc + golang-migrate + RLS baseline ─┤
   │        └─ P0-4 secrets-as-references + gitleaks           │
   ├─ P0-5 argv-only actuation adapter interface (read-only)   │
   ├─ P0-6 LiteLLM model-gateway in compose + fallback ladder  │
   ├─ P0-7 Temporal in compose + worker skeleton               │
   ├─ P0-8 CI lint gates (ban sh -c / string-SQL / gitleaks)   │
   └─ P0-9 boot preflight + mutation_enabled=false             │
                                                               ▼
P1-1 IncidentEnvelope typed canonical + per-field grammar  (needs P0-2,P0-3,P0-4)
   ├─ P1-2 ingest module runtime + one reference ingest module + VerifyAndFetch
   ├─ P1-3 module/plugin runtime (out-of-process, capability-scoped) + reference notifier
   ├─ P1-4 native Go agent loop (read-only tools) over LiteLLM
   ├─ P1-5 single ParseProposal → typed Proposal (one grammar)
   ├─ P1-6 content-hashed ActionManifest + action_id threading
   ├─ P1-7 the read-only Runner Temporal workflow (ingest→triage→context→propose, NO execute)
   ├─ P1-8 per-(source,room) cursors + session-per-workflow isolation
   ├─ P1-9 Temporal Schedules (replaces Cronicle) for periodic read-only jobs
   ├─ P1-10 frontend/ skeleton + first read-only audit/approval console
   └─ P1-11 one-source-of-truth contract gen (OpenAPI from the typed model)
```

Rule of thumb: **P0 makes the surface trustworthy; P1 makes the payload typed.** No P1 task may
be started before its P0 predecessors are green, because a typed spine on an unauthenticated or
shell-interpolating base certifies nothing (the predecessor's exact failure — [O] Verdict).

---

## 2. Phase 0 — Secure, read-only foundation

> Stand up the Go / Temporal / Postgres control-plane with **every trust boundary closed by
> construction** and autonomous mutation globally disabled, so no capability can later be added on
> an unsafe base. [O] Roadmap Phase 0.

### P0-1 — Monorepo skeleton `[R]`

**Do:** Create the Apache-2.0 monorepo `grounder` (CLI `grounder`, alias `tg`) under GitLab group
`products/territory-grounder`, with the service/layer directory layout:

```
core/       deterministic orchestrator primitives, ActionManifest, policy engine, safety core
agent/      native Go ReAct/tool-calling loop (NO Claude Code subprocess)
eval/       judge, prompt-patch trials, RAGAS, 3-set flywheel harness
adapters/   the module INTERFACES (ingest / tracker / notifier+approval / CMDB / actuation / model-provider / observability)
modules/    loadable IMPLEMENTATIONS + a small reference-adapter set + the SDK
temporal/   workflow + activity definitions, schedules, worker wiring
frontend/   TypeScript console service (NOT web/)
deploy/     the single guided docker-compose (single-node profile) + config
sdk/        client + module-author SDK
```

**Depends on:** none. **Blocks:** everything.
**Acceptance:** `go build ./...` succeeds on an empty-but-wired skeleton; `deploy/` compose brings
up nothing dangerous; directory names match exactly (`frontend/` never `web/`; `adapters/` =
interfaces, `modules/` = implementations). [corrections: repo, stack, modules] · [R] paradigm-rule 3.

### P0-2 — One Go router with MANDATORY, non-bypassable auth middleware `[O]`

**Do:** A single chi/grpc-gateway router whose **top-level interceptor** validates **mTLS peer cert
OR per-source HMAC over the raw body with timestamp + nonce** against a Postgres-backed nonce
replay table, *then* dispatches. Handlers are produced by a factory that **takes the authenticated
principal as an argument**, so a route physically cannot be constructed without auth. Each route
registers with a required `auth_method` field; **`auth=none` fails to register at boot** (dead
endpoint, never open). Privileged control ops (replay, chaos, self-heal, session-control) live on a
**separate elevated admin listener with a distinct mTLS trust anchor** — and chaos/replay are
internal Temporal signals, not HTTP at all.

**Depends on:** P0-1, P0-3 (nonce table). **Blocks:** P1-1, P1-2, P1-10.
**Acceptance:** an unauthenticated/unsigned request is rejected **before body-parse**; a route
declared with `auth=none` panics the boot preflight; the admin listener refuses the public trust
anchor. [O] INV-01, P0-1, P0-2, H-01, H-09; threat "unauthenticated actor triggers any receiver".

### P0-3 — Postgres: pgx + sqlc + golang-migrate + RLS baseline `[O]`/`[R]`

**Do:** Exactly **one Postgres reached through one DSN** with declared per-domain ownership. Schema
evolves **only via ordered transactional `golang-migrate` migrations under an advisory lock at
startup**, never inside request handling. The **runtime DB role has DML only, no DDL**. All queries
are **sqlc-generated, compile-time-checked, always-bound** — no string-built SQL anywhere. The
baseline migration establishes: the `nonce` replay table, a `tenants` table, and **`tenant_id`
(NOT NULL) + a row-level-security policy on every table**, with FK / NOT NULL / CHECK / enum
integrity by construction (e.g. `band` enum, `verdict IN ('match','partial','deviation')`).

**Depends on:** P0-1. **Blocks:** P0-2, P0-4, all P1 persistence.
**Acceptance:** a migration lacking a down-migration or introducing an unreferenced table fails CI;
an attempted `CREATE TABLE` at runtime fails at the privilege level; a cross-tenant `SELECT` returns
zero rows under RLS. [O] INV-03, INV-16, M-03, P2-1, P2-2 · [R] paradigm-rule 1, "all DATA tables".

### P0-4 — Secrets as references + per-adapter least-privilege identity + gitleaks `[O]`

**Do:** No credential value ever appears in code/config/log/exported artifact. Docker-compose
injects secrets as env/secret-store **references** resolved by the Go config layer **in memory
only**; a startup config linter rejects high-entropy literals. Every actuation/adapter identity is
**least-privileged and scoped per adapter/operation** (per-tenant + per-agent) — no shared root, no
`StrictHostKeyChecking=no` (it is not expressible), credential-revoke is the kill primitive. Because
orchestration is compiled Go (not exportable JSON), **there is no blob to embed a secret into.**

**Depends on:** P0-1, P0-3. **Blocks:** P0-6, P1-2.
**Acceptance:** `gitleaks` runs in CI and fails on any literal secret; the config linter rejects a
seeded high-entropy string; the single-shared-SSH-identity anti-pattern is structurally absent. [O]
INV-13, P0-4, S8-7 · [R] paradigm-rule 3 (identity is the tenant boundary), "actuation surface".

### P0-5 — Argv-only actuation adapter interface (read-only, no shell) `[O]`

**Do:** Define the actuation adapter contract: `Exec(ctx, argv []string, stdin []byte)` implemented
via `exec.Command(bin, args...)` or `golang.org/x/crypto/ssh` with a **fixed command vector and
pinned host keys**. **No `sh -c`, no `fmt.Sprintf` into a command string, no manual quote-escaping
helper.** Every caller-supplied scalar is parsed into a typed Go value before use. For Phase 0 the
only registered actuation adapters are **read-only** (get/describe/logs class); the interface exists,
mutation does not. Temporal **workflows contain deterministic decision logic only and cannot touch
the OS** — every side effect is an activity against a capability-scoped adapter.

**Depends on:** P0-1, P0-3. **Blocks:** P1-4 (agent tool calls), Phase 2 mutation.
**Acceptance:** a CI grep gate bans `sh -c` and shell-built commands (see P0-8); a fuzz fixture of
metacharacters/newlines/Unicode passed as an argv scalar cannot alter the executed program. [O]
INV-02, P0-3, C-02, C-03, C-04, H-06; threat "OS command injection".

### P0-6 — LiteLLM model-gateway in docker-compose + provider fallback ladder config `[R]`

**Do:** Bundle **LiteLLM** as the **model-gateway** service in compose: one OpenAI-compatible
endpoint fronting N providers, with the auto-fallback ladder, retries, rate-limit handling, and
**per-tenant budgets/quotas** as configuration. Default user-configurable ladder (per-tenant):
`z.ai` (primary) → `DeepSeek` → `Mistral` → then `Anthropic` / `OpenAI` / `Grok` / etc., with
automatic fallback on error/rate-limit/outage. Keep the **single `component → provider/model`
source-of-truth resolver** as a config surface (the predecessor's three hardcoded planes are
dropped; cost/locality is per-tenant policy, local-first-$0 is one selectable mode). Provider keys
resolve through P0-4 references only.

**Depends on:** P0-1, P0-4. **Blocks:** P1-4 (the agent calls LiteLLM, never a Claude Code CLI).
**Acceptance:** the agent reaches all providers through the one LiteLLM endpoint; killing the
primary provider transparently fails over down the ladder; a per-tenant budget cap is enforced at
the gateway. [corrections: LiteLLM model-gateway, fallback ladder] · [R] paradigm-rule 3, 6,
"centralized model routing" · [F] centralized model routing.

### P0-7 — Temporal in compose + worker skeleton `[R]`

**Do:** Stand up Temporal (server + one Go worker) in the compose stack. Establish task-queue and
worker conventions, the `tg/{tenant}/{session_id}` workflow-id scheme, and `WorkflowIdReusePolicy`.
Temporal is deliberately load-bearing: it replaces the predecessor's n8n engine, the Cronicle
scheduler, and most of the watchdog/reconcile machinery — the "no-data ≠ no-alert" `absent()`
principle is preserved but sourced from Temporal worker/workflow health rather than a DIY heartbeat
script. **Fixed 5-slots become workers/task-queues** with per-tenant configurable concurrency +
fair-share queueing.

**Depends on:** P0-1. **Blocks:** P1-7, P1-8, P1-9.
**Acceptance:** a trivial workflow runs to completion on the worker; workflow ids are tenant-scoped;
worker-down is observable via Temporal health, not a sentinel file. [corrections: substrate
migration table] · [R] paradigm-rule 7, "session orchestrator", tension "session concurrency".

### P0-8 — CI lint/security gates `[O]`

**Do:** Wire the CI gates that make the injection/leak class **uncompilable by policy**:
(a) grep/lint **ban `sh -c` and any shell-built command string**; (b) grep/lint **ban string-formatted
SQL** (only sqlc-generated bound queries allowed); (c) **gitleaks** secret scan; (d) fail a migration
without a down-migration or with an unreferenced table; (e) `find_dead_code` / retired-identifier
grep gate so nothing ships "retired-but-present". These gates run on every merge request.

**Depends on:** P0-1 (and evolves with P0-3/P0-5). **Blocks:** none, but gates all merges.
**Acceptance:** a PR introducing `sh -c`, a `fmt.Sprintf` SQL, or a literal token is red. [O] INV-02,
INV-03, INV-13, INV-17, P1-3, P3-2.

### P0-9 — Boot preflight + `mutation_enabled=false` `[O]`

**Do:** A boot-time preflight that asserts **ingress-auth wired (P0-2) + action-binding wired
(placeholder until P1-6) + verification wiring** and **keeps the global `mutation_enabled` flag
false** (read-only actuation) until all are green. The zero-value / unmatched path of every safety
enum is the most-restrictive option (`Band` zero-value = `POLL_PAUSE`), so any error/panic fails
closed. This is the gate that guarantees the whole Phase-0/1 span is non-mutating.

**Depends on:** P0-2, P0-3, P0-5. **Blocks:** Phase 2 (which flips the flag).
**Acceptance:** boot with any trust-boundary wiring absent → refuse to start; `mutation_enabled`
cannot be flipped true without the preflight green; read-only actuation still works. [O] INV-09,
P0-5, H-09 · [F] "graded fail-closed autonomy" · [R] paradigm-rule 8.

---

## 3. Phase 1 — Typed spine and action binding

> Make every input a validated typed envelope and every proposed action a single immutable
> content-hashed object, eliminating the injection and grammar-mismatch classes. Still **no
> execution** — the Runner workflow stops at *propose*. [O] Roadmap Phase 1.

### P1-1 — Canonical typed `IncidentEnvelope` + per-field grammar validation `[O]`/`[R]`

**Do:** One Protobuf/JSON-Schema **`IncidentEnvelope`** Go type is the single boundary
representation. A single ingest handler runs **each identifier field through a typed validator** —
`net.ParseIP`, a compiled hostname regex, exhaustive-switch enums for rule/severity/site/op — and
**rejects with 4xx before enqueueing** any Temporal workflow. `RawEvent` is **unexported past the
ingest package** (compiler-enforced) so no later stage can read the unsanitized body. Rooms / slots
/ hosts / scripts / ops resolve through **server-side allowlisted maps keyed by validated fields**.
Every envelope carries `tenant_id`; the correlation key is `(tenant_id, external_ref)`.

**Depends on:** P0-2, P0-3. **Blocks:** P1-2, P1-4, P1-5, P1-7.
**Acceptance:** a malformed hostname / non-enum severity is rejected pre-enqueue; a downstream stage
cannot compile against `RawEvent`; a missing required field is a loud validation error, never a
silently-empty interpolation (the predecessor's empty-RAG-query bug). [O] INV-04, P1-2, C-04, M-10,
H-05 · [R] paradigm-rule 1.

### P1-2 — Ingest module runtime + one reference ingest module + `VerifyAndFetch` `[R]`/`[O]`

**Do:** Define the **ingest adapter interface** (in `adapters/`) and ship **one reference ingest
module** (in `modules/`, e.g. a Prometheus or LibreNMS reference) that normalizes to the
`IncidentEnvelope` and **publishes a `triage.requested` event**. The interface includes
**`VerifyAndFetch(sig, id) → Canonical`**: verify the per-source HMAC (distinct secret + replay
window), **then re-read the canonical entity by ID from its system-of-record using the platform's
own credential** — posted mutable fields are discarded, payload text is only a trigger. In-code
`normalize → dedup(sha256 + line-count / 24h) → flap → burst → correlate → cooldown` runs **before
any model is spent**, with all dedup/cooldown keys scoped by `tenant_id`. `site` is one field in the
per-tenant estate model, not a forked workflow — **NL/GR-style multi-site is config rows, not
copy-forks.**

**Depends on:** P1-1, P0-4, P0-7, P1-3 (module runtime). **Blocks:** P1-7.
**Acceptance:** the reference module loads/unloads at runtime; a forged payload without a valid
signature never dispatches; a webhook is treated as an untrusted claim (re-fetched by ID); two
"sites" are two config rows satisfying one schema. [O] INV-05, INV-18, C-03, M-08, M-09, P1-1 · [R]
paradigm-rules 3, 9, "event-source receivers" · [F] "deterministic code acts before any model".

### P1-3 — Module/plugin runtime (out-of-process, capability-scoped) + reference notifier `[R]`/`[O]`

**Do:** Implement the **module system** (ADR-0005 default: **out-of-process governed plugins** —
each module a separate process/container over gRPC / HashiCorp go-plugin, with **MCP for
tool/actuation modules**). Modules are **signed, capability-scoped, per-tenant-enabled**; a
**disabled/unregistered module has NO execution path** (this is what kills the predecessor's "dead
OpenClaw path still executable" class). Ship the **generic notifier + async-approval adapter**
interface with **one reference notifier module** (e.g. Matrix or Slack or webhook) and the
**Temporal signal as the resume primitive**; sender-auth + PII/credential redaction are
adapter-agnostic requirements. A capability **exists only if its adapter is compiled in and
explicitly registered at startup** — no runtime "mode" string, no host trust path for an
unregistered backend; a startup reconciler compares live registered modules against a **signed
declared manifest** and refuses to start on mismatch.

**Depends on:** P0-1, P0-3, P0-4. **Blocks:** P1-2, P1-7 (notifier), P1-10.
**Acceptance:** loading a module grants exactly its declared capabilities and nothing cross-tenant;
unregistering a module removes its execution path entirely; a boot with registered modules ≠ signed
manifest refuses to start; the async-approval flow resumes a paused workflow via a Temporal signal.
[corrections: modules are loadable, ADR-0005 out-of-process, governed-by-construction] · [O] INV-17,
INV-01, H-08, M-12, D9-dead-paths · [R] paradigm-rules 3, 4, "human channel + approval polls".

### P1-4 — Native Go agent loop (read-only tools) over LiteLLM `[R]`/`[F]`

**Do:** Build the **`agent/` native Go ReAct / tool-calling loop** that calls LLM **APIs directly
through the bundled LiteLLM gateway** (P0-6) — **there is no `claude -p` / `claude -r` subprocess.**
Port the reasoning discipline verbatim: parseable **CONFIDENCE** scalar with STOP thresholds
(<0.5 STOP-and-wait; escalate below 0.7 or on critical), ReAct structure, and a per-agent turn budget
(≥5 forces POLL, ≥10 hard-halt). (The predecessor's manager-pattern **read-only sub-agents-as-tools**
split is a DEFERRED design under an evidence-gated HOLD — not shipped; TG runs a single native loop. See
CONSTITUTION §4.14.) In Phase 1 the agent may only call **read-only actuation tools**
(P0-5) — it investigates and *proposes*, it never executes. **No model-produced token becomes
control flow, a command string, or a query fragment**; model output enters only as typed, validated,
delimited data.

**Depends on:** P0-5, P0-6, P1-1. **Blocks:** P1-5, P1-7.
**Acceptance:** the loop drives a real LiteLLM call, uses read-only tools, and emits a structured
proposal; write tools are absent from every sub-agent's tool set; a low-confidence / novel incident
terminates or escalates rather than proceeding. [corrections: native agent loop, no Claude Code] ·
[O] INV-08, S8-1, H-05 · [F] "confidence first-class", "least-autonomous topology" · [R]
paradigm-rule 8.

### P1-5 — Single `ParseProposal` → typed `Proposal` (one grammar) `[O]`

**Do:** The model emits a **typed function/tool call (JSON-schema-constrained)**, not markdown with
a sentinel marker. **`ParseProposal(resp) (Proposal, error)` is the sole entry point** with a
fail-closed error path; there is **exactly one proposal/approval grammar**, defined in one Go
package **imported by both the parser and the (future) gate**. Unparseable or non-manifest-expressible
output is **rejected, never routed through a looser fallback path** — this closes the predecessor's
crown-jewel bypass (a second `"Which plan? - Plan X:"` grammar that ran after the gate). The marker
is parsed deterministically and **never trusted as authority**.

**Depends on:** P1-1, P1-4. **Blocks:** P1-6.
**Acceptance:** a property test enumerates every parser path and finds no second grammar; malformed
proposals fail closed (→ POLL_PAUSE); the same grammar object is imported by parser and gate. [O]
INV-06, H-02, P1-5 · [F] "confidence + reasoning discipline (formalized CONTRACT parsing)" · [R]
"stays unchanged: marker parsed deterministically not trusted as authority".

### P1-6 — Content-hashed `ActionManifest` + `action_id` threading `[O]`

**Do:** Define the Go **`Action`** schema with deterministic canonicalization; **`action_id =
SHA-256(canonicalJSON(Action))`** computed once. The immutable content-hashed **`ActionManifest`**
binds normalized **target / op / params / band / plan-hash / prediction-hash / approval-choice /
tool-calls / verification**, sealed at creation and persisted **append-only**. Temporal carries
`action_id` as workflow state; **each activity re-derives and asserts it** (deterministic replay
guarantees equality). **Any mutation of the Action yields a new id that invalidates all prior
authorization and re-enters the gate** — identity, not existence, is what the gate protects. In
Phase 1 the manifest is *built and threaded* (so the binding exists and can be inspected in the
console) but **no execution stage consumes it yet** — that is Phase 2. The Temporal **workflow
activity ordering** is fixed here: `Predict → Approval → Execute → Verify` (with Execute/Verify
stubbed-out under `mutation_enabled=false`).

**Depends on:** P1-5, P0-3, P0-7. **Blocks:** P0-9 preflight (action-binding wiring), all Phase 2.
**Acceptance:** changing any Action field changes `action_id`; a stage receiving a mismatched
`action_id` hard-fails closed; the manifest is append-only and hash-linked. This is the load-bearing
binding lesson — "a prediction exists" must never be mistaken for "the prediction is for the thing
being executed." [O] INV-07, INV-10, H-03, P1-4, S8-3, S8-preserve-meta.

### P1-7 — The read-only Runner Temporal workflow `[F]`/`[R]`

**Do:** Implement the session **Runner** as a Temporal workflow with the gated activities
`lock → cooldown → RAG-context → classify → commit-prediction → build-prompt → agent-loop →
parse → validate → screen → prediction-gate`, **stopping at *propose*** — **the `execute` and
`verify` activities are present but no-op under `mutation_enabled=false`.** Durable pause/resume,
immutable per-turn snapshots (`pending_tool` / `pending_tool_input`), and resume-via-stable
`(tenant, issue_id)` are Temporal-native; the auto-resume loop that was OPEN in the predecessor is
closed as `continue-as-new`. This workflow **is** the deterministic orchestrator — it owns control
flow and the (currently read-only) effect channel; the agent only proposes.

**Depends on:** P0-7, P1-1, P1-4, P1-5, P1-6, P1-8. **Blocks:** Phase 2 (which un-stubs execute/verify).
**Acceptance:** an ingested incident flows end-to-end to a persisted `Proposal` + `ActionManifest`
with no mutation; a killed worker resumes the session from its last durable snapshot; the workflow
contains no OS execution. [F] "session orchestrator (the Runner)", incident lifecycle loop · [R]
"session orchestrator" reframe, tension "session concurrency" · [O] INV-21 (control-flow contains no
OS execution).

### P1-8 — Per-`(source,room)` cursors + session-per-workflow isolation `[O]`

**Do:** Each inbound chat/alert event is processed individually, keyed by
**`(source_id, native_event_id)`** with **per-source ordering and a per-source durable cursor** — no
global cursor, no cross-source concatenation. Ingest writes to a `chat_events` table with
**`UNIQUE(source_id, event_id)` (ON CONFLICT DO NOTHING)** and a per-`(source_id, room_id)` cursor
updated transactionally. An approval/rejection binds to a specific pending decision by
**`decision_id`** (carrying `action_id` + `room_id`) and routes via **Temporal `SignalWithStart` to
the exact owning workflow**. Every session is an isolated Temporal execution keyed by
`(tenant, session_id)`; cancel = `TerminateWorkflow(id)`; **no process-wide `is_current` pointer,
lock, or `pkill`.** Postgres RLS + NOT NULL FK `(tenant_id, session_id)` block cross-tenant reads.

**Depends on:** P0-3, P0-7. **Blocks:** P1-7.
**Acceptance:** a late event in another room is neither dropped nor mis-attributed; an approval lands
on exactly its originating session; a cancel cannot hit a bystander session; cross-tenant access is
impossible under RLS. [O] INV-12, H-04, H-07, P1-6 · [R] paradigm-rule 1.

### P1-9 — Temporal Schedules (replaces Cronicle) for periodic read-only jobs `[R]`

**Do:** Express every periodic job as a **Temporal Schedule** (run-history / retries / dead-man
native) — the predecessor's Cronicle scheduler is dropped entirely. Phase-1 schedules are all
**read-only**: e.g. wiki/RAG recompile, the synthetic-incident canary against an **isolated ephemeral
Postgres** (live-DB-leak counter must stay 0), component-liveness discovery, and judge-death /
metrics rollups. The lean residual **platform-controller** heals only non-Temporal things (the
LiteLLM gateway, a dead module process, a stuck PG pool) and is structurally forbidden from
estate-mutating actions ("heal-platform-never-mission").

**Depends on:** P0-7, P1-3. **Blocks:** none (feeds eval/self-monitoring).
**Acceptance:** a schedule that should have run and didn't is observable in Temporal; the canary runs
against a throwaway DB and never the live one; the platform-controller has no mission verb reachable.
[corrections: substrate migration table] · [R] paradigm-rule 7, "native scheduler", "platform
controller" · [F] "synthetic-incident canary", "Plane-A/Plane-B separation".

### P1-10 — `frontend/` skeleton + first read-only audit / approval console `[R]`

**Do:** Scaffold the **`frontend/`** TypeScript console service and build its **first read-only
views**, consuming the generated OpenAPI (P1-11) — no second contract. First-cut surfaces:
the **ActionManifest timeline** (predicted → approved → executed → verified as one visual chain —
execute/verify empty in Phase 1), the **tamper-evident ledger view**, an **explainability** panel
("why did the agent propose this"), and the **approval console** shell (the human-circuit-breaker
surface — inert while mutation is OFF, but present so Phase 2 lights it up). **Autonomy-band + kill
controls live here (API/RBAC-driven), never on host-local sentinel files.** Multi-tenant admin is a
first-class dimension of every view.

**Depends on:** P0-2, P1-3, P1-6, P1-11. **Blocks:** Phase 2 UX (live approvals).
**Acceptance:** the console renders a real manifest + ledger chain read-only; band/kill controls call
the RBAC-gated API (and are audited on change); the directory is `frontend/`, and it consumes only
the generated OpenAPI. [corrections: first-class UX, controls move onto UI/API+RBAC, API-first,
frontend/ not web/] · [R] paradigm-rule 4 · [O] INV-15 (API-first single contract).

### P1-11 — One-source-of-truth contract generation (OpenAPI from the typed model) `[O]`

**Do:** Each logical entity is **one Go struct**; **sqlc + a schema-emit tool generate the migrations,
JSON Schema, OpenAPI/AsyncAPI, and marshallers from it** — never hand-maintained in parallel.
Contracts round-trip losslessly from the Postgres model, cover **100% of routed endpoints with
declared auth / error / idempotency schemas**, and **every generated artifact embeds a non-null
`generated_at` + source hash + coverage scope**. CI regenerates and **diffs — a nonzero diff, an
uncovered path, a hand-written number, or null provenance fails the build.**

**Depends on:** P0-2, P0-3, P1-1, P1-6. **Blocks:** P1-10 (consumes the OpenAPI).
**Acceptance:** README counts / diagrams / the critical-component table are generated (no hand-written
numbers); a route without an auth scheme fails the coverage gate; a testcontainers round-trip writes a
real row via the production path and validates it against the generated contract. [O] INV-15, M-01,
M-05, M-07, D9-single-source, D9-contract-authority · [R] "open-standard interface contract discipline".

---

## 4. What is explicitly NOT in this plan

- **Any mutation / autonomous remediation.** The RiskClassifier bands, prediction-gate *enforcement*,
  the mechanical verdict, the wired pre/post interception, typed Evidence gating, and the flip of
  `mutation_enabled → true` are **Phase 2** (see ROADMAP.md). Phase 0/1 build the manifest and the
  gate wiring; Phase 2 turns the key. [O] Roadmap Phase 2, INV-09/-10/-11/-19/-20/-21.
- **Anti-drift / decommission automation beyond P1-11** (full manifest reconciler, per-data-class
  retention purge worker) — **Phase 3**. [O] Roadmap Phase 3, INV-14/-15/-17.
- **The adversarial assurance gate** (executable e2e with malicious/concurrent/replayed/delayed/
  partial-failure fixtures, boundary-coverage metric, ledger tamper-replay test) — **Phase 4**;
  Phase 0/1 CI gates (P0-8) are the seed, not the whole suite. [O] Roadmap Phase 4, INV-22.
- **Reduce-scope / optional modules** — teacher-agent pedagogy, parallel-dev decomposition, the
  A2A inter-agent protocol, and the chaos program are **optional per-tenant modules**, not part of
  the governed-autonomy core, and are out of the Phase-0/1 backlog. [R] paradigm-rules "teacher",
  "parallel-dev", "NL-A2A", "chaos"; [F] `reduce-scope` tags.

## 5. Exit criteria for the plan

Phase 0/1 is "done" when, on the running compose stack:

1. No request reaches any handler/planner/module/DB call without verified caller identity, and an
   `auth=none` route fails to register at boot. [O] INV-01.
2. No `sh -c`, no string-built SQL, no literal secret survives CI; actuation is argv-only. [O]
   INV-02, INV-03, INV-13, P0-8.
3. One Postgres, one DSN, DML-only runtime role, `tenant_id` + RLS on every table, cross-tenant
   access impossible. [O] INV-16 · [R] paradigm-rule 1.
4. A real incident flows ingest → typed `IncidentEnvelope` → native-Go agent loop over LiteLLM →
   single `ParseProposal` → content-hashed `ActionManifest`, entirely **read-only**, with the
   Runner Temporal workflow stopping at *propose*. [F]/[R]/[O] P1-1..P1-7.
5. Modules load/unload out-of-process and capability-scoped; an unregistered module has no
   execution path; boot refuses a registered-set ≠ signed-manifest mismatch. [O] INV-17 · [R]
   paradigm-rules 3, 4.
6. The `frontend/` console renders the ActionManifest timeline + tamper-evident ledger read-only,
   consuming the generated OpenAPI; band/kill controls are API/RBAC-driven and audited. [R]
   paradigm-rule 4 · [O] INV-15.
7. `mutation_enabled` is still **false**, and the boot preflight proves it cannot be flipped without
   auth + action-binding + verification green. [O] INV-09, P0-5.
