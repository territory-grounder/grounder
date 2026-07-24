package agent

import (
	"context"
	"testing"
)

const toolCall = `{"action":"tool","tool":"get-logs","args":{"host":"web01"},"confidence":0.8}`

// TestCitationGateBouncesUncitedThenAccepts proves REQ-1007: after gathering an observation, a proposal
// citing NO evidence is re-prompted (not accepted); once it cites the real observation id, it is admitted.
func TestCitationGateBouncesUncitedThenAccepts(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, proposeUncited, proposeHigh}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeProposed {
		t.Fatalf("a cited proposal (after the bounce) must be accepted, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Cycles < 3 {
		t.Fatalf("the uncited proposal should have been bounced (>=3 cycles), got %d", res.Cycles)
	}
	if len(res.Proposal.EvidenceIDs) == 0 {
		t.Fatal("the accepted proposal must carry evidence_ids")
	}
}

// TestCitationGateBouncesFabricatedCitation: citing an id the agent never captured is NOT grounding — it is
// bounced exactly like an empty citation (the gate checks the cited ids against the gathered observations).
func TestCitationGateBouncesFabricatedCitation(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, proposeFakeCite}} // then the script exhausts -> stop
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome == OutcomeProposed {
		t.Fatal("a proposal citing only a fabricated id must not be accepted")
	}
}

// TestStubbornUncitedEscalates: an agent that gathers evidence but keeps proposing without citing it never
// lands an ungrounded auto-proposal — it escalates to a human at the poll limit.
func TestStubbornUncitedEscalates(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, proposeUncited, proposeUncited, proposeUncited, proposeUncited, proposeUncited}}
	res, _ := newAgent(m, Limits{HandoffPoll: 3, HandoffHalt: 10}).Run(context.Background(), nil)
	if res.Outcome != OutcomeEscalate {
		t.Fatalf("a never-citing agent must escalate, not land an ungrounded proposal; got %s", res.Outcome)
	}
}

// TestCitationGateInertWithoutObservations: when no tool was called, there is nothing to cite, so an
// uncited proposal is admitted (the gate fires only when the agent actually gathered evidence).
func TestCitationGateInertWithoutObservations(t *testing.T) {
	m := &scriptedModel{responses: []string{proposeUncited}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeProposed {
		t.Fatalf("with no observations gathered, an uncited proposal must be accepted, got %s (%s)", res.Outcome, res.Reason)
	}
}
