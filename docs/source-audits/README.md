# Source-benchmark audits — TG-38

> **[O] audit-overlay.** This folder holds the per-source audits behind the [Source-Benchmark Catalog](../SOURCE-BENCHMARK-CATALOG.md) (TG-38, compiled 2026-07-18). Territory Grounder was benchmarked against **30 external knowledge sources** — books, courses, certifications, vendor engineering guides, and foundational papers — each scored 0–5. Aggregate adherence: **3.3 / 5**.

## Methodology

Every audit follows the same four steps, and scores TG's **actual code and live deployment** — never its own documentation's claims:

1. **Read the source.** Extract its named practices / best-practices / prescribed patterns.
2. **Map each advice to TG's implementation.** Find the concrete code, spec, or deployment surface that does (or does not) realize it — with a `file:line` evidence reference.
3. **Score the gap.** Classify each practice (see legend) and assign the source a 0–5 adherence score, weighted toward the practices that actually apply to TG's design.
4. **Remediate.** Record what it would take to close each gap; the recurring gaps roll up into the catalog's de-duplicated [remediation backlog](../SOURCE-BENCHMARK-CATALOG.md#remediation-backlog) (the TG-38 child issues).

## Status legend

| Status | Meaning |
|--------|---------|
| **implemented** | TG realizes the practice in shipping code / spec / deployment. |
| **partial** | Present in spirit or scaffolded, but incomplete, shallow, or wired to the wrong surface. |
| **N/A-by-design** | Deliberately excluded by a TG invariant or its single-org topology (e.g. INV-08 excludes code-as-control-flow); scored out of the denominator, not counted as a gap. |
| **gap** | Applicable to TG, recommended by the source, and genuinely absent. |

Scores cluster by theme: TG is strongest on the **safety / governance / durability** sources and weakest on the **retrieval-quality** and **reasoning-diversity** sources — a coherent shape, detailed in the catalog's *Cross-source themes* section.

## Index

All sources, grouped by category and sorted by adherence score (high to low).

| Source | Category | Score | I/P/G/N/A |
|--------|----------|:-----:|:---------:|
| [Anthropic — Building Effective Agents](anthropic-building-effective-agents.md) | Anthropic | 4.1 | 6/3/0/1 |
| [Building Agents with the Claude Agent SDK](anthropic-agent-sdk.md) | Anthropic | 3.8 | 9/2/2/2 |
| [Effective Harnesses for Long-Running Agents (Anthropic)](anthropic-harnesses-long-running-agents.md) | Anthropic | 3.8 | 4/4/0/1 |
| [Writing Effective Tools for Agents](anthropic-writing-effective-tools-for-agents.md) | Anthropic | 3.75 | 4/4/0/0 |
| [Anthropic — Effective Context Engineering for AI Agents](anthropic-context-engineering.md) | Anthropic | 2.7 | 3/2/2/0 |
| [Anthropic — Advanced Tool Use (Tool Search / deferred loading, programmatic + parallel tool calling)](anthropic-advanced-tool-use.md) | Anthropic | 2.5 | 0/2/1/1 |
| [Equipping Agents with Agent Skills](anthropic-agent-skills.md) | Anthropic | 2.5 | 1/1/1/2 |
| [Anthropic — Code Execution with MCP](anthropic-code-execution-with-mcp.md) | Anthropic | 1.5 | 1/3/2/1 |
| [Gulli — Agentic Design Patterns (21 patterns)](gulli-agentic-design-patterns.md) | Book | 3.9 | 12/7/0/2 |
| [Iusztin & Labonne — LLM Engineer's Handbook](llm-engineers-handbook.md) | Book | 3 | 6/8/5/3 |
| [Claude Certified Architect (Foundations)](cca-foundations.md) | Cert | 3.8 | 6/3/0/1 |
| [Google 5-Day AI Agents Intensive](google-5day-ai-agents.md) | Course | 3.7 | 4/4/0/0 |
| [NVIDIA DLI — RAG / Agents / Data Flywheel](nvidia-dli-rag-agents-flywheel.md) | Course | 3.2 | 11/4/5/1 |
| [Google — AI Agent Trends 2026 (atomic transactions, agent undo stacks / reversibility, enterprise adoption)](google-ai-agent-trends-2026.md) | Google | 3.5 | 4/2/1/0 |
| [Google — Choose Agentic AI Architecture Components](google-choose-agentic-architecture-components.md) | Google | 3.1 | 5/2/2/1 |
| [Google — Agent2Agent (A2A) Protocol](google-a2a-protocol.md) | Google | 2.8 | 3/2/2/1 |
| [LangChain — LangGraph Platform GA](langchain-langgraph-platform-ga.md) | LangChain | 4 | 3/2/0/0 |
| [LangChain — State of Agent Engineering 2026](state-of-agent-engineering-2026.md) | LangChain | 3.7 | 9/2/2/0 |
| [LangChain — Framework Docs](langchain-framework-docs.md) | LangChain | 3.6 | 9/5/2/2 |
| [Microsoft — Semantic Kernel](microsoft-semantic-kernel.md) | Microsoft | 4.2 | 3/2/0/0 |
| [Microsoft — Agent Framework 1.0](microsoft-agent-framework-1-0.md) | Microsoft | 3.9 | 2/3/0/0 |
| [OpenAI — A Practical Guide to Building Agents](openai-practical-guide-building-agents.md) | OpenAI | 4.5 | 10/1/0/1 |
| [OpenAI — Evals Guide](openai-evals-guide.md) | OpenAI | 3.4 | 3/7/1/0 |
| [OpenAI — Retrieval Guide](openai-retrieval-guide.md) | OpenAI | 2.1 | 1/3/2/1 |
| [ReAct (Reason + Act)](react-reason-act.md) | Paper | 4 | 6/4/0/0 |
| [Chain-of-Thought](chain-of-thought.md) | Paper | 3.4 | 3/4/2/1 |
| [Agentic RAG Survey (arXiv 2501.09136) — Self-RAG / CRAG / GraphRAG / HyDE](agentic-rag-survey-2501-09136.md) | Paper | 2.3 | 0/3/3/0 |
| [Reflexion](reflexion.md) | Paper | 2.3 | 1/4/2/1 |
| [Self-Consistency](self-consistency.md) | Paper | 2 | 1/3/2/0 |
| [Tree-of-Thoughts](tree-of-thoughts.md) | Paper | 2 | 2/3/3/0 |

---

**See also:** the master roll-up — [Source-Benchmark Catalog](../SOURCE-BENCHMARK-CATALOG.md) (aggregate score, per-source scorecard, cross-source themes, where-TG-exceeds, and the ranked remediation backlog). Companion audit-overlay docs: [IMPROVEMENT-TARGETS.md](../IMPROVEMENT-TARGETS.md), [EXTERNAL-AUDIT-LESSONS.md](../EXTERNAL-AUDIT-LESSONS.md).
