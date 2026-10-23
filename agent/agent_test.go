package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// scriptedModel returns a fixed sequence of responses — a deterministic stand-in for the LiteLLM
// gateway (CI has no live model). After the script is exhausted it returns a stop directive.
type scriptedModel struct {
	responses []string
	i         int
}

func (m *scriptedModel) Complete(_ context.Context, _, _ string, _ []model.Message) (string, error) {
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

type readTool struct{}

func (readTool) Name() string   { return "get-logs" }
func (readTool) ReadOnly() bool { return true }
func (readTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	return ToolResult{ID: "tr-1", Tool: "get-logs", Output: "nginx is down", Success: true}, nil
}

type writeTool struct{}

func (writeTool) Name() string   { return "restart-service" }
func (writeTool) ReadOnly() bool { return false }
func (writeTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	return ToolResult{}, nil
}

func newAgent(model *scriptedModel, lim Limits) *Agent {
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	return &Agent{Model: model, Tools: ts, Limits: lim, ModelName: "primary", User: "agent"}
}

// proposeHigh is a GROUNDED high-confidence proposal — it cites tr-1, the id readTool captures — so it
// passes the citation gate after a tool call (and, with no tools gathered, the gate simply does not fire).
const proposeHigh = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.85,"evidence_ids":["tr-1"]}}`

// proposeUncited gathered observations but cites none; proposeFakeCite cites an id it never captured. Both
// are ungrounded and must be bounced by the citation gate when observations exist.
const proposeUncited = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.85}}`
const proposeFakeCite = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","reversible":true,"confidence":0.85,"evidence_ids":["ghost-id-99"]}}`

// modelRecorder records the model tier used for each Complete call — to prove the forced-decision cycle
// runs on the capable DecisionModelName (TG-60), not the fast investigation tier.
type modelRecorder struct {
	responses []string
	i         int
	models    []string
}

func (m *modelRecorder) Complete(_ context.Context, _, modelName string, _ []model.Message) (string, error) {
	m.models = append(m.models, modelName)
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// TestDecisionCycleUsesDecisionModel: the 5 investigation cycles use "fast"; the poll-limit forced-decision
// cycle uses the capable DecisionModelName ("primary") so it actually obeys "decide now" (TG-60).
func TestDecisionCycleUsesDecisionModel(t *testing.T) {
	stopGrounded := `{"action":"stop","confidence":0.9,"reason":"logrotate reclaimed the disk","evidence_ids":["tr-1"]}`
	m := &modelRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
		stopGrounded,
	}}
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "fast", DecisionModelName: "primary", User: "t"}
	if _, err := ag.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(m.models) < 6 {
		t.Fatalf("expected 6 model calls (5 tool + 1 decision), got %d: %v", len(m.models), m.models)
	}
	for i := 0; i < 5; i++ {
		if m.models[i] != "fast" {
			t.Fatalf("investigation cycle %d must use the fast tier, got %q", i+1, m.models[i])
		}
	}
	if m.models[5] != "primary" {
		t.Fatalf("the forced-decision cycle must use DecisionModelName 'primary', got %q", m.models[5])
	}
}

// tool builds a distinct-arg read-tool directive so the trajectory veto (repeated step) does not fire and
// the loop can reach the poll-handoff limit.
func distinctToolCall(host string) string {
	return `{"action":"tool","tool":"get-logs","args":{"host":"` + host + `"},"confidence":0.8}`
}

