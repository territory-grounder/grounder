package proposal

import (
	"errors"
	"testing"
)

// validToolCall is the ONE accepted grammar: a single schema-valid JSON tool-call.
const validToolCall = `{
  "external_ref": "TG-4617",
  "target": "web01",
  "op_class": "restart-service",
  "op": "restart",
  "params": {"unit": "nginx"},
  "reversible": true,
  "approval_choice": "AUTO-RESOLVE",
  "confidence": 0.82,
  "rationale": "service down; restart is reversible",
  "evidence_ids": ["tr-1", "tr-2"]
}`

func TestParseProposalAcceptsTheOneGrammar(t *testing.T) {
	p, err := ParseProposal([]byte(validToolCall))
	if err != nil {
		t.Fatalf("the one valid grammar must parse, got %v", err)
	}
	if p.ExternalRef != "TG-4617" || p.Action.Target != "web01" || p.Action.OpClass != "restart-service" {
		t.Fatalf("unexpected proposal: %+v", p)
	}
	if p.Confidence != 0.82 || p.ApprovalChoice != "AUTO-RESOLVE" {
		t.Fatalf("scalar fields not parsed: %+v", p)
	}
	// ApprovalChoice is DATA, not authority — parsing it does not grant anything; a gate decides.
}

// TestNoSecondGrammarAccepts enumerates the parser's rejection paths: no markdown, sentinel marker,
// alternate grammar, unknown field, or trailing second object is ever accepted. This is the property
// that closes H-02 (the predecessor's second "Which plan? - Plan X:" grammar after the gate).
func TestNoSecondGrammarAccepts(t *testing.T) {
	rejected := []struct {
		name string
		in   string
	}{
		{"empty", ``},
		{"markdown with AUTO-RESOLVE marker", "Here is my plan.\n\n[AUTO-RESOLVE] restart nginx"},
		{"markdown with POLL marker", "[POLL] awaiting approval"},
		{"the predecessor second grammar", "Which plan? - Plan A: restart - Plan B: reboot"},
		{"which-approach grammar", "Which approach do you prefer? Approach 1..."},
		{"plaintext choice", "Plan A"},
		{"unknown field smuggled", `{"external_ref":"TG-1","target":"h","op_class":"c","op":"o","evil":"run rm -rf"}`},
		{"trailing second object", validToolCall + "\n" + `{"external_ref":"TG-2","target":"h2","op_class":"c","op":"o"}`},
		{"json array not object", `[{"external_ref":"TG-1"}]`},
		{"bare string", `"just a string"`},
		{"number", `42`},
		{"truncated json", `{"external_ref":"TG-1",`},
		{"missing external_ref", `{"target":"h","op_class":"c","op":"o"}`},
		{"missing target", `{"external_ref":"TG-1","op_class":"c","op":"o"}`},
		{"missing op_class", `{"external_ref":"TG-1","target":"h","op":"o"}`},
		{"missing op", `{"external_ref":"TG-1","target":"h","op_class":"c"}`},
	}
	for _, c := range rejected {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseProposal([]byte(c.in)); err == nil {
				t.Fatalf("input %q was ACCEPTED — a second grammar must never parse", c.in)
			}
		})
	}
}

func TestParseProposalConfidenceRange(t *testing.T) {
	for _, bad := range []string{
		`{"external_ref":"TG-1","target":"h","op_class":"c","op":"o","confidence":1.5}`,
		`{"external_ref":"TG-1","target":"h","op_class":"c","op":"o","confidence":-0.1}`,
	} {
		if _, err := ParseProposal([]byte(bad)); !errors.Is(err, ErrConfidenceRange) {
			t.Fatalf("out-of-range confidence must fail closed, got %v", err)
		}
	}
}

func TestParseProposalErrorClasses(t *testing.T) {
	if _, err := ParseProposal([]byte(`not json`)); !errors.Is(err, ErrUnparseable) {
		t.Fatalf("markdown must be ErrUnparseable, got %v", err)
	}
	if _, err := ParseProposal([]byte(`{"external_ref":"TG-1","target":"h","op_class":"c"}`)); !errors.Is(err, ErrIncompleteProposal) {
		t.Fatalf("missing field must be ErrIncompleteProposal, got %v", err)
	}
}
