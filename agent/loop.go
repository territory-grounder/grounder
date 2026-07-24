package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/screen"
)

// protocolPreamble is the system message that teaches the model the ONLY output contract: a single JSON
// directive, the read-only tool allowlist, and the proposal grammar. Without it the model returns prose and
// EVERY session fails closed to "unparseable" — the seed's behavioral skills describe HOW to reason but not
// the WIRE FORMAT this loop parses (an eval over real NL incidents found 0% proposals: bare stops). The tool
// list is built from the LIVE tool set, so the model names only tools it may actually call; an unknown tool
// is still refused (INV-08 — no model token becomes control flow).
func protocolPreamble(tools *ToolSet) model.Message {
	toolList := "none — you cannot gather evidence; propose only if the alert itself is sufficient, else stop"
	if cat := tools.Catalog(); cat != "" {
		// The STRUCTURED catalog (name + description + typed params) replaces the bare name list so the
		// model can call a tool "from its description and parameters alone" (Writing Effective Tools, ACI —
		// design-wisdom #5). Dispatch is still an exact name lookup; the catalog is prompt DATA only.
		toolList = cat
	}
	// The actuatable op-class SCHEMA CATALOG, rendered from the ONE op-class schema registry
	// (core/actuate/opschema) — the SAME source the parser/interceptor/runner/effect leaf read. It publishes,
	// per actuatable op_class, the structured params it requires (name/type/required/example) so the model
	// emits a COMPLETE proposal (e.g. restart-service WITH params.unit) "from its parameters alone" (ACI). This
	// MIRRORS the read-only tool catalog above; it is prompt DATA only — the op_class is validated by an exact
	// registry lookup, never executed as text (INV-08). Without it the model omitted params.unit and the fix
	// only failed at execute-time with an opaque empty argv.
	opClassCatalog := opschema.Catalog()
	if opClassCatalog == "" {
		opClassCatalog = "none declared"
	}
	return model.Message{Role: "system", Content: `You are Territory Grounder's SRE triage agent. Investigate the incident using READ-ONLY tools, then propose ONE conservative, REVERSIBLE fix — or stop if no safe action is warranted or a human must decide.

Reply with EXACTLY ONE JSON object and NOTHING else: no prose, no explanation, no markdown code fences.

Directive schema:
{"action":"tool"|"propose"|"stop","confidence":<0.0-1.0>,"thought":"<optional>","tool":"<tool name>","args":{"<k>":"<v>"},"proposal":{...}}

- Gather evidence FIRST (when a tool is available): {"action":"tool","tool":"<one of the tools below>","args":{...},"confidence":<0.5-0.7 while investigating>}. Call a tool by its EXACT name and pass exactly the parameters listed under it. Each result is returned to you as OBSERVATION[<id>]: <data> — the <id> in brackets is exactly what you cite as evidence. If a call is rejected you receive TOOL_ERROR[<tool>]: <message> instead — read it, then FIX the call or try a DIFFERENT tool; a tool error does not end the session. Keep investigating until you can propose or must stop.
- Propose a fix: {"action":"propose","confidence":<0.0-1.0>,"proposal":{"external_ref":"<the incident ref>","target":"<host>","op_class":"<e.g. restart-service>","op":"<e.g. restart>","params":{"<param>":"<value>"},"reversible":true|false,"rationale":"<why, quoting the observed facts>","evidence_ids":["<the exact bracketed id from each OBSERVATION you used, copied verbatim, e.g. lnms-dev-...>"],"approval_choice":""}} — when the op_class you choose is listed under "Actuatable op-classes" below, "params" MUST carry every required param it declares (e.g. restart-service requires params:{"unit":"nginx"}); the fix cannot be built or executed without them.
- Stop: {"action":"stop","confidence":<0.0-1.0>,"reason":"<one sentence: why no action is warranted, from the observed facts>","evidence_ids":["<the OBSERVATION id(s) your conclusion relies on>"]} — only when no safe reversible action exists. "No action needed" is a CONCLUSION: after any OBSERVATION it must state its reason and cite the observation id(s) that support it (e.g. a DISABLED device, a stale alert).
- Optional "thought": a brief note of your reasoning for THIS step. It is RECORDED for audit only and CHANGES NOTHING that runs — the "action" field alone decides what happens. Omit it when you have nothing to add; never let it stand in for a real action.

Read-only tools available (call one by its EXACT name; pass the parameters listed under it):
` + toolList + `

Actuatable op-classes (when your proposal's op_class is one of these, "params" MUST include every param marked required — copy the exact param name; the fix cannot execute without it):
` + opClassCatalog + `
Grounding is MANDATORY: whenever you gathered any OBSERVATION, your proposal's evidence_ids MUST list the exact bracketed id of every observation your diagnosis relied on — copy them verbatim, never invent an id. A proposal that cites no evidence_ids is an ungrounded guess: it is down-scored and sent for human review, never auto-applied. Confidence must reflect your EVIDENCE: below 0.5 the session halts. Do not stop before investigating when a tool is available.`}
}

