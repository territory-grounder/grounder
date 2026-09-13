package main

// THE WORKER PUBLISHES WHAT IT IS RUNNING (TG-251).
//
// The modules page answers "is this connector on?" from the API process's registry — 15 entries, while
// all 28 worker-resident connectors (every notifier, tracker, cmdb, credsource, discovery, knowledge
// source) were invisible to it. Production rendered notifier/matrix as switched off while it was
// delivering governance polls. MR !866 made unknown read as UNKNOWN; this loop supplies the answer: the
// worker upserts its Capabilities() projection on a heartbeat, and the grounder reads it through a
// staleness cutoff, so a dead worker degrades back to "state not reported here" instead of a remembered
// answer. Removed modules retire the same way — their rows just stop refreshing.

import (
	"context"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/modules"
)

// capabilityPublisher is the seam the loop writes through, so the oracle drives the real loop body with
// a recording fake.
type capabilityPublisher interface {
	Publish(ctx context.Context, rows []db.CapabilityProjectionRow) error
}

// capabilityRows renders the registry's operator-facing view as projection rows. Split out so the oracle
// can pin the mapping (a dropped Enabled bit here would silently re-create the very defect).
func capabilityRows(caps []modules.Capability) []db.CapabilityProjectionRow {
	out := make([]db.CapabilityProjectionRow, 0, len(caps))
	for _, c := range caps {
		out = append(out, db.CapabilityProjectionRow{
			Surface: c.Surface, SourceType: c.SourceType, Capability: c.Capability, Enabled: c.Enabled,
		})
	}
	return out
}

// publishCapabilityProjection runs one publish: registry snapshot → projection rows → upsert.
func publishCapabilityProjection(ctx context.Context, reg *modules.Registry, store capabilityPublisher, logf func(string, ...any)) {
	rows := capabilityRows(reg.Capabilities())
	if err := store.Publish(ctx, rows); err != nil {
		// Loud, never fatal: the projection is an OBSERVABILITY channel. Its absence must degrade the
		// console to "unknown" (the reader's staleness cutoff does that), not degrade the worker.
		logf("capability projection: publish failed (%v) — the modules page will show worker-resident "+
			"modules as unknown once the last row goes stale", err)
		return
	}
	logf("capability projection: %d module row(s) published", len(rows))
}

// runCapabilityProjection is the heartbeat: one publish immediately, then one per interval. The interval
// is the freshness contract the READER builds its staleness window on — keep them in the same ratio
// (reader window = 3× interval) if either changes.
func runCapabilityProjection(ctx context.Context, reg *modules.Registry, store capabilityPublisher, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		interval = time.Minute
	}
	publishCapabilityProjection(ctx, reg, store, logf)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			publishCapabilityProjection(ctx, reg, store, logf)
		}
	}
}
