# Architecture Decision Records — Territory Grounder

This directory records the foundational architecture decisions for **Territory Grounder (TG)** — an open-source, self-hosted, **single-organization / multi-user**, distributable governed-autonomy SRE platform (ADR-0010; not multi-tenant SaaS). Each ADR captures one decision in the format *Title / Status / Context / Decision / Consequences / Alternatives*.

TG is a **greenfield** [R] reimplementation. It **inherits** the predecessor's governed-autonomy IP — the deterministic-orchestrator-over-untrusted-model split, the 3 autonomy bands (AUTO / AUTO_NOTICE / POLL_PAUSE), the inviolable **mechanical safety core**, execution classes (DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED), fail-closed predict-then-verify, and the two-lane fail model — and **re-founds every control behind a typed, authenticated interface bound to one immutable content-hashed `ActionManifest`** (the audit's meta-principle: a control is only as strong as its binding).

## Provenance tags
Claims are tagged by layer so the layering is auditable and cannot be silently re-inverted:
- **[F]** — foundation: the existing system's design TG inherits (`SOURCE-foundation.md`).
- **[R]** — reframe: the multi-user / single-org / de-solo / de-homelab product paradigm (`SOURCE-paradigm.md`, paradigm-rules 1–10; rule 1 corrected to single-org by ADR-0010).
- **[O]** — overlay: the security + quality audit hardening (`SOURCE-overlay.md`, invariants INV-01..22, 15 threats, roadmap Phases 0–4).

## The records

| ADR | Decision | Status | Key layers |
|-----|----------|--------|------------|
| [0001](0001-language-go.md) | Control-plane language is **Go** (over Rust) — Temporal Go SDK is GA/flagship; cloud-native lingua franca | Accepted | [R] stack, [O] INV-02/03/17 |
| [0002](0002-orchestration-temporal.md) | Orchestration substrate is **Temporal** — replaces n8n + Cronicle + watchdog; concentration risk noted | Accepted | [R] rule 7, [O] INV-10/12/21 |
| [0003](0003-state-postgres-pgvector.md) | State is **one PostgreSQL + pgvector** + migrations (single-org, ADR-0010) — replaces SQLite + FAISS | Accepted | [R] rule 5, [O] INV-14/16 |
| [0004](0004-llm-gateway-litellm-no-claude-code.md) | **Native Go agent loop over a bundled LiteLLM gateway** (z.ai→DeepSeek→Mistral ladder) — **no coding-CLI subprocess** | Accepted | [R] rule 3/6, [O] INV-06/08 |
| [0005](0005-module-system-out-of-process-plugins.md) | **Out-of-process governed plugins** + MCP; alternatives compile-time-registry / WASM | **Proposed** | [R] rule 3, [O] INV-17/21 |
| [0006](0006-frontend-typescript.md) | A TypeScript **`frontend/`** console — API-first on generated OpenAPI; framework deferred | Accepted | [R] UX pillar/rule 4, [O] INV-15 |
| [0007](0007-license-apache-2.0.md) | License is **Apache-2.0** — matches the CNCF-adjacent ecosystem | Accepted | [R] open-source |
| [0008](0008-monorepo.md) | **Monorepo**, one repo many images; split the SDK later | Accepted | [R] packaging, [O] INV-15/18 |
| [0009](0009-spec-driven-development-lattice.md) | **Spec-driven development lattice** — fixed `spec/NNN/` five-file shape, `tools/specvalidate` gate, godog acceptance oracles, lockstep hash-drift check; no build obligation without a spec | Accepted | [R] SDD-WORKFLOW, [O] INV-22 |
| [0010](0010-single-org-multi-user-not-multi-tenant.md) | **Single-organization, multi-user** — NOT multi-tenant SaaS; drop `tenant_id`/RLS, keep RBAC/roles/approver-graph | Accepted | [R] rule 1/2 |
| [0011](0011-frontend-stack-react-vite.md) | Frontend stack = **React + Vite + TS + Tailwind + shadcn/ui + TanStack** (refines 0006) | Accepted | [R] UX pillar, [O] INV-15 |
| [0012](0012-skills-adopt-format-reauthor-content.md) | **Skills: adopt the format standards (SKILL.md/MCP), re-author the content in-house** — no vetted infra skill library exists; domain skills gated on read-only vendor MCP tools; third-party content = procedural knowledge only | Accepted | [F] skills, [O] INV-08/11 |
| [0013](0013-mode-is-the-actuation-chokepoint.md) | **The active mode is the SOLE actuation chokepoint** — `TG_MUTATION_ENABLED`, the console mutation toggle and `MutationGate` retired/absorbed; enabling is an authenticated, audited mode transition, never an env flag | Accepted | [O] INV-09, spec/015 REQ-1520/1521 |
| [0014](0014-gate-scope-matches-risk.md) | **Gate scope matches risk** — lockstep lock narrowed to safety-spine + effect channel + measurement plane + the gate itself (202→43 entries); `--restamp` refuses a hash whose spec did not change | Accepted (2026-07-30) | [O] gate integrity |
| [0015](0015-one-console-the-served-one.md) | **One console: the served `deploy/console/v2/`** — the React `frontend/` removed (undoes 0011's stack; 0006's API-first principle stands) | Accepted (2026-07-30) | [R] UX pillar, [O] INV-15 |
| [0016](0016-earned-opclass-overlay.md) | **Earned op-class overlay** — two deliberately unequal tamper domains (embedded lockstep-hashed vs append-only ledger-bound overlay); overlay autonomy ceiling = AUTO_NOTICE; the embed-export MR is the only road to full AUTO | Accepted (owner-approved 2026-07-31; open questions pending) | [O] INV-10/19, spec/028 |
| [0017](0017-prose-artifact-classes.md) | **Prose artifacts are classes on the ONE skill store** — closed `artifact_class` vocabulary {skill, prompt, runbook, rubric} on the existing store (no parallel table); per-class body caps are domain law over a widened schema ceiling; flywheel eligibility = {skill, prompt} only | Accepted (2026-08-14) | [F] skills, [O] INV-08, spec/014 REQ-1315/1316 |

## How these fit together
The stack decisions (0001 Go, 0002 Temporal, 0003 Postgres/pgvector) are the deterministic control-plane that owns the effect channel; 0004 makes the model an untrusted, typed, adapter-resolved suggestion engine over that control-plane; 0005 makes every integration a governed, RBAC-scoped, signed, load/unload-able module; 0006 exposes the governed autonomy through an API-first console where kill-switches and bands are RBAC/config controls (never host-local sentinel files); 0007 and 0008 set the distribution and repository shape. See also the sibling `docs/` set (e.g. `ARCHITECTURE.md`, `THREAT-MODEL.md`, and the manifesto `docs/the-map-is-not-the-territory.md`) for the full constitution these ADRs implement.

## Conventions
- One decision per file, numbered `NNNN-slug.md`, immutable once Accepted — a reversal is a **new** ADR that supersedes, referenced from the old one's Status (we do not rewrite history).
- Terminology is kept identical across all TG docs: `ActionManifest`, the five execution classes, the three autonomy bands, the mechanical safety core, the module system, Temporal, the LiteLLM model-gateway, single-org + RBAC (ADR-0010).
