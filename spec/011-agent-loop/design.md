<!-- spec/011 — provenance tags: [F] foundation / [R] product reframe / [O] audit overlay. -->

# spec/011 — Design: native Go agent loop

How the requirements in `requirements.md` are realized on the Go stack. Where this design and the code
disagree, the code is the bug and this document is the intent. The predecessor logic re-expressed here
is the Claude-Code subprocess ReAct loop (confidence/STOP discipline, handoff/turn limits); none of it is
vendored — it is re-implemented as a single native Go loop over the LiteLLM gateway per
[`docs/PORTING-GUIDE.md`](../../docs/PORTING-GUIDE.md). The predecessor's read-only sub-agents-as-tools
split is NOT re-expressed — TG ships a single loop; sub-agent delegation is deferred under an
evidence-gated HOLD (CONSTITUTION §4.14).

## Components

- **`agent.Agent`** (`agent/loop.go`) — the ReAct loop. `Run(ctx, seed)` calls the model through the
  `Completer` interface (satisfied by `adapters/model.Gateway`, never a subprocess), parses each turn as
  a typed `directive`, and dispatches on `directive.Action` by an exact switch. A tool call is routed to
  an allowlisted read-only tool; a proposal is parsed by the single `ParseProposal` grammar (spec/002);
  a stop/unknown action/unparseable output fails closed. Model text is never executed (REQ-1001/1005).
- **`agent.ToolSet`** (`agent/tools.go`) — the registered, allowlisted tools. `Register` refuses a
  non-read-only tool (`ErrWriteToolWithheld`), so a write/mutating tool is structurally absent from the
  Phase-0/1 agent (REQ-1003). `Get(name)` is an exact lookup; a miss fails closed rather than executing
  the name (REQ-1001).
- **`agent.ParseConfidence`** (`agent/confidence.go`) — extracts the parseable CONFIDENCE scalar. A
  missing/unparseable confidence is 0, which is below the stop threshold, so the absence fails closed
  (REQ-1002).
- **`protocolPreamble`** (`agent/loop.go`) — the system message prepended to the seed that teaches the exact
  directive/proposal WIRE FORMAT and the live read-only tool allowlist (without it the model returns prose and
  the loop stops at 0% proposals). It also states the GROUNDING CONTRACT the eval scores: each tool result is
  returned as `OBSERVATION[<id>]: <data>`, and a proposal's `evidence_ids` MUST copy verbatim the bracketed id
  of every observation the diagnosis relied on — a proposal citing none is an ungrounded guess (down-scored,
  sent for human review, never auto-applied). This is prompt guidance that raises citation adherence; the
  silent-cognition guard (INV-11, spec/012) is the mechanical enforcement.

## The loop (REQ-1001, REQ-1002, REQ-1004, REQ-1005, REQ-1006, REQ-1012)

Each cycle, bounded by the hard-halt limit:

1. Call `Completer.Complete` through the gateway (no subprocess).
2. `json.Unmarshal` the response into a typed `directive`; a parse failure stops (fail closed, REQ-1005).
3. Resolve the confidence (typed field, else the prose scalar); below the stop threshold stops (REQ-1002).
4. Switch on `Action`:
   - `tool` → exact allowlist `Get`; a miss or a non-read-only tool stops; else invoke and append the
     orchestrator-captured `ToolResult` as a delimited OBSERVATION (data, REQ-1001/1006). The result's OUTPUT
     is INPUT-SCREENED first (`screenToolOutput` → `core/screen.Scrub`, the same neutralize-and-flag policy
     the seed blocks pass in `temporal/runner.screenSeedBlock`): a live tool result from a compromised or
     attacker-influenced host is a prompt-injection surface, so an injection span is defanged to its
     `[SCREENED:<cat>]` marker and any leaked secret redacted BEFORE it re-enters the model prompt, while the
     `OBSERVATION[<id>]` envelope and the id (the evidence anchor) stay verbatim. A detection is recorded on
     `Result.ScreenNotes` but never drops the observation or stops the loop — under-triage is the worse
     failure and screening is data hygiene, not a POLL_PAUSE signal (REQ-1012, INV-08). The call is recorded
     as a `TrajectoryStep`; `TrajectoryVeto` (`agent/trajectory.go`) halts a STUCK trajectory — the same call
     repeated `LoopThreshold` times in a row (a loop ignoring its observations) or any call recurring
     `RepeatThreshold` times (thrashing) — before it burns the full cycle budget. The veto is a deterministic
     analysis of the agent's OWN actions; no model token is consulted (INV-08).
   - `propose` → `ParseProposal`; below the escalate threshold → escalate, else → proposed (REQ-1002).
   - `stop` / anything else → stop (fail closed, REQ-1005).
