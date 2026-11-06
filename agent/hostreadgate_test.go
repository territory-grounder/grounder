package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// TG-638: a SERVICE alert must not stand down on monitoring data alone.

type hostSvcTool struct{}

func (hostSvcTool) Name() string   { return "check-host-services" }
func (hostSvcTool) ReadOnly() bool { return true }
func (hostSvcTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	return ToolResult{ID: "hs-1", Tool: "check-host-services", Output: "derived: app [paused]", Success: true}, nil
}

// msgRecModel replays scripted responses and keeps the conversation it was shown last.
type msgRecModel struct {
	scriptedModel
	last []model.Message
}

func (m *msgRecModel) Complete(ctx context.Context, a, b string, msgs []model.Message) (string, error) {
	m.last = append([]model.Message(nil), msgs...)
	return m.scriptedModel.Complete(ctx, a, b, msgs)
}

const (
	hostCall      = `{"action":"tool","tool":"check-host-services","args":{"host":"site01app01"},"confidence":0.8}`
	stopStale     = `{"action":"stop","confidence":0.9,"reason":"device UP, alert looks stale","evidence_ids":["tr-1"]}`
	stopAfterHost = `{"action":"stop","confidence":0.9,"reason":"host shows every service running","evidence_ids":["hs-1"]}`
)

func gatedAgent(m Completer, withTool bool) *Agent {
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	if withTool {
		_ = ts.Register(hostSvcTool{})
	}
	return &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "agent",
		HostReadGate: &HostReadGate{Tool: "check-host-services", Host: "site01app01"}}
}

func TestHostReadGate_HoldsAServiceStopOnceForTheHostRead(t *testing.T) {
	m := &msgRecModel{scriptedModel: scriptedModel{responses: []string{toolCall, stopStale, hostCall, stopAfterHost}}}
	res, err := gatedAgent(m, true).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStop || res.Cycles != 4 {
		t.Fatalf("want a stop after the held stop and the host read (4 cycles), got %s cycles=%d", res.Outcome, res.Cycles)
	}
	read := false
	for _, r := range res.ToolResults {
		if r.Tool == "check-host-services" {
			read = true
		}
	}
	if !read {
		t.Fatal("the host's service state must have been read before the stop was accepted")
	}
	nudged := false
	for _, msg := range m.last {
		if msg.Role == "user" && strings.Contains(msg.Content, "STOP NOT YET ACCEPTED") &&
			strings.Contains(msg.Content, `"tool":"check-host-services"`) && strings.Contains(msg.Content, `"host":"site01app01"`) {
			nudged = true
		}
	}
	if !nudged {
		t.Fatal("the held stop must be answered with the exact host-services call for the incident host")
	}
}

// The safe exit is never blocked: a second stop is accepted even without the read.
func TestHostReadGate_AcceptsARepeatStop(t *testing.T) {
	m := &scriptedModel{responses: []string{stopStale, stopStale}}
	res, _ := gatedAgent(m, true).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop || res.Cycles != 2 {
		t.Fatalf("a repeat stop must be accepted on the second cycle, got %s cycles=%d", res.Outcome, res.Cycles)
	}
}

func TestHostReadGate_SilentOnceTheHostWasRead(t *testing.T) {
	m := &scriptedModel{responses: []string{hostCall, stopAfterHost}}
	res, _ := gatedAgent(m, true).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop || res.Cycles != 2 {
		t.Fatalf("a stop after the host read must be accepted at once, got %s cycles=%d", res.Outcome, res.Cycles)
	}
}

// No gate, or a gate whose tool is not in this session's tool set: the loop is unchanged.
func TestHostReadGate_InertWithoutTheGateOrTheTool(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, stopStale}}
	res, _ := gatedAgent(m, false).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop || res.Cycles != 2 {
		t.Fatalf("a gate whose tool is absent must not hold the stop, got %s cycles=%d", res.Outcome, res.Cycles)
	}
	m2 := &scriptedModel{responses: []string{toolCall, stopStale}}
	res2, _ := newAgent(m2, DefaultLimits()).Run(context.Background(), nil)
	if res2.Outcome != OutcomeStop || res2.Cycles != 2 {
		t.Fatalf("no gate must leave the loop unchanged, got %s cycles=%d", res2.Outcome, res2.Cycles)
	}
}

// TG-638's second shape: the agent never stops. It keeps reading monitoring tools until the hand-off poll
// limit, so the stop hold never fires. At the limit it gets ONE host-read cycle, then the TG-60 decide nudge.
// monitoringReads returns n distinct monitoring reads (distinct args, so the trajectory veto's repeat
// detector does not end the session first).
func monitoringReads(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`{"action":"tool","tool":"get-logs","args":{"host":"web%02d"},"confidence":0.8}`, i)
	}
	return out
}

