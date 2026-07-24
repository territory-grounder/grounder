package agent

import (
	"context"
	"strings"
	"testing"
)

// stripFences must unwrap a ```json … ``` (or ``` … ```) envelope but leave bare JSON untouched, so a
// markdown-wrapping model (Mistral et al.) still yields a parseable directive.
func TestStripFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"{\"a\":1}", `{"a":1}`},
		{"```json\n{\"action\":\"stop\",\"confidence\":0.9}\n```", `{"action":"stop","confidence":0.9}`},
	}
	for _, c := range cases {
		if got := stripFences(c.in); got != c.want {
			t.Errorf("stripFences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The protocol preamble must be a system message that teaches the exact directive + proposal grammar the
// loop parses — the missing piece an eval found (0% proposals: the model was never told the wire format).
func TestProtocolPreambleContract(t *testing.T) {
	msg := protocolPreamble(nil)
	if msg.Role != "system" {
		t.Fatalf("protocol must be a system message, got role %q", msg.Role)
	}
	for _, want := range []string{`"action"`, `"propose"`, `"tool"`, "external_ref", "evidence_ids", "EXACTLY ONE JSON"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("protocol must mention %q", want)
		}
	}
	// with a tool set, the preamble names the allowlisted tool.
	tools := NewReadOnlyToolSet()
	_ = tools.Register(fenceProbeTool{})
	if !strings.Contains(protocolPreamble(tools).Content, "probe-tool") {
		t.Error("protocol must list the live read-only tools")
	}
}

type fenceProbeTool struct{}

func (fenceProbeTool) Name() string   { return "probe-tool" }
func (fenceProbeTool) ReadOnly() bool { return true }
func (fenceProbeTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	return ToolResult{}, nil
}
