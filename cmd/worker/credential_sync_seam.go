package main

import (
	"context"
	"errors"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/temporal/credentialsync"
)

// credentialSyncSeam adapts main()'s per-source sync closure (assigned inside the credential-engine block)
// to the credentialsync.Syncer the workflow activity holds (TG-109). A nil fn — no sources configured —
// errors so the activity renders its honest "lane not wired" result rather than certifying a sync nobody ran.
type credentialSyncSeam struct {
	fn func(ctx context.Context, sourceID string) (credential.SyncRun, error)
}

// SyncOne implements credentialsync.Syncer.
func (s credentialSyncSeam) SyncOne(ctx context.Context, sourceID string) (credential.SyncRun, error) {
	if s.fn == nil {
		return credential.SyncRun{}, errors.New("no credential sources configured — nothing to sync")
	}
	return s.fn(ctx, sourceID)
}

// compile-time proof the seam satisfies the workflow's dependency.
var _ credentialsync.Syncer = credentialSyncSeam{}
