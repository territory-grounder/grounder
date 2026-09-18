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
- Adapters are unexported behind a **single `Execute(ctx, ActionManifest)` chokepoint reachable only through the Go interceptor chain** (admission → territory → policy check → execute → post-tool audit); the gate is **wired by construction** and a startup self-test **fails boot** if it is not — a "dark control" that silently observes is impossible **[O]** (INV-21). A control that cannot execute fails **loud and safe** (refuses the grant), never via a swallowed exception.
  - *Corrected 2026-08-05 (TG-160).* This line previously read "admission → territory/**egress**/policy check". **There was no egress step in the interceptor chain, and there never had been** — `grep -rn -w -i egress --include=*.go .` returned zero over the whole tree while this sentence advertised the control. An advertised control that does not exist is worse than an absent one: it is budgeted for, relied on in review, and never built. The word is removed from the chain here because the chain is not where outbound is controlled; §5.3 states what actually constrains egress and what does not.
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
- **Compromise of TG's own host, secret store, or the Postgres instance** — an attacker with DB DDL or secret-store access is outside the ingress/model/actuation/identity boundary model; mitigations (DDL-less runtime role, per-adapter least privilege, secret references) raise the bar. This document previously ended that sentence with "this threat model assumes the substrate is trusted", which was retired on **2026-08-04 (TG-153)**: the sentence declared out of scope the single most likely intrusion path into an SRE platform. What remains residual is narrower and is stated as its own threat class in §5.1 below — **the compromise of a TG WORKER PROCESS is now in scope, with a bounded blast radius**; the compromise of the OpenBao substrate itself, or of the Postgres superuser, remains out of scope **[O]** (INV-16, INV-13).
- **Supply-chain compromise of a third-party loadable module.** Modules are signed and capability-scoped **[R]** (ADR-0005), and an unregistered module has no execution path (INV-17), but a *maliciously-signed-and-registered* module operating within its granted capabilities is a governance/trust question for the deploying organization, not a construction guarantee.
- **Denial of service / resource exhaustion** at the ingress or model-gateway layer — bounded by org budgets/quotas and Temporal task-queue fair-share **[R]** (paradigm-rule 6) but not a primary focus here.
- **TG itself used as the offensive instrument — the dual-use read lane.** Every class in §3 asks what an attacker can do *to* TG. None asked what an attacker can do *with* TG, and the read/recon lane is the surface where that question has teeth: it fails OPEN by law, and until **2026-08-04 (TG-165)** nothing anywhere bounded its VOLUME. It is now a named class with a stated bound in §5.2 below; what remains residual there — a multi-worker deployment's per-process windows, and the fact that the bound is on volume rather than on intent — is stated in that section **[O]** (INV-08, INV-09).
- **Where the bytes GO — outbound as a covert channel.** §5.2 bounds how much TG reads; nothing bounded where what it read was then SENT. Until **2026-08-05 (TG-160)** there was no egress control on any channel: no network segmentation, no NetworkPolicy, no allowlist and no destination metering anywhere in the system. It is now a named class in §5.3 below, with a network-layer control that blocks and a process-layer meter that observes; what remains residual there — chiefly that the model-provider hop is bounded only coarsely, and that the meter is enforcement-OFF by default — is stated in that section **[O]** (INV-16).

Everything else — the fifteen classes of §3 and the by-construction impossibilities of §4 — is closed structurally, verified by the adversarial boundary-coverage gate (INV-22) that is itself the deployment authority **[O]**.

---

## 5.1 Substrate / worker compromise — an in-scope threat class with a bounded blast radius `[O]`

*Added 2026-08-04 (TG-153). Until this date §5 declared the substrate trusted, which put the most probable intrusion into an SRE platform outside the model by fiat. It is now a named class with a stated bound.*

