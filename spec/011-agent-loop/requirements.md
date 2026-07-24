<!-- spec/011 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/011 — Native Go agent loop

**Owning behavior family:** the native Go ReAct / tool-calling loop (`agent/`).
**Constitution / invariants:** INV-06, INV-08.
**Phase:** Phase 1 (typed spine). **Status:** Approved.

Territory Grounder drops the predecessor's `claude -p` / `claude -r` subprocess mechanism entirely: the
`agent/` service is a native Go ReAct / tool-calling loop that calls LLM APIs **directly through the
bundled LiteLLM model-gateway**. It investigates and *proposes*; it never executes. Its governing
property is that **no model-produced token becomes control flow, a command string, or a query
fragment** — model output enters only as typed, validated, delimited data — and it emits its proposal
only through the single `ParseProposal` grammar (spec/002). This document is the requirement source of
record; the design is in `design.md`, the runnable acceptance oracles are in `acceptance/`, and the
engineering tasks are in `tasks.json`.

## Requirements

- **REQ-1001** — [O] INV-08 · [R] paradigm-rules 4, 7.
  The agent SHALL call LLM APIs only through the bundled LiteLLM gateway `Completer` interface and SHALL
  NOT spawn a subprocess; no model-produced token SHALL become control flow, a command string, or a
  query fragment — a tool is dispatched only by an exact, validated name lookup against the registered
  tool set, never by executing model text.

- **REQ-1002** — [F] confidence-first reasoning discipline · [O] INV-08.
  WHEN the parsed CONFIDENCE scalar is below the stop threshold (0.5), the agent SHALL stop without a
  usable proposal and route to a poll; WHILE the confidence is at or above the stop threshold but below
  the escalate threshold (0.7), the agent SHALL escalate the proposal to a human poll rather than mark
  it for autonomous action. A missing or unparseable confidence SHALL be treated as 0 (fail closed).

- **REQ-1003** — [O] INV-08 · [F] least-autonomous topology.
  WHILE mutation is off (Phase 0/1), the agent's tool set SHALL contain only read-only tools; a
  non-read-only tool SHALL be refused at registration, so a write/mutating tool is structurally absent
  from the agent and its read-only sub-agents.

- **REQ-1004** — [F] handoff / cycle limits.
  WHEN the agent's cycle count reaches the poll-handoff limit without emitting a proposal, the agent
  SHALL escalate to a poll; WHEN it reaches the hard-halt limit, the agent SHALL hard-halt — so an
  unbounded or looping agent is not reachable. HOWEVER, when the poll-handoff limit is reached
  immediately after a TOOL call (the agent was still investigating, not deciding — TG-60), the agent
  SHALL first receive ONE deterministic "decide now" cycle instructing it to converge from its
  observations to a grounded stop or proposal rather than call another tool; that single decision cycle
  SHALL run on the capable reasoning tier (DecisionModelName), not the fast investigation tier, so a
  model strong enough to obey the instruction actually decides; the nudge fires at most
  ONCE (never a grind), and if the agent still does not decide it escalates as above. This converts an
  empty, conclusion-less hand-off into a grounded decision where one is reachable. The nudge is a
  deterministic function of the cycle count and the last action type — no model token becomes control
  flow (INV-08). WHEN the agent nonetheless escalates or hard-halts at a limit, the record SHALL carry a
  DETERMINISTICALLY SYNTHESIZED hand-off rationale — the count of distinct observations gathered and that
  no grounded decision was reached, citing those observation ids — so a hand-off is NEVER conclusion-less
  (the operator + judge always see what was investigated). The synthesis reads only the agent's OWN
  captured evidence, never model text (INV-08).

- **REQ-1005** — [O] INV-06.
  The agent SHALL emit its proposal only as a typed tool-call parsed by the single `ParseProposal`
  entry point; IF the model output is unparseable, non-manifest-expressible, or names an unknown action,
  THEN the agent SHALL fail closed to a stop and SHALL NOT route the output through a looser fallback
  grammar.

- **REQ-1006** — [F] "orchestrator owns the effect channel".
  The agent SHALL only propose — in Phase 0/1 it SHALL invoke no mutating tool and produce no side
  effect on the estate; every effect is deferred to the deterministic orchestrator (spec/006 Runner) and
  the governed gate (spec/001, spec/002).

- **REQ-1007** — [O] INV-08, INV-11 · [F] evidence-grounded proposal.
  WHEN the agent emits a proposal AFTER it has captured at least one tool OBSERVATION, the agent SHALL
  admit that proposal only if its `evidence_ids` cite at least one observation the agent ACTUALLY captured;
  a proposal that cites none, or that cites only ids the agent never captured (a fabricated citation), is
  ungrounded and SHALL be re-prompted to cite the real observation id(s) rather than accepted. A repeat
  offender that never cites its evidence SHALL reach the poll-handoff limit and escalate to a human
  (REQ-1004), never landing an ungrounded auto-proposal. The check is a deterministic function of the
  agent's OWN captured evidence set — no model token becomes control flow (INV-08). WHEN no tool was
  available or called there is nothing to cite, so the gate SHALL NOT fire and an alert-only proposal is
  unaffected.

