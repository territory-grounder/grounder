package agent

import (
	"context"
	"testing"
)

// TG-643: the captured soak reply, a valid directive behind a <thinking> block, must parse.
func TestStripLeadingThinking(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<thinking>Let me check.</thinking>{"action":"stop"}`, `{"action":"stop"}`},
		{"  <Thinking>\nmulti\nline\n</Thinking>\n```json\n{\"action\":\"stop\"}\n```", "```json\n{\"action\":\"stop\"}\n```"},
		{`<think>x</think> {"action":"stop"}`, `{"action":"stop"}`},
		{`<thinking>never closed {"action":"stop"}`, `<thinking>never closed {"action":"stop"}`},
		{`prose first <thinking>x</thinking>{"action":"stop"}`, `prose first <thinking>x</thinking>{"action":"stop"}`},
		{`{"action":"stop"}`, `{"action":"stop"}`},
	}
	for _, c := range cases {
		if got := stripLeadingThinking(c.in); got != c.want {
			t.Errorf("stripLeadingThinking(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := stripFences("<thinking>x</thinking>\n```json\n{\"action\":\"stop\"}\n```"); got != `{"action":"stop"}` {
		t.Errorf("a thinking block before a fenced directive must unwrap both, got %q", got)
	}
}

// End to end: the captured reply shape no longer stops the session as unparseable.
func TestThinkingPrefixedDirectiveIsExecuted(t *testing.T) {
	m := &scriptedModel{responses: []string{
		`<thinking>Let me check current live state before deciding.</thinking>` + toolCall,
		`{"action":"stop","confidence":0.9,"reason":"fine","evidence_ids":["tr-1"]}`,
	}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Reason == "unparseable model output" || len(res.ToolResults) == 0 {
		t.Fatalf("a directive behind a thinking block must parse and run its tool, got %q tools=%d", res.Reason, len(res.ToolResults))
	}
}
