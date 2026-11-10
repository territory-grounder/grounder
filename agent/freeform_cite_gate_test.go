package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// spec/026 REQ-2606 — "An uncited free-form proposal never reaches a shadow record".
//
// WHY THIS NEEDS ITS OWN ORACLE even though agent/cite_gate_test.go already exercises the gate. Every
// existing citation oracle proposes `restart-service` — a LISTED, actuatable op-class. The free-form
// lane is the one spec/026 opened: when no listed op-class addresses the confirmed fault, the agent
// invents a slug and supplies an undo_sketch, and the runner records the result as a SHADOW proposal
// (workflow.go's no-registered-op-class branch, outcome `proposed:shadow`) that is never executable.
//
// That lane is where an ungrounded proposal is most tempting and least visible: it cannot execute, so
// the reflex is to treat it as harmless. It is not. A shadow record is what the console renders, what
// the earned-op-class catalog counts as evidence toward asking an operator for a capability
// (spec/028 REQ-2802), and therefore what eventually argues for a grant. An uncited free-form proposal
// that reached a shadow record would be ungrounded model text accumulating toward a real permission.
//
// The gate's protection is upstream and structural: the agent never returns a proposal at all, so the
// runner has nothing to record. These oracles pin exactly that, and pin that the refusal is about the
// CITATION rather than about free-form-ness.

// recordingScriptedModel is scriptedModel plus the conversation the loop actually sent, so the
// mechanical re-prompt can be asserted as text rather than inferred from an outcome.
type recordingScriptedModel struct {
	responses []string
	i         int
	sent      [][]model.Message
}

func (m *recordingScriptedModel) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	m.sent = append(m.sent, append([]model.Message(nil), msgs...))
	if m.i >= len(m.responses) {
		return `{"action":"stop","confidence":0.9}`, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

func (m *recordingScriptedModel) userTurns() []string {
	var out []string
	for _, conv := range m.sent {
		for _, msg := range conv {
			if msg.Role == "user" {
				out = append(out, msg.Content)
			}
		}
	}
	return out
}

func newRecordingAgent(m *recordingScriptedModel, lim Limits) *Agent {
	ts := NewReadOnlyToolSet()
	_ = ts.Register(readTool{})
	return &Agent{Model: m, Tools: ts, Limits: lim, ModelName: "primary", User: "agent"}
}

// A FREE-FORM proposal: an op-class slug no actuator registers, carrying the undo_sketch REQ-2604
// requires of one. It cites nothing.
const freeFormUncited = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"renew-certificate","op":"renew","reversible":false,"confidence":0.85,"rationale":"the cert expired","undo_sketch":"restore the previous cert from /etc/ssl/backup and reload nginx"}}`

// The SAME free-form proposal, citing the observation the read tool actually captured.
const freeFormCited = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"renew-certificate","op":"renew","reversible":false,"confidence":0.85,"rationale":"the cert expired","undo_sketch":"restore the previous cert from /etc/ssl/backup and reload nginx","evidence_ids":["tr-1"]}}`

func TestUncitedFreeFormProposalNeverReachesAShadowRecord(t *testing.T) {
	// The agent gathers a real observation, then proposes free-form without citing it — every time.
	m := &recordingScriptedModel{responses: []string{
		toolCall, freeFormUncited, freeFormUncited, freeFormUncited, freeFormUncited, freeFormUncited,
	}}
	res, err := newRecordingAgent(m, Limits{HandoffPoll: 3, HandoffHalt: 10}).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Precondition: an observation WAS gathered, so the gate is live rather than inert. Without this the
	// test would pass for the wrong reason on any future change that stops the tool from being called.
	if len(res.ToolResults) == 0 {
		t.Fatal("precondition: the agent must have gathered an observation, or the citation gate does not fire at all")
	}

	// (1) THE MECHANICAL RE-PROMPT. Asserted as the text the model received, not as an outcome — an
	// outcome alone cannot distinguish "re-prompted" from "silently dropped", and only one of those
	// lets the agent recover.
	var reprompts int
	for _, u := range m.userTurns() {
		if strings.Contains(u, "REJECTED — ungrounded proposal") {
			reprompts++
			if !strings.Contains(u, "tr-1") {
				t.Errorf("the re-prompt must name the real OBSERVATION id the agent may cite, got: %s", u)
			}
		}
	}
	if reprompts == 0 {
		t.Fatal("an uncited free-form proposal must be re-prompted — the agent has to be TOLD what to cite")
	}

	// (2) NO SHADOW RECORD IS POSSIBLE. The runner writes `proposed:shadow` from a returned proposal;
	// there is none. A stubborn agent escalates to a human instead of landing an ungrounded record.
	if res.Outcome == OutcomeProposed {
		t.Fatalf("an uncited free-form proposal must never be accepted — a shadow record would then exist, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Outcome != OutcomeEscalate {
		t.Errorf("a repeat offender escalates to a human rather than grinding, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Proposal.Action.OpClass != "" || res.Proposal.Action.Op != "" {
		t.Fatalf("no proposal may be carried out of the loop — the runner would record it as a shadow proposal: %+v", res.Proposal)
	}
}

// The mirror, so the refusal above is provably about the CITATION and not about the free-form lane
// itself. Free-form is a legitimate, spec/026-opened duty: the SAME slug, the SAME undo_sketch, one
// real evidence id — and it is admitted, carrying its free-form op-class intact for the runner to
// record as a shadow proposal.
func TestACitedFreeFormProposalIsAdmittedWithItsSlugIntact(t *testing.T) {
	m := &recordingScriptedModel{responses: []string{toolCall, freeFormCited}}
	res, err := newRecordingAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != OutcomeProposed {
		t.Fatalf("a CITED free-form proposal must be admitted — the gate checks grounding, not the op-class lane; got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Proposal.Action.OpClass != "renew-certificate" {
		t.Errorf("the free-form slug must survive the loop verbatim (the runner keys the shadow record on it), got %q", res.Proposal.Action.OpClass)
	}
	if res.Proposal.UndoSketch == "" {
		t.Error("REQ-2604 requires an undo_sketch on a free-form proposal, and it must reach the record")
	}
	if len(res.Proposal.EvidenceIDs) == 0 {
		t.Error("the admitted proposal must carry the evidence it was admitted for")
	}
	for _, u := range m.userTurns() {
		if strings.Contains(u, "REJECTED — ungrounded proposal") {
			t.Errorf("a cited proposal must not be re-prompted: %s", u)
		}
	}
}

// A fabricated citation on the free-form lane is not grounding either. Free-form proposals are exactly
// where an invented id is cheapest to produce — nothing executes, so nothing fails loudly — and the
// catalog would still count the record as evidence toward a future grant.
func TestAFreeFormProposalCitingAnInventedIDIsRefused(t *testing.T) {
	const fabricated = `{"action":"propose","confidence":0.85,"proposal":{"external_ref":"TG-1","target":"web01","op_class":"renew-certificate","op":"renew","reversible":false,"confidence":0.85,"undo_sketch":"restore the previous cert","evidence_ids":["lnms-dev-99999"]}}`
	m := &recordingScriptedModel{responses: []string{toolCall, fabricated}}
	res, _ := newRecordingAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome == OutcomeProposed {
		t.Fatal("a free-form proposal citing only an id the agent never captured must not be accepted")
	}
	if res.Proposal.Action.OpClass != "" {
		t.Fatalf("no proposal may leave the loop, got %+v", res.Proposal)
	}
}