**The threat.** An attacker achieves code execution inside a TG worker process. The realistic route is not a memory-corruption exploit: TG *reads attacker-authored text by design* — alert bodies, device syslog, the stdout of commands run on estate hosts, ticket comments — and feeds it to an LLM in a loop with tools. The July-2026 HuggingFace intrusion walked exactly this chain: untrusted data reached a processing worker, and from that foothold the actor harvested every credential the worker could reach.

**Why "the model can't emit control flow" is not an answer here.** §2.2 and §4 are about what a *model output* can become. This class is about what an attacker who is already *inside the process* can reach. Such an attacker does not call the guarded function; they read the key out of memory, or they simply ask OpenBao for it with the identity the process already holds. Moving a secret from a file into a vault does not change that: **a process that may fetch a credential holds that credential**, and the extra hop costs one API call. Every credential improvement before TG-153 moved *where the secret lives*; none moved *which process may fetch it*.

**The control: two planes, two processes, two identities.** Enforcement is at the substrate first and in TG second:

| | **Triage plane** (`TG_CREDENTIAL_PLANE=triage`) | **Actuation plane** (`=actuation`) |
|---|---|---|
| Runs | the LLM agent over untrusted content; alert ingest; investigation reads | the estate-mutating activity, and nothing else |
| Temporal queue polled | `tg.runner` only | `tg.actuate` only |
| OpenBao AppRole / policy | `tg-triage` / `tg-triage-ro` — reads `secret/data/tg/*`, **DENIED** `tg/actuator` + `tg/proxmox` | `tg-actuate` / `tg-actuate-ro` — reads **only** `tg/actuator` + `tg/proxmox` |
| Holds | estate read tokens, host-diagnostics / syslog read keys | the actuation SSH identity, the proxmox and AWX write tokens |
| Cannot hold | any actuation reference — the process **refuses to boot** if handed one | any read-triage reference — same refusal, mirrored |

Verified live 2026-08-04 against the running OpenBao: the triage token receives **403** on `tg/actuator` and **200** on the host-diagnostics path; the actuate token receives **200** on `tg/actuator` and **403** on host diagnostics. The refusal is the substrate's, not TG's own code's — which is what makes it survive a compromise of TG's code.

**What each plane's compromise yields — stated as a bound, not as "nothing":**

- **A compromised TRIAGE worker yields**: everything that worker could already read — estate inventory, device syslog, the output of the fixed read-only diagnostic commands on allowlisted hosts, the incident corpus, and the read-scoped substrate token. It yields **no** estate-mutating credential: OpenBao refuses `tg/actuator` and `tg/proxmox` to that identity, no SSH mutate key exists in the process (the reference is never read, so it is never resolved and never in memory), and the process does not poll `tg.actuate`, so an actuation task cannot be delivered to it. To mutate the estate from there an attacker must additionally compromise OpenBao, the actuation worker, or an estate host directly.
- **A compromised ACTUATION worker yields**: the SSH mutate identity and the proxmox/AWX write tokens — i.e. the ability to run the allowlisted operations on the allowlisted targets. It does **not** yield the triage plane's host read keys, and there is no path by which attacker-authored text reaches it: it registers no agent toolset, no ingest poller and no host-log reader. Its mutations still traverse the full mechanical chain — the mode chokepoint (fail-closed to Shadow), the unit/container allowlist, the host-match gate, the ledger and the breaker — so a popped actuation worker inherits an *authorized* capability, not an unlimited one.
- **`TG_CREDENTIAL_PLANE=both`** (the DEFAULT, and every deployment that has not opted in) yields **both of the above from one compromise**. That is the pre-TG-153 posture and it is retained deliberately: a security fix that broke every existing installation on upgrade would not be deployed, and an undeployed control protects nobody. The worker's boot log says plainly which posture is running, and under `both` it says that this is *not* a split.

**Honest residuals, because a bound that overstates itself is worse than none:**

