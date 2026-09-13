package runner

import (
	"errors"
	"strings"
	"testing"
)

// TestFailedInvestigationLeavesADurableRecord is MECH-003.
//
// This path returned bare for its whole life: no triage row, no ledger entry, no escalation. A CRITICAL
// incident whose investigation crashed left a failed Temporal workflow and nothing else — invisible to
// the judge, the console, the eval, and every operator surface. The branch immediately below it in
// workflow.go already refuses to conflate "a model call failed" with "no action warranted"; this closes
// the same gap one level up, where the session never reached an outcome at all.
//
// KILLING MUTATION: blank the Outcome (or drop the call). The row then carries no terminal state and is
// indistinguishable from a session that simply never ran. RED.
//
// This oracle exists because the FIRST version of this change shipped with no test, and a mutation that
// blanked the outcome passed the whole suite — the exact pathology the change was written to close.
func TestFailedInvestigationLeavesADurableRecord(t *testing.T) {
	got := failedInvestigateResult(RunnerResult{}, errors.New("model gateway: 503 upstream"))

	if got.Outcome != "failed:investigate" {
		t.Fatalf("a crashed investigation must carry a terminal outcome, got %q", got.Outcome)
	}
	// The outcome must be DISTINGUISHABLE from a considered stop. If these collided, an infrastructure
	// failure would be counted as TG deciding no action was warranted — which flatters every quality
	// metric that reads outcomes.
	if strings.HasPrefix(got.Outcome, "no-proposal") {
		t.Fatalf("a failure must not read as a grounded stop, got %q", got.Outcome)
	}
	if !strings.Contains(got.StopReason, "503 upstream") {
		t.Fatalf("StopReason must carry the orchestrator's account of WHY, got %q", got.StopReason)
	}
	// Conclusion is untrusted agent text and must stay empty here: the agent produced nothing, and
	// inventing a conclusion would put words in its mouth on the one path where it never spoke.
	if got.Conclusion != "" {
		t.Fatalf("a failed investigation must not invent a conclusion, got %q", got.Conclusion)
	}
}

// TestFailedInvestigationSurvivesANilError keeps the record honest when the error is lost in transit —
// the row must still say the investigation failed, not silently degrade to an empty reason.
func TestFailedInvestigationSurvivesANilError(t *testing.T) {
	got := failedInvestigateResult(RunnerResult{}, nil)
	if got.Outcome != "failed:investigate" || !strings.Contains(got.StopReason, "investigate activity failed") {
		t.Fatalf("a nil error must still produce an honest record, got outcome=%q reason=%q", got.Outcome, got.StopReason)
	}
}