- **REQ-1008** — [O] INV-08, INV-11 · [F] grounded no-action conclusion.
  WHEN the agent emits a stop AFTER it has captured at least one tool OBSERVATION and the stop directive
  carries no citation of an observation the agent actually captured, the agent SHALL re-prompt the model
  once for a stop that states its reason and cites the real observation id(s); a second uncited stop SHALL
  be accepted — a stop is the fail-safe end state and SHALL NOT be blocked into further cycles. The accepted
  stop's reason SHALL be surfaced as untrusted, size-capped DATA distinct from the loop's mechanical reason,
  and its cited ids SHALL be filtered to observations the agent actually captured before being recorded
  (a fabricated id is dropped, never stored). No token of the reason SHALL become control flow (INV-08).

- **REQ-1009** — [O] INV-08 · [F] Agent-Computer-Interface (Writing Effective Tools).
  The agent's tool contract SHALL let a tool publish a human-facing description and a typed parameter
  schema (name, type, required flag, enum, example), and the preamble SHALL render that description and
  schema for every tool that publishes one — so the model can call the tool from its description and
  parameters alone; a tool that publishes no schema SHALL be rendered by name. WHEN a tool call omits a
  required parameter or supplies a value outside a declared enum, the agent SHALL refuse the call with an
  actionable error message and SHALL NOT invoke the tool against the estate. The rendered schema and the
  argument validation are DATA and a screen — no field of either SHALL become control flow; dispatch
  SHALL remain an exact, validated name lookup (INV-08).

- **REQ-1010** — [O] INV-08 · [F] ReAct exception-handling.
  WHEN a tool invocation returns an error, the agent SHALL append an actionable tool-error observation
  for that call and SHALL continue the loop so the model may correct the call or select a different
  tool, rather than terminating the whole session on the first tool error. The recovery SHALL remain
  bounded by the existing cycle and trajectory limits (REQ-1004 and the trajectory veto), so a tool that
  keeps failing SHALL still reach a limit and hand off. The tool-error text SHALL enter the conversation
  only as delimited DATA — no token of it SHALL become control flow (INV-08).

- **REQ-1011** — [O] INV-08 · [F] ReAct / chain-of-thought trace.
  The directive grammar SHALL accept an OPTIONAL, size-capped `thought` string that the model MAY emit
  alongside its action. The agent SHALL parse the `thought` ONLY AS DATA and SHALL record it for audit,
  and SHALL NEVER read it for control flow: dispatch SHALL switch on the action field alone, so a
  `thought` that names a different action — for example a `thought` asking to stop while the action is a
  tool call — SHALL NOT change what the agent runs (INV-08). A missing `thought` SHALL leave behavior
  unchanged.

- **REQ-1012** — [O] INV-08 · [F] input screen at the tool-result trust boundary.
  WHEN a read-only tool invocation returns a result, the agent SHALL pass that result's OUTPUT text through
  the input screen (the neutralize-and-flag `screen.Scrub` policy the seed blocks pass) BEFORE the output
  re-enters the model prompt as an OBSERVATION — a tool result from a compromised or attacker-influenced host
  during read-only investigation is an untrusted prompt-injection surface (the "lethal trifecta" tool-result
  path). A detected injection span SHALL be neutralized in place to its screen marker and any leaked secret
  SHALL be redacted; the `OBSERVATION[<id>]` envelope and the observation id SHALL be preserved verbatim (the
  id is the evidence anchor). The agent SHALL NEVER drop a screened observation, and a screened result SHALL
  NOT by itself force a poll-pause or stop the loop — under-triage is the worse failure, and screening is
  data hygiene consistent with the secret-redaction pass, which is deliberately not a detection signal. A
  detection SHALL be recorded on the session record as a per-result screen note so a screened tool result is
  visible. The screening is a deterministic function of the tool output — no model token SHALL become control
  flow (INV-08). Output that carries no injection and no secret SHALL pass through byte-identical.

## Out of scope

The prediction gate that a proposal enters is spec/002. The three-band classification of a proposal is
spec/001. The Runner Temporal workflow that drives the agent and threads its proposal into the gate is
the Phase-1 Runner (EXECUTION-PLAN P1-7). The write-tool actuation the agent is forbidden from calling
is a Phase-2 concern gated by the mechanical safety core. REQ-1009 defines the ACI tool CONTRACT (the
`ACITool` extension, the rendered catalog, the argument screen); the follow-on adoption of that schema
by each existing read-only investigation tool (LibreNMS, syslog-ng, estate, host-diag) is a later port
and is out of scope here — a tool that has not adopted the schema is rendered by name and validates
trivially, so the contract lands without changing any existing tool.