1. The actuation worker still reads the estate **topology** (device inventory and the object graph) and the post-execution **alert list** (`{host, rule, site}` tuples) — the mechanical verifier and the host-match/blast-radius gates cannot refuse anything without them. These are structured inventory reads, not prose, and none of it enters a model prompt in that process; but the LibreNMS read token does live there. It is a smaller surface than the triage plane's, not an empty one.
2. On a split deployment the triage worker cannot derive TG's own SSH actor fingerprint (that derivation requires reading the actuation private key), so `journal`-domain self-attribution must be declared another way or TG's own heals read attributed-suspicious.
3. Both planes still share **one** ledger, and — until **2026-08-05 (TG-164)** — **one Postgres role**. That was the gap TG-153 left open, and it was measured on the live box the day after it shipped: `worker -> tg_runtime` and `worker-actuate -> tg_runtime`. The process split therefore bounded what a popped triage worker could *fetch* and nothing about what it could *write*: it could still author `action_verdict`, `action_execution`, `interceptor_gate_verdict` and `policy_decision` — the record of the actuation it could not perform — and this codebase reads those tables back (`PriorVerdictsActivity`, the graduation evidence, the console audit). TG-164 gives each plane its own role (`tg_triage` / `tg_actuate`), derived from `tg_runtime`'s privileges so a plane role can never exceed the un-split posture, and withholds each plane's off-plane writes; see §5.1a. **What is still shared and is not fixed by it**: the governance ledger (both planes append — the triage plane must record human votes, INV-12/19; its tamper-*evidence* is the hash chain plus the append-only revoke, not an exclusive writer) and the graduation ladder (`policy_graduation` / `graduation_credit` are written from `GraduationActivity`/`ReconcileActivity`, which run on the **triage** queue, so a compromised triage worker can still advance an op-class toward `auto`). Compromise of the Postgres superuser, of the OpenBao substrate itself, or of the box's root remains **out of scope** — this class bounds a compromised *TG process*, not a compromised *host*.
4. The split does not partition the model gateway budget or the audit ledger; it partitions credentials and untrusted-content reach.

#### 5.1a The database half of the plane split (TG-164, 2026-08-05)

*Added because §5.1's residual 3 named the gap and the gap was live: two processes, two AppRoles, one database identity.*

Each plane now authenticates to Postgres as its own role. Privileges are **derived from `tg_runtime`'s** on every grounder boot (`db.ApplyPlaneGrants` → `tg_apply_plane_grants`, migration 0059) rather than enumerated, so a plane role can never hold a privilege the un-split role lacks — which is what keeps the fourteen migrations of append-only `REVOKE`s intact instead of silently re-granting them inside a hardening change.

| | `tg_triage` | `tg_actuate` |
|---|---|---|
| May **not** write | what **records or authorises an actuation**: `action_verdict`, `action_execution`, `interceptor_gate_verdict`, `policy_decision`, `regime_resolution`, `regime_actuation`, `deferred_verdict`, `pending_verification` | the **untrusted-content corpus a mutation is grounded in**: `agent_step`, `agent_step_evidence`, `ingest_alert`, `session_triage` |
| May still read | all of the above (the triage workflow reads back the verdict the actuation plane wrote) | all of the above (the evidence gate binds cited tool-result ids against that corpus) |

The lists were derived by tracing the composition root, not by reading table names: `RegisterActuationActivities` registers exactly one activity (`ExecuteActivity`), so a table written only from `core/actuate.Interceptor`'s chain is actuation-only, a table written from any other activity is triage's, and a table written from a boot-path goroutine belongs to **both** — which is why `runtime_posture`, `estate_snapshot`, `governance_ledger`, `mutation_breaker_state`, `control_plane_config` and the projection tables are granted to both planes. A split that broke the system would not be a security improvement.

