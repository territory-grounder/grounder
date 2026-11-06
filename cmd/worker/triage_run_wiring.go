package main

import (
	"context"
	"log"

	"go.temporal.io/sdk/activity"

	"github.com/territory-grounder/grounder/core/judge"
)

// triageRecorder is the session_triage writer plus the per-run writer (core/db TriageStore).
type triageRecorder interface {
	RecordTriage(ctx context.Context, row judge.TriageRow) error
	RecordRun(ctx context.Context, runID string, row judge.TriageRow) error
}

// workflowRunID is the Temporal workflow RUN id of the activity ctx runs in, or "" outside an activity.
func workflowRunID(ctx context.Context) string {
	if !activity.IsActivity(ctx) {
		return ""
	}
	return activity.GetInfo(ctx).WorkflowExecution.RunID
}

// recordTriageWithRun wraps the session_triage writer so the same record ALSO lands one triage_run row per
// workflow run (TG-633). session_triage stays first-write-wins per alert, exactly as before; the run row is
// best-effort and never fails the record: a lost run row costs one benchmark sample, a failed triage record
// costs the session's durable outcome. runIDOf is a seam for the test (production: workflowRunID).
func recordTriageWithRun(s triageRecorder, runIDOf func(context.Context) string) func(context.Context, judge.TriageRow) error {
	return func(ctx context.Context, row judge.TriageRow) error {
		err := s.RecordTriage(ctx, row)
		if runID := runIDOf(ctx); runID != "" {
			if rerr := s.RecordRun(ctx, runID, row); rerr != nil {
				log.Printf("triage run record %s (run %s): %v (best-effort — the session is unaffected)", row.ExternalRef, runID, rerr)
			}
		}
		return err
	}
}
