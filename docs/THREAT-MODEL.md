# THREAT-MODEL.md — Territory Grounder

> Layer tags: **[F]** foundation (inherited predecessor design) · **[R]** product reframe (multi-user / de-solo) · **[O]** audit overlay (security + quality hardening). Source ids in brackets: `INV-NN` (invariants), `C-/H-/M-/P-/S8-` (audit findings), `spec/00x` (EARS specs), `paradigm-rule N`.
>
> Sibling docs: architecture and the deterministic control-plane in **ARCHITECTURE.md**; the mechanical safety core, bands, and never-auto floor in **CONSTITUTION.md**; the module/adapter interfaces in **ARCHITECTURE.md**; the founding constitution in **the-map-is-not-the-territory.md**; per-invariant enforcement detail in **CONSTITUTION.md**.

## 1. Purpose and posture

Territory Grounder (TG) is an open-source, self-hosted, **single-organization, multi-user** governed-autonomy SRE platform: a deterministic orchestrator that owns the effect channel, driving an untrusted probabilistic model over a bundled LiteLLM model-gateway, on Go + Temporal + PostgreSQL/pgvector **[R]** (paradigm-rule 7). This document is its threat model.

**TG does not assume a benign perimeter.** The predecessor's safety story was "real in intent but false in binding" — a deterministic-orchestrator-over-untrusted-model architecture whose every trust boundary leaked because it depended on an **unowned network perimeter** and on author discipline rather than construction **[O]** (audit verdict; C-01). That assumption is retired. TG is a distributable product with a real, external attack surface: it is installed by parties TG's authors will never meet, exposed to whatever ingress each deployment permits, and operated by many users whose roles and privileges differ **[R]** (paradigm-rule 1). A "homelab perimeter" is nobody's control here.

The governing design principle is the audit's meta-lesson: **retain every good control but move it behind a typed, authenticated interface bound to the exact action being authorized — a control is only as strong as its binding** **[O]** (S8-preserve-meta). TG's mandate is therefore to make the injection / bypass / drift class **structurally uncompilable**, not merely discouraged (audit verdict).

### Threat-actor model
- **Unauthenticated network actor** — can reach any exposed listener; assumed present on every deployment.
- **Malicious/compromised upstream** — a monitoring source, ticket, or chat message whose *content fields* are attacker-controlled (a forged webhook body, a crafted issue summary, a poisoned alert label).
- **The model itself** — treated as an untrusted, possibly-manipulated component (prompt-injected, jailbroken, or simply wrong). No model-produced token is trusted as authority **[F/O]** (S8-1, INV-08).
- **A hostile, careless, or over-reaching user/agent** — a low-privilege operator, or an agent whose scoped credential has been captured, attempting to act beyond its granted role/capability on data, credentials, or the control plane **[R]** (paradigm-rule 1).
- **A privileged-but-fallible operator/approver role** — can approve, but the mechanical safety core still refuses irreversible auto-action regardless (CONSTITUTION.md) **[R/F]**.

---

## 2. Trust boundaries

Four boundaries partition the system. Each is a place where a **lower-trust claim** must be converted into a **higher-trust fact** by deterministic code — never carried across on faith.

### 2.1 Ingress boundary — network → control-plane `[O]`
Every inbound request (alert receiver, ticket webhook, chat/approval event, stats/replay API, admin control op) crosses here. **The claim is a request; the fact is an authenticated, schema-valid, canonical envelope.**

- Authentication is **a property of construction, not configuration** **[O]** (INV-01): a single Go router's top-level interceptor validates mTLS peer cert *or* per-source HMAC over the raw body with a monotonic timestamp + nonce replay window (Postgres-backed nonce table) **before body-parse**; handlers are built by a factory that *takes the authenticated principal as an argument*, so a route cannot be wired without auth, and a route declaring `auth=none` **fails to register at boot** — forgetting to configure auth yields a *dead* endpoint, not an open one.
- Privileged control operations (replay, chaos, self-heal, session control) are a **separate elevated authorization tier on a distinct admin listener with its own mTLS trust anchor** — never a plain ingress path; chaos/replay are **internal Temporal signals, not HTTP** **[O]** (INV-01).
- A webhook body is an **untrusted claim, never a fact** **[O]** (INV-05): after signature verification, the canonical entity is **re-read by ID from its system-of-record** using TG's own credential; posted mutable fields are discarded, the payload text is only a trigger.
- Every field normalizes to **one schema-validated typed envelope** with per-field grammars (hostname/IP/rule/issue-id/enum) rejected on mismatch **[O]** (INV-04). The correlation key is `external_ref`, unique within the org's own trackers **[R]** (paradigm-rule 1).

