package main

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
)

type fakeTriageRecorder struct {
	triage, runs []string
	triageErr    error
	runErr       error
}

func (f *fakeTriageRecorder) RecordTriage(_ context.Context, row judge.TriageRow) error {
	f.triage = append(f.triage, row.ExternalRef)
	return f.triageErr
}

func (f *fakeTriageRecorder) RecordRun(_ context.Context, runID string, row judge.TriageRow) error {
	f.runs = append(f.runs, row.ExternalRef+"/"+runID)
	return f.runErr
}

// TG-633: every record also lands its run row; the run row never changes the triage record's outcome.
func TestRecordTriageWithRunWritesBothAndStaysBestEffort(t *testing.T) {
	row := judge.TriageRow{ExternalRef: "ref-1"}
	run := func(context.Context) string { return "run-7" }

	f := &fakeTriageRecorder{}
	if err := recordTriageWithRun(f, run)(context.Background(), row); err != nil || len(f.triage) != 1 || len(f.runs) != 1 || f.runs[0] != "ref-1/run-7" {
		t.Fatalf("want one triage record and one run row keyed by the run id, got triage=%v runs=%v err=%v", f.triage, f.runs, err)
	}

	f = &fakeTriageRecorder{runErr: errors.New("db down")}
	if err := recordTriageWithRun(f, run)(context.Background(), row); err != nil {
		t.Fatalf("a failed run row must not fail the triage record, got %v", err)
	}

	f = &fakeTriageRecorder{triageErr: errors.New("refused")}
	if err := recordTriageWithRun(f, run)(context.Background(), row); err == nil || len(f.runs) != 1 {
		t.Fatalf("the triage record's own error must still be returned (and the run row still attempted), got err=%v runs=%v", err, f.runs)
	}

	f = &fakeTriageRecorder{}
	if err := recordTriageWithRun(f, func(context.Context) string { return "" })(context.Background(), row); err != nil || len(f.runs) != 0 {
		t.Fatalf("outside an activity (no run id) no run row is written, got runs=%v err=%v", f.runs, err)
	}
	if workflowRunID(context.Background()) != "" {
		t.Fatal("a plain context is not an activity and has no run id")
	}
}