// stripFences unwraps a fenced code block (` + "```json … ``` or ``` … ```" + `) that many chat models emit
// around a JSON directive. It is a deterministic UNWRAP of a known envelope, NOT a looser grammar: the
// content inside must still be strictly valid JSON for the loop to act on it, so unparseable content after
// unwrapping still fails closed (INV-08).
func stripFences(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return raw
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:] // drop the opening fence line (``` or ```json)
	}
	if j := strings.LastIndex(s, "```"); j >= 0 {
		s = s[:j] // drop the closing fence
	}
	return strings.TrimSpace(s)
}

// Completer is the minimal LLM interface the agent needs; the bundled LiteLLM gateway
// (adapters/model.Gateway) satisfies it. The agent calls ONLY this — it never spawns a subprocess.
type Completer interface {
	Complete(ctx context.Context, user, modelName string, msgs []model.Message) (string, error)
}

// Outcome is the terminal state of an agent run.
type Outcome int

const (
	// OutcomeStop: the agent halted without a usable proposal (low confidence / unparseable / unknown
	// action) — fail closed to a poll. Zero value on purpose.
	OutcomeStop Outcome = iota
	// OutcomeEscalate: a proposal exists but confidence is below the escalate threshold (or a limit was
	// hit) — it requires a human poll, not autonomous action.
	OutcomeEscalate
	// OutcomeProposed: a high-confidence, schema-valid proposal was produced.
	OutcomeProposed
	// OutcomeHardHalt: the cycle limit was reached — the agent hard-halts.
	OutcomeHardHalt
)

func (o Outcome) String() string {
	switch o {
	case OutcomeEscalate:
		return "escalate"
	case OutcomeProposed:
		return "proposed"
	case OutcomeHardHalt:
		return "hard-halt"
	default:
		return "stop"
	}
}

// Limits bound the ReAct loop so an unbounded/looping agent is not reachable. [F] handoff/cycle limits.
type Limits struct {
	HandoffPoll int // at this many cycles without a proposal, force an escalate/poll
	HandoffHalt int // at this many cycles, hard-halt
}

// DefaultLimits: escalate to a poll at 5 cycles, hard-halt at 10.
func DefaultLimits() Limits { return Limits{HandoffPoll: 5, HandoffHalt: 10} }

// directive is the typed shape a model turn must emit. The agent dispatches on Action by an exact
// switch (never by executing model text); an unknown action fails closed. When Action=="propose" the
// Proposal payload is handed to the single ParseProposal grammar.
type directive struct {
	Action     string          `json:"action"` // "tool" | "propose" | "stop"
	Tool       string          `json:"tool"`
	Args       argMap          `json:"args"`
	Confidence float64         `json:"confidence"`
	Proposal   json.RawMessage `json:"proposal"`
	// Reason + EvidenceIDs ground a STOP: "no action warranted" is a conclusion and needs its evidence
	// like any other (REQ-1008). Both are DATA — they never influence control flow beyond the citation
	// check, and only ids the agent actually captured are kept.
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids"`
	// Thought is an OPTIONAL, size-capped ReAct/CoT reasoning trace the model MAY emit. It is parsed
	// ONLY AS DATA and recorded for audit/forensics (design-wisdom #7); NO token of it becomes control
	// flow — the dispatch switch reads Action alone, so a `thought` saying "stop" while Action=="tool"
	// still runs the tool (INV-08). The preamble reinstates the CoT channel the "one JSON object" rule
	// had suppressed, without letting a model token decide dispatch.
	Thought string `json:"thought"`
}