**Opt-in, exactly like the process split.** With no plane roles the derivation is a no-op and both workers connect as `tg_runtime` — the pre-TG-164 posture, unchanged on upgrade. Each worker's boot log states which role it authenticated as and whether that role can still write its off-plane tables, reading **LIVE EXPOSURE** until it cannot: the DSN is the one part of this design Postgres does not enforce, so it is the part that gets said out loud.

**What a compromised triage worker can still do**, stated as a bound: propose, seal a manifest, append to the ledger, and advance the graduation ladder. It cannot write the row that says an action executed, that a gate passed, or that policy authorised it. The manifest it can seal is content-addressed and re-asserted against the action id at execute time, and every gate re-runs in the actuation process — so a forged manifest buys a *refused* execution, not an execution.

**Where §5.1a is enforced.** Migration `0059_credential_plane_db_roles` (the derivation function), `core/db/plane_roles.go` (the two table lists and `ApplyPlaneGrants`), `cmd/grounder/main.go` (called after every migration pass), `cmd/worker/credential_plane.go` + `cmd/worker/main.go` (`planeDBDSN` and the boot self-check), `deploy/postgres-init/01-plane-roles.sh`. Oracles, all against a real Postgres: `TestTriageRoleCannotForgeTheRecordOfAnActuation`, its control `TestTriageRoleCanStillDoEverythingTriageNeeds`, the symmetric `TestActuationRoleCannotForgeTheEvidenceItActsOn`, the no-regression `TestUnsplitRuntimeRoleIsUnchanged`, and the executed mutation control `TestPlaneGrantsMutationControl` — which grants `tg_triage` the withheld write and asserts the security oracle goes red.

**Where it is enforced.** Substrate: the two OpenBao policies + AppRoles above, plus the two Postgres roles of §5.1a. Process: `core/credential/plane_split.go` (`PlaneSet.ValidateFor` — a triage process declaring any actuation reference fails the boot, and vice versa), `cmd/worker/credential_plane.go` (off-plane configuration is withheld at *acquisition*, so the credential is never fetched, never resolved and never resident), `temporal/runner/workflow.go` (the estate-mutating activity is scheduled onto `tg.actuate`), and `deploy/docker-compose.yml` (the opt-in `worker-actuate` service, `split-planes` profile). Oracles: `TestTriagePlaneCannotReachAnyActuationCredential`, `TestActuationPlaneCannotReachAnyUntrustedContentSource`, `TestActuationActivityIsScheduledOnTheActuationQueue`, and the no-regression pair `TestBothPlaneIsByteIdenticalToTheHistoricCheck` / `TestBothPlaneIsIdenticalToPreSplitBehaviour`.

**One deployment-level trap, recorded because it defeated this control once already.** A compose `.env` file is *interpolation-only* — it is read while compose parses the YAML and is **not** injected into containers. A variable reaches a service only if that service names it under `environment:`. The first cut of this work documented "set `TG_CREDENTIAL_PLANE=triage` in `.env`" while the `worker` service forwarded no such variable, so the declaration would have been silently discarded: the triage worker would have booted as `both`, kept every actuation credential its environment block already forwards, and polled `tg.actuate` alongside `worker-actuate` — the two racing for the same estate-mutating tasks. The boot log would have said `plane=both … This is not a split`, which is true, but the operator had already been told otherwise. **A split that is believed and not in effect is the worst of the three postures, because it is the one nobody re-checks.** `deploy/credential_plane_wiring_test.go` (`TestWorkerServiceCanReceiveTheCredentialPlane`) now fails if the variable is not plumbed, and asserts its default is `both`.

---

## 5.2 TG as an offensive agent — the dual-use read lane `[O]`

*Added 2026-08-04 (TG-165). Every other class in this document asks what an attacker can do TO TG. This one asks what an attacker can do WITH TG, and nobody owned it: the fifteen classes of §3 are inherited from an audit of a system being attacked, not of a system being used.*