### 2.2 Model boundary — control-plane → LLM and back `[F/O]`
The native Go ReAct/tool-calling agent loop calls LLM APIs through the bundled LiteLLM model-gateway (auto-fallback ladder, org budgets) **[R]**. Crossing *into* the model, user-supplied content enters **only as delimited data inside structured message/tool-result blocks** — there is no "sanitize the prompt" step on the trust path **[O]** (INV-08). Crossing *back out*, model output is the deepest-distrust surface: **the model is a suggestion engine, never an authority.**

- **No LLM-produced token ever becomes control flow, a command string, or a query fragment** — model output enters the control-plane strictly as typed, validated, delimited data through a pluggable `LLMProvider` interface returning validated Go structs **[F/O]** (S8-1, INV-08, H-05, M-13).
- A proposed action is extracted **exactly once, by a single canonical parser, into a typed `Proposal`** — the only representation any later stage may consume. The model emits a **JSON-schema-constrained tool-call**, not markdown-with-a-sentinel; there is **exactly one proposal/approval grammar shared by parser and gate**, and unparseable/non-manifest-expressible output is rejected, never routed through a looser fallback **[O]** (INV-06). This is where the predecessor's "[POLL]-vs-looser-fallback" bypass class dies (§4).
- Marker parsing is deterministic; the `[AUTO-RESOLVE]` / `[POLL]` marker is **parsed, never trusted as authority** **[F]** (foundation, Phase 5) — reconciled into the typed proposal by construction **[O]** (INV-06).

### 2.3 Actuation boundary — control-plane → estate `[F/O/R]`
Every side effect on the org's estate (SSH/kubectl/API/MCP module calls) crosses here through **typed, individually-permissioned, capability-scoped adapters**. **The claim is a proposed command; the fact is an approved, action-bound, gate-cleared execution.**

- **The control-plane never spawns a shell and never string-interpolates a command** **[O]** (INV-02): actuation is `Exec(ctx, argv []string, stdin []byte)` — fixed argv arrays or a validated JSON envelope over stdin to a fixed script — with pinned SSH host keys; `sh -c` and `StrictHostKeyChecking=no` are **not expressible**. Temporal **workflows hold deterministic decision logic only and cannot touch the OS**; every effect is an activity against a capability-scoped adapter.
- Adapters are unexported behind a **single `Execute(ctx, ActionManifest)` chokepoint reachable only through the Go interceptor chain** (admission → territory/egress/policy check → execute → post-tool audit); the gate is **wired by construction** and a startup self-test **fails boot** if it is not — a "dark control" that silently observes is impossible **[O]** (INV-21). A control that cannot execute fails **loud and safe** (refuses the grant), never via a swallowed exception.
- The mechanical **never-auto floor** (mkfs / dropdb / zpool-zfs destroy / tofu destroy / kubectl delete-drain / credential-revoke / config-overwrite / reboot-halt / P0-reboot / jailbreak) is enforced at the actuation adapter **and** the classifier — defense in depth — and no policy, flag, or config lifts it **[F/R]** (foundation risk floor; paradigm-rule 8). Unknown/unrecognized mutation is **never "safe" by omission** and clamps to never-auto **[F/O]** (INV-09).
- Each mutating command is captured to the `execution_log` with pre/post state and its exact `rollback_command` — reversibility captured as data, bound to the scoped credential that ran it **[F/R]** (foundation; per-agent least privilege per paradigm-rule 1).

### 2.4 Identity & authority boundary — user/agent → granted role/capability `[R]`
Net-new in TG and structurally load-bearing. **The claim is "I am authorized to do this"; the fact is an RBAC-checked user/role and a capability-scoped credential.**