// argMap tolerates a model that puts NON-string values (arrays, objects, numbers) in a tool's args — it
// stringifies each value instead of failing the whole directive. A general model prompted to call a tool
// routinely over-specifies args (e.g. "checks":["cmd1","cmd2"]); with a strict map[string]string that
// rejected the ENTIRE tool-call as "unparseable" and stalled every session (found by an eval — the model
// wanted to investigate, the parser refused it). The tool still receives string args (INV-08 unchanged: an
// unknown tool is still refused, no model token is executed); a non-string value keeps its compact JSON.
type argMap map[string]string

func (a *argMap) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*a = nil
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			m[k] = s // a JSON string value
		} else {
			m[k] = strings.TrimSpace(string(v)) // array/object/number → its compact JSON form
		}
	}
	*a = m
	return nil
}

// Result is the outcome of an agent run.
type Result struct {
	Outcome     Outcome
	Proposal    proposal.Proposal // valid iff Outcome is Proposed or Escalate
	Confidence  float64
	Cycles      int
	ToolResults []ToolResult
	Reason      string // TG's mechanical reason (control-plane text, never model output)
	// Conclusion is the model's grounded no-action rationale on a requested stop (REQ-1008) — untrusted
	// DATA for the session record/judge, distinct from the mechanical Reason. ConclusionEvidence keeps
	// only ids the agent ACTUALLY captured (a fabricated citation is dropped, never stored).
	Conclusion         string
	ConclusionEvidence []string
	// Thoughts is the OPTIONAL, size-capped ReAct/CoT reasoning the model emitted per cycle, captured in
	// emission order as untrusted DATA for auditability/forensics (design-wisdom #7). It is NEVER read
	// for control flow — dispatch switches on Action alone (INV-08): a `thought` claiming "stop" while
	// Action is "tool" still runs the tool. Empty thoughts are skipped.
	Thoughts []string
	// ScreenNotes records, per screened OBSERVATION, that a live tool RESULT re-entering the model prompt
	// tripped the input screen — the tool-result analogue of the seed-block provenance notes (REQ-1012).
	// A tool result from a compromised or attacker-influenced host during read-only investigation is a
	// prompt-INJECTION surface (the "lethal trifecta" tool-result path); each result is passed through the
	// SAME neutralize-and-flag screen the seed blocks get (screen.Scrub) before it is appended as an
	// OBSERVATION, so a hostile span is defanged and any leaked secret redacted. This slice is untrusted
	// DATA for the session record only: a screened observation is NEVER by itself a POLL_PAUSE/stop signal
	// (mirroring how secret redaction is deliberately not a Detect signal), so an attacker cannot suppress
	// triage by embedding an injection string — under-triage is the worse failure (INV-08). Empty ⇒ every
	// observation passed the screen byte-clean.
	ScreenNotes []string
	// Steps is the cycle-ALIGNED ReAct transcript for the decision tracer (spec/020 T-020-8): exactly one
	// AgentCycle per loop iteration, in order, carrying the REAL per-cycle ordinal — so a per-cycle tracer row
	// pairs the right thought with the right tool. UNTRUSTED DATA, NEVER read for control flow (INV-08). The
	// sparse Thoughts/ToolResults slices are retained UNCHANGED for their existing consumers (evidence binding,
	// metrics, judge); Steps is a superset transcript, not a replacement.
	Steps []AgentCycle
}

// AgentCycle is one ReAct cycle's cycle-aligned transcript record (spec/020 T-020-8, REQ-2008): its real
// ordinal, the optional CoT thought, the action taken, the tool name (tool cycles only), a short NON-SECRET
// observation summary, and the per-cycle outcome. DATA-only for the tracer — never control flow (INV-08); every
// text field is run through screen.Scrub before it is persisted to agent_step.
type AgentCycle struct {
	Cycle       int
	Thought     string
	Action      string
	Tool        string
	Observation string
	Outcome     string
}

