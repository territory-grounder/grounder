# Source-Benchmark Catalog — TG-38

*TG-38 deliverable — source-benchmark audit roll-up. Compiled 2026-07-18. Per-source detail lives in [`source-audits/`](source-audits/README.md).*

> **Provenance:** [O] audit-overlay. TG-38 benchmarked Territory Grounder against **30 external
> knowledge sources** (books, courses, certs, vendor engineering guides, and foundational papers),
> each scored 0–5 by auditing the source's named practices line-by-line against TG's *actual code and
> live deployment* — not its docs. This catalog is the roll-up: a per-source scorecard, one aggregate
> number, the gaps that **recur** across independent sources (a recurring gap outranks a one-off), and
> the places TG demonstrably **exceeds the predecessor's own posture**. The deduplicated remediation
> backlog below becomes the TG-38 child issues on project **TG**. Companion to
> [`IMPROVEMENT-TARGETS.md`](IMPROVEMENT-TARGETS.md) (competitive-analysis adoption backlog) and
> [`EXTERNAL-AUDIT-LESSONS.md`](EXTERNAL-AUDIT-LESSONS.md).

## Aggregate adherence

| Metric | Value |
|--------|-------|
| Sources audited | 30 (+1 addendum: source #31, arXiv 2607.14275 — see below) |
| **Weighted adherence** (by count of applicable, non-N/A-by-design practices per source) | **3.35 / 5** |
| Unweighted mean | 3.24 / 5 |
| Highest | OpenAI *Practical Guide to Building Agents* — 4.5 |
| Lowest | Anthropic *Code Execution with MCP* — 1.5 |

**By category** (mean): Microsoft 4.05 · LangChain 3.77 · Book 3.45 · Course 3.45 · OpenAI 3.33 ·
Google 3.13 · Anthropic 3.08 · **Paper 2.67**. The spread is the story (see *Cross-source themes*): TG
scores highest on the governance/safety/durability sources and lowest on the retrieval and
reasoning-diversity sources — a coherent shape, not noise.

## Per-source scorecard

Sorted by score. Counts are **I**mplemented / **P**artial / **G**ap / **N/A**-by-design.

| Source | Category | Score | I/P/G/N/A | One-line verdict |
|--------|----------|:-----:|:---------:|------------------|
| [OpenAI — Practical Guide to Building Agents](source-audits/openai-practical-guide-building-agents.md) | OpenAI | 4.5 | 10/1/0/1 | Exemplary safety-forward realization; the one real miss is the injection screen guarding output, not the untrusted input. |
| [Microsoft — Semantic Kernel](source-audits/microsoft-semantic-kernel.md) | Microsoft | 4.2 | 3/2/0/0 | Nails tool-count, native function-calling, reads-safe/writes-approved; partial only on OTel + a composable filter pipeline. |
| [Anthropic — Building Effective Agents](source-audits/anthropic-building-effective-agents.md) | Anthropic | 4.1 | 6/3/0/1 | Pattern progression met with governance the source only gestures at; routing/parallelization decided-not-branched. |
| [LangChain — LangGraph Platform GA](source-audits/langchain-langgraph-platform-ga.md) | LangChain | 4.0 | 3/2/0/0 | Temporal substrate meets/exceeds LangGraph durability + HITL; orchestrator-worker split & continue-as-new unbuilt. |
| [ReAct (Reason + Act)](source-audits/react-reason-act.md) | Paper | 4.0 | 6/4/0/0 | Faithful, hardened ReAct that exceeds the paper on grounding; thin on the observable reasoning trace + self-consistency. |
| [Gulli — Agentic Design Patterns (21)](source-audits/gulli-agentic-design-patterns.md) | Book | 3.9 | 12/7/0/2 | Strong on the safety/governance half; falls short on the intelligence-amplifier half (RAG, reasoning techniques, MCP fleet). |
| [Microsoft — Agent Framework 1.0](source-audits/microsoft-agent-framework-1-0.md) | Microsoft | 3.9 | 2/3/0/0 | "If you can write a function, do that" is structurally enforced; memory is shallow-lexical and streaming is the weak spot. |
| [Claude Certified Architect (Foundations)](source-audits/cca-foundations.md) | Cert | 3.8 | 6/3/0/1 | Sub-agent principles made structural/binding; execclass routing decided-but-not-acted; sub-agent spec-vs-code gap. |
| [Anthropic — Harnesses for Long-Running Agents](source-audits/anthropic-harnesses-long-running-agents.md) | Anthropic | 3.8 | 4/4/0/1 | Durable Temporal core beats home-grown harnesses; no per-turn checkpoint, continue-as-new is a doc overclaim. |
| [Anthropic — Building Agents with the Claude Agent SDK](source-audits/anthropic-agent-sdk.md) | Anthropic | 3.8 | 9/2/2/2 | gather→act→verify spine faithful and exceeds on verification; sub-agent + MCP scaffolded-not-exercised, retrieval lexical/empty. |
| [Anthropic — Writing Effective Tools for Agents](source-audits/anthropic-writing-effective-tools-for-agents.md) | Anthropic | 3.75 | 4/4/0/0 | Semantic IDs + fail-soft errors are load-bearing; no typed tool schema/enums, sealed holdout still planned. |
| [Google — 5-Day AI Agents Intensive](source-audits/google-5day-ai-agents.md) | Course | 3.7 | 4/4/0/0 | Harness-first/spec-governed/human-circuit-breaker thesis met & exceeds predecessor; trajectory eval + Phase-2 cage unwired. |
| [LangChain — State of Agent Engineering 2026](source-audits/state-of-agent-engineering-2026.md) | LangChain | 3.7 | 9/2/2/0 | Quality-gates-production & eval machinery exceed the source; zero OTel tracing, no cost/latency/token metrics. |
| [LangChain — Framework Docs](source-audits/langchain-framework-docs.md) | LangChain | 3.6 | 9/5/2/2 | First-class context engineering + safety HITL; misses context-editing, tool-retry, tool-output redaction, dynamic tool filtering. |
| [Google — AI Agent Trends 2026](source-audits/google-ai-agent-trends-2026.md) | Google | 3.5 | 4/2/1/0 | Reversibility-as-gate + idempotent durable checkpointing strong; no atomic multi-step transaction/rollback coordinator (G7). |
| [OpenAI — Evals Guide](source-audits/openai-evals-guide.md) | OpenAI | 3.4 | 3/7/1/0 | Durable production judge + Welch flywheel + shuffled-null control exceed the guide; thin on day-two eval ops (CI gate, holdout, calibration). |
| [Chain-of-Thought](source-audits/chain-of-thought.md) | Paper | 3.4 | 3/4/2/1 | INV-08 half exemplary + strong auditability; JSON-only contract compresses explicit reasoning, advanced reasoning family absent. |
| [NVIDIA DLI — RAG / Agents / Data Flywheel](source-audits/nvidia-dli-rag-agents-flywheel.md) | Course | 3.2 | 11/4/5/1 | Excellent on the flywheel/guardrail half; the RAG-deck half (hybrid + rerank + fusion + HyDE) is a deliberate regression. |
| [Google — Choose Agentic Architecture Components](source-audits/google-choose-agentic-architecture-components.md) | Google | 3.1 | 5/2/2/1 | Runtime selection (Temporal) beats predecessor sprawl; no semantic memory, no per-incident complexity routing (G11). |
| [Iusztin & Labonne — LLM Engineer's Handbook](source-audits/llm-engineers-handbook.md) | Book | 3.0 | 6/8/5/3 | Exemplary governance/LLMOps discipline; the entire RAG feature+inference pipeline is unbuilt in code, full-trace monitoring unwired. |
| [Google — Agent2Agent (A2A) Protocol](source-audits/google-a2a-protocol.md) | Google | 2.8 | 3/2/2/1 | Task-lifecycle substance (manifest chain, hash ledger, HITL) strong; zero interop artifacts, CONSTITUTION §4.14 honesty gap. |
| [Anthropic — Effective Context Engineering](source-audits/anthropic-context-engineering.md) | Anthropic | 2.7 | 3/2/2/0 | Durable memory + prompt layering strong; retrieval is recall-hostile lexical top-3, no compaction, no token budget. |
| [Anthropic — Equipping Agents with Agent Skills](source-audits/anthropic-agent-skills.md) | Anthropic | 2.5 | 1/1/1/2 | Skill composition meets/exceeds source; SKILL.md 1:1 mapping asserted-not-implemented, progressive disclosure only coarse, no console panel. |
| [Anthropic — Advanced Tool Use](source-audits/anthropic-advanced-tool-use.md) | Anthropic | 2.5 | 0/2/1/1 | Achieves the token-efficiency goal by other means; neither Tool-Search/deferred loading nor parallel tool calling exists. |
| [Reflexion](source-audits/reflexion.md) | Paper | 2.3 | 1/4/2/1 | Strong adjacent memory + Generator-Critic; fundamentally non-reflective — no verbal self-reflection, learns only from clean successes. |
| [Agentic RAG Survey (2501.09136)](source-audits/agentic-rag-survey-2501-09136.md) | Paper | 2.3 | 0/3/3/0 | Estate causal graph satisfies GraphRAG for topology; precedent corpus is thin/lexical/disjoint — CRAG/HyDE/self-correct absent. |
| [OpenAI — Retrieval Guide](source-audits/openai-retrieval-guide.md) | OpenAI | 2.1 | 1/3/2/1 | RAG-security row exceeds the guide; the 5-signal RRF/pgvector/rerank/rewrite pipeline the docs promise is unbuilt. |
| [Tree-of-Thoughts](source-audits/tree-of-thoughts.md) | Paper | 2.0 | 2/3/3/0 | Single-path fail-closed loop meets the core of ToT only offline (prompt flywheel); no in-loop branching/lookahead/backtrack. |
| [Self-Consistency](source-audits/self-consistency.md) | Paper | 2.0 | 1/3/2/0 | Robustness intent met by deterministic/independent-check substitutes; the actual technique (N samples + majority vote) absent. |
| [Anthropic — Code Execution with MCP](source-audits/anthropic-code-execution-with-mcp.md) | Anthropic | 1.5 | 1/3/2/1 | Architecturally the anti-pattern this source contrasts against (INV-08 excludes code-as-control-flow, N/A); round-trips/tool-scale weak. |

## Cross-source themes

### Recurring gaps (ranked by how many independent sources raised them)

The value of a 30-source audit is convergence: when unrelated sources fault the same thing, it is a real
gap, not one reviewer's hobby-horse. In descending recurrence:

1. **The semantic/vector RAG plane is provisioned but unbuilt** — flagged by **9** sources (Gulli GAP-1,
   LLM Engineer's Handbook, NVIDIA DLI, Context Engineering, Agent SDK, OpenAI Retrieval, Google
   Choose-Arch, MS Agent Framework, Agentic RAG Survey). pgvector is in the deploy stack and ADR-0003 +
   ARCHITECTURE.md promise embeddings + 5-signal RRF, but retrieval is a single-pass lexical top-3 and
   **no migration ever creates a vector column** (SYSTEM-MAP.md:92,115 concedes it). This is the single
   most-cited gap in the whole audit and the predecessor's own GAP-1.
2. **Advanced reasoning is absent** (Tree-of-Thought / self-consistency / step-back / multi-hypothesis /
   adaptive thinking budget) — **7** sources (ToT, Self-Consistency, CoT, Gulli, ReAct, Building
   Effective Agents, Choose-Arch). TG runs one fail-closed ReAct path and consumes only `Choices[0]`.
   Sharpened by an honesty gap: **CONSTITUTION.md:116 promises self-consistency/hedging flagging that no
   code delivers.**
3. **Execution-class routing is decided but does not act** — **6** sources (CCA, Building Effective
   Agents, Choose-Arch G11, LangGraph, CoT, Gulli). `execclass.Classify` picks 5 classes but only steers
   skill composition; `SkipsAgent`/`NeedsDeepContext` are unwired, the agent model is hard-coded `fast`,
   and there is no thinking-budget dial. Wiring it unlocks both the cheap-path payoff and per-severity
   reasoning depth.
4. **Eval does not yet gate deploys** (no CI eval gate on prompt/skill/model/code changes; sealed holdout
   + overfitting gate protected-by-construction but never run; no negative-control incidents; no judge
   calibration split) — **6** sources (OpenAI Evals, State of Agent Eng, OpenAI Practical, Writing Tools,
   LLM Handbook, Google 5-Day). *MEMORY records this as a hard user requirement: the scorecard is the
   admission gate.*
5. **No OpenTelemetry / distributed tracing and no per-session cost/token/latency/tool metrics** — **6**
   sources (State of Agent Eng, Semantic Kernel, Google 5-Day, LLM Handbook, MS Agent Framework, Code
   Execution-MCP). `ExportSpans` has no live caller, the gateway discards the usage block, and the LLM
   judge grades the session *record*, not the ordered tool *trajectory*.
6. **Context compaction + per-turn durability in the ReAct loop** — **6** sources (Context Engineering,
   Agent SDK, Harnesses, Code Execution-MCP, LangChain Docs, CCA). Observations append verbatim, uncapped
   to the cycle limit; the whole loop runs in one non-durable activity (a mid-investigation crash
   re-runs it, not resume-from-turn-N).
7. **The sub-agent / A2A spec-vs-code honesty gap** — **4** sources (CCA, Agent SDK, A2A, MS Agent
   Framework). CONSTITUTION §4.14 and ARCHITECTURE claim a manager-with-read-only-sub-agents
   DEEP_INVESTIGATION pattern (and a "capability-card contract is kept") that `agent/loop.go` does not
   build — either build the binding depth-1 delegation or downgrade the claim.
8. **Per-session token/cost budget ceiling** — **5** sources (Gulli, Context Engineering,
   Code Execution-MCP, Building Effective Agents, LLM Handbook). The model client sets no token budget at
   all and no daily cost cap exists.
9. **Parallel / batched read-only tool dispatch + child-workflow fan-out** — **5** sources (Gulli, Code
   Execution-MCP, Advanced Tool Use, Building Effective Agents, LangGraph). The loop is strictly
   one-tool-per-turn, collapsing multi-host cascade investigations to N sequential round-trips.
10. **Screen untrusted INPUT for prompt-injection** (not just the agent's own proposal output) — **2**
    sources but **high-severity** because it is the *only* material miss on the top-scoring source
    (OpenAI Practical, 4.5) plus NVIDIA DLI. `core/screen` is high-quality but wired to the wrong surface.

Lower-recurrence-but-real: advanced-RAG stages + a retrieval-quality (RAGAS-analog) eval (5),
structured tool-calling schema/enums + deferred tool surfacing (3), persist a per-step reasoning trace +
ordered trajectory (2–3), SKILL.md import/export + progressive disclosure + skill console (2), live
read-only MCP client consumed in the loop (2), the Phase-2 safety cage + atomic transaction/undo
coordinator (2). Genuinely single-source but distinctive: Reflexion's verbal self-reflection loop, and
the GraphRAG precedent↔estate linkage (unbuildable today because the two planes are disjoint).

### Where TG demonstrably exceeds the predecessor's own posture

The audit is not one-sided — on the safety-critical spine TG repeatedly *beats* both the sources and the
battle-tested predecessor it inherits from:

- **The skill/prompt graduation flywheel is real code with a Welch t-test + offline admission gate +
  post-graduation auto-rollback regression watch** — where the predecessor's pipeline was dark/starved
  and completed **0 trials** (NVIDIA, Gulli, Building Effective Agents, OpenAI Evals, State of Agent Eng).
- **INV-08 is structural, not conventional** — "no model token becomes control flow" makes the entire
  injection/bypass/drift class *uncompilable*, rather than blocked by command blocklists the predecessor
  relied on (CoT, OpenAI Practical, Semantic Kernel, Google 5-Day).
- **Verification is first-class**: a committed falsifiable prediction diffed by a deterministic
  sole-author verifier the acting model may never self-adjudicate (INV-10), plus a citation gate
  (REQ-1007) that operationalizes anti-hallucination as a correctness check — stronger than the sources'
  `check_evidence` patterns (Agent SDK, Gulli, Writing Tools, NVIDIA).
- **The degree-preserving shuffled-graph falsifiability control (INV-22)** is a rigorous statistical
  negative control stronger than the guide's "should-not-trigger" notion (OpenAI Evals), and TG added
  judge-liveness + frontier drift/death cross-checks the predecessor only bolted on late (Gulli, State of
  Agent Eng).
- **Durable execution beats home-grown harnesses and vanilla LangGraph**: the Runner *is* a Temporal
  workflow — state survives crashes/deploys, POLL_PAUSE waits on a durable 24h timer + signal, histories
  are version-guarded — and human approval is an action-id-bound, deny-by-default, DoS-bounded signal
  wait stricter than a LangGraph interrupt (Harnesses, LangGraph, MS Agent Framework).
- **Memory hygiene**: the learned corpus admits only confirmed-clean sessions, refusing to learn from
  deviations/partials — hygiene the sources recommend but rarely enforce (Context Engineering, Reflexion).
- **The A2A de-scope is live-vindicated**: the predecessor's own A2A bus was proven dead
  (`a2a_task_log` = 53 completion-only rows abandoned after two weeks), so single-native-agent is a
  decision backed by evidence, not an omission (A2A).
- **The spec lattice governs TG's own safety-critical code** via a CI-enforced hash lockstep — the
  predecessor had no spec for itself (Google 5-Day).

### The shape of the result

TG's adherence curve is coherent and self-consistent with its thesis: it is **strongest on the sources
about building a *safe, governed, durable* agent** (OpenAI Practical 4.5, Semantic Kernel 4.2, Building
Effective Agents 4.1, LangGraph 4.0, MS Agent Framework 3.9, Gulli 3.9) and **weakest on the sources
about *retrieval quality* and *reasoning diversity*** (Code Execution-MCP 1.5, ToT 2.0, Self-Consistency
2.0, OpenAI Retrieval 2.1, Reflexion 2.3, Agentic RAG 2.3). Two of those weak clusters are deliberate and
defensible (code-as-control-flow and inter-agent federation are excluded by INV-08 and the single-org
topology, correctly scored N/A-by-design), but **two are genuine, high-leverage debt the moat does not
justify**: the unbuilt semantic-retrieval plane (the docs write a cheque the code doesn't cash) and the
absent reasoning-diversity family (partly an outright honesty gap against CONSTITUTION.md:116). Closing
those two, plus turning the eval scorecard into a real deploy gate and adding OTel observability, is what
moves the aggregate from 3.35 toward the safety-cluster's own 4.0+.

## Remediation backlog

The ranked, de-duplicated backlog below is the output that matters — each item becomes a TG-38 child
issue on project **TG**. `recurring=true` marks a gap raised by 2+ independent sources (higher priority).
See the `rankedRemediations` payload accompanying this catalog.

| # | Severity | Recurring | Sources | Remediation |
|:-:|:--------:|:---------:|:-------:|-------------|
| 1 | high | yes (9) | 9 | Build the pgvector semantic-retrieval plane behind knowledge.Retriever (embedding write/read path + ANN index), replacing lexical top-3 |
| 2 | high | yes (2) | 2 | Wire the prompt-injection/jailbreak screen onto untrusted INPUT (alert summary, tracker body, retrieved precedent) before it seeds the agent — not only the agent's own proposal output |
| 3 | high | yes (6) | 6 | Make execclass routing act: branch model tier + deep/fast context + the deterministic no-agent shortcut + a thinking/reasoning-effort budget, keyed on the selected class |
| 4 | high | yes (6) | 6 | Make 'eval gates deploys' real: a CI eval gate that runs the LLM-judge corpus (or a fixed mechanical grounding score) on prompt/skill/model/code changes, an operational sealed holdout with the >20pt overfitting deploy-gate, negative-control (should-NOT-act) incidents, and a judge calibration split with TPR/TNR floors |
| 5 | high | yes (5) | 5 | Adopt OpenTelemetry tracing + per-session cost/token/latency/tool metrics, and land trajectory-aware evaluation over the exported spans (judge the ordered tool path, with a deterministic trajectory veto that overrides the LLM judge) |
| 6 | high | yes (4) | 4 | Resolve the sub-agent / A2A spec-vs-code honesty gap: build binding depth-1 read-only DEEP_INVESTIGATION delegation (+ a minimal self-describing AgentCard / versioned handoff envelope), OR downgrade the CONSTITUTION §4.14 manager-pattern and 'capability-card contract' claims to the single-loop reality |
| 7 | high | yes (7) | 7 | Add the advanced-reasoning family gated by execclass/severity: self-consistency (N diverse samples + majority vote before the gate) and bounded multi-hypothesis/Tree-of-Thought + step-back, each branch typed under INV-08 — and deliver the CONSTITUTION.md:116 self-consistency/hedging flag that no code delivers |
| 8 | medium | yes (6) | 6 | Add recall-optimized context compaction/summarization to the ReAct loop and checkpoint it at cycle granularity (per-turn durable snapshot), so long investigations stay in budget and a mid-run crash resumes from turn-N instead of re-running the whole loop |
| 9 | medium | yes (5) | 5 | Enforce a per-session token/cost budget ceiling (+ daily cap) with plan-only degradation, and cap tool-output ingestion |
| 10 | medium | yes (5) | 5 | Add concurrent/batched read-only tool dispatch per directive (errgroup) and child-workflow fan-out for correlated incidents, to collapse multi-host cascade investigations from N sequential round-trips to 1 |
| 11 | medium | yes (5) | 5 | Build the advanced-RAG retrieval stages once the vector plane exists: query rewrite/multi-query + cross-encoder rerank + RRF hybrid fusion + a configurable min-relevance threshold + XML-delimited data blocks, plus a retrieval-quality (RAGAS-analog: context precision/recall) eval |
| 12 | medium | yes (2) | 2 | Capture and persist a per-step reasoning trace (optional typed untrusted `thought` DATA field, never dispatched) and the ordered trajectory step-sequence to the ledger, for audit, eval, and CoT/ReAct fidelity |
| 13 | medium | no | 1 | Add the Reflexion loop: generate + persist a verbal self-reflection over failed/escalated/deviated trajectories into a caution lane (not only confirmed-clean successes), and feed judge comments + failing sessions into the skill-flywheel generator instead of a numeric-only regression signal |
| 14 | medium | no | 1 | Graph-link the incident-precedent corpus into the estate causal graph (incident-knowledge GraphRAG) so blast-radius precedent — 'what past incidents hit the hosts in this alert's blast radius' — becomes a graph walk |
| 15 | medium | yes (3) | 3 | Surface typed per-tool parameter schemas + examples to the model (structured tool-calling) and constrain tool args + proposal op_class with enums at emission (poka-yoke, reusing the actuator's reversible-op allowlist); add relevance-scoped / deferred tool surfacing as the read-only catalog grows |
| 16 | medium | yes (2) | 2 | Implement SKILL.md import/export (YAML frontmatter <-> skill_version row) + the canonical `description` field to make ADR-0012's '1:1 mapping' real; add within-skill progressive disclosure (always-load a trigger line, defer the 8KiB body), a console skill-library panel (spec/014 REQ-1313), and a skill router / trust-tiers / inspector / collision eval |
| 17 | low | yes (2) | 2 | Wire a live read-only vendor MCP client (e.g. NetBox) into the agent loop behind the pinned-hash interceptor, so MCP is exercised as a consumed investigation tool rather than a disabled registry scaffold |
| 18 | low | no | 3 | Runtime hardening cluster: extend PII/credential redaction to model-bound tool outputs + the audit ledger (today only outbound human notices), add tool-retry with exponential backoff+jitter for transient read-only failures, add NeMo-style semantic topic rails on intermediate steps, and stream in-flight triage sessions (SSE of cycles/observations/proposal) to the console |
| 19 | medium | yes (2) | 2 | Phase-2 governed-autonomy prerequisites: the context-as-perimeter cage (egress/exfil allowlist + JIT/ephemeral downscoped credentials with zero ambient authority + zone-based egress), an atomic multi-step transaction coordinator (all-or-nothing plan execution + compensation on partial failure, predecessor G7), an applied undo executor that consumes the recorded execution_log inverse, and pre-mutation state capture |

### Backlog detail

#### R1 — Build the pgvector semantic-retrieval plane behind knowledge.Retriever (embedding write/read path + ANN index), replacing lexical top-3

*Severity: **high** · recurring · 9 source(s).*

**Sources:** [`gulli-agentic-design-patterns`](source-audits/gulli-agentic-design-patterns.md), [`llm-engineers-handbook`](source-audits/llm-engineers-handbook.md), [`nvidia-dli-rag-agents-flywheel`](source-audits/nvidia-dli-rag-agents-flywheel.md), [`anthropic-context-engineering`](source-audits/anthropic-context-engineering.md), [`anthropic-agent-sdk`](source-audits/anthropic-agent-sdk.md), [`openai-retrieval-guide`](source-audits/openai-retrieval-guide.md), [`google-choose-agentic-architecture-components`](source-audits/google-choose-agentic-architecture-components.md), [`microsoft-agent-framework-1-0`](source-audits/microsoft-agent-framework-1-0.md), [`agentic-rag-survey-2501-09136`](source-audits/agentic-rag-survey-2501-09136.md)

The single most-cited gap in the audit (9 independent sources) and the predecessor's own GAP-1. pgvector is provisioned in deploy and ADR-0003/ARCHITECTURE.md promise embeddings + 5-signal RRF, but no migration creates a vector column and retrieval is a recall-hostile single-pass lexical top-3 (SYSTEM-MAP.md:92,115 concedes it). The docs write a cheque the code does not cash; this is the highest-leverage capability fix and unblocks items 13 and 14.

#### R2 — Wire the prompt-injection/jailbreak screen onto untrusted INPUT (alert summary, tracker body, retrieved precedent) before it seeds the agent — not only the agent's own proposal output

*Severity: **high** · recurring · 2 source(s).*

**Sources:** [`openai-practical-guide-building-agents`](source-audits/openai-practical-guide-building-agents.md), [`nvidia-dli-rag-agents-flywheel`](source-audits/nvidia-dli-rag-agents-flywheel.md)

The one material miss on the top-scoring source (OpenAI Practical, 4.5) and flagged again by NVIDIA DLI. core/screen is a high-quality 5-category detector with homoglyph/zero-width folding, but it runs on the wrong surface — the outgoing proposal instead of the untrusted text it was built to protect. Low effort, closes a real injection surface; a security fix, so it outranks larger-but-lower-severity items.

#### R3 — Make execclass routing act: branch model tier + deep/fast context + the deterministic no-agent shortcut + a thinking/reasoning-effort budget, keyed on the selected class

*Severity: **high** · recurring · 6 source(s).*

**Sources:** [`cca-foundations`](source-audits/cca-foundations.md), [`anthropic-building-effective-agents`](source-audits/anthropic-building-effective-agents.md), [`google-choose-agentic-architecture-components`](source-audits/google-choose-agentic-architecture-components.md), [`langchain-langgraph-platform-ga`](source-audits/langchain-langgraph-platform-ga.md), [`chain-of-thought`](source-audits/chain-of-thought.md), [`gulli-agentic-design-patterns`](source-audits/gulli-agentic-design-patterns.md)

6 sources fault the same decided-but-dead machinery: execclass.Classify picks 5 classes but only steers skill composition; SkipsAgent/NeedsDeepContext are unwired, the agent model is hard-coded 'fast', and there is no thinking-budget dial (predecessor gap G11). Wiring it delivers both the cheap-incident simplicity payoff and per-severity reasoning depth, and is a prerequisite for items 7 and 9.

#### R4 — Make 'eval gates deploys' real: a CI eval gate that runs the LLM-judge corpus (or a fixed mechanical grounding score) on prompt/skill/model/code changes, an operational sealed holdout with the >20pt overfitting deploy-gate, negative-control (should-NOT-act) incidents, and a judge calibration split with TPR/TNR floors

*Severity: **high** · recurring · 6 source(s).*

**Sources:** [`openai-evals-guide`](source-audits/openai-evals-guide.md), [`state-of-agent-engineering-2026`](source-audits/state-of-agent-engineering-2026.md), [`openai-practical-guide-building-agents`](source-audits/openai-practical-guide-building-agents.md), [`anthropic-writing-effective-tools-for-agents`](source-audits/anthropic-writing-effective-tools-for-agents.md), [`llm-engineers-handbook`](source-audits/llm-engineers-handbook.md), [`google-5day-ai-agents`](source-audits/google-5day-ai-agents.md)

6 sources, and MEMORY records this as a hard user requirement (the scorecard is the admission gate; the order was corrected twice). Today the corpus eval runs manually on-box, asserts no quality bar, the sealed holdout is protected-by-construction but never run, there are zero negative-control incidents, and the judge has no calibration. Turning the differentiator scorecard into a binding gate is core to the moat.

#### R5 — Adopt OpenTelemetry tracing + per-session cost/token/latency/tool metrics, and land trajectory-aware evaluation over the exported spans (judge the ordered tool path, with a deterministic trajectory veto that overrides the LLM judge)

*Severity: **high** · recurring · 5 source(s).*

**Sources:** [`state-of-agent-engineering-2026`](source-audits/state-of-agent-engineering-2026.md), [`microsoft-semantic-kernel`](source-audits/microsoft-semantic-kernel.md), [`google-5day-ai-agents`](source-audits/google-5day-ai-agents.md), [`llm-engineers-handbook`](source-audits/llm-engineers-handbook.md), [`microsoft-agent-framework-1-0`](source-audits/microsoft-agent-framework-1-0.md)

5-6 sources; State of Agent Eng quantifies that 94% of prod agents are instrumented and TG has none. ExportSpans has no live caller, the gateway discards the usage block, /metrics exposes only the gate+breaker, and the judge grades the session record rather than the ordered tool trajectory. Observability is a Phase-2-readiness prerequisite and makes any cost/quality claim measurable.

#### R6 — Resolve the sub-agent / A2A spec-vs-code honesty gap: build binding depth-1 read-only DEEP_INVESTIGATION delegation (+ a minimal self-describing AgentCard / versioned handoff envelope), OR downgrade the CONSTITUTION §4.14 manager-pattern and 'capability-card contract' claims to the single-loop reality

*Severity: **high** · recurring · 4 source(s).*

**Sources:** [`cca-foundations`](source-audits/cca-foundations.md), [`anthropic-agent-sdk`](source-audits/anthropic-agent-sdk.md), [`google-a2a-protocol`](source-audits/google-a2a-protocol.md), [`microsoft-agent-framework-1-0`](source-audits/microsoft-agent-framework-1-0.md)

4 sources independently flag that CONSTITUTION §4.14 + ARCHITECTURE + agent/README.md claim a manager-with-read-only-sub-agents pattern (and an inter-agent capability-card contract) that agent/loop.go does not build. A false claim in the constitution is a credibility liability that undermines the honesty-over-marketing build culture; either build it or downgrade the claim.

#### R7 — Add the advanced-reasoning family gated by execclass/severity: self-consistency (N diverse samples + majority vote before the gate) and bounded multi-hypothesis/Tree-of-Thought + step-back, each branch typed under INV-08 — and deliver the CONSTITUTION.md:116 self-consistency/hedging flag that no code delivers

*Severity: **high** · recurring · 7 source(s).*

**Sources:** [`tree-of-thoughts`](source-audits/tree-of-thoughts.md), [`self-consistency`](source-audits/self-consistency.md), [`chain-of-thought`](source-audits/chain-of-thought.md), [`gulli-agentic-design-patterns`](source-audits/gulli-agentic-design-patterns.md), [`react-reason-act`](source-audits/react-reason-act.md), [`anthropic-building-effective-agents`](source-audits/anthropic-building-effective-agents.md), [`google-choose-agentic-architecture-components`](source-audits/google-choose-agentic-architecture-components.md)

7 sources; TG runs one fail-closed ReAct path and consumes only Choices[0]. Sharpened into an honesty gap because CONSTITUTION.md:116 already promises self-consistency/hedging-mismatch flagging that no code delivers. Scope to high-severity/high-ambiguity incidents (INV-08-safe orchestrator-driven branching + mechanical selection) and pair with item 3's thinking budget; add only where an eval shows it improves triage outcomes.

#### R8 — Add recall-optimized context compaction/summarization to the ReAct loop and checkpoint it at cycle granularity (per-turn durable snapshot), so long investigations stay in budget and a mid-run crash resumes from turn-N instead of re-running the whole loop

*Severity: **medium** · recurring · 6 source(s).*

**Sources:** [`anthropic-context-engineering`](source-audits/anthropic-context-engineering.md), [`anthropic-agent-sdk`](source-audits/anthropic-agent-sdk.md), [`anthropic-harnesses-long-running-agents`](source-audits/anthropic-harnesses-long-running-agents.md), [`anthropic-code-execution-with-mcp`](source-audits/anthropic-code-execution-with-mcp.md), [`langchain-framework-docs`](source-audits/langchain-framework-docs.md), [`cca-foundations`](source-audits/cca-foundations.md)

6 sources. Observations append verbatim and uncapped to the 10-cycle limit, and the entire loop runs inside one non-durable activity (a crash re-runs it, not resume-from-turn-N). Also reconcile the continue-as-new / 'per-turn snapshots' doc overclaim (EXTERNAL-AUDIT-LESSONS.md:69, TESTING-AND-BENCHMARK.md:175). Largely masked by bounded triage today but load-bearing for DeepInvestigation and Phase-2.

#### R9 — Enforce a per-session token/cost budget ceiling (+ daily cap) with plan-only degradation, and cap tool-output ingestion

*Severity: **medium** · recurring · 5 source(s).*

**Sources:** [`gulli-agentic-design-patterns`](source-audits/gulli-agentic-design-patterns.md), [`anthropic-context-engineering`](source-audits/anthropic-context-engineering.md), [`anthropic-code-execution-with-mcp`](source-audits/anthropic-code-execution-with-mcp.md), [`anthropic-building-effective-agents`](source-audits/anthropic-building-effective-agents.md), [`llm-engineers-handbook`](source-audits/llm-engineers-handbook.md)

5 sources; the model client sets no token budget at all and the gateway ignores the usage block, so cost is unbounded and unmeasured. Depends on item 5's token accounting to enforce; a guardrail (not just observability) that also caps a runaway high-severity investigation.

#### R10 — Add concurrent/batched read-only tool dispatch per directive (errgroup) and child-workflow fan-out for correlated incidents, to collapse multi-host cascade investigations from N sequential round-trips to 1

*Severity: **medium** · recurring · 5 source(s).*

**Sources:** [`gulli-agentic-design-patterns`](source-audits/gulli-agentic-design-patterns.md), [`anthropic-code-execution-with-mcp`](source-audits/anthropic-code-execution-with-mcp.md), [`anthropic-advanced-tool-use`](source-audits/anthropic-advanced-tool-use.md), [`anthropic-building-effective-agents`](source-audits/anthropic-building-effective-agents.md), [`langchain-langgraph-platform-ga`](source-audits/langchain-langgraph-platform-ga.md)

5 sources; the loop is strictly one-tool-per-turn. Low-severity today because the agent-facing catalog is only 4 tools, but it is the right structural fix as the read-only catalog and blast-radius investigations grow, and pairs with LangGraph's orchestrator-worker fan-out.

#### R11 — Build the advanced-RAG retrieval stages once the vector plane exists: query rewrite/multi-query + cross-encoder rerank + RRF hybrid fusion + a configurable min-relevance threshold + XML-delimited data blocks, plus a retrieval-quality (RAGAS-analog: context precision/recall) eval

*Severity: **medium** · recurring · 5 source(s).*

**Sources:** [`llm-engineers-handbook`](source-audits/llm-engineers-handbook.md), [`nvidia-dli-rag-agents-flywheel`](source-audits/nvidia-dli-rag-agents-flywheel.md), [`openai-retrieval-guide`](source-audits/openai-retrieval-guide.md), [`agentic-rag-survey-2501-09136`](source-audits/agentic-rag-survey-2501-09136.md), [`openai-evals-guide`](source-audits/openai-evals-guide.md)

5 sources; the architecture already commits to these stages (all of which the predecessor HAD) but the code has a trivial score>0 floor, plain-text delimiters, and no rerank/rewrite/fusion or retrieval eval. Strictly downstream of item 1 (needs the embedding channel first), so sequence it after the vector plane lands.

#### R12 — Capture and persist a per-step reasoning trace (optional typed untrusted `thought` DATA field, never dispatched) and the ordered trajectory step-sequence to the ledger, for audit, eval, and CoT/ReAct fidelity

*Severity: **medium** · recurring · 2 source(s).*

**Sources:** [`react-reason-act`](source-audits/react-reason-act.md), [`chain-of-thought`](source-audits/chain-of-thought.md)

The JSON-only directive contract suppresses the explicit interleaved reasoning trace (no thought field parsed/persisted) and the computed trajectory is used only for the loop-veto, never persisted. Adding an untrusted thought field stays INV-08-safe (data, never control flow), closes the core CoT/ReAct gap, and is a prerequisite for trajectory scoring in item 5.

#### R13 — Add the Reflexion loop: generate + persist a verbal self-reflection over failed/escalated/deviated trajectories into a caution lane (not only confirmed-clean successes), and feed judge comments + failing sessions into the skill-flywheel generator instead of a numeric-only regression signal

*Severity: **medium** · non-recurring · 1 source(s).*

**Sources:** [`reflexion`](source-audits/reflexion.md)

Single-source but a whole coherent low-scoring theme: TG is fundamentally non-reflective — it learns only from confirmed-clean successes and the critic's verbal feedback dead-ends because the flywheel generator is driven by a numeric regression signal alone. Must be built carefully to avoid corpus poisoning (a separate caution lane, not the learned-match tier), which is exactly why the hygiene gate makes this non-trivial.

#### R14 — Graph-link the incident-precedent corpus into the estate causal graph (incident-knowledge GraphRAG) so blast-radius precedent — 'what past incidents hit the hosts in this alert's blast radius' — becomes a graph walk

*Severity: **medium** · non-recurring · 1 source(s).*

**Sources:** [`agentic-rag-survey-2501-09136`](source-audits/agentic-rag-survey-2501-09136.md)

Single-source but distinctive and high-value: TG already has a first-class entity-relation estate graph (satisfying GraphRAG for topology) and a precedent corpus, but the two planes are disjoint, making the highest-value GraphRAG query unbuildable. Unlike generic RAG advice this exploits an asset TG uniquely has; sequence after item 1.

#### R15 — Surface typed per-tool parameter schemas + examples to the model (structured tool-calling) and constrain tool args + proposal op_class with enums at emission (poka-yoke, reusing the actuator's reversible-op allowlist); add relevance-scoped / deferred tool surfacing as the read-only catalog grows

*Severity: **medium** · recurring · 3 source(s).*

**Sources:** [`anthropic-writing-effective-tools-for-agents`](source-audits/anthropic-writing-effective-tools-for-agents.md), [`anthropic-advanced-tool-use`](source-audits/anthropic-advanced-tool-use.md), [`anthropic-code-execution-with-mcp`](source-audits/anthropic-code-execution-with-mcp.md)

3 sources; tools are passed as a bare comma-list of names plus skill prose with free-text args (constrained only downstream). Constraining at emission is a correctness/safety win, and deferred loading keeps the preamble lean as the catalog grows past the current 4 tools.

#### R16 — Implement SKILL.md import/export (YAML frontmatter <-> skill_version row) + the canonical `description` field to make ADR-0012's '1:1 mapping' real; add within-skill progressive disclosure (always-load a trigger line, defer the 8KiB body), a console skill-library panel (spec/014 REQ-1313), and a skill router / trust-tiers / inspector / collision eval

*Severity: **medium** · recurring · 2 source(s).*

**Sources:** [`anthropic-agent-skills`](source-audits/anthropic-agent-skills.md), [`google-5day-ai-agents`](source-audits/google-5day-ai-agents.md)

2 sources; the skill store is genuinely strong on composition but ADR-0012 asserts a SKILL.md 1:1 mapping with no serializer/parser/export and no `description` key anywhere (an honesty gap), progressive disclosure is only a coarse whole-body analog, and the versioned library/rationale log is backend-only with no frontend panel.

#### R17 — Wire a live read-only vendor MCP client (e.g. NetBox) into the agent loop behind the pinned-hash interceptor, so MCP is exercised as a consumed investigation tool rather than a disabled registry scaffold

*Severity: **low** · recurring · 2 source(s).*

**Sources:** [`gulli-agentic-design-patterns`](source-audits/gulli-agentic-design-patterns.md), [`anthropic-agent-sdk`](source-audits/anthropic-agent-sdk.md)

2 sources; MCP is scaffolded + security-designed (ADR-0012, interceptor, pinned hashes) but disabled in Phase 0/1, so no MCP tool is exercised. Consuming a read-only vendor server (never executables, per the adopt-format/re-author-content strategy) broadens investigation reach without violating INV-08.

#### R18 — Runtime hardening cluster: extend PII/credential redaction to model-bound tool outputs + the audit ledger (today only outbound human notices), add tool-retry with exponential backoff+jitter for transient read-only failures, add NeMo-style semantic topic rails on intermediate steps, and stream in-flight triage sessions (SSE of cycles/observations/proposal) to the console

*Severity: **low** · non-recurring · 3 source(s).*

**Sources:** [`langchain-framework-docs`](source-audits/langchain-framework-docs.md), [`nvidia-dli-rag-agents-flywheel`](source-audits/nvidia-dli-rag-agents-flywheel.md), [`microsoft-agent-framework-1-0`](source-audits/microsoft-agent-framework-1-0.md)

Distinct single-source robustness items grouped to avoid backlog bloat: redaction currently scrubs only outbound notices (leak surface on tool outputs/ledger), any tool error fails closed with no retry, there are no in-distribution rails on intermediate steps, and you cannot watch a triage session unfold (only aggregate posture). Each is bounded and independent.

#### R19 — Phase-2 governed-autonomy prerequisites: the context-as-perimeter cage (egress/exfil allowlist + JIT/ephemeral downscoped credentials with zero ambient authority + zone-based egress), an atomic multi-step transaction coordinator (all-or-nothing plan execution + compensation on partial failure, predecessor G7), an applied undo executor that consumes the recorded execution_log inverse, and pre-mutation state capture

*Severity: **medium** · recurring · 2 source(s).*

**Sources:** [`google-5day-ai-agents`](source-audits/google-5day-ai-agents.md), [`google-ai-agent-trends-2026`](source-audits/google-ai-agent-trends-2026.md)

2 sources naming the mutating-runtime gaps that gate the Phase-2 flip: today each execute runs a single argv with no plan-level atomicity/rollback (G7 still open), the 'undo stack' is a recorded inverse no code ever applies, there is no pre-state capture, and no egress/JIT-credential cage exists. Not urgent while mutation is OFF, but these are the concrete blockers to a credible staged canary — track together so the flip is not attempted without them.

---

## Addendum 2026-07-19 — Source #31 (context-engineering audit · YT TG-69)

*Added after the original 30-source roll-up (Sources audited is now **31**). Remediation tracked in **YT TG-69**. The gaps below map to existing recurring-gap IDs where noted (crit 4→R15, crit 7→R8/R9, crit 6→R2, crit 2→R18) plus three new items (entailment-grade grounding, skill-prose↔wire-format reconciliation, externalize/version the role prompt).*

## Source #31 — Bousetouane, *AI Agents Do Not Fail Alone: The Context Fails First* (arXiv 2607.14275v1)

*Category: Paper. Score: **4.0 / 5** (mean of 7 criteria). Audited 2026-07-19 against Table 1 / §3.2 definitions (pp. 10–14), scoring TG's actual Go code + prompts + config on the paper's own 0–10 criteria mapped to this catalog's 0–5 scale.*

> **Thesis:** context-engineering quality is a *measurable leading indicator* of agent reliability, scored on seven criteria kept ISOLATED from behavioral metrics (Q_CE(X) ∉ B(A,X)) so it can be validated as a predictor, not a tautology. Criterion→behavior map (Table 2): grounding→hallucination resistance, guardrails→manipulation resistance, instruction consistency→instruction following, tool-schema→tool-use reliability. Hardened context = escalation behavior + injection separation + confirm-before-mutate. This source measures TG's own core thesis, so a high score is expected — and TG posts its **highest catalog score to date on the safety/grounding half while its two lowest criteria (tool-schema, token efficiency) match the recurring catalog weaknesses.**

**Per-criterion scorecard** (score / verdict):

| # | Criterion | Score | Verdict |
|:-:|-----------|:-----:|---------|
| 1 | Role clarity | 5 | Textbook read-only preamble + typed terminal-state enum (loop.go:26,66-92); role is hardcoded in Go, not a versioned prompt asset. |
| 2 | Guardrail coverage | 5 | Fail-closed bands + unexported immutable never-auto floor + mutation-OFF-behind-preflight + single interceptor chokepoint w/ confirm-before-mutate (safety.go:22-104, interceptor.go:158-285); PII is a comment, not an instructed policy. |
| 3 | Instruction consistency | 4 | Law-level precedence + single ParseProposal grammar (CONSTITUTION.md:23-30, parse.go:40-59); ported prose chatops skills conflict with the strict JSON/read-only contract (loop.go:16 — 0% proposals in an eval). |
| 4 | Tool schema quality | 3 | Outstanding structural least-privilege; model-facing schema is just tool NAMES — no Description/typed/enum args (untyped map[string]string), lenient argMap parser is the smoking gun (tools.go:21-25, loop.go:119-147). |
| 5 | Grounding sufficiency | 4 | TG's strongest area — real connectors + dual-channel lexical+pgvector RRF + enforced four-axis evidence gate; presence-not-entailment (one citation suffices), substring TargetRelevant, no-tools-no-gate hole. |
| 6 | Injection hardening | 4 | Excellent DATA-not-instructions separation + structural INV-08; brittle closed English+Greek regex screen (Cyrillic-only homoglyph fold) + unscreened tool-OUTPUT channel (loop.go:269). |
| 7 | Token efficiency | 3 | Good relevance-sizing; ZERO token accounting, no history compaction across ≤10 monotonic cycles, 1 MiB per-observation ceiling with no global loop cap. |

**Recurring-gap linkage:** crit 4 → R15 (typed tool schemas + enums); crit 7 → R8/R9 (compaction + token budget + tool-output cap); crit 6 → R2 (screen untrusted INPUT — extend to tool OUTPUTS + de-brittle detector); crit 5 → new (entailment-grade grounding: close one-citation + substring + no-tools-no-gate holes); crit 3 → new (rewrite ported skill prose to the wire format); crit 2 → R18 (redact model-bound tool outputs + ledger, instruct PII policy); crit 1 → new (externalize/version the role prompt).

**One-line verdict:** *Nails the paper's safety/grounding/injection thesis with mechanical, defense-in-depth enforcement that exceeds the predecessor's posture; the two mechanical-efficiency criteria — tool-schema formalization and token discipline — are the same recurring catalog weaknesses and are where the ceiling-lift is.*