func handoffNudged(msgs []model.Message) (host, decide int) {
	for _, msg := range msgs {
		if msg.Role != "user" {
			continue
		}
		if strings.Contains(msg.Content, "HOST READ REQUIRED") && strings.Contains(msg.Content, `"tool":"check-host-services"`) &&
			strings.Contains(msg.Content, `"host":"site01app01"`) {
			host++
		}
		if strings.Contains(msg.Content, "DECISION REQUIRED") {
			decide++
		}
	}
	return host, decide
}

func TestHostReadGate_HandoffLimitAsksForTheHostReadThenTheDecision(t *testing.T) {
	lim := DefaultLimits()
	script := append(monitoringReads(lim.HandoffPoll), hostCall, stopAfterHost)
	m := &msgRecModel{scriptedModel: scriptedModel{responses: script}}
	res, err := gatedAgent(m, true).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStop || res.Cycles != lim.HandoffPoll+2 {
		t.Fatalf("want a stop on the decision cycle after the host read (%d cycles), got %s cycles=%d (%s)",
			lim.HandoffPoll+2, res.Outcome, res.Cycles, res.Reason)
	}
	if host, decide := handoffNudged(m.last); host != 1 || decide != 1 {
		t.Fatalf("want exactly one host-read nudge then one decide nudge, got host=%d decide=%d", host, decide)
	}
	read := false
	for _, r := range res.ToolResults {
		read = read || r.Tool == "check-host-services"
	}
	if !read {
		t.Fatal("the host read must have run before the decision")
	}
}

// The extra cycle is bounded: an agent that ignores both nudges still hands off, one cycle later than before.
func TestHostReadGate_HandoffNudgeAddsAtMostOneCycle(t *testing.T) {
	lim := DefaultLimits()
	m := &msgRecModel{scriptedModel: scriptedModel{responses: monitoringReads(lim.HandoffHalt)}}
	res, _ := gatedAgent(m, true).Run(context.Background(), nil)
	if res.Outcome != OutcomeEscalate || res.Cycles != lim.HandoffPoll+2 {
		t.Fatalf("want the hand-off escalation one cycle after the ungated one (%d), got %s cycles=%d",
			lim.HandoffPoll+2, res.Outcome, res.Cycles)
	}
	if host, decide := handoffNudged(m.last); host != 1 || decide != 1 {
		t.Fatalf("each nudge fires once, got host=%d decide=%d", host, decide)
	}
	ungated := &scriptedModel{responses: monitoringReads(lim.HandoffHalt)}
	res2, _ := newAgent(ungated, lim).Run(context.Background(), nil)
	if res2.Outcome != OutcomeEscalate || res2.Cycles != lim.HandoffPoll+1 {
		t.Fatalf("without the gate the hand-off is unchanged (%d cycles), got %s cycles=%d", lim.HandoffPoll+1, res2.Outcome, res2.Cycles)
	}
}

// Silent when the host was already read, and when the stop hold already spent the once-only nudge.
func TestHostReadGate_HandoffSilentWhenReadOrAlreadyNudged(t *testing.T) {
	lim := DefaultLimits()
	read := &msgRecModel{scriptedModel: scriptedModel{responses: append([]string{hostCall}, monitoringReads(lim.HandoffHalt)...)}}
	if res, _ := gatedAgent(read, true).Run(context.Background(), nil); res.Cycles != lim.HandoffPoll+1 {
		t.Fatalf("host already read: the hand-off must be unchanged, got cycles=%d", res.Cycles)
	}
	if host, _ := handoffNudged(read.last); host != 0 {
		t.Fatalf("host already read: no host-read nudge, got %d", host)
	}
	held := &msgRecModel{scriptedModel: scriptedModel{responses: append([]string{stopStale}, monitoringReads(lim.HandoffHalt)...)}}
	if res, _ := gatedAgent(held, true).Run(context.Background(), nil); res.Cycles != lim.HandoffPoll+1 {
		t.Fatalf("stop hold already fired: the hand-off must be unchanged, got cycles=%d", res.Cycles)
	}
	if host, _ := handoffNudged(held.last); host != 0 {
		t.Fatalf("stop hold already fired: no second host-read nudge, got %d", host)
	}
}

// TG-638's third shape (soak 2026-11-06 19:34): the agent STOPS exactly at the poll limit. The stop is held for the
// host read, and the held cycle must not fall through to the hand-off escalation: the next cycle makes the read,
// then the decide-now nudge, then the decision.
func TestHostReadGate_StopHeldAtThePollLimitGetsTheRead(t *testing.T) {
	lim := DefaultLimits()
	script := append(monitoringReads(lim.HandoffPoll-1), stopStale, hostCall, stopAfterHost)
	m := &msgRecModel{scriptedModel: scriptedModel{responses: script}}
	res, err := gatedAgent(m, true).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStop || res.Cycles != lim.HandoffPoll+2 {
		t.Fatalf("want the stop accepted after the host read (%d cycles), got %s cycles=%d (%s)", lim.HandoffPoll+2, res.Outcome, res.Cycles, res.Reason)
	}
	read := false
	for _, r := range res.ToolResults {
		read = read || r.Tool == "check-host-services"
	}
	if !read {
		t.Fatal("the held stop at the limit must have led to the host read")
	}
}