// Agent is the native Go ReAct / tool-calling loop over the LiteLLM gateway.
type Agent struct {
	Model     Completer
	Tools     *ToolSet
	Limits    Limits
	ModelName string
	// DecisionModelName is the tier used for the ONE forced-decision cycle at the poll limit (TG-60): the
	// low-latency ModelName ("fast") is right for the many-call investigation, but too weak to reliably obey
	// the "decide now" nudge, so the agent kept investigating and handed off empty. A single reasoning-tier
	// call there converges far more often (its latency is paid once, not per cycle). Empty ⇒ same as ModelName.
	DecisionModelName string
	User              string
}

// Run drives the loop over a seeded conversation. Each turn: call the model through the gateway, parse
// the response as a typed directive; if a tool call, dispatch to the allowlisted READ-ONLY tool and
// append the orchestrator-captured result as an observation; if a proposal, parse it via the single
// ParseProposal grammar. Confidence below StopThreshold stops; the cycle limits force an escalate then
// a hard-halt. ANY unparseable output or unknown tool/action fails closed — model text is never
// executed. [O] INV-08, spec/011.
func (a *Agent) Run(ctx context.Context, seed []model.Message) (Result, error) {
	// Prepend the output protocol so the model knows the directive/proposal WIRE FORMAT + the read-only tool
	// allowlist. Without it the model returns prose → every session fails closed to "unparseable". The seed's
	// skills teach HOW to reason; this teaches the format the loop parses (spec/011).
	msgs := append([]model.Message{protocolPreamble(a.Tools)}, seed...)
	lim := a.Limits
	if lim.HandoffHalt == 0 {
		lim = DefaultLimits()
	}
	res := Result{}
	var traj []TrajectoryStep // the record of tool calls, analyzed for a stuck loop (deterministic, INV-08)
	stopNudged := false       // the grounded-stop nudge (REQ-1008) fires at most once — never grind the safe exit
	decideNudged := false     // the poll-limit decide-now nudge (TG-60) fires at most once — converge before handing off
	decideCycle := false      // the cycle immediately AFTER the nudge is the forced decision — run it on the capable tier

	for cycle := 1; cycle <= lim.HandoffHalt; cycle++ {
		res.Cycles = cycle
		// spec/020 T-020-8: one cycle-aligned transcript record per iteration, updated IN PLACE via a pointer
		// (never re-appended within this cycle, so the pointer stays valid) — captured even when the cycle
		// returns early. DATA-only (INV-08); it changes no dispatch.
		res.Steps = append(res.Steps, AgentCycle{Cycle: cycle})
		step := &res.Steps[len(res.Steps)-1]
		investigatedThisCycle := false // true iff this cycle was a tool call — the TG-60 decide-nudge targets ONLY that
		modelName := a.ModelName
		if decideCycle {
			decideCycle = false
			if a.DecisionModelName != "" {
				modelName = a.DecisionModelName // TG-60: the forced-decision cycle runs on the capable tier
			}
		}
		raw, err := a.Model.Complete(ctx, a.User, modelName, msgs)
		if err != nil {
			res.Outcome, res.Reason = OutcomeStop, "model call failed"
			return res, err
		}

		var d directive
		// Unwrap a ```json … ``` fence if the model wrapped its directive (common for general chat models
		// like Mistral). A deterministic unwrap of a known envelope — NOT a looser grammar: the content must
		// still be strictly valid JSON, so unparseable content after unwrapping still fails closed (INV-08).
		// Decode the FIRST JSON value and ignore trailing bytes — some models append hallucinated content
		// (a fabricated OBSERVATION, a stray token) after their directive. The directive grammar itself is
		// still strict (unknown fields are ignored by the loop's typed struct; a non-JSON first value still
		// fails closed); only trailing noise after a valid directive is tolerated.
		if json.NewDecoder(strings.NewReader(stripFences(raw))).Decode(&d) != nil {
			// unparseable model output — fail closed, do NOT fall back to a looser grammar
			res.Outcome, res.Reason = OutcomeStop, "unparseable model output"
			return res, nil
		}
		// #7 thought-as-data: record the OPTIONAL CoT trace for audit BEFORE any dispatch decision, so it is
		// captured on every parsed cycle (even one that then stops on low confidence) and it is provably NOT
		// consulted for control flow — the switch below reads d.Action alone (INV-08). A `thought` claiming a
		// different action changes nothing that runs.
		if th := clipThought(d.Thought); th != "" {
			res.Thoughts = append(res.Thoughts, th)
			step.Thought = th // cycle-aligned copy for the tracer (spec/020 T-020-8); Thoughts kept as-is
		}
		step.Action = d.Action
		conf := d.Confidence
		if conf == 0 {
			// no typed confidence — try the parseable prose scalar; still absent ⇒ 0 ⇒ fail closed
			if v, ok := ParseConfidence(raw); ok {
				conf = v
			}
		}
		d.Confidence = conf
		res.Confidence = conf
		if conf < StopThreshold {
			res.Outcome, res.Reason = OutcomeStop, "confidence below stop threshold"
			return res, nil
		}

		switch d.Action {
		case "tool":
			step.Tool = d.Tool           // cycle-aligned tool name for the tracer (spec/020 T-020-8)
			t, ok := a.Tools.Get(d.Tool) // exact allowlist lookup — never execute model text
			if !ok {
				// #4 recoverable unknown-tool: a mis-named tool (e.g. the model reaching for
				// "get-host-services" when the real diagnostic is "check-host-services") becomes an
				// actionable TOOL_ERROR that lists the tools that DO exist, NOT a session abort — the model
				// retries with a valid name (the SAME ReAct recovery as the arg-validation and tool-invoke
				// errors below). The unknown name is NEVER dispatched, so fail-closed / INV-08 is preserved
				// (no model token becomes control flow); the loop stays bounded by the same cycle/thrash
				// limits. (Formerly this OutcomeStop-ed the whole session on the FIRST tool-name miss — an
				// empty stand-down with no grounding, e.g. a service-fault triage that could not reach
				// check-host-services and stood down without investigating.)
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_ERROR[%s]: no such tool. Available tools: %s", d.Tool, strings.Join(a.Tools.Names(), ", "))})
				step.Observation, step.Outcome = "TOOL_ERROR (unknown tool)", "tool-error"
				investigatedThisCycle = true
				break // recover: fall through to the cycle-limit check — the session continues, never aborts
			}
			if !t.ReadOnly() {
				res.Outcome, res.Reason = OutcomeStop, "write tool withheld"
				return res, nil
			}
			// Record the step and veto a stuck trajectory — a loop re-asking the same question, ignoring its
			// observations, is halted BEFORE it burns the full cycle budget (a deterministic check of the
			// agent's own actions; no model token is consulted, INV-08). Recorded BEFORE dispatch so a tool
			// call that repeatedly ERRORS with the same args (below, #6) is still thrash-bounded, not just a
			// call that returns.
			traj = append(traj, TrajectoryStep{Tool: d.Tool, ArgsKey: ArgsKey(d.Args)})
			if veto, reason := TrajectoryVeto(traj); veto {
				res.Outcome, res.Reason = OutcomeStop, "trajectory veto — "+reason
				return res, nil
			}
			// #5 poka-yoke: screen the args against the tool's declared ACI schema (missing-required /
			// out-of-enum) BEFORE invoking. A bad call is NOT executed against the estate — it becomes an
			// actionable TOOL_ERROR observation the model can act on (fix the call or pick another tool). A
			// tool with no schema validates trivially, so existing tools are unaffected.
			if verr := ValidateArgs(t, d.Args); verr != nil {
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_ERROR[%s]: %s", d.Tool, verr.Error())})
				step.Observation, step.Outcome = "TOOL_ERROR (arg validation)", "tool-error"
				investigatedThisCycle = true
				break // recover: fall through to the cycle-limit check — the session continues, never aborts
			}
			tr, err := t.Invoke(ctx, d.Args)
			if err != nil {
				// #6 recoverable tool-error: a tool's Go-error becomes an actionable observation, NOT a session
				// abort — the model may fix the call or try a DIFFERENT tool (ReAct exception handling). The
				// error text is DATA (delimited), never control flow; the loop stays bounded by the same
				// cycle/thrash limits as any other cycle. (Formerly this OutcomeStop-ed the whole session on the
				// first transient tool failure.)
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("TOOL_ERROR[%s]: %s", d.Tool, err.Error())})
				step.Observation, step.Outcome = "TOOL_ERROR", "tool-error"
				investigatedThisCycle = true
				break // recover: fall through to the cycle-limit check — the session continues, never aborts
			}
			res.ToolResults = append(res.ToolResults, tr)
			step.Observation, step.Outcome = "observed "+tr.ID, "investigate" // non-secret evidence id; Scrub'd on persist
			// A live tool RESULT is attacker-influenceable DATA — a compromised or hostile host during
			// read-only investigation can return text that tries to hijack the loop (the "lethal trifecta"
			// tool-result injection surface). Screen the payload through the SAME neutralize-and-flag input
			// screen the seed blocks pass (mirrors temporal/runner.screenSeedBlock) BEFORE it re-enters the
			// model prompt as an OBSERVATION: an injection span is defanged to its [SCREENED:<cat>] marker and
			// any leaked secret redacted (REQ-1012). The OBSERVATION[<id>] envelope and the real tr.ID — the
			// evidence anchor — are kept VERBATIM; only the payload is screened. A detection is RECORDED on
			// ScreenNotes but NEVER drops the observation or stops the loop: under-triage is the worse failure
			// and screening is data hygiene, not a Detect/POLL_PAUSE signal (INV-08 — mechanical policy in
			// code, no model token decides anything).
			screened, notes := screenToolOutput(tr.ID, tr.Output)
			res.ScreenNotes = append(res.ScreenNotes, notes...)
			// append the captured observation as DATA (delimited, screened), then continue reasoning
			msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
			msgs = append(msgs, model.Message{Role: "user", Content: fmt.Sprintf("OBSERVATION[%s]: %s", tr.ID, screened)})
			investigatedThisCycle = true

		case "propose":
			p, perr := proposal.ParseProposal(d.Proposal)
			if perr != nil {
				res.Outcome, res.Reason = OutcomeStop, "proposal failed the single grammar"
				return res, nil
			}
			// Mechanical citation gate (REQ-1007): when the agent gathered OBSERVATIONS, its proposal must
			// cite at least one REAL one — an evidence_ids list that is empty, or that names only ids the
			// agent never captured, is not grounding. The preamble mandates this; here we ENFORCE it: an
			// uncited/fabricated-citation proposal is re-prompted (not accepted), and a repeat offender
			// escalates at the poll-handoff limit below rather than landing an ungrounded auto-proposal. A
			// deterministic check of the agent's OWN captured evidence — no model token becomes control flow
			// (INV-08). When no tool was available/called there is nothing to cite, so the gate does not fire.
			if len(res.ToolResults) > 0 && !citesGatheredEvidence(p.EvidenceIDs, res.ToolResults) {
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: "REJECTED — ungrounded proposal. You gathered OBSERVATION(s) but evidence_ids cites none of them. Re-emit the SAME proposal with evidence_ids listing the exact bracketed OBSERVATION id(s) your diagnosis relied on (e.g. \"" + firstToolID(res.ToolResults) + "\"), copied verbatim. Do not invent ids."})
				break // fall through to the poll-handoff check — a repeat offender escalates, not grinds
			}
			res.Proposal = p
			if d.Confidence < EscalateThreshold {
				res.Outcome, res.Reason = OutcomeEscalate, "confidence below escalate threshold"
			} else {
				res.Outcome, res.Reason = OutcomeProposed, "high-confidence proposal"
			}
			step.Outcome = res.Outcome.String()
			return res, nil

		case "stop":
			// REQ-1008 — a stop after gathered observations is a CONCLUSION ("no action warranted") and
			// should ground itself like a proposal. Nudge an uncited stop ONCE; a repeat is accepted —
			// a stop is the fail-safe end state and must never be blocked into grinding on.
			if len(res.ToolResults) > 0 && !citesGatheredEvidence(d.EvidenceIDs, res.ToolResults) && !stopNudged {
				stopNudged = true
				msgs = append(msgs, model.Message{Role: "assistant", Content: raw})
				msgs = append(msgs, model.Message{Role: "user", Content: "UNGROUNDED stop. You gathered OBSERVATION(s) but stated no grounded reason. Re-emit the stop as {\"action\":\"stop\",\"confidence\":...,\"reason\":\"<one sentence: why no action is warranted, from the observed facts>\",\"evidence_ids\":[\"" + firstToolID(res.ToolResults) + "\", ...]} citing the exact bracketed OBSERVATION id(s) your conclusion relies on. Do not invent ids."})
				break // fall through to the poll-handoff check, mirroring the proposal gate
			}
			res.Outcome, res.Reason = OutcomeStop, "agent requested stop"
			res.Conclusion = clipConclusion(d.Reason)
			res.ConclusionEvidence = gatheredOnly(d.EvidenceIDs, res.ToolResults)
			step.Outcome = res.Outcome.String()
			return res, nil

		default:
			// an unrecognized action never becomes control flow
			res.Outcome, res.Reason = OutcomeStop, "unknown action "+d.Action
			return res, nil
		}

		if cycle >= lim.HandoffPoll {
			// TG-60: at the poll limit the agent has usually GATHERED enough but kept calling tools, so it
			// hands off with NO conclusion. Give it ONE deadline cycle to CONVERGE — decide from its
			// observations (a grounded stop or a proposal) rather than escalate empty. Fires at most once
			// (never grind); if it still doesn't decide next cycle it escalates as before. Safe-direction: a
			// grounded decision beats an empty hand-off, and the citation/confidence gates + mutation-OFF still
			// govern any forced proposal. Deterministic (cycle count, not model text) — INV-08 holds.
			if investigatedThisCycle && !decideNudged {
				decideNudged = true
				decideCycle = true // TG-60: run the next (decision) cycle on the capable DecisionModelName tier
				msgs = append(msgs, model.Message{Role: "user", Content: "DECISION REQUIRED — you have investigated enough; do NOT call another tool. From your OBSERVATION(s), either PROPOSE one conservative reversible action (with evidence_ids citing the observation id(s)) or STOP with a grounded reason (with evidence_ids). This is your final cycle before hand-off."})
				continue
			}
			// still no decision after the nudge — escalate, but with a SYNTHESIZED rationale so the hand-off
			// is never conclusion-less (TG-60 option 2): deterministic, doesn't depend on the model deciding.
			res.Conclusion, res.ConclusionEvidence = synthesizeHandoffConclusion(res.ToolResults)
			res.Outcome, res.Reason = OutcomeEscalate, "handoff poll limit reached"
			return res, nil
		}
	}

	// Hard-halt: same guarantee — record WHAT was investigated so a budget-exhausted hand-off carries context.
	res.Conclusion, res.ConclusionEvidence = synthesizeHandoffConclusion(res.ToolResults)
	res.Outcome, res.Reason = OutcomeHardHalt, "cycle hard-halt limit reached"
	return res, nil
}