- Authority for any mutating action or approval is **checked against the authenticated user/role**, never inferred from a request field **[R]** (paradigm-rule 1). Tables are org-global; there is no `tenant_id` and no cross-org row-level-security isolation — this is one organization, and the boundary being enforced is *privilege within it*, not *isolation between orgs*.
- **Least-privilege identity replaces the single shared SSH key** **[R]** (Tensions-resolved): the predecessor's single shared SSH identity (with permission-skipping) is replaced by **per-source HMAC secrets and per-agent scoped credentials/mTLS**, so a captured or over-reaching credential is confined to its granted capability; credential-revoke-as-kill is the API/RBAC control from day one **[R/O]** (INV-13).
- Humans are **roles, not a person** **[R]** (paradigm-rule 2): approval/veto authority is checked against the acting user/role via an approver graph (RBAC + on-call rotation/escalation + quorum + fallback), never a global authorized-sender list.
- Autonomy controls are **API/RBAC/config-driven feature-flags, audited on change — never host-local sentinel files** **[R]** (paradigm-rule 4). The ships-dark + observe-before-live principle is kept; the mechanism moves onto the org policy store and the console.
- **Site is a label, not a boundary** **[R]**: a host's `site`/`estate` field filters and routes; every operator sees the whole estate subject to RBAC, never a hard isolation wall.
- Each session is an isolated Temporal execution keyed `tg/{session_id}`; cancel = `TerminateWorkflow(id)` — **no process-wide "current" pointer, shared lock, or `pkill`** **[O/R]** (INV-12).

---

## 3. Threat classes (15)

The fifteen classes below are inherited directly from the audit's threat model **[O]** (SOURCE-overlay `threat_model`). Each names the threat, the vector observed against the predecessor, and TG's control-by-construction with the governing invariant(s). They are grouped by the boundary they cross.

### Ingress boundary

**T-01 — Unauthenticated actor triggers any receiver, planner, or privileged control op**
- *Vector:* Reaching a receiver directly — the predecessor exported ~25 webhook paths as `auth=none`, and replay / chaos / heal / session-control were ordinary webhooks; the only barrier was an unowned network perimeter.
- *TG control [O]:* **INV-01** — mandatory non-bypassable auth middleware (mTLS / HMAC + nonce) before any handler; an `auth=none` route fails to register at boot; control-tier ops on a separate elevated listener; chaos/replay are internal Temporal signals, not HTTP. *(C-01, H-01, H-09, P0-1, P0-2)*

**T-02 — Forged payload trusted as fact drives privileged action**
- *Vector:* A webhook body accepted after only syntax / bot-name checks — no signature, no canonical re-fetch; an unauthenticated `action=register` poisons dedup/correlation state.
- *TG control [O]:* **INV-05** — signature-verify then **re-fetch the canonical entity by ID** with TG's own credential before any dispatch; **INV-01** auth on all receivers; correlation writes only from authenticated internal callers via typed RPC with ownership checks. Temporal idempotency keys dedupe replays. *(C-03, P1-1, M-08)*

