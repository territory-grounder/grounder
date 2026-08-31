package main

import (
	"context"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/db"
)

// objectGroupRefreshInterval is how often the worker re-reads the operator-authored object groups (TG-481)
// from the store into the credential resolver, so a group created or removed through the write lane takes
// effect WITHOUT a worker restart. Object groups change rarely (an operator edits them by hand), so a modest
// poll is simpler and lower-risk than coupling a resolver side-effect into the write activity — and, unlike an
// activity callback, it refreshes EVERY worker in a multi-worker deployment, not only the one that ran the
// write.
const objectGroupRefreshInterval = 30 * time.Second

// objectGroupLister reads the persisted object groups. *db.EstateObjectGroupStore satisfies it; a fake stands
// in for the unit test so the conversion + replace semantics are provable without a database.
type objectGroupLister interface {
	List(ctx context.Context) ([]db.EstateObjectGroupRow, error)
}

// objectGroupSink receives the converged object-group set. *credential.SyncEngine satisfies it via
// SetObjectGroups; Resolve then unions the matching groups into a Target.Groups on each sync.
type objectGroupSink interface {
	SetObjectGroups(groups []credential.ObjectGroup)
}

// loadObjectGroupsInto reads every object group and hands the converged set to the resolver. It REPLACES the
// resolver's set with exactly what the store holds, so a deleted group stops resolving after the load and an
// empty store disarms the seam (resolution runs with no groups — exactly as before TG-481). The resolver
// keys on name + host-glob patterns only; precedence stays in the store for the console. Returns the count
// handed over.
func loadObjectGroupsInto(ctx context.Context, lister objectGroupLister, sink objectGroupSink) (int, error) {
	rows, err := lister.List(ctx)
	if err != nil {
		return 0, err
	}
	groups := make([]credential.ObjectGroup, 0, len(rows))
	for _, r := range rows {
		groups = append(groups, credential.ObjectGroup{Name: r.Name, Patterns: r.Patterns})
	}
	sink.SetObjectGroups(groups)
	return len(groups), nil
}

// startObjectGroupRefresh polls the store on a fixed cadence and re-loads it into the resolver until the
// process is interrupted. A read failure is logged and the LAST good set is LEFT IN PLACE — a transient DB
// blip must never silently DISARM live resolution — and the next tick retries. A non-positive interval
// disables the poll (the boot load still applies). Runs in its own goroutine and returns immediately.
func startObjectGroupRefresh(lister objectGroupLister, sink objectGroupSink, interval time.Duration, stop <-chan interface{}) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, err := loadObjectGroupsInto(ctx, lister, sink)
				cancel()
				if err != nil {
					log.Printf("object groups: refresh read failed: %v — keeping the last loaded set (TG-481)", err)
				}
			}
		}
	}()
}