// screenToolOutput runs the input screen over ONE live tool RESULT before it re-enters the model prompt as
// an OBSERVATION — the tool-result analogue of temporal/runner.screenSeedBlock, so the loop's two screening
// sites (the seed on the way IN, the tool results that re-enter the loop) share one discipline. Clean output
// passes through byte-identical (screen.Scrub allocates nothing on the no-detection path). A detection
// NEUTRALIZES the payload in place — screen.Scrub defangs each injection span with its [SCREENED:<category>]
// marker over the normalized fold (so a homoglyph / zero-width disguise cannot survive) AND redacts any
// leaked secret to a [REDACTED:<kind>] marker — and returns an `input-screened:tool-result[<id>]:<categories>`
// note the caller records on Result.ScreenNotes. The observation is NEVER dropped: an attacker must not be
// able to suppress triage by embedding an injection string in a tool result (under-triage is the worse
// failure), and a screened result is data hygiene — deliberately NOT a POLL_PAUSE/stop signal (INV-08). The
// id is the evidence anchor and is passed through untouched; only the payload is screened.
func screenToolOutput(id, output string) (string, []string) {
	clean, hits := screen.Scrub(output)
	if len(hits) == 0 {
		return output, nil
	}
	return clean, []string{"input-screened:tool-result[" + id + "]:" + screenCategories(hits)}
}