// TestDecideNudgeConvergesToGroundedStop: at the poll limit (5 cycles) the agent, nudged to DECIDE (TG-60),
// stops with a grounded reason instead of handing off with an empty conclusion.
func TestDecideNudgeConvergesToGroundedStop(t *testing.T) {
	stopGrounded := `{"action":"stop","confidence":0.9,"reason":"logrotate already reclaimed the disk; no action warranted","evidence_ids":["tr-1"]}`
	m := &scriptedModel{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"),
		stopGrounded, // emitted on the final nudged cycle
	}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStop {
		t.Fatalf("the decide-now nudge must let the agent converge to a grounded stop, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Conclusion == "" {
		t.Fatal("the converged stop must carry a grounded conclusion, not an empty hand-off")
	}
}

// TestDecideNudgeStillEscalatesIfNoDecision: the nudge fires ONCE; if the agent keeps investigating, it
// escalates as before (never grinds past the budget).
func TestDecideNudgeStillEscalatesIfNoDecision(t *testing.T) {
	m := &scriptedModel{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), distinctToolCall("h3"), distinctToolCall("h4"), distinctToolCall("h5"), distinctToolCall("h6"), distinctToolCall("h7"),
	}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeEscalate {
		t.Fatalf("without a decision after the nudge, the loop must escalate, got %s (%s)", res.Outcome, res.Reason)
	}
	// TG-60 option 2: the hand-off is NEVER conclusion-less — it carries a synthesized rationale + the
	// distinct observation ids the agent gathered (here: tr-1, deduped from repeated readTool calls).
	if res.Conclusion == "" {
		t.Fatal("an escalated hand-off must carry a synthesized (non-empty) conclusion")
	}
	if len(res.ConclusionEvidence) != 1 || res.ConclusionEvidence[0] != "tr-1" {
		t.Fatalf("the hand-off conclusion must cite the gathered observation ids, got %v", res.ConclusionEvidence)
	}
}

