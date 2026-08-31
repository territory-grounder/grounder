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

// TestUndoSketchIsTheOneAdditiveFieldAndNeverTouchesTheAction — spec/026 REQ-2602 (O-2601 grammar half).
// The grammar accepts a free-form op_class with an OPTIONAL undo_sketch; the sketch lands on the proposal
// RECORD and never inside manifest.Action (INV-07: the action id must not move when a sketch is added).
//
// RED mutation control (executed 2026-07-31): with the `UndoSketch: pj.UndoSketch` copy in ParseProposal
// removed, the "undo_sketch dropped at parse" assertion below fails; restored green.
func TestUndoSketchIsTheOneAdditiveFieldAndNeverTouchesTheAction(t *testing.T) {
	raw := []byte(`{"external_ref":"TG-9","target":"svc01","op_class":"rotate-flux-capacitor",` +
		`"op":"rotate","reversible":true,"rationale":"observed drift","undo_sketch":"rotate back one notch"}`)
	p, err := ParseProposal(raw)
	if err != nil {
		t.Fatalf("the one grammar must accept a free-form op_class with an optional undo_sketch: %v", err)
	}
	if p.UndoSketch != "rotate back one notch" {
		t.Fatalf("undo_sketch dropped at parse: %q", p.UndoSketch)
	}
	if p.Action.OpClass != "rotate-flux-capacitor" {
		t.Fatalf("free-form op_class mangled: %q", p.Action.OpClass)
	}

	// The sketch must not perturb the content-hashed action identity (INV-07): the SAME action with and
	// without a sketch has the SAME action id, because the sketch lives on the record, not the Action.
	noSketch := []byte(`{"external_ref":"TG-9","target":"svc01","op_class":"rotate-flux-capacitor",` +
		`"op":"rotate","reversible":true,"rationale":"observed drift"}`)
	q, err := ParseProposal(noSketch)
	if err != nil {
		t.Fatalf("sketchless parse: %v", err)
	}
	idWith, err := p.Action.ID()
	if err != nil {
		t.Fatalf("action id: %v", err)
	}
	idWithout, err := q.Action.ID()
	if err != nil {
		t.Fatalf("action id: %v", err)
	}
	if idWith != idWithout {
		t.Fatalf("undo_sketch changed the action identity (INV-07 violation): %s != %s", idWith, idWithout)
	}
	if q.UndoSketch != "" {
		t.Fatalf("absent undo_sketch must parse as empty, got %q", q.UndoSketch)
	}
}

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

// TestParseProposalPrediction: the committed falsifiable OUTCOME prediction (axis A2) is parsed, and it is
// OPTIONAL — a proposal without it still parses (empty), so pre-field proposals never fail closed.
func TestParseProposalPrediction(t *testing.T) {
	withPred := `{"external_ref":"TG-1","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.8,"rationale":"nginx dead","prediction":"is-active=active + health 200 within 2 min","evidence_ids":["obs-1"],"approval_choice":""}`
	p, err := ParseProposal([]byte(withPred))
	if err != nil {
		t.Fatalf("valid proposal with prediction must parse: %v", err)
	}
	if p.Prediction != "is-active=active + health 200 within 2 min" {
		t.Fatalf("prediction not parsed: %q", p.Prediction)
	}
	noPred := `{"external_ref":"TG-2","target":"web01","op_class":"restart-service","op":"restart","params":{"unit":"nginx"},"reversible":true,"confidence":0.8,"rationale":"x","evidence_ids":["obs-1"],"approval_choice":""}`
	p2, err := ParseProposal([]byte(noPred))
	if err != nil {
		t.Fatalf("proposal WITHOUT prediction must still parse (optional field): %v", err)
	}
	if p2.Prediction != "" {
		t.Fatalf("absent prediction should be empty, got %q", p2.Prediction)
	}
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