// screenCategories joins the distinct categories of a detection set in Scrub's stable order — the compact
// per-result tag the ScreenNotes entry carries (mirrors temporal/runner.screenCategories).
func screenCategories(ms []screen.Match) string {
	seen := make(map[screen.Category]bool, len(ms))
	var out []string
	for _, m := range ms {
		if !seen[m.Category] {
			seen[m.Category] = true
			out = append(out, string(m.Category))
		}
	}
	return strings.Join(out, ",")
}

// synthesizeHandoffConclusion builds a deterministic, honest hand-off rationale from the observations the
// agent gathered when it reached a cycle limit WITHOUT deciding (TG-60). It records HOW MANY observations
// were investigated and that no grounded decision was reached — giving the operator + judge the context an
// empty hand-off never did — and cites the distinct real observation ids. Pure: no model token becomes the
// text (INV-08); the count/ids come from the agent's OWN captured evidence.
func synthesizeHandoffConclusion(results []ToolResult) (string, []string) {
	seen := map[string]bool{}
	var ids []string
	for _, tr := range results {
		if tr.ID != "" && !seen[tr.ID] {
			seen[tr.ID] = true
			ids = append(ids, tr.ID)
		}
	}
	if len(ids) == 0 {
		return "Reached the investigation budget without a gathered observation or a grounded decision; escalated for human review.", nil
	}
	return fmt.Sprintf("Investigated %d observation(s) but reached the cycle budget without a grounded stop or safe reversible proposal; escalated for human decision.", len(ids)), ids
}

