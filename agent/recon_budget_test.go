package agent

// THE READ BUDGET, PROVEN FROM THE LOOP (TG-165).
//
// core/safety holds the meter and its oracles; these hold the two things only the loop can prove: that a
// refused read NEVER reaches the estate and is told to the model in words (a bound that silently returned
// less would produce a confident stand-down over an investigation that never happened), and that EVERY
// dispatch is metered — including the ones that error or come back empty, which is what an enumeration
// probe looks like.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// fakeLimiter admits `allow` reads and refuses everything after, recording every metered dispatch.
type fakeLimiter struct {
	mu       sync.Mutex
	allow    int
	admitted int
	refusals int
	recorded []string // "session|tool|target" per metered dispatch
}

func (l *fakeLimiter) Admit(session string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.admitted >= l.allow {
		l.refusals++
		return errors.New("estate read refused: budget reached — the investigation is INCOMPLETE, not empty")
	}
	l.admitted++
	return nil
}

func (l *fakeLimiter) Record(session, tool, target string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, session+"|"+tool+"|"+target)
}

func (l *fakeLimiter) metered() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.recorded...)
}

// promptRecorder is a scripted model that also keeps every message it was handed, so an oracle can assert
// what the loop actually TOLD the model.
type promptRecorder struct {
	responses []string
	i         int
	mu        sync.Mutex
	seen      []string
}

func (m *promptRecorder) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	m.mu.Lock()
	for _, msg := range msgs {
		m.seen = append(m.seen, msg.Content)
	}
	m.mu.Unlock()
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

func (m *promptRecorder) prompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.seen, "\n")
}

// countingTool records how many times the estate was actually touched.
type countingTool struct {
	mu    sync.Mutex
	calls int
	fail  bool
}

func (*countingTool) Name() string   { return "get-logs" }
func (*countingTool) ReadOnly() bool { return true }
func (c *countingTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	c.mu.Lock()
	c.calls++
	fail := c.fail
	c.mu.Unlock()
	if fail {
		return ToolResult{}, errors.New("ssh: connect: no route to host")
	}
	return ToolResult{ID: "tr-1", Tool: "get-logs", Output: "nginx is down", Success: true}, nil
}

func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// A REFUSED READ NEVER REACHES THE ESTATE, AND THE MODEL IS TOLD WHY.
//
// KILLING MUTATION: delete the `a.Recon.Admit` block in loop.go. RED — the tool is invoked for every
// directive the model emits, so the budget is a counter that bounds nothing, and no refusal ever appears in
// the transcript: a hijacked read lane enumerates the estate while /metrics shows a bound "in force".
func TestAReadRefusedByTheBudgetNeverReachesTheEstateAndIsToldToTheModel(t *testing.T) {
	tool := &countingTool{}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	lim := &fakeLimiter{allow: 2}
	m := &promptRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"), // served
		distinctToolCall("h3"), distinctToolCall("h4"), // refused
		`{"action":"stop","confidence":0.9,"reason":"bounded by the read budget","evidence_ids":["tr-1"]}`,
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "agent", Recon: lim}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := tool.count(); got != 2 {
		t.Fatalf("the estate was touched %d times under a budget of 2 — a refused read must never be "+
			"dispatched, or the bound exists only on paper", got)
	}
	if res.ReconRefusals != 2 {
		t.Fatalf("want 2 recorded refusals on the result, got %d — an operator/judge reading this session "+
			"cannot otherwise tell a truncated investigation from a quiet estate", res.ReconRefusals)
	}
	if !strings.Contains(res.ReconRefusalReason, "INCOMPLETE, not empty") {
		t.Errorf("the first refusal's reason must be kept verbatim on the result, got %q", res.ReconRefusalReason)
	}
	prompt := m.prompt()
	if !strings.Contains(prompt, "TOOL_REFUSED[get-logs]") {
		t.Fatalf("the model was never TOLD the read was refused — it would conclude from a silently shortened "+
			"investigation. Transcript:\n%s", prompt)
	}
	if !strings.Contains(prompt, "INCOMPLETE, not empty") {
		t.Errorf("the refusal shown to the model must say the investigation is incomplete rather than empty")
	}
	// The session is BOUNDED, not aborted: it still reached its own terminal directive on what it had.
	if res.Outcome == OutcomeHardHalt {
		t.Errorf("a budget refusal must not hard-halt the session — it concludes on the evidence in hand; got %v", res.Outcome)
	}
}

// EVERY DISPATCH IS METERED, INCLUDING THE ONES THAT ERROR. A probe against a host that does not answer is
// still a probe against the estate, and a long run of them that find nothing is exactly what a sweep looks
// like from the outside.
//
// KILLING MUTATION: move the `a.Recon.Record` call below the `if err != nil` branch in loop.go. RED —
// probing unreachable or nonexistent hosts becomes free, which is the cheapest enumeration there is.
func TestAnErroringProbeIsStillMeteredAgainstTheReconBudget(t *testing.T) {
	tool := &countingTool{fail: true}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	lim := &fakeLimiter{allow: 100}
	m := &promptRecorder{responses: []string{
		distinctToolCall("nx-host-1"), distinctToolCall("nx-host-2"),
		`{"action":"stop","confidence":0.9}`,
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "agent", Recon: lim}
	if _, err := ag.Run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	metered := lim.metered()
	if len(metered) != 2 {
		t.Fatalf("want 2 metered dispatches for 2 failed probes, got %d (%v) — an unmetered failure is a free "+
			"enumeration channel", len(metered), metered)
	}
	// The estate object must be metered from the ARGS (TG-166's rule), so fan-out is counted even when a
	// failing tool returns no result to stamp.
	if !strings.HasSuffix(metered[0], "|get-logs|nx-host-1") || !strings.HasSuffix(metered[1], "|get-logs|nx-host-2") {
		t.Fatalf("the metered target must come from the call's arguments, got %v", metered)
	}
	// And it must be keyed on the session id the loop stamped, not on the empty shared bucket.
	if sess := strings.SplitN(metered[0], "|", 2)[0]; !strings.HasPrefix(sess, "agent#") {
		t.Fatalf("reads must be metered under the stamped session id (TG-297), got %q", sess)
	}
}

// AN UNWIRED BUDGET IS THE PRE-TG-165 BEHAVIOUR. Every existing caller (oracles, the eval harness) runs with
// Recon nil and must be untouched — no panic, no refusal, no change in what the estate sees.
func TestAnUnwiredReconBudgetLeavesTheLoopExactlyAsItWas(t *testing.T) {
	tool := &countingTool{}
	ts := NewReadOnlyToolSet()
	if err := ts.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	m := &promptRecorder{responses: []string{
		distinctToolCall("h1"), distinctToolCall("h2"),
		`{"action":"stop","confidence":0.9,"reason":"done","evidence_ids":["tr-1"]}`,
	}}
	ag := &Agent{Model: m, Tools: ts, Limits: DefaultLimits(), ModelName: "primary", User: "agent"}
	res, err := ag.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if tool.count() != 2 || res.ReconRefusals != 0 {
		t.Fatalf("an unwired budget must change nothing: calls=%d refusals=%d", tool.count(), res.ReconRefusals)
	}
}
