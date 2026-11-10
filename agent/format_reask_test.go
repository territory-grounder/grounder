package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// TG-643 format re-ask. One malformed reply is answered with a fixed FORMAT_ERROR line and the NEXT reply is
// decoded strictly; the malformed reply itself is never parsed for content.

// msgCapturingModel records every prompt it receives, then answers from responses.
type msgCapturingModel struct {
	responses []string
	i         int
	seen      [][]model.Message
}

func (m *msgCapturingModel) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	m.seen = append(m.seen, append([]model.Message(nil), msgs...))
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// reaskAgent is newAgent over the prompt-capturing model (same read-only tool set).
func reaskAgent(m *msgCapturingModel, lim Limits) *Agent {
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	return &Agent{Model: m, Tools: ts, Limits: lim, ModelName: "primary", User: "agent"}
}

func TestFormatReaskRecoversAndNeverRunsTheMalformedReply(t *testing.T) {
	// The 2026-11-07 shape: a valid directive behind a "TOOL_CALL:" label. It must NOT run: only the
	// re-asked reply is decoded.
	const marker = "zq7-bad-reply-marker"
	m := &msgCapturingModel{responses: []string{
		"TOOL_CALL: " + marker + " " + toolCall,
		`{"action":"stop","confidence":0.9,"reason":"fine","evidence_ids":[]}`,
	}}
	res, _ := reaskAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Reason == "unparseable model output" {
		t.Fatalf("one malformed reply must be re-asked, not stop the session")
	}
	if len(res.ToolResults) != 0 {
		t.Fatalf("the malformed reply's tool call must NEVER run (it is not parsed for content), ran %d", len(res.ToolResults))
	}
	if len(m.seen) != 2 {
		t.Fatalf("expected exactly one re-ask (2 model calls), got %d", len(m.seen))
	}
	second := m.seen[1]
	if n := len(second); n < 2 || second[n-2].Role != "assistant" || second[n-1].Role != "user" || !strings.HasPrefix(second[n-1].Content, "FORMAT_ERROR:") {
		t.Fatalf("the re-ask must send the bad reply back as the assistant turn plus the fixed FORMAT_ERROR line, got %+v", second[len(second)-2:])
	}
	for _, msg := range second {
		if strings.Contains(msg.Content, marker) {
			t.Fatalf("no byte of the bad reply may be re-sent (injection authority, compaction), found it in %q", msg.Content)
		}
	}
	if !strings.Contains(res.Steps[0].Observation, "format re-ask sent") {
		t.Fatalf("the re-asked step must say so on its record, got %q", res.Steps[0].Observation)
	}
}

func TestFormatReaskFiresAtMostOncePerSession(t *testing.T) {
	m := &msgCapturingModel{responses: []string{
		"TOOL_CALL: " + toolCall,  // re-asked
		toolCall,                  // a good tool cycle
		"[Assistant] " + toolCall, // a second malformed reply: no second re-ask, fail closed
	}}
	res, _ := reaskAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Reason != "unparseable model output" {
		t.Fatalf("a second malformed reply in the same session must fail closed, got %q", res.Reason)
	}
	if len(res.ToolResults) != 1 {
		t.Fatalf("only the one well-formed tool call may run, ran %d", len(res.ToolResults))
	}
}

func TestFormatReaskNeverFiresAtThePollLimit(t *testing.T) {
	// At or after the poll limit the TG-60/TG-638 limit handling owns the cycle: no re-ask, fail closed.
	lim := DefaultLimits()
	lim.HandoffPoll = 1
	m := &msgCapturingModel{responses: []string{"TOOL_CALL: " + toolCall}}
	res, _ := reaskAgent(m, lim).Run(context.Background(), nil)
	if res.Reason != "unparseable model output" || len(m.seen) != 1 {
		t.Fatalf("at the poll limit a malformed reply must fail closed with no re-ask, got %q after %d calls", res.Reason, len(m.seen))
	}
}

func TestFormatReaskFiresJustBelowThePollLimit(t *testing.T) {
	// The other boundary: on the cycle before the poll limit the re-ask still fires.
	lim := DefaultLimits()
	lim.HandoffPoll = 2
	m := &msgCapturingModel{responses: []string{"TOOL_CALL: " + toolCall, `{"action":"stop","confidence":0.9,"reason":"fine","evidence_ids":[]}`}}
	res, _ := reaskAgent(m, lim).Run(context.Background(), nil)
	if res.Reason == "unparseable model output" || len(m.seen) != 2 {
		t.Fatalf("on cycle 1 with the poll limit at 2 the re-ask must fire, got %q after %d calls", res.Reason, len(m.seen))
	}
}

// The flag is checkpointed: a run RESUMED after the re-ask was spent must not re-ask again (a durable resume
// neither repeats nor skips it).
func TestFormatReaskFlagSurvivesADurableResume(t *testing.T) {
	var last *Checkpoint
	hooks := &CheckpointHooks{Emit: func(c Checkpoint) { cc := c; last = &cc }}
	m := &msgCapturingModel{responses: []string{"TOOL_CALL: " + toolCall, toolCall}}
	a := reaskAgent(m, DefaultLimits())
	a.Checkpoint = hooks
	_, _ = a.Run(context.Background(), nil)
	if last == nil || !last.FormatReasked {
		t.Fatalf("the checkpoint emitted after a re-ask must carry FormatReasked=true, got %+v", last)
	}
	// Resume from that checkpoint with a model that now answers malformed: no second re-ask, fail closed.
	m2 := &msgCapturingModel{responses: []string{"[Assistant] " + toolCall}}
	b := reaskAgent(m2, DefaultLimits())
	b.Checkpoint = &CheckpointHooks{Resume: last}
	res, _ := b.Run(context.Background(), nil)
	if res.Reason != "unparseable model output" || len(m2.seen) != 1 {
		t.Fatalf("a resumed run that already spent its re-ask must fail closed with no second re-ask, got %q after %d calls", res.Reason, len(m2.seen))
	}
}
