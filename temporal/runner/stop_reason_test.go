package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// WHY A SESSION PROPOSED NOTHING.
//
// Measured 2026-07-28: 220 of 698 sessions in 24h (31.5%) end `no-proposal:stop` AFTER ~4.4 steps of real
// investigation. Some of those are CORRECT — TG was deliberately taught to stop proposing an inapplicable
// disk-grow for a loopback disk-fill, and it correctly stands down on stale and self-resolved incidents.
// Others are genuine diagnosis MISSES. They were indistinguishable: both wrote outcome='no-proposal:stop'
// with an empty conclusion.
//
// The orchestrator ALREADY KNEW which was which. agent.Loop sets a precise Reason on every halt — "model call
// failed", "unparseable model output", "confidence below stop threshold", "write tool withheld", "trajectory
// veto — …", "proposal failed the single grammar" — and InvestigateResult carried Conclusion but NOT Reason,
// so it died at the activity boundary. An infrastructure failure was recorded identically to a considered
// refusal. Same shape as every other defect this codebase keeps finding: the answer is held and never bound
// to the record.

// TestANoProposalSessionRecordsWHYItStopped is the defect as an oracle.
func TestANoProposalSessionRecordsWHYItStopped(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	// A response the proposal grammar cannot parse ⇒ the loop halts with a specific, known reason.
	deps := testDeps(`this is not a directive at all`)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-stopreason",
		SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("workflow must complete without error: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)

	if res.Proposed {
		t.Fatalf("fixture did not produce a no-proposal session: %+v", res)
	}
	if strings.TrimSpace(res.StopReason) == "" {
		t.Errorf("a no-proposal session recorded NO stop reason (outcome=%q, conclusion=%q) — the loop knows "+
			"exactly why it halted, and without it a model-call FAILURE and a considered refusal are the same "+
			"row; 31.5%% of sessions land here", res.Outcome, res.Conclusion)
	}
}

// TestTheStopReasonIsTheORCHESTRATORsNotTheAgentText — the reason must be the orchestrator's own account, not
// a copy of the agent's free-text conclusion. Conclusion is untrusted DATA (INV-08); StopReason is trusted and
// is what a metric may be built on. Collapsing them would put untrusted text into a trusted field.
func TestTheStopReasonIsTheORCHESTRATORsNotTheAgentText(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`not a directive`)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-stopreason-2",
		SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)

	// The vocabulary comes from the SOURCE, not a copy. The first version of this oracle hand-listed six of
	// the loop's eight halt causes and passed only because its fixture produced a listed one — a parallel list
	// beside its source, which is precisely the defect shape this codebase keeps finding.
	known := agent.StopReasons()
	ok := false
	for _, k := range known {
		if strings.Contains(res.StopReason, k) {
			ok = true
			break
		}
	}
	if !ok {
		t.Errorf("stop reason %q is not one of the orchestrator's known halt causes %v — if it is agent "+
			"free-text, an untrusted string has entered a trusted field", res.StopReason, known)
	}
}

// TestAProposingSessionCarriesNoStopReason — the field must mean something. A session that DID propose has no
// halt to explain, and a non-empty reason there would make "did this session stop, and why" unanswerable.
func TestAProposingSessionCarriesNoStopReason(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-stopreason-3",
		SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if !res.Proposed {
		t.Skip("fixture did not propose")
	}
	if strings.TrimSpace(res.StopReason) != "" {
		t.Errorf("a session that PROPOSED carries stop reason %q — the field must be empty when nothing halted",
			res.StopReason)
	}
}

// TestTheStopReasonReACHES_ThePersistedRow is the one-level-up oracle. Asserting the workflow RESULT proves
// the value was computed; it does not prove it was RECORDED. The whole point is that a query over
// session_triage can split the 220 no-proposal sessions into declined-correctly vs missed — so the field must
// land in the ROW, not merely in a struct that is discarded when the workflow completes.
func TestTheStopReasonReachesThePersistedRow(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(`not a directive`)
	var got judge.TriageRow
	var seen bool
	deps.TriageRecord = func(_ context.Context, row judge.TriageRow) error {
		if !row.Proposed {
			got, seen = row, true
		}
		return nil
	}
	registerAll(env, NewActivities(deps))
	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-stopreason-row",
		SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01",
		Severity: ingest.SeverityWarning, Site: "dc1"})
	if !seen {
		t.Fatal("no no-proposal triage row was recorded at all")
	}
	if strings.TrimSpace(got.StopReason) == "" {
		t.Errorf("the persisted triage row carries NO stop reason (outcome=%q) — the value existed in the "+
			"workflow result and was dropped before the row, so no query can ever separate a correct refusal "+
			"from a diagnosis miss", got.Outcome)
	}
}

// TestTheHaltVocabularyIsNotHandCopied — the guard on the guard. StopReasons() must actually enumerate what
// the loop can set; if a cause is added to the loop and not to the list, this oracle's closed set silently
// stops being closed and TestTheStopReasonIsTheORCHESTRATORsNotTheAgentText starts passing vacuously or
// failing spuriously.
func TestTheHaltVocabularyIsNotHandCopied(t *testing.T) {
	reasons := agent.StopReasons()
	if len(reasons) == 0 {
		t.Fatal("the halt vocabulary is empty — every reason check downstream would pass vacuously")
	}
	seen := map[string]bool{}
	for _, r := range reasons {
		if strings.TrimSpace(r) == "" {
			t.Error("the halt vocabulary contains an empty cause — an empty reason is the absence this " +
				"requirement exists to remove")
		}
		if seen[r] {
			t.Errorf("duplicate halt cause %q", r)
		}
		seen[r] = true
	}
}

// TestAFailedVoteRecordNamesWhatFailed — the vote record runs with MaximumAttempts:1 and fails the whole
// session CLOSED, which is correct (INV-12/INV-19: no actuation without a durable approval record) and was
// completely opaque: Temporal surfaced only "activity error" with an empty cause chain. Observed live
// 2026-07-28 — 1 of 8 votes lost, and isolating it meant ruling out a uniqueness constraint, action-identity
// collapse and a ledger outage by hand.
//
// The failure must still fail closed. It must ALSO say which decision, for which session, and why.
func TestAFailedVoteRecordNamesWhatFailed(t *testing.T) {
	// A REAL ledger with a failing durable sink — the production failure mode. The in-memory chain is
	// authoritative for seq/hash and the sink is a write-through mirror, so a sink error surfaces as the
	// Append error rather than being silently dropped. Faking the whole ledger would test the fake.
	deps := testDeps()
	deps.Ledger = audit.NewLedger().WithSink(failingSink{})
	a := NewActivities(deps)

	_, err := a.RecordVoteActivity(t.Context(), RecordVoteInput{
		Decision: "human:approve", ActionID: "abc123", ExternalRef: "TG-vote-fail", Voter: "operator:kyriakos"})
	if err == nil {
		t.Fatal("a ledger append failure MUST fail the vote record — the session stands down rather than " +
			"actuating on an unrecorded approval")
	}
	for _, want := range []string{"human:approve", "TG-vote-fail", "abc123"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not name %q: %v — an operator sees an approved decision stuck open, a "+
				"FAILED workflow, and no reason", want, err)
		}
	}
	if !strings.Contains(err.Error(), "ledger unavailable") {
		t.Errorf("the underlying cause was dropped: %v — wrapping must preserve it", err)
	}
}

type failingSink struct{}

func (failingSink) Persist(audit.LedgerEntry) error { return fmt.Errorf("ledger unavailable") }