// clipConclusion bounds the model's stop rationale before it enters the session record — untrusted DATA,
// size-capped so a runaway generation cannot bloat the record (it is never parsed or executed).
func clipConclusion(s string) string {
	s = strings.TrimSpace(s)
	const maxConclusion = 500
	if len(s) > maxConclusion {
		return s[:maxConclusion] + "…"
	}
	return s
}

// clipThought bounds the model's optional ReAct/CoT reasoning trace before it enters the session record
// (design-wisdom #7) — untrusted DATA, size-capped so a runaway generation cannot bloat the record. It
// is only recorded for audit/forensics: never parsed, never executed, never a dispatch input (INV-08).
func clipThought(s string) string {
	s = strings.TrimSpace(s)
	const maxThought = 1000
	if len(s) > maxThought {
		return s[:maxThought] + "…"
	}
	return s
}

// gatheredOnly filters the model's cited ids down to observations the agent ACTUALLY captured — a
// fabricated id is dropped, never stored (INV-08/INV-11: the record only ever references real evidence).
func gatheredOnly(cited []string, gathered []ToolResult) []string {
	real := make(map[string]bool, len(gathered))
	for _, tr := range gathered {
		real[tr.ID] = true
	}
	var out []string
	for _, id := range cited {
		// Trim like citesGatheredEvidence does — the two must agree, or an id that PASSES the gate
		// ("tr-1 " with a stray space) would be silently dropped here, recording a stop that claims
		// grounding with an empty evidence list.
		if t := strings.TrimSpace(id); real[t] {
			out = append(out, t)
		}
	}
	return out
}

// citesGatheredEvidence reports whether the proposal's evidence_ids cite at least one observation the agent
// ACTUALLY captured — the grounding check behind the citation gate. Citing nothing, or citing only ids the
// agent never gathered (a fabricated citation), is not grounding and returns false.
func citesGatheredEvidence(cited []string, gathered []ToolResult) bool {
	if len(cited) == 0 || len(gathered) == 0 {
		return false
	}
	ids := make(map[string]struct{}, len(gathered))
	for _, tr := range gathered {
		ids[tr.ID] = struct{}{}
	}
	for _, c := range cited {
		if _, ok := ids[strings.TrimSpace(c)]; ok {
			return true
		}
	}
	return false
}

// firstToolID returns the id of the first captured observation, to name a concrete example in the re-prompt.
func firstToolID(gathered []ToolResult) string {
	if len(gathered) > 0 {
		return gathered[0].ID
	}
	return ""
}
