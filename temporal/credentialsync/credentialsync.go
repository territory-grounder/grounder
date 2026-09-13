// Package credentialsync is the credential engine's "Sync now" button: it re-runs ONE registered source's
// read-only sync for real, on operator demand, and republishes the non-secret state projection the console
// reads (TG-109, spec/016 REQ-1615's on-demand half).
//
// WHY IT IS A WORKFLOW AND NOT AN HTTP HANDLER. The SyncEngine lives in the WORKER — the grounder holds no
// engine handle (core/credential/source.go names "the console invokes on demand" as an intent exactly
// because this gap exists), so the process receiving the request cannot sync and the process that can sync
// receives no requests. This mirrors moduletest/configwrite, which cross the same gap for the same reason.
//
// WHAT A SYNC MAY NOT DO. It is READ-ONLY against the source's upstream (inventory/credential pull — the
// same call the schedule makes), it never mutates the estate, and it never traverses the mode chokepoint.
// A failed sync is a RESULT, not an activity error: the engine retains the prior converged state
// (fail-closed), and retrying automatically would hammer a third-party system an operator just watched
// refuse — one attempt, like moduletest.
package credentialsync

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/credential"
)

// Request names the registered source to sync. It carries no configuration: the source under sync is the
// one TG has ACTUALLY got registered (INV-17 — registered at startup, or not at all), because "does the
// source I configured work" is the operator's question.
type Request struct {
	SourceID string
	// Operator is the authenticated principal, recorded so the projection's consumers can see who
	// triggered the run (the sync-run row itself is the audit surface).
	Operator string
}

// Result is what the operator is shown: the same non-secret facts one SyncRun carries, plus elapsed. No
// field can hold a secret — mirrors credential.SyncRun (INV-13).
type Result struct {
	SourceID string `json:"source_id"`
	OK       bool   `json:"ok"`
	// Summary is one line, safe to display; Detail names the failure class an operator can act on.
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
	Added   int    `json:"added"`
	Changed int    `json:"changed"`
	Removed int    `json:"removed"`
	// Entries is the ABSOLUTE count the source now holds — the anti-quiet-zero fact (a converged source
	// and a starved one both report 0 drift; only this distinguishes them).
	Entries   int   `json:"entries"`
	Starved   bool  `json:"starved"` // synced OK and holds ZERO bindings — a configuration fault, not success
	ElapsedMS int64 `json:"elapsed_ms"`
}

// Syncer re-runs one registered source's sync and republishes the state projection. Implemented in the
// worker composition root, where the live SyncEngine is.
type Syncer interface {
	SyncOne(ctx context.Context, sourceID string) (credential.SyncRun, error)
}

// Deps is what the activity needs.
type Deps struct {
	Syncer Syncer
}

// Activities holds the sync activity.
type Activities struct{ D Deps }

// SyncSourceActivity runs the real per-source sync.
func (a *Activities) SyncSourceActivity(ctx context.Context, req Request) (Result, error) {
	res := Result{SourceID: req.SourceID}
	if a.D.Syncer == nil {
		res.Summary = "the sync lane is not wired"
		res.Detail = "the worker has no credential engine handle — a pass here would certify a sync nobody ran"
		return res, nil
	}
	start := time.Now()
	run, err := a.D.Syncer.SyncOne(ctx, req.SourceID)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		// A FAILED SYNC IS A RESULT, NOT AN ACTIVITY ERROR: the engine kept the prior converged state
		// (fail-closed), and a retry would re-hit the upstream an operator just watched refuse.
		res.Summary = "the sync failed — the prior converged state is retained"
		res.Detail = err.Error() // engine errors are non-secret by construction (SyncRun.Err discipline)
		return res, nil
	}
	res.Added, res.Changed, res.Removed, res.Entries = run.Added, run.Changed, run.Removed, run.Entries
	res.Starved = run.Starved()
	res.OK = run.Outcome == credential.SyncOK
	switch {
	case !res.OK:
		res.Summary = "the sync failed — the prior converged state is retained"
		res.Detail = run.Err
	case res.Starved:
		res.Summary = "synced OK and produced ZERO host bindings"
		res.Detail = "the pull worked but this source contributes nothing to resolution — check its configured path/prefix and what its account can actually see"
	default:
		res.Summary = fmt.Sprintf("synced OK — %d entries (+%d ~%d -%d)", run.Entries, run.Added, run.Changed, run.Removed)
	}
	return res, nil
}

func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// The engine's own scheduled sync runs under a 2-minute budget; match it, bounded so a dialog
		// cannot hang.
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			// ONE attempt: a retry re-hits a third-party credential system an operator just watched fail.
			MaximumAttempts: 1,
		},
	}
}

// CredentialSyncWorkflow is the distinctly-named entry point the grounder starts (Temporal registers by
// bare function name; a plain SyncWorkflow would collide — the 2026-07-17 boot-loop lesson).
func CredentialSyncWorkflow(ctx workflow.Context, req Request) (Result, error) {
	ctx = workflow.WithActivityOptions(ctx, activityOpts())
	var a *Activities
	var res Result
	if err := workflow.ExecuteActivity(ctx, a.SyncSourceActivity, req).Get(ctx, &res); err != nil {
		return Result{SourceID: req.SourceID, OK: false,
			Summary: "the sync could not be run",
			Detail:  fmt.Sprintf("the worker did not complete the sync: %v", err)}, nil
	}
	return res, nil
}
