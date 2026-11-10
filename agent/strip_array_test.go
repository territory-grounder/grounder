package agent

import (
	"context"
	"testing"
)

// TG-643: the captured 2026-11-07 reply, a valid batched directive inside a one-element array, must parse.
// Everything that is not exactly one object stays unchanged, so it still fails closed on decode (INV-08).
func TestUnwrapSingleDirectiveArray(t *testing.T) {
	cases := []struct{ in, want string }{
		{`[{"action":"stop"}]`, `{"action":"stop"}`},
		{" \n[ {\"action\":\"stop\"} ]\n", `{"action":"stop"}`},
		{`[{"action":"stop"}] trailing noise`, `{"action":"stop"}`},
		{`[]`, `[]`},
		{`[{"action":"stop"},{"action":"tool"}]`, `[{"action":"stop"},{"action":"tool"}]`},
		{`["stop"]`, `["stop"]`},
		{`[[{"action":"stop"}]]`, `[[{"action":"stop"}]]`},
		{`[{"action":"stop"}`, `[{"action":"stop"}`},
		{`{"action":"stop"}`, `{"action":"stop"}`},
		{`TOOL: get-device-status(host=x)`, `TOOL: get-device-status(host=x)`},
	}
	for _, c := range cases {
		if got := unwrapSingleDirectiveArray(c.in); got != c.want {
			t.Errorf("unwrapSingleDirectiveArray(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The envelopes compose: a thinking block, then a fence, then the one-element array.
	if got := stripFences("<thinking>x</thinking>\n```json\n[{\"action\":\"stop\"}]\n```"); got != `{"action":"stop"}` {
		t.Errorf("thinking + fence + one-element array must unwrap all three, got %q", got)
	}
}

// End to end: the captured reply shape runs its read instead of stopping as unparseable.
func TestOneElementArrayDirectiveIsExecuted(t *testing.T) {
	m := &scriptedModel{responses: []string{
		`[` + toolCall + `]`,
		`{"action":"stop","confidence":0.9,"reason":"fine","evidence_ids":["tr-1"]}`,
	}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Reason == "unparseable model output" || len(res.ToolResults) == 0 {
		t.Fatalf("a directive inside a one-element array must parse and run its tool, got %q tools=%d", res.Reason, len(res.ToolResults))
	}
}

// Two directives in one array are ambiguous: the session must still stop as unparseable, running nothing.
func TestTwoElementArrayDirectiveStillFailsClosed(t *testing.T) {
	m := &scriptedModel{responses: []string{
		`[` + toolCall + `,` + toolCall + `]`,
		`[` + toolCall + `,` + toolCall + `]`, // the TG-643 format re-ask gets the same ambiguous answer
	}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Reason != "unparseable model output" || len(res.ToolResults) != 0 {
		t.Fatalf("a two-directive array must fail closed, got %q tools=%d", res.Reason, len(res.ToolResults))
	}
}
