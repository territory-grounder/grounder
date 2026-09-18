package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/learn"
)

// TG-451 — the late-bound estate-source loaders behind an atomic handoff.
//
// estateRelayLoad (TG-346) and chaosLoad (TG-188) are DB-backed loaders that main() can only bind AFTER the pool
// connects, yet the estate-refresh goroutine — armed earlier in main() — can tick and read them first. As plain
// closure vars written once at bind and read from that goroutine, the cross-goroutine handoff had NO happens-before
// edge: a textbook data race the -race detector would flag if cmd/worker had a race-exercising test. It is benign
// in practice (pool-connect is synchronous and completes minutes before the first refresh tick, and per-source
// error isolation degrades a not-yet-bound read to "no edges yet", never corruption), but latent-not-observed is
// not proven-safe.
//
// Each loader now lives behind an atomic.Pointer (the same house pattern as guestLivenessStore): bind() Stores the
// loader and load() Loads it, so the write and the refresh read are ordered by a memory barrier. Until bind lands,
// load() returns the same "pool not yet connected" error the closures did, preserving the exact prior behaviour —
// now race-free. Extracted out of main()'s body so the handoff is unit-testable under -race (see the killing
// mutation in estate_loaders_test.go: revert either .fn to a plain field and the concurrent test reddens).

type estateRelayFunc func(context.Context) (estate.Snapshot, time.Time, error)

// estateRelayLoader is the late-bound snapshot-relay loader (TG-346), read across a memory barrier.
type estateRelayLoader struct {
	fn atomic.Pointer[estateRelayFunc]
}

// bind installs the loader on the post-connect path. Safe to call concurrently with load.
func (l *estateRelayLoader) bind(fn estateRelayFunc) { l.fn.Store(&fn) }

// load calls the bound loader, or errors (never panics) until the post-connect prime binds it — per-source
// isolation degrades that to "no edges yet" and keeps the prior graph.
func (l *estateRelayLoader) load(ctx context.Context) (estate.Snapshot, time.Time, error) {
	fn := l.fn.Load()
	if fn == nil {
		return estate.Snapshot{}, time.Time{}, fmt.Errorf("database pool not yet connected — the post-connect prime retries this")
	}
	return (*fn)(ctx)
}

type recoveryFeedFunc func(context.Context, db.RecoveryCursor) ([]learn.ClearObservation, db.RecoveryCursor, error)

// recoveryFeedLoader is the late-bound recovery-transition feed (TG-188 organic recovery learning): the
// estate-refresh goroutine pulls ingest_transition kind='recovery' rows past its cursor and feeds them to
// the co-occurrence learner's onset→clear pairing. Same TG-451 atomic handoff as the loaders above — the
// pool binds it post-connect while the refresh goroutine may already be ticking.
type recoveryFeedLoader struct {
	fn atomic.Pointer[recoveryFeedFunc]
}

// bind installs the feed on the post-connect path. Safe to call concurrently with load.
func (l *recoveryFeedLoader) bind(fn recoveryFeedFunc) { l.fn.Store(&fn) }

// load calls the bound feed, or errors (never panics) until the post-connect prime binds it — the caller
// treats that as "no clears yet" and keeps its cursor.
func (l *recoveryFeedLoader) load(ctx context.Context, cur db.RecoveryCursor) ([]learn.ClearObservation, db.RecoveryCursor, error) {
	fn := l.fn.Load()
	if fn == nil {
		return nil, cur, fmt.Errorf("database pool not yet connected — the post-connect prime retries this")
	}
	return (*fn)(ctx, cur)
}

type chaosLoadFunc func(context.Context) ([]estate.ChaosCascade, error)

// chaosLoader is the late-bound chaos-cascade loader (TG-188), read across a memory barrier.
type chaosLoader struct {
	fn atomic.Pointer[chaosLoadFunc]
}

// bind installs the loader on the post-connect path. Safe to call concurrently with load.
func (l *chaosLoader) bind(fn chaosLoadFunc) { l.fn.Store(&fn) }

// load calls the bound loader, or errors (never panics) until the post-connect prime binds it — per-source
// isolation degrades that to "no cascade edges yet" and keeps the prior graph.
func (l *chaosLoader) load(ctx context.Context) ([]estate.ChaosCascade, error) {
	fn := l.fn.Load()
	if fn == nil {
		return nil, fmt.Errorf("database pool not yet connected — the post-connect prime retries this")
	}
	return (*fn)(ctx)
}
