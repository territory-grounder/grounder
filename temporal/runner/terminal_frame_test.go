package runner

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/judge"
)

// TG-81 borrow 1: a session that DIES leaves a durable terminal frame. Kill the very first activity
// (suppress) so runSession errors before any recordTriage — the wrapper must still write a
// session_triage row carrying the synthetic stop reason and a typed session-fatal outcome.
// KILLING MUTATION: delete the synthetic-terminal block in RunnerWorkflow — the row assertion fails
// (no RecordTriageActivity call at all).
func TestSessionFatalErrorStillLeavesATerminalFrame(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	a := NewActivities(deps)
	registerAll(env, a)

	env.OnActivity(a.SuppressActivity, mock.Anything, mock.Anything).
		Return(SuppressResult{}, errors.New("suppress store on fire"))
	var recorded []judge.TriageRow
	env.OnActivity(a.RecordTriageActivity, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { recorded = append(recorded, args.Get(1).(judge.TriageRow)) }).
		Return(RecordTriageResult{}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-b1-1", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if env.GetWorkflowError() == nil {
		t.Fatal("the session must still FAIL — the synthetic frame records the death, it does not swallow it")
	}
	if len(recorded) != 1 {
		t.Fatalf("a dying session must leave exactly ONE terminal frame, got %d", len(recorded))
	}
	row := recorded[0]
	if row.StopReason != "synthetic-terminal" {
		t.Fatalf("the frame must name itself synthetic, got StopReason=%q", row.StopReason)
	}
	if row.Outcome != "error:session-fatal:activity" {
		t.Fatalf("the frame must carry the typed session-fatal class, got Outcome=%q", row.Outcome)
	}
	if row.ExternalRef != "TG-b1-1" {
		t.Fatalf("frame bound to the wrong session: %q", row.ExternalRef)
	}
}

// A session that records its own rows must NOT gain a synthetic frame from the wrapper: the happy path
// stays byte-identical (a healthy session already records more than once by design — the TG-201/TG-394
// shadow+terminal twice-pattern — so the pin is "no synthetic frame", not a count).
func TestHealthySessionGainsNoSyntheticFrame(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	deps := testDeps(proposeWeb01)
	a := NewActivities(deps)
	registerAll(env, a)

	var rows []judge.TriageRow
	env.OnActivity(a.RecordTriageActivity, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { rows = append(rows, args.Get(1).(judge.TriageRow)) }).
		Return(RecordTriageResult{}, nil)

	env.ExecuteWorkflow(RunnerWorkflow, ingest.IncidentEnvelope{ExternalRef: "TG-b1-2", SourceID: "prometheus-dc1", AlertRule: "NginxDown", Host: "web01", Severity: ingest.SeverityWarning, Site: "dc1"})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("healthy session must complete: %v", env.GetWorkflowError())
	}
	var res RunnerResult
	_ = env.GetWorkflowResult(&res)
	if !res.TriageRecorded {
		t.Fatal("the session's own record must set TriageRecorded")
	}
	if len(rows) == 0 {
		t.Fatal("the healthy session recorded nothing — the harness went vacuous")
	}
	for _, r := range rows {
		if r.StopReason == "synthetic-terminal" || strings.HasPrefix(r.Outcome, "error:session-fatal") {
			t.Fatalf("a healthy session must never carry a synthetic frame: %+v", r)
		}
	}
}

// The two-tier classifier: session-fatal classes are typed from the temporal error family; anything
// unrecognized stays the honest generic, never a guessed subclass.
func TestTerminalErrorClassVocabulary(t *testing.T) {
	if got := terminalErrorClass(temporal.NewCanceledError()); got != "session-fatal:cancelled" {
		t.Fatalf("canceled: %q", got)
	}
	if got := terminalErrorClass(errors.New("mystery")); got != "session-fatal" {
		t.Fatalf("generic: %q", got)
	}
}
