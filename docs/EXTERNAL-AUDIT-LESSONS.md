# Territory Grounder — Lessons from the external audit of the predecessor (2026-07-15)

Two independent external audits of the predecessor agentic system (claude-gateway) — a quality/architecture
audit (**7.3/10, B+**) and a security audit (**29 findings: 4 critical / 11 high / 14 medium**). TG is the
migration of that system; these are the mistakes TG must **not** repeat and the improvements to bake in. This
doc is a build directive, ranked. (Companion to `PORT-FIDELITY-AUDIT.md` + `SYSTEM-MAP.md`.)

## The pivotal reframe

> "The next maturity jump comes from **subtraction and standardization, not additional capability.**"

The predecessor's main problem is **no longer missing capability** — it accumulated so many workflows, hooks,
gates, memories, evaluators and agents that the system itself became the object requiring orchestration
(443 n8n nodes; a 53-node Runner and 76-node Matrix Bridge that are *applications encoded as workflows*).
**TG is that consolidation**: Go control-plane · thin Temporal orchestration · one Postgres · one typed spine.
So "build everything" = re-express every predecessor *capability* in TG's consolidated architecture, and
**prioritize the consolidation primitives below over peripheral subsystems.** Never re-import the sprawl.

## Architecture/quality lessons → TG action (ranked)

| # | Lesson (external audit) | TG status | Action |
|---|---|---|---|
| 1 | **Keep domain logic OUT of the orchestration layer** (n8n became an app runtime; a 14-hour outage came from a Build-Prompt Code node). | ✅ Go control-plane + Temporal activities isolate side-effects | VERIFY the runner workflow stays control-flow-only; no prompt-compile/parse/policy logic in the workflow. |
| 2 | **One immutable ActionManifest / ExecutionRecord** binding trigger → normalized incident → context snapshot → proposed op → predicted effect → autonomy class → approved option → **actual tool calls** → observed postconditions → verdict. | 🟡 `core/manifest` content-hashes the action; the chain is NOT yet end-to-end (executed tools + verdict not bound in) | BUILD: extend the manifest to the full immutable chain; make it the single record for replay/eval/observability. |
| 3 | **Execution classes** — DETERMINISTIC / FAST_AGENT / STANDARD_AGENT / DEEP_INVESTIGATION / HUMAN_LED — chosen BEFORE expensive context construction (not just model tiers). Most incidents shouldn't pay the full ceremonial lifecycle. | ❌ docs only (DEEP_INVESTIGATION named, unbuilt) | BUILD: an execution-class router at the top of the runner; cheap classes skip RAG/prediction/large-prompt. |
| 4 | **Phase-aware prompt compiler** — inject only the sections the class+phase needs; keep irrelevant prompt tokens **<10–15%**. | ❌ | BUILD: a prompt compiler + `prompt_section_tokens` metric. |
| 5 | **RAG latency modes** — CACHE_ONLY (p95<500ms) / FAST_RAG (p95<5–8s) / DEEP_RAG (p95<20s). The deep 20s-p95 path must not be the default. | ❌ (knowledge plane unbuilt) | BUILD with the knowledge/RAG plane; route by execution class. |
| 6 | **Unified state**: one canonical DB, versioned deploy-time migrations, STRICT tables, foreign keys, one data-access library. Predecessor had multiple SQLite paths, runtime CREATE/ALTER, 5 FKs, no STRICT, no migration authority. | ✅ one Postgres + migration runner + DML/DDL role split | VERIFY: CHECK/FK/NOT-NULL on governed tables (Postgres has no STRICT — use constraints), one persistence library, NO runtime DDL. |
| 7 | **Prediction gate bound to the EXACT final action** — predecessor's gate inspected `[POLL]` text but the parser accepted a natural-language fallback that BYPASSED it, and the committed prediction was the pre-generated plan, not the exact proposal+executed commands. | ✅ one `ParseProposal` (no second grammar), full-SHA-256 `plan_hash` | VERIFY the one-grammar property under test; bind prediction→approval→executed-action via the manifest (#2). |
| 8 | **Contracts must match canonical persistence** — predecessor's `incident_knowledge` contract wanted `rule`+`schema_version` but the table had `alert_rule` and no version; `infragraph_dynamics` contract wanted `src`/`dst` but the table keyed on `rel_id`. Contracts couldn't validate the rows they governed. | ✅ `tools/gencontracts` generates from routes/entities + `schema_version` | VERIFY generated contracts validate REAL rows (round-trip a live row through the contract). |
| 9 | **Measure multi-agent benefit before enabling it** — don't add agents without proven latency/quality lift; predecessor's delegation was inert (max depth 2, thresholds never fired). | ✅ single agent loop (both audits vindicate this) | HOLD: no multi-agent until `delegation_precision`/`parallel_speedup`/`multi_agent_quality_lift` show net value. |
| 10 | **Evaluation = representative end-to-end replay with independently-verified environmental outcomes** — NOT synthetic smoke tests or acceptance tests that "check schema shape, source strings, or no-op steps." Predecessor's headline result was 10 synthetic incidents + 4 invariants. | 🟡 INV-22 mandates real oracles, but some spec tests risk shape-only checks | AUDIT TG oracles for shape-only tests; BUILD the 200–300 incident-replay corpus + the **Agentic Utility** composite (0.40 verified outcome · 0.15 autonomy · 0.15 reliability · 0.10 evidence · 0.10 latency · 0.10 cost). |