**The threat.** TG is, by construction, a credentialed autonomous reader of the whole estate: it holds host-diagnostic keys, an estate/CMDB graph, device syslog access and a read connector per source, and it drives them in a loop on behalf of an untrusted model over attacker-authored text. An actor who obtains that loop — by prompt-injecting it through an alert body (§2.2), by compromising a triage worker (§5.1), or simply by holding a credential that can open incidents — gets a *governed, audited, permitted* reconnaissance platform. No mutation is required, and none of TG's mutation-side controls apply: the mode chokepoint, the never-auto floor, the territory gate, the prediction gate and the ledger are all downstream of a decision to CHANGE something. This is the pre-actuation half of the ATLAS chain (Discovery / Lateral movement, `AML.T0098` / `AML.T0025`) and OWASP Agentic **ASI10 (Rogue Agents)**; it is the half in which the July-2026 HuggingFace intrusion did most of its damage, long before anything was destroyed.

**Why the read lane's fail-open posture is the residual surface.** CONSTITUTION.md §3.3 requires the advisory/triage lane to **fail OPEN**, and core/territory rules a read-only action *"never gated"*. That is right and is NOT being retired: an investigation that errors must degrade to pre-feature behaviour, because under-triage — standing down on an incident TG could have diagnosed — is the worse failure, and a read gate that denies on error is a self-inflicted outage of triage. What was wrong is that a fail direction was doing a second job it was never designed for. **"Never gated" was silently also serving as "never bounded".** Measured 2026-08-04, before this section existed:

| What existed | What it bounded | What it did not bound |
|---|---|---|
| Per-invocation limits (`tail -n`, `grep -F -m`, byte caps, context deadlines) | the size of ONE answer | how many answers |
| The DoW / cost breaker (spec/013 REQ-1211) | dollars per UTC day | scope and rate — a thousand cheap probes cost less than one reasoning cycle |
| `agent.TrajectoryVeto` (loop/thrash) | *identical* `(tool, ArgsKey)` repeats | 10 DISTINCT probes per session, free |
| `DefaultSearchSessionCap` (TG-297) | one tool, per session | every other read tool; every other session |
| `POST /halt` | the MUTATION chokepoint (`ForceShadow`) | recon, which ran straight THROUGH a halt |
| — | — | anything cross-session at all: a Temporal retry mints a fresh session id, hence a fresh everything |

**The control: a volume meter with three bounds, one alarm, and a halt that reaches both lanes.** `core/safety/recon_budget.go` (`ReconGovernor`), consulted by the agent loop immediately before every estate read:

- **Per session** — 25 reads (`TG_RECON_PER_SESSION`). Deliberately above the loop's own 10-cycle ceiling: it is a backstop for read paths the cycle limit does not govern, not the operative bound on the ReAct loop.
- **Per hour, across ALL sessions** — 500 reads (`TG_RECON_PER_HOUR`). This is the bound that did not exist in any form. Sized against TG's own corpus (3241 sessions / 17759 recorded reads as of 2026-08-03 ≈ 5.5 reads per session), 500/hour is ~90 full-budget investigations an hour — an order of magnitude above anything this estate produces, and far below what enumeration needs.
- **Burst** — 150 reads in 5 minutes (`TG_RECON_BURST` / `TG_RECON_BURST_WINDOW`) is a first-class ANOMALY, not a busy hour: it forces the mode to Shadow through the same `ForceShadow` seam the deviation breaker and `POST /halt` use, logs the fan-out (distinct estate objects touched), and refuses further reads **until the window drains**. Self-draining on purpose: the operator's answer to a burst is often "investigate it", which needs reads.
- **`POST /halt` now stops the READ lane too.** `ReconGovernor.Halt` holds `ForceShadow`'s contract — safe, idempotent, never refused, never re-enabling — and the halt response reports `recon_halted` so an operator is never told "halted" about half a system.

**Two properties this control is held to, because they are what make it survivable:**