5. At the poll-handoff limit without a proposal → escalate; at the hard-halt limit → hard-halt (REQ-1004).

The `Outcome` zero value is `OutcomeStop`, so any un-set/errored path is the most-restrictive terminal
state. The agent produces no side effect — every effect is deferred to the orchestrator and the gate
(REQ-1006).

## ACI tool contract, recoverable tool-errors, and the thought trace (REQ-1009, REQ-1010, REQ-1011)

The reasoning layer is sharpened by three tightly-coupled additions in `agent/tools.go` and
`agent/loop.go` (design-wisdom #5/#6/#7):

- **ACI tool contract (REQ-1009).** `ParamSpec` types one argument (name, type hint, required flag,
  enum, example, one-line description). `ACITool` is the OPTIONAL extension of `Tool` — a tool that
  ALSO publishes `Description()` and `Params()`. `ToolSet.Catalog()` renders every tool's name +
  description + typed parameters into the preamble (replacing the bare comma-joined name list), so the
  model can call a tool "from its description and parameters alone" (Anthropic, Writing Effective
  Tools). `ValidateArgs` is the poka-yoke screen: a missing required arg, or a value outside a declared
  enum, is refused with a single actionable message BEFORE the tool is invoked. A plain `Tool` that has
  not adopted the schema is rendered by name only and validates trivially, so the contract lands without
  touching any existing tool — per-tool adoption is a follow-on port. Neither the catalog nor the
  validation is a dispatch input; dispatch remains the exact `Get(name)` lookup (INV-08).
- **Recoverable tool-errors (REQ-1010).** A failed argument screen or a tool `Invoke` error no longer
  `OutcomeStop`s the whole session. Instead the loop appends `TOOL_ERROR[<tool>]: <message>` as
  delimited DATA and continues, so the model may fix the call or try a different tool (ReAct exception
  handling). The failing call is recorded in the trajectory BEFORE dispatch, so a tool that keeps
  failing with the same args is still caught by the trajectory veto, and the cycle limits still bound
  the run — recovery is not an escape from the budget.
- **Thought-as-data (REQ-1011).** The directive grammar accepts an OPTIONAL, size-capped `thought`
  string. It is parsed the moment the directive decodes and recorded on `Result.Thoughts` for audit,
  BEFORE any dispatch decision — and it is never read for control flow. The `switch` reads `Action`
  alone, so a `thought` claiming "stop" while `Action=="tool"` still runs the tool. This reinstates the
  ReAct/CoT channel the "one JSON object" rule had suppressed without letting a model token decide
  dispatch (INV-08 holds).

## Decision procedure (per turn)

1. Model output that is not a single typed directive → stop (REQ-1005).
2. An optional `thought` is recorded as audit DATA and never consulted for dispatch (REQ-1011).
3. Confidence < 0.5 → stop; 0.5 ≤ confidence < 0.7 on a proposal → escalate (REQ-1002).
4. A tool name absent from the allowlist, or a non-read-only tool → stop (REQ-1001/1003).
5. A tool call whose args fail the declared ACI schema, or whose invocation errors, becomes a
   `TOOL_ERROR` observation and the loop continues — bounded by the cycle/trajectory limits (REQ-1009/1010).
6. A schema-valid proposal at/above 0.7 → proposed (REQ-1005/1006).
7. Cycle poll-limit → escalate; hard-halt limit → hard-halt (REQ-1004).

## Out of scope

The `ParseProposal` grammar itself is spec/002. The gate the proposal enters and the band it receives
are spec/002 and spec/001. The mutating actuation the agent is forbidden from calling is the Phase-2
safety core.
