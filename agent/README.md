# `agent/` — native Go agent loop

The native Go ReAct / tool-calling loop that calls LLM APIs **directly through the bundled LiteLLM
model-gateway** — there is **no `claude -p` / `claude -r` subprocess** ([corrections]; [O] INV-08).

It ports the reasoning discipline verbatim: a parseable **CONFIDENCE** scalar with STOP thresholds
and a **per-agent turn budget** (poll@5 / halt@10) that bounds the single loop. The manager pattern
with **read-only sub-agents-as-tools** (write tools structurally withheld) is a **deferred design, not
shipped** — it stays on an evidence-gated HOLD until `delegation_precision` / `parallel_speedup` /
`multi_agent_quality_lift` show net value (see `../docs/EXTERNAL-AUDIT-LESSONS.md`, lesson 9). In
Phase 1 the agent may only call **read-only** actuation tools (`adapters/actuation`); it investigates
and *proposes*, it never executes.

**Status:** interface consumers exist (`adapters/model`, `adapters/actuation`); the loop itself is
**P1-4** (see `docs/EXECUTION-PLAN.md`). No model-produced token ever becomes control flow, a
command string, or a query fragment.