1. **A refusal SAYS SO.** Every refusal is a typed `*ReconRefusal` naming the bound, the count and the window, rendered to the model as `TOOL_REFUSED[<tool>]: … the investigation is INCOMPLETE, not empty: conclude from the evidence already gathered`, recorded on the run result and logged against the incident. A bound that quietly returned less would produce a confident stand-down over an investigation that never happened — worse than either a refused read or a slow one.
2. **It counts DISPATCHES, not successes.** A probe that finds nothing writes no `agent_step_evidence` row, and "returned nothing" is exactly what most enumeration looks like; an errored or empty probe is metered like any other.

**Honest residuals, because a bound that overstates itself is worse than none:**

1. **The windows are per PROCESS.** Two workers each meter their own hour, so a split-plane or scaled-out deployment's effective ceiling is `N × PerHour`. The boot log states the bounds; nothing yet states the sum. A shared (Postgres-backed) window is the obvious next step and is deliberately not in this change.
2. **The seed is a floor.** The rolling hour is re-loaded at boot from `agent_step_evidence` so a restart does not hand a fresh hour to whatever was mid-burst — but only *evidence-bearing* reads leave a row, so the seed under-reports. Under-reporting is the chosen direction: it can only ever admit reads a perfect meter would also have admitted.
3. **This bounds VOLUME, not INTENT.** A patient adversary reading 400 hosts an hour, every hour, is inside every bound here. What changes is that the volume is now metered, alertable (`tg_recon_reads_total`, `tg_recon_targets_hour`, `tg_recon_burst_total`), and attached to a kill path — not that it is prevented.
4. **Reads outside the agent loop are not metered.** Ingest pollers, topology/discovery readers and the console's own queries do not pass through the governor. They are not model-driven and not attacker-steerable in the same way, but they are estate reads and they are not on this meter.
5. **The bound is a rate limit, not an authorization check.** It cannot tell a legitimate wide investigation from a sweep; that is what the fan-out signal is *reported* for, and why no threshold is placed on it today.

**Where it is enforced.** `core/safety/recon_budget.go` (the meter, the bounds, the burst alarm and the read-lane halt), `agent/loop.go` (`Admit` before every dispatch, `Record` after it — a refused read never reaches the estate), `temporal/runner/activities.go` (`Deps.Recon`, one governor per process so a Temporal retry cannot reset it), `cmd/worker/main.go` (construction beside the chokepoint, the `agent_step_evidence` seed, the runner and admin wiring) and `cmd/worker/admin.go` (`POST /halt` stops recon; the counters reach `/metrics`). Oracles: `TestReconBudgetBoundsReadsACROSSSessions`, `TestReconBurstForcesTheModeToShadowAndFiresOncePerEpisode`, `TestHaltStopsTheReadLaneAndNeverReEnablesIt`, `TestHaltStopsTheReadLaneNotOnlyMutation`, `TestAReadRefusedByTheBudgetNeverReachesTheEstateAndIsToldToTheModel`, `TestAnErroringProbeIsStillMeteredAgainstTheReconBudget`, `TestShippedReconBudgetCannotTruncateARealInvestigation` and `TestReconGovernorIsWiredByTheCompositionRoot`.

---

## 5.3 Unconstrained outbound — the model-call covert channel `[O]`

*Added 2026-08-05 (TG-160). §5.2 bounds how much of the estate TG reads. This section is about where what it read then goes. Until this date the answer was "anywhere", on every channel, unmetered.*

**The measurement that opened the ticket.** All of this was true of the deployed system on 2026-08-04:

| Layer | State |
|---|---|
| Go | `grep -rn -w -i egress --include=*.go .` → **zero hits**, whole tree — while §2.3 of this document advertised an "egress" step in the interceptor chain |
| compose | **no `networks:` stanza at all** — all fourteen services on the default bridge with full outbound NAT |
| host | `iptables -S DOCKER-USER` → the stock `-A DOCKER-USER -j RETURN` |
| helm | **no NetworkPolicy** in the chart |
| model plane | all five model tiers egress off-host |

