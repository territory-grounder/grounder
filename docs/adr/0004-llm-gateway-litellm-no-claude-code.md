# ADR 0004 — Native agent loop over a bundled LiteLLM gateway (no vendor coding-CLI subprocess)

## Status
Accepted.

## Context
The predecessor launched and resumed an interactive coding-CLI subprocess to do its reasoning, streaming JSONL, and resumed it by session id [F]. That coupled the agent to one vendor's CLI, exposed an unauthenticated resume-with-attacker-prompt hijack (H-01) [O], relied on a `--dangerously-skip-permissions` posture [O], and hardcoded three model "planes" (a flat-rate subscription, a fixed paid multiplexer, local Ollama) with "never Anthropic for eval" baked in [F]. TG is **model-and-vendor-agnostic via adapters** [R paradigm-rule 3] and multi-tenant [R rule 1], so model selection, cost, and locality must be **per-tenant policy**, not a wired-in stack.

## Decision
**Drop the entire CLI-subprocess mechanism.** TG's `agent/` service is a **native Go ReAct / tool-calling loop that calls LLM APIs directly** [R corrections]. Model output enters only as typed, validated, delimited data — no model token becomes control flow, a command string, or a query fragment (INV-08) [O]; the agent emits a **typed tool-call** parsed once by one canonical parser (INV-06) [O], never a markdown sentinel.
- **Bundle LiteLLM** in docker-compose as the **model-gateway**: one OpenAI-compatible endpoint, N providers, retries, rate-limit handling, and **per-tenant budgets/quotas** [R corrections][R rule 6].
- **Default auto-fallback ladder (per-tenant configurable):** `z.ai` (primary) → `DeepSeek` → `Mistral` → then `Anthropic` / `OpenAI` / `Grok` / … Automatic fallback on error / rate-limit / outage [R corrections].
- Keep the single **component→provider/model source-of-truth resolver** [F]; drop the three hardcoded planes → per-tenant cost/locality policy (local-first / cloud-frontier-primary / hybrid) [R rule 6].
- Providers are one `LLMProvider` interface returning typed structs validated before use (INV-08) [O]; the judge's frontier cross-check anchor becomes a configurable higher-capability model per tenant [R].

## Consequences
- **No coding-CLI session-resume primitive** — re-engagement mints a new Temporal workflow re-running the full gate (ADR-0002), closing H-01 [O].
- `llm_usage` becomes the per-tenant **chargeback/billing substrate** — real tokens only, no fabrication, `tenant_id` + RLS [R rule 6][F].
- "Local-first-$0 / Max subscription" is demoted from mission to **one selectable mode** [R rule 6].
- Behavioral adaptation stays **prompt-policy iteration + RAG, never fine-tuning** (ADR-forbidden) [F][R].

## Alternatives
- **Keep the coding-CLI subprocess** [F] — rejected [R corrections]: vendor-locked, hijackable (H-01), un-tenantable.
- **Direct per-provider SDKs, no gateway** — rejected: re-implements fallback, rate-limit, quota, and per-tenant budgeting that LiteLLM supplies as one OpenAI-compatible endpoint.