**T-03 — Privileged session hijack via resume with attacker prompt**
- *Vector:* An unauthenticated resume endpoint re-ran a prior session with an attacker-supplied prompt and permission-skipping; leaked session IDs (via logs/chat/history) were the only barrier.
- *TG control [O/R]:* **INV-01 + INV-07** — **there is no resume primitive** (the predecessor's `claude -r` subprocess mechanism is dropped entirely **[R]**). Re-engagement mints a **new Temporal workflow that re-runs the full gate from zero**, seeded only by an immutable, read-only `ContextSnapshot`; mutating endpoints require a principal + single-use capability token; tool authorization lives in the adapter and no permission-skip flag exists. *(H-01, P0-2)*

**T-04 — Stored-XSS / HTML injection into the human channel**
- *Vector:* Security / DSM parsers concatenated external fields into the chat channel's formatted body with no local escaping, trusting downstream client sanitization.
- *TG control [O]:* **INV-04 + INV-19** — a **single templating layer HTML/markdown-escapes every interpolated field at the sink** (`html/template`) by construction; external fields are typed values, never raw HTML. Applies uniformly across every notifier reference adapter (Matrix/Slack/Teams/email/webhook) **[R]**. *(M-08)*

### Model boundary

**T-05 — Prediction-gate bypass via alternate proposal grammar**
- *Vector:* The crown-jewel bypass — the fail-closed gate keyed on `^\[POLL\]` while the poll-builder **also** accepted a looser "Which plan? - Plan X:" fallback that ran *after* the gate, so a proposal recognized by one grammar but not the other became a live approval poll **with no committed prediction**.
- *TG control [O]:* **INV-06** — one canonical parser, **one grammar shared by parser and gate by construction**; the model emits a typed tool-call; `BuildApprovalPoll` accepts only a `GatedProposal` — a type constructible **only** by the `PredictionGate` activity — making "poll without gate" **uncompilable**. *(H-02, P1-5)*

**T-06 — Fabricated evidence satisfies the auto-resolve guard**
- *Vector:* The evidence guard treated any triple-backtick line as proof, so an empty / fabricated / unrelated / stale code fence authorized high-confidence auto-resolution.
- *TG control [O]:* **INV-11** — an auto-resolve / high-confidence claim is admissible only if it cites **orchestrator-captured `ToolResult` IDs**; the verdict gate mechanically re-checks provenance, recency (freshness window), success, and target relevance; mutating actions run an independent post-condition activity. Evidence is a typed `Evidence{source, collected_at, target_ref, verification_status}` row referenced from the manifest — the agent narrating evidence is **not** evidence. *(M-13, P3-3)*

### Actuation boundary

**T-07 — OS command injection executes attacker shell before any model/gate**
- *Vector:* Untrusted body fields (pid, issueId, host, rule, summary/description) interpolated into SSH command strings / lock paths / triage args; `JSON.stringify` mistaken for shell escaping — executed **before any model or gate ran**.
- *TG control [O]:* **INV-02** — **no shell anywhere**; actuation is fixed argv arrays / validated stdin-JSON to fixed scripts; scalars parsed to typed Go values before use; Temporal workflows cannot touch the OS; a CI lint/grep gate bans `sh -c` and string-built commands. *(C-02, C-03, C-04, H-06, P0-3)*

**T-08 — SQL injection / silent state corruption**
- *Vector:* Parser fields concatenated into SQL strings; quote-doubling used as "escaping."
- *TG control [O]:* **INV-03** — exclusively parameterized `pgx` / `sqlc`-generated (compile-time-checked, always-bound) queries; a CI lint bans string-built SQL; shell scripts never touch the DB. *(C-03, C-04, P1-3)*

**T-09 — Approval-of-X replayed to execute-Y (unbound prediction/approval)**
- *Vector:* The prediction was committed against an early hypothetical plan; the live session then substituted a materially different final action; nothing hash-bound `{approval, executed commands, prediction}` — "a prediction exists" was checked, never "the prediction is *for the thing being executed*."
- *TG control [O]:* **INV-07** — a single canonical `action_id = SHA-256(canonicalJSON(Action))` is computed once and **threaded unchanged and re-asserted at every stage** (risk-classification → prediction commit → approval-poll options → execution authorization → PreToolUse enforcement → post-action verdict). The immutable content-hashed **ActionManifest** binds normalized target/op/params/band/plan-hash/prediction-hash/approval-choice/tool-calls/verification, sealed at creation and persisted append-only. The PreToolUse plan-adherence gate refuses any tool call not mapping to the approved manifest hash (constant-time compare); any mutation of the Action yields a **new id that invalidates prior authorization and re-enters the gate**. *(H-03, P1-4, S8-3, S8-preserve-meta)*

**T-10 — Inert guard (sanitize/authorize after the artifact is built)**
- *Vector:* The routed message was built *before* the sanitize/review step, and the original pre-sanitization string was what the command detector consumed — the filter was dead code with respect to the routed value.
- *TG control [O]:* **INV-04 + INV-08** — immutable pipeline stages each **RETURN the transformed event**; `RawEvent` is **unexported past the ingest package** (compiler-enforced), so no later stage can read the unsanitized body; the command is derived from a typed enum, not re-scanned free text. *(H-05)*

### Cross-boundary / identity & lifecycle

**T-11 — Cross-room approval misattribution and silent event loss**
- *Vector:* The bridge merged all rooms into one array, global-sorted, concatenated bodies, took sender/room from the last event, and advanced one shared cursor — a genuinely-late event in another room was silently dropped, or an approval was bound to the wrong session.
- *TG control [O]:* **INV-12** — per-`(source_id, room_id)` durable cursor, `UNIQUE(source_id, event_id)` idempotent insert (`ON CONFLICT DO NOTHING`), and an approval routed by `decision_id` (carrying `action_id` + `room_id`) via `SignalWithStart` to **exactly the owning workflow** whose `pending_decisions` row matches. No global cursor, no cross-source concatenation. *(H-04, P1-6)*

**T-12 — Cross-session interference / bystander kill**
- *Vector:* A global `is_current` cursor, a shared lockfile, and `pkill -f claude` let one room's cancel/cleanup hit unrelated concurrent sessions.
- *TG control [O/R]:* **INV-12** — each session is an isolated Temporal workflow keyed `tg/{session_id}`; cancel = `TerminateWorkflow(id)`; **NOT NULL FKs bind every row to its owning session**; there is no process-wide lock, no `is_current` column, no `pkill`. Session-per-workflow isolation makes bystander reach across sessions structurally impossible **[R]**. *(H-07, H-06)*

**T-13 — Dead decommissioned path re-invoked / real incident suppressed as maintenance**
- *Vector:* A "retired" subsystem's alternate modes + root SSH stayed executable; a stale reboot-suppression rule with no expiry still demoted genuine incidents; a legacy trigger had `active=null` with a privileged launch. A half-removed subsystem is a latent re-activation vulnerability.
- *TG control [O]:* **INV-17** — a capability exists **only if its adapter is compiled in and explicitly registered**; there is no runtime "mode" string, no host trust path for an unregistered backend; retiring a capability = **deleting its package**; a startup reconciler refuses to boot if live adapters/workflows don't match a signed manifest; CI grep + `find_dead_code` gates forbid retired identifiers. **INV-20** — every suppression rule is a **temporally-bounded, live-config-verified row** (`valid_from`/`valid_until`/`last_verified_at`) that **fails OPEN** when expired, unverified, or contradicted; suppression knowledge is never hardcoded into a prompt. This directly discharges the module-system guarantee that a disabled/unregistered module has **no execution path** **[R]** (ADR-0005). *(H-08, M-12, D9-dead-paths, H-11)*

**T-14 — Credential leak via exported artifact / backup**
- *Vector:* Secrets were embedded directly in exportable orchestration JSON, and unbounded execution history was persisted forever — an export/backup dump leaked long-lived credentials.
- *TG control [O/R]:* **INV-13** — no credential value appears in any versioned/exportable artifact; secrets are **references resolved at runtime from a secret store**; orchestration is **compiled Go, not exportable JSON**, so there is no blob to embed a secret into; per-adapter least-privilege identities; gitleaks CI. **INV-14** — redact-before-write, NOT NULL `expires_at`, automated audited purge. Retention is **org policy** over the purgeable operational body, while the tamper-evident audit spine is preserved by integrity-preserving archival, never deletion **[R]** (paradigm-rule 5). *(H-10, P0-4, M-06)*

**T-15 — Synthetic self-eval masquerades as production safety proof**
- *Vector:* The orchestration scorecard scored 1.0 over 10 synthetic incidents / 4 invariants (testing none of auth / injection / routing / binding) yet was promoted to a headline safety claim; generated artifacts carried null timestamps.
- *TG control [O]:* **INV-22** — the synthetic canary is **advisory-only, against an isolated ephemeral Postgres** (live-DB-leak counter must stay 0), and a low-weight Prometheus metric; **release is gated by adversarial boundary-coverage** (≥1 adversarial test per declared trust boundary) **plus production-like canaries**, and every generated artifact carries a non-null `generated_at` + source hash + coverage scope. Governed code cannot be excluded from the runnable suite. *(M-14, P3-5, M-02, D9-contract-authority)*

---

## 4. Classes that are structurally impossible in TG *by construction*

The audit's central demand is that the injection / bypass / drift class be made **uncompilable, not merely discouraged** **[O]** (audit verdict). The following are not "mitigated" risks with residual probability — in the TG build they have **no expressible code path**. Each rests on a single structural decision.

- **Shell / OS command injection** — *impossible via typed Go + argv.* There is no `sh -c`, no `fmt.Sprintf` into a command, no shell anywhere; actuation is `Exec(ctx, argv []string, stdin []byte)` with pinned host keys, and Temporal workflows cannot touch the OS. A CI grep gate rejects the syntax at build time. An attacker cannot inject a metacharacter into a string that never becomes shell syntax **[O]** (INV-02; C-02/C-03/C-04). *(actuation boundary)*

- **SQL injection / silent state corruption** — *impossible via bound parameters.* All persistence is `pgx` / `sqlc`-generated queries where runtime values are **always bound, never concatenated**; no manual quote-escaping helper exists and CI fails on string-built SQL. There is no code site where an untrusted value becomes SQL syntax **[O]** (INV-03; C-03/C-04). *(actuation / persistence boundary)*

- **Prediction-gate bypass via a second/looser grammar** — *impossible via one-grammar single-parser.* Exactly one grammar is shared by parser and gate; the model emits a JSON-schema-constrained tool-call; `BuildApprovalPoll` accepts only a `GatedProposal` constructible solely by the `PredictionGate` activity. "A poll without a committed prediction" **does not typecheck** **[O]** (INV-06; H-02). *(model boundary)*

- **Approval-of-X executed as Y (unbound authorization)** — *impossible via content-hashed action identity.* A single `action_id = SHA-256(canonicalJSON(Action))` is threaded and re-asserted at every stage; execution refuses any command whose `action_id` is not the approved one, and any change to the Action mints a new id that re-enters the gate. What the gate protects is **identity, not existence** **[O]** (INV-07; H-03). *(actuation boundary)*

- **Model output escalating to control flow / a command / a query fragment** — *impossible via typed-data-only ingress from the model.* No LLM-produced token ever becomes control flow, a command string, or a query fragment; model output enters only as typed, validated, delimited data, and all action authority is decided by typed policy **outside** the model. There is no "sanitize the prompt" step on the trust path because the prompt is never on it **[F/O]** (S8-1, INV-08, H-05). *(model boundary)*

- **An unauthenticated (or auth-forgotten) open endpoint** — *impossible via auth-as-construction.* Handlers are built by a factory that takes the authenticated principal as an argument; a route declaring no auth method **fails to register at boot**. Forgetting to configure auth produces a dead endpoint, not an open one **[O]** (INV-01; C-01). *(ingress boundary)*

- **A dormant retired capability silently re-invoked** — *impossible via compiled-in-only capability registry.* A capability exists only if its adapter package is compiled and registered; retiring = deleting the package; a boot reconciler refuses to start on manifest mismatch. There is no "mode string" that can select an unregistered backend, and the module system gives a disabled/unregistered module **no execution path** **[O/R]** (INV-17; ADR-0005; H-08/M-12). *(all boundaries)*

- **A dark / observe-only guard that was left unwired** — *impossible via wired-by-construction interception.* Adapters are unexported behind a single `Execute(ctx, ActionManifest)` chokepoint reachable only through the interceptor chain; a startup self-test fails boot if the gate is unwired; an activity failure propagates as a typed fail-closed error, never a swallowed exception **[O]** (INV-21; M-04). *(actuation boundary)*

- **Privilege escalation beyond a granted role or capability** — *structurally blocked by RBAC + per-agent identity.* Authority for any mutating action or approval resolves against the acting user/role; per-source and per-agent scoped credentials keep each adapter and agent to its granted capability, and credential-revoke instantly kills an agent's reach. A low-privilege user or a compromised agent is contained by the auth boundary and least-privilege credentials, not by application-layer care **[R/O]** (paradigm-rule 1; INV-12/INV-13). *(identity & authority boundary)*

> Note the pattern: every impossibility above is a **type-system, schema, or registration** guarantee — enforced by the Go compiler, `pgx`/`sqlc`, JSON-schema-constrained tool-calls, PostgreSQL constraints, or a CI/boot gate — **not** by a runtime check an author might forget to write. That is the difference between "discouraged" and "uncompilable," and it is the whole point of the overlay **[O]** (S8-preserve-meta, INV-22).

---

## 5. Residual risk and what this model does *not* cover

To stay honest (a build-culture value; see CONTRIBUTING) **[R]** (paradigm-rule 10), the following are explicitly *out of scope or residual* for this document:

- **A correct-but-wrong model proposal within an approved, reversible, well-predicted action class.** The safety core bounds *blast radius and reversibility*, not model correctness; mechanical post-execution verification (`match/partial/deviation`, deviation ⇒ never auto-resolve) catches surprise, but a plausible reversible mistake inside the AUTO band is contained, not prevented **[F]** (INV-10). See CONSTITUTION.md.
- **Compromise of TG's own host, secret store, or the Postgres instance** — an attacker with DB DDL or secret-store access is outside the ingress/model/actuation/identity boundary model; mitigations (DDL-less runtime role, per-adapter least privilege, secret references) raise the bar but this threat model assumes the substrate is trusted **[O]** (INV-16, INV-13).
- **Supply-chain compromise of a third-party loadable module.** Modules are signed and capability-scoped **[R]** (ADR-0005), and an unregistered module has no execution path (INV-17), but a *maliciously-signed-and-registered* module operating within its granted capabilities is a governance/trust question for the deploying organization, not a construction guarantee.
- **Denial of service / resource exhaustion** at the ingress or model-gateway layer — bounded by org budgets/quotas and Temporal task-queue fair-share **[R]** (paradigm-rule 6) but not a primary focus here.

Everything else — the fifteen classes of §3 and the by-construction impossibilities of §4 — is closed structurally, verified by the adversarial boundary-coverage gate (INV-22) that is itself the deployment authority **[O]**.
