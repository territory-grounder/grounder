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
	msg := protocolPreamble(nil, "", "")
	if msg.Role != "system" {
		t.Fatalf("protocol must be a system message, got role %q", msg.Role)
	}
	// `"tools":[` is the TG-49 batched-dispatch grammar: the loop accepts a batch only if the preamble
	// TEACHES it, or the capability exists on the wire but no model ever learns to use it.
	for _, want := range []string{`"action"`, `"propose"`, `"tool"`, `"tools":[`, "external_ref", "evidence_ids", "EXACTLY ONE JSON"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("protocol must mention %q", want)
		}
	}
	// with a tool set, the preamble names the allowlisted tool.
	tools := NewReadOnlyToolSet()
	_ = tools.Register(fenceProbeTool{})
	if !strings.Contains(protocolPreamble(tools, "", "").Content, "probe-tool") {
		t.Error("protocol must list the live read-only tools")
	}
}

type fenceProbeTool struct{}

func (fenceProbeTool) Name() string   { return "probe-tool" }
func (fenceProbeTool) ReadOnly() bool { return true }
func (fenceProbeTool) Invoke(_ context.Context, _ map[string]string) (ToolResult, error) {
	return ToolResult{}, nil
}

// spec/026 REQ-2601(b) (T-026-1) — the preamble carries the open-proposal-plane contract: the grammar
// teaches undo_sketch, the free-form duty is declared (record-only, no substitution, no stand-down for a
// missing verb), and actor evidence is explicitly non-suppressive. The anchors live in the general schema
// text, so protocolPreamble(nil) exercises them without a tool set.
func TestProtocolPreambleDeclaresTheFreeFormProposeDuty(t *testing.T) {
	content := protocolPreamble(nil, "", "").Content
	for _, want := range []string{
		`"undo_sketch"`,                // the one additive grammar field is taught (REQ-2602 surface)
		"free-form op_class slug",      // the duty exists in the schema text
		"RECORDED for operator review", // record-only honesty — the model must not believe it executes
		"never a substitute",           // the anti-substitution law survives
		"never suppresses a proposal",  // REQ-2609 in the wire-format contract
	} {
		if !strings.Contains(content, want) {
			t.Errorf("preamble must contain %q (open proposal plane contract)", want)
		}
	}
}

// TG-201 — A DIMENSION MAY ONLY GRADE WHAT THE AGENT WAS ASKED FOR.
//
// core/judge scores `diagnosis_grounded` off the typed claim, and part of that score is the ABSENCE of a
// diagnosis under a causal claim. That penalty is only legitimate if the grammar offers the field: grading
// a model down for withholding something it was never told to emit is not a rubric, it is a trap — and it
// would floor the axis across every live session at once, which is precisely how the flywheel's Regressed
// trigger once fired for every skill (TG-61).
//
// KILLING MUTATION: delete the "diagnosis" object from the propose schema in protocolPreamble. RED — the
// judge would then charge every proposal 2/5 for a field the agent was never offered.
func TestProtocolPreambleAsksForTheTypedDiagnosis(t *testing.T) {
	content := protocolPreamble(nil, "", "").Content
	// The exact wire keys core/proposal's parser reads — a near-miss key parses to an EMPTY diagnosis and
	// scores as if the agent withheld one, so the schema text and the parser must name the same fields.
	for _, want := range []string{
		`"diagnosis"`, `"root_cause"`, `"mechanism"`, `"supporting"`, `"contradicting"`, `"ruled_out"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("preamble must teach the typed-diagnosis key %q — the judge scores this field, and the "+
				"model can only fill a field it was shown", want)
		}
	}
	// The honesty half. If the preamble asked for a diagnosis without saying that disclosing disconfirming
	// evidence is FREE and that "cause unknown" is acceptable, the cheapest way for a model to raise its
	// score would be to hide contradictions and invent a confident cause — the rubric would be paying for
	// the fabrication the typed claim exists to expose.
	for _, want := range []string{
		"argues AGAINST your own root cause",
		"never costs you anything",
		`leave "root_cause" empty`,
		"Never invent a confident cause",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("preamble must state %q — an ask for a diagnosis without an explicit licence to admit "+
				"uncertainty trains the model to fabricate confidence", want)
		}
	}
}
