# ADR 0006 — Frontend: a TypeScript `frontend/` service

## Status
Accepted (framework choice deferred).

## Context
The predecessor had **no first-class UX** — the human-in-the-loop surface was Matrix polls and host-local sentinel files [F]. TG makes **first-class UX/UI a named product pillar** [R corrections]: the web **console** is where governed autonomy becomes usable and trustworthy — an **approval console** (the human circuit-breaker), an **ActionManifest timeline/replay** rendering predicted→approved→executed→verified as one visual chain, a **tamper-evident ledger** view, **explainability** ("why did the agent do this"), **autonomy-band + kill-switch controls**, and **multi-tenant admin**. Critically, the autonomy toggles and kill-switches **move OFF host-local sentinel files ONTO the UI/API + RBAC** [R corrections][R paradigm-rule 4].

## Decision
Build a dedicated **`frontend/`** service in **TypeScript** [R corrections/stack]. It is **API-first**: the UI consumes the **generated OpenAPI** — no second contract [R corrections]. This binds the UI directly to INV-15 [O]: every wire contract is generated from one authoritative entity definition (100% endpoint coverage, declared auth/error/idempotency, non-null `generated_at` + source hash), so the console can never drift from the control-plane's real API.
- The console renders the `ActionManifest` (INV-07) [O] as the timeline: the same content-hashed object threaded through risk→prediction→approval→execution→verification is what the UI visualizes.
- Autonomy-band, DARK→SHADOW→ENFORCE, and kill-switch controls are **RBAC-gated, per-tenant, audited-on-change** API calls against the feature-flag/policy store (INV-01 auth, control-tier on an elevated authorization tier) [O][R rule 4] — not `touch`/`rm` of a file.
- Approvals are bound to a specific `decision_id` (carrying `action_id` + room) and routed as Temporal signals (INV-12) [O].
- **The service is `frontend/`, never `web/`.**

## Consequences
- The UI is a governed client of the same authenticated, default-deny API as every other caller (INV-01) [O]; no privileged UI-only backdoor.
- Framework (React / Svelte) is **deliberately deferred** [R corrections]; the API-first contract means the framework decision is reversible and does not block the control-plane.

## Alternatives
- **Server-rendered Go templates** — keeps one language, but a rich manifest-replay/timeline/explainability console is a genuine SPA; Go `html/template` remains the escaping layer at any message *sink* (INV-04 rendering) [O], not the console framework.
- **No console, chat-only** (predecessor [F]) — rejected [R]: the differentiator is only usable through a real console; kill-switches must be RBAC-gated, which a sentinel file cannot be.
