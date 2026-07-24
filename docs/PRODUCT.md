# Territory Grounder — PRODUCT

> *"The Map Is Not the Territory."* — Territory Grounder is the one channel in your automation that is allowed to tell you **no**.

**What this document is.** The product definition of Territory Grounder (TG): what it is, who it is for, and what it must do. It is the entry point to the founding set — see [ARCHITECTURE.md](ARCHITECTURE.md) for how it is built, [CONSTITUTION.md](CONSTITUTION.md) for the inviolable safety rules, [TESTING-AND-BENCHMARK.md](TESTING-AND-BENCHMARK.md) for how "done" and "correct" are proven, and the manifesto [the-map-is-not-the-territory.md](the-map-is-not-the-territory.md) for the worldview.

**Provenance convention.** Every substantive claim is tagged by layer: **[F]** foundation (the predecessor system's design TG inherits), **[R]** product reframe (multi-user / de-solo / de-homelab), **[O]** audit overlay (security + quality hardening). Source ids (INV-NN, spec/00x, paradigm-rule N) make provenance auditable so the layering cannot be silently re-inverted. [R] paradigm-rule 10

**Identity.** Name: Territory Grounder. CLI: `grounder` (alias `tg`). License: Apache-2.0. Monorepo under GitLab group `products/territory-grounder`, flagship project `grounder`. [R]

---

## 1. Product paradigm

Territory Grounder is an **open-source, self-hosted, single-organization multi-user, distributable governed-autonomy SRE platform**. It is an always-on agentic on-call layer for infrastructure: it triages alerts, investigates root causes, and either proposes or *autonomously executes* reversible fixes, escalating to the organization's configured on-call approver roles only for the genuinely irreversible or security-sensitive. [R] paradigm-rule 2, [F] mission

Four properties define the paradigm:

- **Open-source & self-hosted.** Apache-2.0, runs entirely inside the adopter's own trust boundary. No SaaS dependency, no phone-home, no vendor lock-in. Every capability is inspectable and every decision is reconstructable from the adopter's own database. [O] S8-7
- **Single-organization, multi-user.** One deployment serves one organization with many operators, roles, and teams — not mutually-distrusting tenants sharing an install, and not a solo operator. There is no `tenant_id` and no cross-org isolation to carry; the correlation key is a bare `external_ref` (ids are unique within the org's own trackers). Authority is checked against the acting user/role, and least-privilege identity (per-source HMAC secrets, per-agent scoped credentials/mTLS, credential-revoke-as-kill) is defense-in-depth, not tenancy. [R] paradigm-rule 1; [O] INV-12, INV-16
- **Distributable.** TG installs against *any* estate and *any* stack. Every integration surface is a loadable module; every vendor is one selectable backend behind a stable interface. It ships as a guided container stack, not a bespoke deployment. [R] paradigm-rule 3, paradigm-rule 7
- **Governed autonomy.** The system acts on its own on the safe-and-recoverable, pages a human on the impactful-but-reversible, and holds only the irreversible or security event — under a mechanical safety core that no org policy, confidence score, or feature flag can lift. [F] founding principle "human as circuit-breaker"; [R] paradigm-rule 8

TG is a **greenfield** Go + Temporal + PostgreSQL/pgvector reimplementation. It is not a port of a legacy runtime and carries no legacy-compat layer: the "off" state of any autonomy feature is simply the non-autonomous baseline, not a byte-identical prior behavior. [R] paradigm-rule 7

### Provenance, not target market

The paradigm was proven by a predecessor: a solo-operated, roughly 300-machine homelab whose single absent human voted on **0 of 824** approval polls in 30 days, forcing the design of a human-as-circuit-breaker autonomy model rather than a human-as-gatekeeper one. [F] founding principle; [R] mission That predecessor — with its single shared SSH identity, hand-maintained one-estate graph, and n8n / Cronicle / OpenClaw / Matrix / YouTrack stack — is the **battle-test and provenance** for TG, and every solo, single-user, and single-vendor assumption it carried is deliberately inverted here. Wherever this document refers to that history it is provenance framing, never the product's target. [R] paradigm-rule 1, paradigm-rule 9

---

## 2. Audience

TG is for **anyone running infrastructure who wants governed autonomy they can trust and audit — not a black box.** [R] mission The audience spans:

- **Solo homelab operators** — the origin case, still a first-class supported operator. [R] mission
- **Small platform / SRE teams** who want an on-call teammate that safely closes reversible toil instead of paging a person for every recoverable blip. [R] paradigm-rule 2
- **Multi-team production estates** where distinct teams inside one organization share one control-plane: each team has its own RBAC roles, approver graph, budgets, retention policy, and adapters, and the estate is sliced by a descriptive `site`/`estate` label for filtering and routing — every operator sees the whole estate subject to RBAC, never a hard isolation wall. [R] paradigm-rule 1
- **Managed-service / MSP operators** running many *isolated* customer orgs behind one install are **out of scope for this design** — that reintroduces a hard org-isolation boundary and is deferred to a separate explicit ADR (see [adr/0010](adr/0010-single-org-multi-user-not-multi-tenant.md)). TG today is one deployment per organization. [R] paradigm-rule 6

Across all of these, the **human is a set of roles, not a person**. Where the predecessor had one named on-call human, TG has an approver graph: RBAC roles, on-call rotation and escalation policy, quorum, and a fallback approver. AUTO_NOTICE and POLL_PAUSE route to the configured on-call group; veto and approval authority are checked against the acting user/role. [R] paradigm-rule 2

The unifying need: teams that must let software *act* on infrastructure but cannot accept an unaccountable actuator. TG's answer is autonomy that is **graded, bounded, predicted-before-acting, mechanically verified after, and cryptographically auditable by construction.** [F] founding principles; [O] verdict

---

## 3. The market gap — governed autonomous *actuation* (the wedge)

Existing tools cluster at three unsatisfying points. TG's wedge is the missing fourth. [R] mission; [O] verdict

| Category | What it does | Why it fails the on-call need |
|---|---|---|
| **Diagnose-only** (AIOps assistants, chatbots, RCA copilots) | Investigates, summarizes, suggests. | A human still has to *do* every fix. The toil is untouched; the assistant is a smarter pager, not a teammate. |
| **Approve-everything** (runbook automation gated on manual approval) | Proposes an action, waits for a click. | When the on-call human is asleep, out, or — as the predecessor proved — votes on 0/824 polls, a backlog of *reversible* work piles up behind a gate no one opens. A growing POLL_PAUSE backlog of safe work is **defined as a policy failure**, not prudence. [F] founding principle "human as circuit-breaker" |
| **Blind-auto** (scripted auto-remediation, alert→action rules) | Fires a fixed action on a fixed trigger, no reasoning, no guardrail on consequence. | No reversibility model, no consequence prediction, no verification, no audit — one bad rule takes down the estate. Confidence and blast-radius are invisible to it. |

**TG occupies the wedge: governed *autonomous actuation*.** It actually executes fixes — but only inside a mechanical governance envelope: [F] founding principles; [R] paradigm-rule 8; [O] Preserve

- **Reversibility is the primary risk dial**, with a mechanical, non-configurable **NEVER-auto floor** (mkfs, dropdb, zpool/zfs destroy, tofu destroy, kubectl delete/drain, credential-revoke, config-file overwrite, reboot/halt, code/repo destruction, P0-host reboot, real jailbreak). No confidence score, org policy, or flag lifts the floor; an *unrecognized* mutation is never "safe" by omission. [F] risk-appetite model; [R] paradigm-rule 8
- **Three autonomy bands** decide the human's role per session: **AUTO** (act silently on reversible, predicted, low-risk work), **AUTO_NOTICE** (act, and notify the on-call group in parallel for out-of-band veto), **POLL_PAUSE** (hold, page, and never proceed on timeout). [F] spec/001; [R] paradigm-rule 2
- **Five execution classes** scope how much machinery a decision earns — `DETERMINISTIC` (no model), `FAST_AGENT`, `STANDARD_AGENT`, `DEEP_INVESTIGATION`, `HUMAN_LED` — with class-aware automation ceilings (never-auto / canary / staged / auto) that irreversible or stateful classes can never breach. [F] confidence discipline; [O] INV-09, S8-6
- **Predict before acting, verify mechanically after.** A machine consequence prediction, computed *outside* the model and keyed to an immutable action hash, is committed *before* any approval poll; the remediation lane fails **closed** without it. After execution, deterministic code — never the acting model — writes the only match / partial / deviation verdict, and a deviation can never auto-resolve. [F] fail-closed gate; [O] INV-10
- **Every decision is auditable by construction**, appended to a tamper-evident SHA-256 hash-chained governance ledger. [F] governance hash-chain; [O] INV-19

This is the differentiator, and it is the thing the predecessor's external audit found was *real in intent but false in binding* — so in TG each control is re-founded behind typed, authenticated interfaces bound to **one immutable content-hashed action**, making the whole injection/bypass/drift class structurally uncompilable rather than merely discouraged. [O] verdict, S8-preserve-meta The mechanism lives in [ARCHITECTURE.md](ARCHITECTURE.md) and [CONSTITUTION.md](CONSTITUTION.md); this document only stakes the claim.

---

## 4. Deployability — one guided `docker-compose`

TG must be **installable by a competent operator in an afternoon**, not integrated by a services engagement. [R] paradigm-rule 7

- **Packaging.** A single guided **`docker-compose`** single-node profile brings up the full stack: the Go control-plane, Temporal, PostgreSQL + pgvector, the bundled LiteLLM model-gateway, the `frontend/` console, and the default reference modules. It is honestly a multi-service stack rather than one image, but it is one guided command and one config file to first light. [R] corrections (Packaging)
- **One database, one DSN.** All state is in a single PostgreSQL reached through one DSN, with schema evolved only by ordered transactional migrations applied at deploy/startup under an advisory lock; the runtime DB role holds DML only, no DDL. This replaces the predecessor's SQLite + FAISS. [R] corrections (State); [O] INV-16
- **Secure on first boot.** Every ingress is authenticated and default-deny by construction — a forgotten auth config yields a *dead* endpoint, not an open one — and the system boots **read-only**: a global `mutation_enabled=false` preflight keeps TG in investigation-only mode until auth, action-binding, and verification self-tests pass green. [O] INV-01, INV-09, Phase 0
- **Estate knowledge self-populates.** There is no hand-maintained inventory to seed. The infragraph causal graph, blast-radius edges, criticality (P0) tiers, component/liveness registry, and tool inventory are **discovered** from the org's own CMDB, live-config, monitoring, and running workers — each tagged with owner and a liveness contract. [R] paradigm-rule 9
- **No site-forking.** There is exactly one implementation of each pipeline stage and alert-source type; per-site behavior is configuration, never copied-forked logic. Two sites are two config rows, not two workflows. [O] INV-18

Deployment is greenfield: there is no n8n engine, Cronicle scheduler, OpenClaw tier, or operating-mode abstraction to stand up. Temporal is the load-bearing substrate for durable workflows, activities, schedules, and signals — it replaces the predecessor's orchestration engine, scheduler, and most of the watchdog/reconcile machinery. [R] corrections (Stack), paradigm-rule 7

---

## 5. The UX/UI pillar — a named product surface

The predecessor had **no first-class UI**; its human interface was a chat bridge and host-local files. In TG the console is a **named product pillar**, because the governed-autonomy differentiator only becomes *usable and trustworthy* when a team can see, steer, and audit it. [R] corrections (UX/UI)

The `frontend/` service (a TypeScript service — framework choice deferred; **not** named `web/`) delivers: [R] corrections (Frontend, UX/UI)

- **Approval console** — the human circuit-breaker made concrete. On-call approvers see pending POLL_PAUSE and AUTO_NOTICE decisions, the proposed plan with its 2+ approaches, the committed prediction, and the reversibility/blast-radius signals; they approve, veto, or hand off — with authority checked against the approver RBAC graph. [R] paradigm-rule 2; [F] Phase 6 approve
- **ActionManifest timeline / replay** — the predicted → approved → executed → verified chain rendered as **one visual chain** over a single immutable content-hashed `ActionManifest`, so an operator can watch (or replay after the fact) exactly what was reasoned, authorized, and done, and how the mechanical verdict landed. [R] corrections (UX/UI); [O] INV-07
- **Tamper-evident ledger view** — a browsable, verifiable window onto the append-only SHA-256 hash-chained governance ledger, so "who let the agent act, and why" is provable, not asserted. [F] governance hash-chain; [O] INV-19
- **Explainability** — "why did the agent do this": the retrieval context, the risk banding and its signals, the execution class, the confidence trajectory, and the tool-result evidence that backed any auto-resolve. [O] INV-11
- **Autonomy-band + kill-switch controls** — the on/off and DARK→SHADOW→ENFORCE controls for every autonomy layer. **These move off host-local sentinel files onto the UI/API + RBAC**: they are org feature-flags/policy in the datastore, RBAC-gated, and audited on change. The strong underlying principle is kept verbatim — every autonomy layer is independently, instantly disableable and ships **dark** by default with observe-before-live promotion — only the *mechanism* changes from a filesystem `touch`/`rm` to an audited API/config control. [R] corrections (UX/UI), paradigm-rule 4
- **Org admin** — RBAC user/role and team management, approver-graph and on-call configuration, module enablement, model-routing policy, budgets/quotas, and retention/TTL policy. [R] paradigm-rule 1, paradigm-rule 5, paradigm-rule 6

**API-first.** The console consumes the generated OpenAPI contract — there is no second, hand-maintained contract. Every wire contract, DDL, validator, and human-facing count is generated from one authoritative source per entity, and CI fails on drift or a hand-written number. This is what keeps the UI, the API, and the running system from ever disagreeing. [R] corrections (UX/UI); [O] INV-15

---

## 6. The module system — all integrations are loadable

**Every integration surface is a loadable/unloadable module**, never a hardcoded dependency. The surfaces are: ingest sources, ticket trackers, notifier + approval channels, CMDB, actuation, model providers, and observability sinks. [R] corrections (Modules), paradigm-rule 3

- **Interfaces vs implementations.** The repo separates `adapters/` (the stable module **interfaces**) from `modules/` (loadable **implementations** plus a small reference-adapter set and an SDK). Every named vendor — YouTrack / Jira / GitHub Issues / ServiceNow; Matrix / Slack / Teams / email / webhook; LibreNMS / Prometheus / CrowdSec; NetBox; Twilio; the LLM providers; the reranker/embedder — becomes **one selectable backend behind an interface**, resolved by org config. [R] corrections (Modules), paradigm-rule 3
- **Default mechanism (ADR-0005, proposed): out-of-process governed plugins.** Each module is a separate process/container over a stable protocol (gRPC / HashiCorp go-plugin; **MCP** for tool/actuation modules). This gives runtime load/unload, third-party modules, process isolation, and per-module capability scoping. (Alternatives considered: B, a compile-time registry; C, WASM/Extism — see the ADR.) [R] corrections (Modules)
- **Governed by construction.** Modules are signed, capability-scoped, and RBAC-enabled. A **disabled or unregistered module has no execution path** — this is the product-grade closure of the predecessor's "dead OpenClaw path still executable" failure class: a capability exists only if its adapter is registered, and retiring a capability means deleting it from the build, never leaving it dormant. [R] corrections (Modules); [O] INV-17

The reference-adapter set makes TG usable out of the box; the SDK makes it extensible without forking; the governance makes third-party modules safe to load. See [ARCHITECTURE.md](ARCHITECTURE.md) for the interface catalog and ADR-0005.

---

## 7. LLM-provider config + auto-fallback ladder

TG's agent is a **native Go ReAct / tool-calling loop** in the `agent/` service that calls LLM **APIs directly**. There is no external CLI subprocess to launch or resume; the predecessor's subprocess mechanism is dropped entirely. [R] corrections (NO Claude Code)

- **Bundled LiteLLM model-gateway.** `docker-compose` bundles **LiteLLM** as the model-gateway: one OpenAI-compatible endpoint fronting N providers, with the auto-fallback ladder expressed as config, plus retries, rate-limit handling, and org budgets and quotas. [R] corrections (NO Claude Code)
- **Auto-fallback ladder (user-configurable).** The default ladder is `z.ai` (primary) → `DeepSeek` → `Mistral` → then `Anthropic` / `OpenAI` / `Grok` / etc., failing over automatically on error, rate-limit, or provider outage. The org configures its own ladder and provider set. [R] corrections (NO Claude Code)
- **One resolver, org policy.** The single component→provider/model source-of-truth resolver is kept from the predecessor, but the three hardcoded planes (subscription / fixed paid API / local $0) are dropped. Cost/locality — local-first / cloud-frontier-primary / hybrid — is an **org-configurable policy**, and even the judge's frontier cross-check anchor is an org model-routing choice. [R] paradigm-rule 3, paradigm-rule 6; corrections (NO Claude Code)
- **Local-first-$0 is a mode, not the mission.** Local Ollama / $0 routing is one selectable cost/locality profile, not the product's reason for being. Cost is real per-token per provider, and the `llm_usage` ledger (real tokens only, no fabrication) becomes the org chargeback substrate with quotas, budgets, and alerts. [R] paradigm-rule 6; [F] cost ledger
- **The model stays untrusted.** No provider choice changes the constitution: the LLM is a suggestion engine, never an authority. No model-produced token becomes control flow, a command string, or a query fragment; model output enters the system only as typed, validated, delimited data. [O] INV-08

---

## 8. Non-goals

TG deliberately does **not**: [R] paradigm-rules 7, 6, 10; [O] Phase 0; [F] mission

- **Ship a hardcoded vendor stack or require any specific vendor.** Named vendors are reference adapters, never requirements. [R] paradigm-rule 3
- **Carry legacy-compat.** No n8n engine, Cronicle scheduler, OpenClaw tier, operating-mode abstraction, or "reverts to byte-identical legacy behavior" clause. The off-state is the non-autonomous baseline. [R] paradigm-rule 7
- **Launch or resume an external agent CLI subprocess.** The agent loop is native and calls LLM APIs directly. [R] corrections (NO Claude Code)
- **Use host-local sentinel files as a control mechanism.** All autonomy controls are API/RBAC/config-driven and audited on change. [R] paradigm-rule 4
- **Let the model hold its own effect channel, grade its own outcome, or become control flow.** The deterministic orchestrator owns control flow and the effect channel; the verdict is mechanical. [F] founding principles; [O] INV-08, INV-10
- **Fine-tune model weights to change behavior.** Behavioral adaptation is prompt-policy iteration + RAG, promoted only through a one-sided Welch-t significance gate; weight updates are ADR-forbidden. [F] policy-change principle
- **Hoard data forever.** "Save everything forever" is not a reachable configuration; only the compact tamper-evident audit spine is immutable, preserved by integrity-preserving archival, never by "memory never shrinks." All operational memory has configurable TTL and right-to-erasure. [R] paradigm-rule 5; [O] INV-14
- **Be a general chatbot, ITSM suite, or monitoring system.** TG consumes alerts from monitoring via ingest modules and drives tickets via a tracker module; it does not replace them. The teacher/pedagogy and parallel-dev decomposition surfaces are optional out-of-constitution modules, not core. [R] reframes (teacher-agent, parallel-dev)
- **Expose privileged control as ordinary ingress.** No `session-replay`, `chaos-start`, `self-heal`, or `session-control` HTTP endpoint on the plain ingress path; re-engagement mints a fresh gated workflow, and privileged ops are internal Temporal signals or a separate elevated-auth tier. [O] INV-01, Phase 0, threat model

---

## 9. Success criteria

TG is successful when its claims are *proven*, not asserted — the honest quality signal is external and adversarial, never a self-scorecard. Each criterion below binds to an executable gate defined in [TESTING-AND-BENCHMARK.md](TESTING-AND-BENCHMARK.md). [O] INV-22; [F] eval flywheel

1. **The governance envelope holds under attack.** Every safety control (ingest auth, injection boundaries, routing, prediction gate, verdict, banding, ledger chaining) is exercised by executable tests driving the *actual* code path with malicious, concurrent, replayed, delayed, and partial-failure inputs, asserting on observed output/state — no source-string or schema-presence assertions. Release is gated by a boundary-coverage map requiring at least one adversarial test per declared trust boundary. [O] INV-22, Phase 4
2. **The wedge works end to end.** A reversible incident flows event → triage → context → plan/risk-band → commit-prediction → propose → approve → execute → verify → learn and auto-resolves *only after* the alert condition is confirmed cleared, with a per-incident best-outcome record so alert storms cannot inflate the auto-resolve rate. [F] spec/003; success measured per-incident, not per-event
3. **Safety never composes away.** The orchestration invariant benchmark's I1 holds: every irreversible operation lands in POLL_PAUSE/high regardless of how it is framed; the mechanical NEVER-auto floor is never breached; a deviation never auto-resolves. [F] orchestration benchmark; [O] INV-09, INV-10
4. **No prediction, no action.** The remediation lane fails closed: a poll cannot start without a persisted prediction bound to the same content-hashed action that executes; a prediction-gate bypass property test enumerates every parser path and asserts gate-then-poll ordering. [O] INV-06, INV-07, Phase 4
5. **Least-privilege identity holds.** Per-source and per-agent credential-scoping tests prove each adapter and agent acts only within its granted capability; authority checks resolve against user/role, and credential-revoke instantly kills an agent's reach. [R] paradigm-rule 1; [O] INV-12, INV-13
6. **Nothing ships retired-but-present, and nothing runs dark.** The manifest reconciler refuses to boot if live registered adapters/workflows diverge from the signed manifest; every component carries a liveness contract with absent()-guarded staleness alerts and a synthetic canary against an isolated ephemeral database (live-DB-leak counter must stay 0). [F] dark-component principle; [O] INV-17, INV-22
7. **The judge is calibrated and can fail loudly.** Judge TPR/TNR floors (≥0.70) hold against a non-local frontier anchor; judge-death is detected from judge-independent tables; the sealed monthly holdout the system may never tune to shows no >20-point regression-vs-holdout gap (which is *defined* as overfitting failure). [F] eval principles
8. **Docs, contracts, counts, and the running system can never disagree.** Every generated artifact carries a non-null `generated_at`, a source hash, and a coverage scope; CI fails on any hand-written number, uncovered endpoint, or drift. [O] INV-15

The full acceptance matrix, adversarial fixture corpus, and the four roadmap-phase gates (Phase 0 secure read-only foundation → Phase 1 typed spine + action binding → Phase 2 governed autonomy → Phase 3 anti-drift/decommission → Phase 4 adversarial assurance) live in [TESTING-AND-BENCHMARK.md](TESTING-AND-BENCHMARK.md). [O] roadmap phases
