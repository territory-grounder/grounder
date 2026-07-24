package agent

import (
	"context"
	"strings"
	"testing"
)

const (
	stopUncited  = `{"action":"stop","confidence":0.95}`
	stopGrounded = `{"action":"stop","confidence":0.95,"reason":"device is administratively DISABLED — planned downtime, not a fault","evidence_ids":["tr-1"]}`
	stopFakeCite = `{"action":"stop","confidence":0.95,"reason":"looks fine","evidence_ids":["ghost-99"]}`
)

// TestStopNudgedOnceThenGrounded proves REQ-1008: an uncited stop after an observation is re-prompted; the
// grounded re-emission is accepted with its conclusion + verified citations surfaced.
func TestStopNudgedOnceThenGrounded(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, stopUncited, stopGrounded}}
	res, err := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeStop {
		t.Fatalf("want stop, got %s (%s)", res.Outcome, res.Reason)
	}
	if res.Cycles != 3 {
		t.Fatalf("the uncited stop must be nudged exactly once (3 cycles), got %d", res.Cycles)
	}
	if !strings.Contains(res.Conclusion, "DISABLED") {
		t.Fatalf("the grounded conclusion must surface, got %q", res.Conclusion)
	}
	if len(res.ConclusionEvidence) != 1 || res.ConclusionEvidence[0] != "tr-1" {
		t.Fatalf("the verified citation must surface, got %v", res.ConclusionEvidence)
	}
}

// TestStubbornUncitedStopAccepted: a second uncited stop is ACCEPTED — the safe exit is never blocked into
// grinding on (asymmetric with the proposal gate, which escalates a repeat offender).
func TestStubbornUncitedStopAccepted(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, stopUncited, stopUncited}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("a stubborn uncited stop must still be accepted, got %s", res.Outcome)
	}
	if res.Conclusion != "" || len(res.ConclusionEvidence) != 0 {
		t.Fatalf("an uncited stop carries no conclusion evidence, got %q %v", res.Conclusion, res.ConclusionEvidence)
	}
}

// TestStopFabricatedCitationFiltered: a stop citing an id the agent never captured keeps its (data-only)
// reason but the fabricated id is dropped — the record only ever references real evidence (INV-11).
func TestStopFabricatedCitationFiltered(t *testing.T) {
	m := &scriptedModel{responses: []string{toolCall, stopFakeCite, stopFakeCite}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("want stop, got %s", res.Outcome)
	}
	if len(res.ConclusionEvidence) != 0 {
		t.Fatalf("a fabricated citation must be dropped, got %v", res.ConclusionEvidence)
	}
}

// TestStopWithoutObservationsInert: with no tools called there is nothing to cite — the stop is accepted
// immediately and any stated reason still surfaces as data.
func TestStopWithoutObservationsInert(t *testing.T) {
	m := &scriptedModel{responses: []string{`{"action":"stop","confidence":0.9,"reason":"alert-only; no tool available"}`}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop || res.Cycles != 1 {
		t.Fatalf("a stop with no observations must be accepted immediately, got %s cycles=%d", res.Outcome, res.Cycles)
	}
	if !strings.Contains(res.Conclusion, "alert-only") {
		t.Fatalf("the stated reason must surface as data, got %q", res.Conclusion)
	}
}

// TestConclusionSizeCapped: a runaway reason is clipped before entering the record (500 + the 3-byte
// ellipsis rune).
func TestConclusionSizeCapped(t *testing.T) {
	long := strings.Repeat("x", 2000)
	m := &scriptedModel{responses: []string{`{"action":"stop","confidence":0.9,"reason":"` + long + `"}`}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if len(res.Conclusion) > 503 {
		t.Fatalf("conclusion must be size-capped at 500+ellipsis, got %d bytes", len(res.Conclusion))
	}
}

// TestStopCitationWhitespaceTolerated: a cited id with stray whitespace passes the gate AND survives into
// the record — the gate and the filter must agree (the review's trim-inconsistency finding).
func TestStopCitationWhitespaceTolerated(t *testing.T) {
	stopSpacey := `{"action":"stop","confidence":0.9,"reason":"device DISABLED","evidence_ids":[" tr-1 "]}`
	m := &scriptedModel{responses: []string{toolCall, stopSpacey}}
	res, _ := newAgent(m, DefaultLimits()).Run(context.Background(), nil)
	if res.Outcome != OutcomeStop {
		t.Fatalf("want stop, got %s (%s)", res.Outcome, res.Reason)
	}
	if len(res.ConclusionEvidence) != 1 || res.ConclusionEvidence[0] != "tr-1" {
		t.Fatalf("a whitespace-padded real citation must be recorded trimmed, got %v", res.ConclusionEvidence)
	}
}

// TestStopNudgeAtPollLimitEscalates: a stop nudged AT the poll-handoff boundary escalates to a human
// (the nudge falls through to the poll check, mirroring the proposal gate) — bounded, never a grind.
func TestStopNudgeAtPollLimitEscalates(t *testing.T) {
	// HandoffPoll=2: cycle1 tool, cycle2 uncited stop -> nudge -> poll limit reached -> escalate.
	m := &scriptedModel{responses: []string{toolCall, stopUncited, stopGrounded}}
	res, _ := newAgent(m, Limits{HandoffPoll: 2, HandoffHalt: 10}).Run(context.Background(), nil)
	if res.Outcome != OutcomeEscalate {
		t.Fatalf("a nudge at the poll boundary must escalate to a human, got %s (%s)", res.Outcome, res.Reason)
	}
}
