package main

// THE DEPTH GAUGE ON THE HUMAN GATE (TG-173, OWASP Agentic T10 — overwhelming-HITL).
//
// The rate governor answers load by clamping auto -> APPROVE: under pressure it routes MORE decisions to
// the human, which is when the human can least keep up. Counted 2026-08-06 on the running worker: 47
// published tg_* metric families, none of them the depth of the poll queue. The one control every residual
// safety property of Full-auto rests on had no instrument at all.
//
// CountOpen exists and is served on /v1/stats as pending_polls, so the number is on the console page. That
// is a number for an operator who is already looking. The failure mode here is the operator not knowing to
// look — a storm, or a manufactured flood of low-value in-grammar proposals, turns the human gate into a
// rubber stamp and hides one real malicious proposal among ninety trivial ones.
//
// Same hand-off shape as the ingest-freshness job, for the same reason: a /metrics scrape must never
// trigger a database query.

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/persist"
)

// pollQueueReader is the seam the job depends on — an interface so the job is testable without a database
// and so a nil store degrades to silence instead of panicking.
type pollQueueReader interface {
	OpenDecisions(ctx context.Context) ([]persist.PendingDecision, error)
}

// pollQueueSamples converts one reading of the open queue into the published gauges.
//
// Takes `now` rather than calling time.Now() so the age arithmetic is deterministic under test — this
// codebase's standing rule for anything computing an elapsed time.
func pollQueueSamples(open []persist.PendingDecision, now time.Time) []metrics.Sample {
	st := persist.ComputeQueueStats(open, now)
	out := []metrics.Sample{
		{
			Name: "tg_poll_queue_open", Kind: metrics.Gauge,
			Help: "polls waiting for a human vote. ALWAYS emitted, including at 0 — this is the depth of " +
				"the control every residual safety property of Full-auto rests on, and its absence must " +
				"not read as an empty queue.",
			Value: float64(st.Open),
		},
		{
			Name: "tg_poll_queue_distinct_shapes", Kind: metrics.Gauge,
			Help: "how many genuinely DIFFERENT proposals the waiting polls represent (site + proposed " +
				"action). Read against tg_poll_queue_open: equal means every waiting decision is its own " +
				"question; far smaller means one fault is fanning out, which is also what a manufactured " +
				"flood looks like — volume is easier to manufacture than variety.",
			Value: float64(st.DistinctShapes),
		},
		{
			Name: "tg_poll_queue_largest_shape", Kind: metrics.Gauge,
			Help: "size of the biggest near-duplicate group among waiting polls — how many of them ONE " +
				"review would actually settle.",
			Value: float64(st.LargestShape),
		},
		{
			Name: "tg_poll_queue_irreversible", Kind: metrics.Gauge,
			Help: "waiting polls binding an action that cannot be undone. Under flood these are the ones " +
				"that must not be rubber-stamped; they are ordered first in the console for the same reason.",
			Value: float64(st.Irreversible),
		},
	}
	// The oldest-age gauge is emitted only when something is actually waiting. On an empty queue there is
	// no oldest poll to measure from, and publishing 0 there would be indistinguishable from "a poll opened
	// this instant" — which is the one reading that should never trigger a staleness rule.
	if st.Open > 0 {
		out = append(out, metrics.Sample{
			Name: "tg_poll_queue_oldest_seconds", Kind: metrics.Gauge,
			Help: "how long the longest-waiting poll has been open. Read beside tg_poll_queue_open: a " +
				"SHALLOW queue whose oldest entry is a day old is an operator who has stopped voting (or a " +
				"poll whose workflow died); a DEEP queue that is minutes old is a storm in progress.",
			Value: st.OldestAge.Seconds(),
		})
	}
	return out
}

// newPollQueueJob builds the reader and its refresh, publishing one reading immediately so the gauges
// exist before the first tick rather than after it.
//
// SPLIT OUT SO THE REFRESH IS DRIVABLE. The first version of the error-path guard set the store to fail and
// then re-read the held pointer — which asserts nothing, because reading the pointer does not refresh it
// and the ticker was an hour away. The mutation "clear the samples on a read error" SURVIVED that test.
// A seam the test can step is the difference between a guard and a comment.
//
// Returns (reader, nil) when there is no store: nothing to tick, and the reader emits nothing.
func newPollQueueJob(ctx context.Context, store pollQueueReader) (func() []metrics.Sample, func()) {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("poll queue: no store — the HUMAN GATE IS UNMEASURED. A flood of the approval queue " +
			"(OWASP Agentic T10) cannot be distinguished from a quiet estate, and the rate governor " +
			"answers load by routing MORE decisions here.")
		return reader, nil
	}
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		open, err := store.OpenDecisions(rctx)
		if err != nil {
			// Deliberately does NOT clear the held samples. Zeroing the depth on a transient database error
			// looks exactly like the operator having caught up, which is the single most misleading thing
			// this gauge could say.
			log.Printf("poll queue: read failed, keeping the previous reading: %v", err)
			return
		}
		s := pollQueueSamples(open, time.Now().UTC())
		held.Store(&s)
	}
	refresh() // publish immediately, so the gauges exist before the first tick
	return reader, refresh
}

// startPollQueueJob polls the open-decision projection on a cadence and hands the samples to the admin
// surface through an atomic pointer.
//
// Returns the reader the admin surface calls. A nil store yields a reader that emits nothing and says so
// at boot, so an un-pooled worker degrades to silence rather than panicking — and rather than leaving the
// operator to infer from an absent series that the queue is empty.
func startPollQueueJob(ctx context.Context, store pollQueueReader, every time.Duration) func() []metrics.Sample {
	reader, refresh := newPollQueueJob(ctx, store)
	if refresh == nil {
		return reader // no store: nothing to tick
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
	log.Printf("poll queue: watching the human approval queue every %s — depth, near-duplicate collapse, "+
		"and oldest wait are now visible to monitoring, not only to a console nobody has open", every)
	return reader
}

// pollQueueStoreOrNil keeps a TYPED NIL out of the interface, and must therefore take the concrete pool
// rather than the interface: `var p *db.Pool = nil; var r pollQueueReader = db.NewPendingStore(p)` builds a
// NON-nil interface holding a store with a nil pool, so the `store == nil` guard above would not fire and
// the first refresh would panic on a worker that has no database. Converting after the fact cannot undo
// that — by then the nil is already wrapped — which is why the check happens here, on *db.Pool.
func pollQueueStoreOrNil(pool *db.Pool) pollQueueReader {
	if pool == nil {
		return nil
	}
	return db.NewPendingStore(pool)
}