**The threat.** TG's model-call path carries unconstrained prompt/response content to a third-party gateway, and the worker legitimately beacons to a dozen further destinations — estate APIs, trackers, notifiers, observability exporters. That is exactly the cover the July-2026 HuggingFace intrusion used: a self-migrating C2 riding ordinary public-SaaS traffic, indistinguishable from normal behaviour because nothing was counting normal behaviour. SecretRef redaction keeps *credentials* out of payloads; it does nothing about estate recon or attacker-authored content leaving inside a prompt. MITRE ATLAS `AML.T0025` (exfiltration via inference API); NIST SP 800-207 policy-enforcement-point.

**The control is the NETWORK layer, and the ordering is deliberate.** A process that can execute code can bypass its own HTTP transport; it cannot bypass the bridge. So:

- **compose — three tiers** (`deploy/docker-compose.yml`). `tg-backplane` (`internal: true`) carries east-west traffic and has no route off the box. `tg-frontdoor` is a bridge with IP masquerade disabled: a published port still serves, and outbound has no return path — "reachable, cannot reach out". `tg-egress` is the only path off the box, and every service on it is enumerated with what it reaches. **Nine of fourteen services lost outbound entirely**, including Postgres, Grafana, the Temporal UI and the console (the only service published on `0.0.0.0`). The boundaries were measured on docker 20.10.24, not assumed — including the constraint that an internal-only container cannot serve a published port, which is why the middle tier exists at all.
- **helm — a default-deny egress NetworkPolicy** with a per-component allowlist (`deploy/helm/grounder/templates/networkpolicy.yaml`). Ships **disabled**: enabling default-deny on an existing release with empty allow lists severs the worker from the estate, so the operator declares destinations first. The deny and the allows share one enable gate, because allows without a deny are not a weaker control — Kubernetes unions policies, so they are *no* control.
- **process — a destination/volume meter** (`core/egress`), installed once over `http.DefaultTransport`, which is what every module in this tree that does not set its own Transport resolves to. It counts requests, bytes out and bytes in per allowlist rule, names each undeclared destination in the log on first sighting, and publishes the whole lane to `/metrics`. Its allowlist is derived from the deployment's **own** endpoint configuration, so a connector that is configured is a connector that is declared. This is defence in depth, not the control.

**Honest residuals, because a control that overstates itself is worse than none:**

1. **The meter ENFORCES on the two worker planes and METERS on the grounder** *(corrected 2026-08-07, TG-324; this line previously read "the meter is OBSERVE-ONLY by default … and it is off")*. Enforcement was earned rather than assumed: both workers ran metered until `tg_egress_offallowlist_requests_total` held flat at 0 against a **non-zero** `tg_egress_allowlist_rules` — the anti-vacuity pairing, because a flat zero against an empty allowlist proves nothing — and `deploy/docker-compose.yml` now commits `enforce` for both so a clean redeploy cannot drop the control back to counting. Measured 2026-08-07: `tg_egress_enforcing 1` / rules 33 / off-allowlist 0 (worker), and 1 / 10 / 0 (worker-actuate). **The grounder was carrying no meter at all until TG-324** — `grounder:8080/metrics` served zero `tg_egress_*` series while this section's own enforcement list named only `cmd/worker/*`, so the control was strongest on the least exposed process and absent on the only one attached to the published `tg-frontdoor`. It now installs the same meter, in **meter** mode, and will be flipped the way the workers were: by watching its own series, not by assertion. All three refuse to *enter* enforce against an empty allowlist, because that is a configuration fault rather than a policy.
2. **The litellm → model-provider hop is bounded only by the network tier.** TG's process talks to litellm; litellm talks to Anthropic/OpenAI/DeepSeek/Mistral/xAI. That second hop is inside a different container and the Go meter cannot see it. It is the hop that carries the prompt content, and it is constrained today only by "litellm is on `tg-egress` and eleven other services are not".
3. **Both restricted compose tiers still reach the Docker host itself.** Docker's isolation rules live in `FORWARD`; host-destined traffic goes through `INPUT`. Measured: a backplane-only container still opened `:22` on the bridge gateway and on the host's LAN address. Closing that needs a `DOCKER-USER` rule, which is host configuration and is not in this repository.
4. **Non-HTTP egress is not metered.** SSH actuation (`crypto/ssh`), the Temporal gRPC client, LDAP and DNS do not pass through the meter. They are covered by the network tier and by nothing else.
5. **A NetworkPolicy selects by CIDR, not by name.** A destination behind rotating public addresses — a SaaS notifier, a model provider — cannot be pinned narrowly at that layer. Where an estate has an egress proxy the right shape is to allow only the proxy; where it does not, a wide CIDR is still strictly better than the "anywhere" that shipped before, and the meter names the actual hosts.
6. **Under a CNI that does not implement NetworkPolicy, the helm objects apply cleanly and enforce nothing, silently.** Verify with a connectivity test, never with a successful `helm upgrade`.
7. **This meters and segments; it does not inspect.** There is no DLP and no content classification on the model-call payload. What changed is that an off-allowlist destination is now visible and countable, not that prompt content is examined.