## Security lessons → TG (each maps to an existing INV — VERIFY it is ENFORCED, not just claimed)

| External finding | TG invariant | Verify |
|---|---|---|
| Every webhook omits workflow-level auth (25 nodes) | INV-01 every route authenticated (`auth=none` panics at boot) | non-bypassable router; no unauthenticated route compiles |
| Progress-poller: caller `pid`/`issueId` → remote shell (command injection before any gate) | INV-02 no `sh -c`, argv-only actuation | forbidden-pattern gate + argv-only adapters |
| Forged webhook text → shell before Claude; `JSON.stringify` misused as shell escaping | INV-02 + typed ingest | no string-built commands anywhere |
| Alert-receiver fields → SSH/lock-path/SQLite (systemic shell+SQL injection) | no-string-SQL (sqlc/`$1`) | forbidden-pattern gate; parameterized only |
| Session-replay resumes an agent launched `--dangerously-skip-permissions` | mutation gate + no skip-permissions bypass | no privileged resume path; mutation OFF by construction |
| Prediction gate bypassable + not bound to final action | one `ParseProposal` + plan_hash + manifest chain (#2/#7) | property test: no second grammar; manifest binds action |
| Matrix events not isolated by room (global stream, late sanitization) | INV-12 vote binds to the exact decision it answers | per-decision binding; no global cursor |
| Retired OpenClaw paths remain executable (incomplete decommission) | INV-17 unregistered ⇒ no execution path | dead code has no reachable execute path |
| Plaintext workflow credentials | INV-13 secrets as env:/file: references | no literal secrets (gitleaks + forbidden-pattern) |

## The one strategic rule for the migration

> Do NOT add another agent, sentinel, evaluator, workflow or memory subsystem until the Runner, the state
> model, the ActionManifest chain, and the execution classes are consolidated. Subtraction and
> standardization first; peripheral capability after.

## Part 2 — product maturity & portability (the path from 7.3 → 9.0)

The follow-up external audit: TG has "a potentially top-tier **architecture**, not yet a top-tier **product**."
Raising the score is *evidence and packaging*, not more features. This reframes the migration's endgame.

### TG's defensible category (the north star)

> **Self-hosted, closed-loop, infrastructure-agnostic SRE control plane with policy-driven remediation and
> independent outcome verification.**

Most competitors (HolmesGPT, Aurora, OpenSRE, IncidentFox) stop at *investigate → explain → propose*. TG's
opening is the **governed execution-and-verification loop**. The 7 dimensions to dominate — TG already
targets all seven; keep them the priority:
1. **ActionManifest** — exact typed representation of the intended change (✅ the immutable chain, MR !46).
2. **Policy-based autonomy** — deterministic AUTO / NOTICE / POLL / prohibit (✅ risk bands + never-auto floor).
3. **Durable execution** — incidents survive crashes + human waiting (✅ Temporal continue-as-new).
4. **Independent verification** — success = observed state, not the agent's narrative (✅ mechanical verdict).
5. **Heterogeneous infrastructure** — SSH/Docker/K8s/NAS/net/CI/observability/arbitrary APIs (✅ connector fleet).
6. **One-image standalone** — genuinely useful outside Kubernetes (🟡 compose exists; tighten to one image + volumes).
7. **Outcome-labelled memory** — learn only from *verified* outcomes (❌ the lessons loop, still to build).

### Portability = config-not-code (a HARD rule for every TG file)

The predecessor lost portability points for hard-coding its own estate. **TG must never hard-code**:
hostnames, Matrix rooms, sites, NL/GR branches, database locations, approval policies, or a private
infra taxonomy. The internal model must be generic (`resources:` docker/k8s/ssh, `signals:` prometheus,
`notifications:` matrix/slack) so a stranger configures it *without editing source*. When writing any TG
code, take estate specifics from config/typed inputs, never literals. (Test data may name real hosts;
production code paths must not.)

### Product-maturity checklist (the 8→9 work, mostly post-Phase-2)

Clean install on an unrelated machine · config without editing source · backward-compatible migrations ·
reliable upgrades + rollbacks · useful error messages · stable adapter contracts · fixtures for different
infras · complete session replay/recovery · versioned releases · docs a stranger can follow · a
representative incident-replay benchmark (200–300 trajectories) scored on **verified environmental
outcomes** (the Agentic Utility composite), not synthetic smoke tests. "Mature = strangers operate it
successfully without you."
