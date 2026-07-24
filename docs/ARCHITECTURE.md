# Territory Grounder — Architecture

> Provenance tags on every substantive claim: **[F]** foundation (the predecessor system's design TG inherits) · **[R]** product reframe (multi-user / de-solo) · **[O]** audit overlay (security + quality hardening), with source ids (INV-NN, spec/00x, paradigm-rule N).
>
> Companion documents: **DATA-MODEL.md** (the Postgres/pgvector schema and the append-only audit spine), **the-map-is-not-the-territory.md** (the manifesto), **CONTRIBUTING.md** (build-culture), and the decision records under **adr/** (notably ADR-0005, the out-of-process governed-module runtime). Terminology in this file — ActionManifest, the five execution classes, the three autonomy bands, the mechanical safety core, the module system, Temporal, the LiteLLM model-gateway, the RBAC roles / approver graph — is identical across all sibling docs by construction.

---

## 1. System overview

Territory Grounder (TG) is an open-source, self-hosted, **single-organization, multi-user** governed-autonomy SRE platform. Its single architectural thesis is inherited verbatim and made structural: **a deterministic orchestrator owns control flow and the effect channel; the probabilistic model is untrusted and only proposes and communicates** [F]. The model never holds its own effect channel — it is isomorphic to an L1→L2→SRE rotation where the language model does the reasoning of an on-call engineer but a deterministic control-plane is the only thing permitted to touch the estate [F].

The predecessor asserted this split but leaked it: untrusted strings were interpolated into shell and SQL, ingress was unauthenticated by default, the prediction gate could be walked past through a second proposal grammar, and the committed prediction was never bound to the action actually executed [O: C-01..C-04, H-02, H-03]. TG's mandate is to make that entire injection/bypass/drift class **structurally uncompilable, not merely discouraged by author discipline** [O: verdict]. Every good control — risk gate, prediction, approval, verification, ledger — is re-founded behind a typed, authenticated interface bound to **one immutable content-hashed action** [O: S8-preserve-meta]. A control is only as strong as its binding.

Two properties frame everything below:

- **The mechanical safety core is invariant and non-configurable** [R: paradigm-rule 8]. Regardless of any org policy: the deterministic orchestrator owns the effect channel; reversibility is the primary risk dial with a hard NEVER-auto floor that no configuration lifts; the two-lane fail model is never blurred (advisory fails OPEN, remediation/mutation fails CLOSED); predict-before-acting is fail-closed with a mechanical post-hoc verdict the acting agent can never write; and guardrails gate on **structure** (committed plan, territory, egress), never on enumerating bad command strings [F][R][O: INV-08, INV-09, INV-10].
- **Single-organization, multi-user by default** [R: paradigm-rule 1]. One deployment serves one organization with many operators, roles, and teams — there is no `tenant_id` and no cross-org row-level-security isolation. The correlation key is a bare `external_ref` (ids are unique within the org's own trackers). Authority is checked against the acting user/role, and least-privilege identity — per-source HMAC secrets, per-agent scoped credentials/mTLS, credential-revoke-as-kill — keeps every adapter and agent to its granted capability as defense-in-depth, not tenancy [R][O: INV-12, INV-13].

The original single-operator ~300-machine estate — with its one shared SSH identity, one absent human voting 0/824 approval polls, hand-maintained one-estate dependency graph, and n8n/Cronicle/OpenClaw/Matrix/YouTrack stack — is the **provenance and battle-test that proved the paradigm, never the target market** [R: corrected mission]. Every solo/single-estate/vendor assumption it carried is inverted here.

---

## 2. The operating loop (Phase 0→9)

Every alert, ticket transition, or chat command an ingest source, tracker, or operator sends flows through one estate-agnostic incident lifecycle. In TG this choreography is a **Temporal workflow** — a durable, replayable state machine whose deterministic decision logic lives in the workflow and whose every side effect is a capability-scoped **activity** [R: paradigm-rule 7; O: INV-02, INV-21]. Deterministic code always acts on an event before any model is spent [F].

| Phase | Name | What happens (product-scoped) | Provenance |
|---|---|---|---|
| **0** | Ingest & admission | A pluggable **ingest adapter** normalizes a heterogeneous signal to one schema-validated typed envelope (`CanonicalAlert`/`IncidentEnvelope`); each identifier field is validated against an explicit grammar and rejected on mismatch. Then in-code **dedup** (sha256 + line-count / 24h fingerprint), **flap** detection (2+ cycles), **burst/correlation** (3+ hosts), and a **cooldown** guard run before any model is spent. Dedup/cooldown state is keyed by the canonical event fingerprint. | [F] Phase 0; [O] INV-04, INV-05 |
| **1** | Tier-1 deterministic triage & suppression | Deterministic scripts + a fast model produce a diagnosis with a parseable **CONFIDENCE** scalar. The pre-model suppression chain — dedup → org-declared blast-radius fold → scheduled-reboot phase SR → known-transient pattern → active-memory — suppresses only **known-benign/child** alerts and **fails open**; critical or unknown always escalates to Tier-2 (confidence < 0.7 or critical). | [F] Phase 1; [O] INV-20 |
| **2** | Context assembly | Tiered retrieval (§8): 5-signal RRF RAG (incident_knowledge + keyword + wiki + verbatim transcripts + chaos baselines) fused with CMDB identity, the org's self-populating **infragraph** dependency/blast-radius context, and targeted knowledge paths. | [F] Phase 2; [R] rule 9 |
| **3** | Plan & risk band | Build Plan → **Risk Classifier** writes exactly one `session_risk_audit` row emitting `{risk_level, band, notify_required, signals, plan_hash}`, banding the session **AUTO / AUTO_NOTICE / POLL_PAUSE** on the four dials (reversibility, blast-radius, statefulness, prediction confidence). The classifier is a typed deterministic gate whose zero-value band is the most-restrictive (`POLL_PAUSE`), so any error, panic, or unmatched path fails closed. | [F] Phase 3; [R] rule 2; [O] INV-09, spec/001 |
| **4** | Commit prediction (fail-closed) | The orchestrator commits a `plan_hash`-keyed machine consequence prediction (computed by the infragraph, **outside** the model) to the prediction log **before any approval poll**. A proposal with no committed prediction is rewritten to a WITHHELD marker and demoted to analysis-only. The remediation lane fails **CLOSED**. | [F] Phase 4; [O] INV-10, spec/002 |
| **5** | Propose | The model ends its ReAct reasoning by emitting a **typed tool-call** carrying the proposal (not markdown with a sentinel), parsed exactly once by a single canonical `ParseProposal` into one typed `Proposal`; polling presents 2+ remediation approaches. The marker is parsed deterministically — never trusted as authority. | [F] Phase 5; [O] INV-06, INV-08 |
| **6** | Approve (humans as circuit-breaker) | The band decides. **AUTO** executes silently. **AUTO_NOTICE** executes and notifies the configured on-call group in parallel for out-of-band veto. **POLL_PAUSE** holds on durable pause/resume state for an async human reaction and pages the org's escalation policy — **never proceeding on timeout**. Approvers are an RBAC/on-call/quorum **role graph**, not a person. | [F] Phase 6; [R] rule 2; [O] INV-12 |
| **7** | Execute | Gated actuation through typed, individually-permissioned adapters (SSH/kubectl/MCP/API), each invoked as fixed **argv vectors** (never `sh -c`). Every mutating command is captured to the execution ledger with pre/post state and its exact `rollback_command`. Each call traverses the wired-by-construction pre/post interception gate. | [F] Phase 7; [O] INV-02, INV-21 |
| **8** | Verify (mechanical) | Independent code (never the acting model) diffs observed alerts against the committed prediction and writes a **match / partial / deviation** verdict. A deviation is *surprise* and can **never** auto-resolve. | [F] Phase 8; [O] INV-10 |
| **9** | Learn & close-out | Session End scores the trajectory, runs the 5-dimension LLM-judge, archives the transcript as embedded pgvector chunks, exports OTel traces, parses tool calls, populates incident_knowledge + lessons_learned, recompiles the wiki, and the **band-aware reconciler** transitions the ticket (AUTO/AUTO_NOTICE→Done, completed/unknown→To Verify, POLL→human). A session is done only when its knowledge is written back. | [F] Phase 9 |

Three nested feedback loops run over this spine: within-session ReAct, across-session memory, and across-month policy iteration [F].

---

## 3. The immutable content-hashed ActionManifest spine

The load-bearing lesson of the audit: the predecessor committed a prediction against a **pre-session hypothetical plan** the live session was then free to mutate, and its execution gate only checked that *a* prediction artifact existed — never that it was **for the action being executed** [O: H-03]. TG closes this by construction with the **ActionManifest**.

A single canonical `action_id = SHA-256(canonicalJSON(Action))` is computed **once** and threaded unchanged through every stage: risk-classification → prediction commit → approval-poll options → execution authorization → pre-tool enforcement → post-action verdict [O: INV-07]. Every stage re-derives and **asserts** that its input's `action_id` equals the id it was authorized for; a mismatch is a hard fail-closed abort. **Any change to the Action yields a new id that invalidates all prior approval and prediction and re-enters the gate** — identity, not existence, is what the gate protects [O: INV-07, S8-preserve-meta].

The ActionManifest is sealed at creation and persisted append-only. It binds:

- **normalized target** (host/service/resource, validated grammar)
- **op** (the operation class)
- **params** (typed, validated scalars — no free text)
- **band** (AUTO / AUTO_NOTICE / POLL_PAUSE)
- **plan_hash** (joins the risk-audit row)
- **prediction_hash** (hash-linked to the committed prediction record)
- **approval_choice** (the gate-constructed `GatedProposal` selection)
- **tool_calls** (the exact authorized argv/tool set)
- **verification** (the post-action verdict record reference)

Temporal carries `action_id` as workflow state; deterministic replay guarantees equality across activities. The pre-tool plan-adherence gate refuses any tool call not mapping to the approved manifest hash via constant-time compare; a substituted plan produces a new id and a child-workflow re-gate [O: INV-07]. `BuildApprovalPoll` accepts **only** a `GatedProposal` — a type constructible *only* by the PredictionGate activity — so "poll without a committed prediction" is uncompilable [O: INV-06].

### Prediction and verification are two distinct immutable artifacts [O: INV-10]

The Temporal workflow enforces the ordering `PredictActivity → ApprovalActivity → ExecuteActivity → VerifyActivity`. Both prediction and verification are append-only Postgres records, each hash-linked to the manifest and mechanically computed. A Go pure function `computeVerdict(pred, observed)` produces the verdict — the model never adjudicates its own outcome, and deviation forces never-auto-resolve.

### ExecutionRecord & Evidence fields (the quality-audit binding) [F][O: INV-11, INV-19]

Each mutating action writes an **ExecutionRecord** (reversibility captured as data, so auto-rollback is a lookup, not a re-derivation [F]):

- `action_id` (manifest binding)
- `device` / target_ref
- `command` (the fixed argv vector actually run)
- `pre_state`, `post_state`
- `exit_code`
- `rolled_back` (bool) + `rollback_command` (the exact captured undo)
- `collected_at`

Any AUTO or high-confidence claim is admissible **only** if it references at least one recent, successful, relevant **Evidence** row the orchestrator itself captured — never the agent's free-text or a bare code fence [O: INV-11]. An Evidence row is typed:

- `source` (the tool that produced it)
- `collected_at` (checked against a freshness window)
- `target_ref` (checked for target relevance)
- `verification_status`
- a bound `ToolResult` id (checked for provenance and success)

Finally, **every governance decision** is a *required output* of its decision function (a `Decision{decision, reason, action_id, withheld_flag}` struct the caller must persist — omitting a field is a type error) and is appended to the tamper-evident hash-chained governance ledger, so the full chain `event → classification → prediction → approval → execution → verification` is reconstructable from persistence alone [O: INV-19, S8-8]. See **DATA-MODEL.md** for the ledger DDL, the org-global chain design, and the archival/sealing retention policy.

---

## 4. Execution classes

Every admitted incident is routed to exactly one of **five execution classes**. The class is chosen deterministically at Phase 1–3 from alert category, confidence, risk band, and estate criticality — it declares *how much compute and autonomy the incident earns*, and is orthogonal to the autonomy band (which governs *who must approve*). The class maps the inherited three-tier decision model (T1 deterministic → T2 model → T3 human) and the "prefer the least-autonomous topology that works" principle onto a typed, per-incident routing decision [F].

| Execution class | What runs | Typical retrieval tier | Autonomy reachable |
|---|---|---|---|
| **DETERMINISTIC** | Code-only: normalize → dedup → flap → burst → suppression-chain → mechanical verdict. No model is spent. Known-benign suppression, mechanical auto-close of a recovered read-only alert. | CACHE_ONLY | AUTO (only within the never-auto floor) |
| **FAST_AGENT** | A single-agent, low-token ReAct pass with a fast model over a cheap context. High-confidence, low-blast-radius, reversible alerts. | CACHE_ONLY / FAST_RAG | AUTO / AUTO_NOTICE |
| **STANDARD_AGENT** | The full single-agent triage→context→propose loop with 5-signal retrieval and the prediction gate. The default for a genuine incident. | FAST_RAG | AUTO_NOTICE / POLL_PAUSE |
| **DEEP_INVESTIGATION** | **Deferred design — not shipped.** The manager pattern (a manager agent with read-only sub-agents-as-tools, Edit/Write structurally withheld; multi-hop KG traversal; synthesis; depth-1 delegation with cycle limits) is held behind the evidence gate noted below; today novel/cross-host incidents run the single ReAct loop and escalate to POLL_PAUSE. | DEEP_RAG | POLL_PAUSE (novelty forces it) |
| **HUMAN_LED** | Investigation continues but every mutating action is held for the org's approver graph; the model advises only. Irreversible/security-sensitive classes, deviations, and any never-auto-floor match land here regardless of confidence. | any | POLL_PAUSE only |

The classes are **least-autonomous-that-works** [F]: today TG ships a **single native Go ReAct loop** whose per-agent turn budget forces POLL_PAUSE at cycle 5 and hard-halts at 10 [F] (`agent/loop.go`). The manager-with-read-only-sub-agents split (sub-agents are tools the manager owns, not peers it hands off to; write tools structurally excluded from read-only sub-agents) is a **deferred design on an evidence-gated HOLD**, not a shipped capability — `delegation_precision`/`parallel_speedup`/`multi_agent_quality_lift` must show net value before it is built ([EXTERNAL-AUDIT-LESSONS.md](EXTERNAL-AUDIT-LESSONS.md) lesson 9). Class selection is itself a graded, fail-closed gate — an unrecognized incident class defaults to HUMAN_LED [O: INV-09].

---

## 5. Component topology

TG is a small set of Go services plus its durable substrate, packaged as one guided `docker-compose` single-node profile (honestly a multi-service stack, not one image) [R: stack]. Language is **Go** for the control-plane, core, agent, and adapters [R: stack]; the framework choice for `frontend/` (React/Svelte) is deferred [R: stack].

```
                          ┌───────────────────────────────────────────────┐
   Operators / on-call ──► │  frontend/ (TypeScript SPA)                    │
   approvers / admins     │  approval console · ActionManifest timeline    │
                          │  /replay · tamper-evident ledger view ·        │
                          │  explainability · autonomy-band + kill-switch  │
                          │  controls · org admin                          │
                          └───────────────────┬───────────────────────────┘
                                              │  generated OpenAPI (INV-15)
                                              ▼
   ┌──────────────────────────────────────────────────────────────────────┐
   │  GO CONTROL-PLANE  (the only authority over the effect channel)       │
   │  ┌────────────┐  auth middleware: mTLS / per-source HMAC + nonce      │
   │  │ chi/grpc   │  (non-bypassable; auth=none route fails to register)  │
   │  │  router    │  ── separate elevated admin listener for control ops  │
   │  └─────┬──────┘                                                       │
   │        ▼                                                              │
   │  ingest adapters → RiskClassifier → PredictionGate → PolicyEngine     │
   │  → ParseProposal → interception chain (territory/egress/plan) → verdict│
   │  → band-aware reconciler → governance ledger (SHA-256 hash-chain)     │
   └───┬───────────────┬───────────────┬───────────────┬──────────────────┘
       │               │               │               │
       ▼               ▼               ▼               ▼
 ┌───────────┐   ┌───────────┐   ┌─────────────┐  ┌────────────────────────┐
 │ TEMPORAL  │   │ POSTGRES  │   │  LiteLLM    │  │  MODULE / PLUGIN        │
 │ workflows │   │ + pgvector│   │ model-      │  │  RUNTIME (out-of-proc)  │
 │ activities│   │ one DSN   │   │ gateway     │  │  gRPC / go-plugin · MCP │
 │ schedules │   │ one       │   │ N providers │  │  ┌──────────────────┐   │
 │ signals   │   │ migrated  │   │ auto-fallbk │  │  │ ingest · tracker │   │
 │ workers/  │   │ schema    │   │ org-wide    │  │  │ notifier+approval│   │
 │ queues    │   │ DML-only  │   │ budgets/    │  │  │ CMDB · actuation │   │
 │           │   │           │   │ quotas      │  │  │ model-provider   │   │
 │           │   │           │   │             │  │  │ observability    │   │
 └───────────┘   └───────────┘   └─────────────┘  │  └──────────────────┘   │
                                        ▲          │  signed · capability-   │
                                        └──────────┤  scoped · RBAC-gated    │
                                    agent/ (native │  (INV-17)               │
                                    Go ReAct loop  └────────────────────────┘
                                    calls LLM APIs
                                    via the gateway)
```

- **Go core / control-plane** — the deterministic orchestrator. It owns persistence, the auth middleware, the risk/prediction/policy/verdict gates, and the governance ledger. Temporal workflows contain deterministic decision logic only and **cannot touch the OS**; every side effect is an activity against a capability-scoped adapter [O: INV-02].
- **`agent/`** — a **native Go ReAct / tool-calling loop** that calls LLM APIs directly through the model-gateway. There is **no** external agent-CLI subprocess to launch or resume; the predecessor's `claude -p` / `claude -r` subprocess mechanism is dropped entirely [R: no-Claude-Code]. Model output enters the control-plane only as typed, validated, delimited data [O: INV-08].
- **Temporal** — durable workflows, activities, Schedules, signals, and continue-as-new. Deliberately load-bearing (§6).
- **Postgres + pgvector** — one database, one DSN, deploy-time ordered migrations; the runtime role has DML only and no DDL [R: stack; O: INV-16]. See **DATA-MODEL.md**.
- **LiteLLM model-gateway** — one OpenAI-compatible endpoint fronting N providers with the auto-fallback ladder as config, retries, rate-limit handling, and **org budgets/quotas** [R: no-Claude-Code]. Default ladder (user-configurable): `z.ai` → `DeepSeek` → `Mistral` → then `Anthropic` / `OpenAI` / `Grok` / etc. The predecessor's "local-first $0 / subscription" default is retired to one selectable org cost/locality mode [R: rule 6].
- **`frontend/`** — the TypeScript console where the differentiator becomes usable: the **approval console** (the human circuit-breaker), the **ActionManifest timeline/replay** (predicted→approved→executed→verified as one visual chain), the **tamper-evident ledger** view, **explainability** ("why did the agent do this"), **autonomy-band + kill-switch controls**, and **org admin** [R: UX pillar]. It is a net-new product pillar the predecessor had none of, and it is API-first (§9).
- **Module / plugin runtime** — out-of-process governed adapters (§7).

---

## 6. Runtime-substrate migration (Temporal is deliberately load-bearing)

TG is greenfield: there is **no legacy to revert to**, and the off-state of any autonomy layer is simply the non-autonomous baseline [R: rule 7]. The predecessor's runtime is retired and its responsibilities are absorbed by Temporal and a lean residual controller.

| Predecessor runtime | TG replacement | Provenance |
|---|---|---|
| n8n (orchestration engine: Runner / Bridge / Poller / ~14 receivers) | **DROP** → Temporal workflows/activities + the Go control-plane | [F][R] rule 7 |
| Cronicle (scheduler) | **DROP** → **Temporal Schedules** (per-job run-history, retries, dead-man native) | [F: drop] |
| gateway-watchdog dead-man (trap-EXIT heartbeat script) | **subsumed** by Temporal worker/workflow health; the `absent()` "no-data ≠ no-alert" principle is kept | [F][R] rule 4; [O] INV-01 |
| platform-controller Plane-A self-healing (host restarts / pkill / job re-runs) | **lean residual** — Temporal handles retries/reconciliation; the controller heals only non-Temporal things (the LiteLLM gateway, a dead module process, a stuck PG pool) via API, never host `pkill`; the "heal-the-platform-never-the-mission" boundary is intact and verified by inspection | [F][R] rule 11 |
| OpenClaw Tier-1 + the operating-mode abstraction (cc-cc/oc-cc/…) | **DROP** entirely; centralized model routing supersedes it | [F: drop]; [O] INV-17 |
| Host-local sentinel-file kill-switches (`touch`=on / `rm`=off) | **DROP the mechanism** → org, RBAC-gated **feature-flag / policy store**, audited on change; the "ships-dark + observe-before-live + instantly disableable" *principle* is kept verbatim | [R] rule 4 |

**Why Temporal is deliberately load-bearing:** consolidating orchestration + scheduling + durable pause/resume + retries + dead-man liveness into one substrate is the single biggest simplification over the predecessor's four-system sprawl. The sequential gated choreography becomes gated activities; the fixed 5 named concurrency slots become Temporal workers/task-queues with **org-configurable concurrency and fair-share queueing**; durable pause/resume and per-turn immutable snapshots are native; and the predecessor's OPEN auto-resume recovery loop is closed as `continue-as-new` [R: rule 7, tension "5 fixed slots"]. Because the substrate is load-bearing, the overlay extends the component-registry liveness-contract model to **every** Temporal workflow and activity, so the port itself introduces no dark component [O: overlay attach — dark-component].

---

## 7. The module system

Every integration surface — **ingest / tracker / notifier+approval / CMDB / actuation / model-provider / observability** — is a **loadable/unloadable module**, never a hardcoded stack [R: modules]. This is how TG becomes estate-agnostic and vendor-agnostic: every named vendor (YouTrack/Jira/GitHub Issues/ServiceNow; Matrix/Slack/Teams/email/webhook; LibreNMS/Prometheus/CrowdSec; NetBox; Twilio; z.ai/DeepSeek/Mistral/Anthropic/OpenAI/Ollama) becomes **one selectable reference backend behind a stable interface**, resolved by org config [R: rule 3].

- **Repo layout:** `adapters/` holds the module **interfaces**; `modules/` holds the loadable **implementations** plus a small reference-adapter set and an SDK [R: modules].
- **Default mechanism (ADR-0005, proposed):** **out-of-process governed plugins** — each module is a separate process/container over a stable protocol (gRPC / HashiCorp go-plugin; **MCP** for tool/actuation modules). This gives runtime load/unload, third-party modules, process isolation, and per-module capability scoping. Alternatives considered — B: a compile-time registry; C: WASM/Extism — are recorded in **adr/**.
- **Governed by construction** [R: modules; O: INV-17]: modules are **signed, capability-scoped, and RBAC-enabled**. A capability exists **only if** its adapter is registered; there is no runtime "mode" string selecting an alternate backend, no host trust path for an unregistered backend, and no null/ambiguous activation state. A disabled or unregistered module has **no execution path** — which kills the predecessor's "dead OpenClaw path still executable" class. A startup reconciler compares the live registered adapters/workflows against a signed declared manifest and **refuses to start on mismatch**; retiring a capability means deleting its adapter package, not leaving it dormant.

All actuation reaches the estate through a single `Execute(ctx, ActionManifest)` chokepoint that is reachable **only** through the Go interceptor chain (admission → territory/egress/policy gate → execute → post-audit); adapters are unexported behind it, so a dark control is impossible and a control that cannot execute fails LOUD and SAFE rather than observe-only [O: INV-21]. Each actuation identity is per-source + per-agent scoped (mTLS / scoped service accounts), with credential-revoke-as-kill — never one shared key [R: rule 3; O: INV-13].

---

## 8. Tiered retrieval

Context assembly (Phase 2) is served by three latency-bounded retrieval tiers, so cheap incidents pay cheap retrieval and only deep investigations pay for the full pipeline. Env-tuned weights become org retrieval-policy rows [R: rule 1, RRF reframe].

| Tier | Budget | What runs |
|---|---|---|
| **CACHE_ONLY** | **< 500 ms** | Exact/keyword and cached-embedding lookups against incident_knowledge and the compiled wiki; no model call. Serves DETERMINISTIC and most FAST_AGENT incidents. |
| **FAST_RAG** | **< 5–8 s** | The 5-signal RRF fusion (semantic / keyword / wiki / verbatim transcript / chaos baselines) with Reciprocal Rank Fusion and path-based rank surgery; asymmetric query/document embeddings via the model-gateway. Serves STANDARD_AGENT. |
| **DEEP_RAG** | **< 20 s** | FAST_RAG plus multi-perspective query rewrite, cross-encoder rerank (independent-healthcheck service with a deterministic multi-tier fallback ladder), confidence-triggered multi-chunk synthesis (only when rerank max < 0.4) + LongContextReorder, and LLM-planned multi-hop KG traversal (recursive CTE) with two deterministic fallbacks. Serves DEEP_INVESTIGATION. |

Trust is graded and encoded as weight/rank: wiki < semantic/keyword, transcript and chaos discounted, and incident-mined graph edges capped at 0.75 — deliberately below the 0.8 suppression-eligibility cutoff [F]. Every external RAG call is wrapped by a named, observable **circuit breaker** with half-open recovery; an outage lowers quality but stays deterministic and is not a critical outage [F]. Embeddings live in **pgvector**, replacing the predecessor's inline-TEXT + FAISS approach [R: rule 1; F: data]. The rerank/synthesis models resolve through the model-agnostic gateway; local GPU, a hosted rerank API, or no-rerank are org-selectable modes [R: RRF reframe].

---

## 9. API-first

TG is API-first: the `frontend/` console consumes the **generated OpenAPI** — there is no second, hand-maintained contract [R: UX pillar]. Each logical entity has exactly **one authoritative definition** (a Go struct with tags); every wire contract (OpenAPI / AsyncAPI / JSON Schema), storage DDL, in-code validator, human-facing count, and diagram is **generated** from it, never hand-maintained in parallel [O: INV-15].

Consequences enforced in CI:

- Contracts round-trip losslessly from the Postgres model and cover **100% of routed endpoints** with declared auth/error/idempotency schemas.
- Every generated artifact embeds a **non-null `generated_at` + source hash + coverage scope**; CI fails on any hand-written number, uncovered path, drift, or missing provenance.
- A testcontainers round-trip test writes a real row via the production path and validates it against the generated contract [O: INV-15, M-01].

This is the anti-drift discipline that closes the predecessor's class of published schemas requiring fields real rows never had, diverging README/registry/diagram counts, and an OpenAPI that listed privileged paths with no auth scheme [O: INV-15]. The autonomy-band and kill-switch controls that the predecessor kept in host-local files move **onto** this API surface, RBAC-gated and audited on change [R: rule 4, UX pillar].

---

## 10. Multi-user, single-org

TG serves one organization with many users. Roles and least-privilege identity are construction properties, not feature flags [R: rule 1]. There is one deployment, one estate, and no cross-org isolation to carry — a future SaaS/managed-service offering with isolated orgs on one install would be a separate explicit ADR (see [adr/0010](adr/0010-single-org-multi-user-not-multi-tenant.md)).

- **One org, no `tenant_id`.** State, memory, audit, eval, cost, graph, and session tables are org-global; sessions are Temporal workflows keyed `tg/{session_id}`, with NOT NULL FKs on `session_id` [R: rule 1; O: INV-12, INV-16].
- **Correlation key is `external_ref`** — a bare ticket id from the org's own tracker, unique within the org [R: rule 1].
- **Humans are roles, not a person.** The circuit-breaker is an approver graph: RBAC roles + on-call rotation/escalation + quorum + fallback approver. AUTO_NOTICE/POLL_PAUSE route to the configured on-call group; veto/approval authority is checked against the acting user/role [R: rule 2].
- **Least-privilege identity is defense-in-depth.** Per-source HMAC secrets and per-agent scoped credentials/mTLS with credential-revoke-as-kill keep each adapter and agent to its granted capability; this is layered defense, not a tenancy boundary [R: rule 3; O: INV-13].
- **Site is a label, not a boundary.** A host's `site`/`estate` field filters and routes; it is never a security or isolation wall — every operator sees the whole estate subject to RBAC.
- **Estate knowledge is self-populating.** The infragraph causal graph, blast-radius edges, criticality (P0) tiers, component/liveness registry, and tool inventory are all **discovered** from the org's CMDB/live-config/monitoring/running-worker adapters, tagged owner + liveness-contract. The confidence-graded truth-layering, self-expiring edges, and liveness-as-a-governed-set principles port unchanged; the concrete counts of the predecessor's one estate are provenance only [R: rule 9].
- **Retention is org policy; only the audit ledger is immutable.** The tamper-evident governance/decision ledger (`session_risk_audit`, the SHA-256 hash-chain, the prediction log) stays append-only and is preserved by integrity-preserving **archival/sealing**, never deletion. All operational memory (transcripts, diaries, incident_knowledge, wiki, embeddings, event/tool streams) is governed by configurable TTL + hard-delete/right-to-erasure [R: rule 5; O: INV-14]. This split is drawn explicitly in **DATA-MODEL.md**.
- **Cost is org-global.** The single-source `llm_usage` ledger (real tokens only, no fabrication) is the org chargeback substrate with quotas, budgets, and alerts [R: rule 6; F].
- **One implementation per stage.** Per-site behavior is supplied as configuration/adapter instances at runtime; no logic is copy-forked per site — a security fix cannot land on one and silently miss another [O: INV-18].

---

*See **DATA-MODEL.md** for the full Postgres schema (the append-only audit spine vs. the purgeable operational body, pgvector embeddings, and schema-version discipline), and **adr/** for the decision records — including ADR-0005 (out-of-process governed-module runtime) and the ADR forbidding model fine-tuning in favor of prompt-policy iteration + RAG.*