**Where it is enforced.** `deploy/docker-compose.yml` (the three network tiers and the per-service attachment), `deploy/helm/grounder/templates/networkpolicy.yaml` + `values.yaml` (`networkPolicy.egress`, default-deny plus per-component allowlist), `core/egress/allowlist.go` (the declared-destination scan and the matcher), `core/egress/meter.go` (the `http.RoundTripper` meter, the bounded destination register, the enforce path), `core/egress/install.go` (the SHARED install: allowlist compilation, the empty-allowlist enforcement guard, and the `http.DefaultTransport` replacement — extracted by TG-324 so a second process could not receive an almost-right copy of the refusal), `cmd/worker/egress.go` + `cmd/worker/main.go` (installed before the first outbound call; the insecure estate poller routed through the same meter), `cmd/worker/admin.go` (the `tg_egress_*` series on `/metrics`), and — added by TG-324 — `cmd/grounder/egress.go` + `cmd/grounder/main.go` (the same install, positioned after the saved-settings load and BEFORE OpenBao credential delivery, so this process's very first outbound call is already counted). Oracles: `TestComposeEgressPostureMatchesTheDeclaredTable`, `TestEgressNetworkDefinitionsHaveTheMeasuredProperties`, `TestEveryEgressGrantCarriesAnEnumeratedReason`, `TestHelmChartShipsADefaultDenyEgressPolicyThatCannotRenderWithoutItsAllows`, `TestNetworkPolicyValuesContractIsSafeByDefaultAndDeclaresItsShape`, `TestInstallEgressMeterReplacesDefaultTransportAndCountsDefaultClientTraffic`, `TestWorkerMetricsCarryTheEgressLane`, `TestEgressEnforceIsRefusedAgainstAnEmptyAllowlist`, `TestEnforceModeRefusesOffAllowlistWithANamedError`, `TestMeterModeNeverBlocks`, `TestDeclaredDestinationsScanIsNotVacuous`, `TestAllowlistIsNotVacuous`, `TestEmptyAllowlistPermitsNothing` and `TestOffAllowlistDestinationCardinalityIsBounded`, and for the grounder plane `TestTheGrounderInstallReplacesDefaultTransport`, `TestNoMeterPublishesNoSeriesRatherThanZeros` (absent, never a fabricated row of zeros — otherwise "no meter" and "a quiet meter" read alike), `TestAnInstalledMeterPublishesTheWholeLane` and `TestTheGrounderShipsInMeterModeNotEnforce`.