func TestAgentDrivesToolThenProposes(t *testing.T) {
	m := &scriptedModel{responses: []string{
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,
		proposeHigh,
	}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProposed {
		t.Fatalf("want proposed, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Proposal.ExternalRef != "TG-1" || res.Proposal.Action.Op != "restart" {
		t.Fatalf("proposal not parsed via ParseProposal: %+v", res.Proposal)
	}
	if len(res.ToolResults) != 1 || res.ToolResults[0].ID != "tr-1" {
		t.Fatalf("read-only tool result not captured: %+v", res.ToolResults)
	}
}

// TestUnknownToolRecoversAndRetries: a mis-named READ tool (the real failure that stood down a service-
// fault triage — the model reached for "get-host-services" when the diagnostic is "check-host-services")
// becomes an actionable TOOL_ERROR, NOT a session abort. The model then names a VALID tool, grounds, and
// proposes. The unknown name is still never dispatched (INV-08) — recovery does not weaken fail-closed.
func TestUnknownToolRecoversAndRetries(t *testing.T) {
	m := &scriptedModel{responses: []string{
		`{"action":"tool","tool":"get-host-services","confidence":0.9}`,                 // unknown ⇒ TOOL_ERROR, recover
		`{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`,  // valid ⇒ tr-1
		proposeHigh,
	}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProposed {
		t.Fatalf("after recovering from an unknown tool name, the agent must retry a valid tool and propose; got %s (%s)", res.Outcome, res.Reason)
	}
	if len(res.ToolResults) != 1 || res.ToolResults[0].ID != "tr-1" {
		t.Fatalf("the unknown tool must not dispatch; only the valid retry captures a result: %+v", res.ToolResults)
	}
}

func TestWriteToolStructurallyWithheld(t *testing.T) {
	ts := NewReadOnlyToolSet()
	if err := ts.Register(writeTool{}); !errors.Is(err, ErrWriteToolWithheld) {
		t.Fatalf("a write tool must be withheld, got %v", err)
	}
	_ = ts.Register(readTool{})
	if !ts.AllReadOnly() {
		t.Fatal("the phase-1 tool set must be entirely read-only")
	}
	if _, ok := ts.Get("restart-service"); ok {
		t.Fatal("the withheld write tool must be absent from the set")
	}
}

func TestLowConfidenceStops(t *testing.T) {
	m := &scriptedModel{responses: []string{`{"action":"propose","confidence":0.4,"proposal":{}}`}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("confidence below stop threshold must stop, got %s", res.Outcome)
	}
}

func TestEscalateBelowThreshold(t *testing.T) {
	m := &scriptedModel{responses: []string{
		`{"action":"propose","confidence":0.6,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","confidence":0.6}}`,
	}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeEscalate {
		t.Fatalf("confidence in [0.5,0.7) must escalate, got %s", res.Outcome)
	}
	if res.Proposal.ExternalRef != "TG-1" {
		t.Fatal("an escalated proposal should still be parsed and returned")
	}
}

func TestUnparseableFailsClosed(t *testing.T) {
	m := &scriptedModel{responses: []string{"here is my markdown plan, no JSON"}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("unparseable output must fail closed to stop, got %s", res.Outcome)
	}
}

func TestUnknownToolAndMetacharsNeverExecute(t *testing.T) {
	// a tool name with shell metacharacters is not in the allowlist ⇒ exact-lookup miss ⇒ stop.
	// This proves dispatch is by validated name, never by executing model text (INV-08).
	m := &scriptedModel{responses: []string{`{"action":"tool","tool":"get-logs; rm -rf /","confidence":0.9}`}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("an unknown/injection tool name must not execute; want stop, got %s", res.Outcome)
	}
}

func TestUnknownActionFailsClosed(t *testing.T) {
	m := &scriptedModel{responses: []string{`{"action":"execute-now","confidence":0.99}`}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("an unknown action must fail closed, got %s", res.Outcome)
	}
}

// tool builds a get-logs call with a DISTINCT host arg per cycle, so the agent makes progress (never
// proposing) and reaches the cycle/poll limit WITHOUT tripping the trajectory loop-veto (which fires only on
// identical repeats — see TestTrajectoryVeto).
func distinctToolCalls(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = `{"action":"tool","tool":"get-logs","args":{"host":"h` + itoa(i) + `"},"confidence":0.9}`
	}
	return out
}

func TestHardHaltOnCycleLimit(t *testing.T) {
	m := &scriptedModel{responses: distinctToolCalls(5)}
	res, _ := newAgent(m, Limits{HandoffPoll: 100, HandoffHalt: 3}).Run(context.Background(), nil)
	if res.Outcome != OutcomeHardHalt {
		t.Fatalf("reaching the cycle limit must hard-halt, got %s (cycles=%d)", res.Outcome, res.Cycles)
	}
}

func TestHandoffPollLimitEscalates(t *testing.T) {
	m := &scriptedModel{responses: distinctToolCalls(6)}
	res, _ := newAgent(m, Limits{HandoffPoll: 3, HandoffHalt: 10}).Run(context.Background(), nil)
	if res.Outcome != OutcomeEscalate {
		t.Fatalf("reaching the handoff poll limit must escalate, got %s", res.Outcome)
	}
	// The handoff-limit escalate carries NO proposal: res.Proposal is the ZERO value (an EMPTY action). The
	// Runner boundary relies on this — an escalate without a validated non-empty action is treated as
	// no-proposal (never sealed/predicted/polled). A phantom proposal here would resurrect the empty-action
	// approval-poll defect, so this is a governance invariant, not a cosmetic check.
	if res.Proposal.ExternalRef != "" || res.Proposal.Action.Target != "" ||
		res.Proposal.Action.OpClass != "" || res.Proposal.Action.Op != "" {
		t.Fatalf("a handoff-limit escalate must carry no phantom proposal, got %+v", res.Proposal)
	}
}

func TestParseConfidenceProseFallback(t *testing.T) {
	if v, ok := ParseConfidence(`I think CONFIDENCE: 0.9 here`); !ok || v != 0.9 {
		t.Fatalf("prose confidence not parsed: v=%v ok=%v", v, ok)
	}
	if _, ok := ParseConfidence(`no scalar here`); ok {
		t.Fatal("absent confidence must report ok=false (treated as 0, fail closed)")
	}
}
