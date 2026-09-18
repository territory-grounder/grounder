package main

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/temporal/credentialsync"
	tg "github.com/territory-grounder/grounder/temporal"
)

// grounderCredentialSync is the process-wide "Sync now" backend, set once at boot when a Temporal client
// exists — a package var rather than a buildPublicAPI parameter, per the documented positional-rebind
// hazard on that signature (the rollback backend records the same decision). nil ⇒ the route 503s.
var grounderCredentialSync httpapi.CredentialSyncer

// temporalCredentialSync starts the worker-side "Sync now" lane (TG-109). The SyncEngine lives in the
// worker; this process holds no engine handle, so the request crosses that gap exactly like the module
// TEST button.
type temporalCredentialSync struct{ c client.Client }

// SyncSource implements httpapi.CredentialSyncer.
func (t temporalCredentialSync) SyncSource(ctx context.Context, sourceID, operator string) (httpapi.CredentialSyncOutcome, error) {
	run, err := t.c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// Keyed per source so two operators syncing different sources do not queue behind each other,
		// while a double-click on ONE source coalesces instead of double-hitting its upstream.
		ID:        fmt.Sprintf("tg-credential-sync-%s", sourceID),
		TaskQueue: tg.TaskQueueRunner,
	}, credentialsync.CredentialSyncWorkflow, credentialsync.Request{SourceID: sourceID, Operator: operator})
	if err != nil {
		return httpapi.CredentialSyncOutcome{}, err
	}
	var res credentialsync.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.CredentialSyncOutcome{}, err
	}
	return httpapi.CredentialSyncOutcome{
		SourceID: res.SourceID, OK: res.OK, Summary: res.Summary, Detail: res.Detail,
		Added: res.Added, Changed: res.Changed, Removed: res.Removed,
		Entries: res.Entries, Starved: res.Starved, ElapsedMS: res.ElapsedMS,
	}, nil
}
