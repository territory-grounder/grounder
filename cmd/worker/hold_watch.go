package main

// THE HOLD WATCH (TG-667, deep-review F6/F7). A HOLD is TG deciding, on purpose, NOT to heal: a person's
// deliberate change, an unknown actor, a stateful or destructive target, an operator's denial. Until TG-667 that
// decision lived in one ledger row and one notice, and nothing counted it afterwards. On 2026-11-07 ten such
// holds sat in the ledger, two investigations had ended `failed:investigate` with no retry, and no gauge, no
// alert and no console surface said so: an incident TG had chosen to leave alone looked, from monitoring,
// exactly like an incident TG had healed.
//
// This job reads the hold projection (hold_decision, written by RecordVoteActivity) and the terminal triage
// records on a cadence and publishes what the operator needs in order to act on what TG did not:
//
//   tg_hold_open                       every hold still inside its re-detection window, ALWAYS emitted (0 is a
//                                      reading; absence is the watcher being gone)
//   tg_hold_sticky_open                the subset the prior-hold rule carries to the next detection
//   tg_hold_redetected_open            open holds whose incident has fired again from ANOTHER session within the
//                                      last holdRefireRecent — STILL RAISING: the alert keeps coming back and TG
//                                      is deliberately not touching it, so a person must. A re-fire older than
//                                      that is an incident that has since gone quiet (fresh-eyes review,
//                                      2026-11-07: without the recency bound one early re-fire kept a resolved
//                                      incident "raising" for the whole 24h window)
//   tg_hold_redetected_oldest_seconds  how long the longest-standing still-raising hold has been held; emitted
//                                      only when there is one, for the same reason tg_poll_queue_oldest_seconds is
//   tg_failed_investigation_unretried  sessions that ended failed:investigate in the last 7 days with no later
//                                      non-failed session on the same (host, rule) — TG tried, crashed, and
//                                      never came back
//   tg_hold_watch_last_success_timestamp_seconds  when the reading above was last refreshed from the database,
//                                      so a frozen reading under a DB outage is visibly stale
//
// Same hand-off shape as the poll-queue job (poll_queue.go), for the same reason: a /metrics scrape must never
// trigger a database query, and a read error keeps the PREVIOUS reading rather than zeroing it — a zero on a
// transient DB fault reads exactly like "every hold was released", the one thing this gauge must never say.

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/holds"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/metrics"
)

// holdWatchReader is the seam the job depends on: db.HoldStore in production, a fake under test, nil when the
// worker has no pool (the reader then emits nothing and the boot log says so).
type holdWatchReader interface {
	OpenHolds(ctx context.Context, now time.Time) ([]holds.HoldDecision, error)
	HoldRedetections(ctx context.Context, now time.Time) (map[int64]time.Time, error)
	FailedInvestigationsUnretried(ctx context.Context, since time.Time) ([]holds.FailedInvestigation, error)
}

// holdWatchEvery is the refresh cadence. Not a knob: the projection is small, the joins are covered by migration
// 0128's index, and a hold's age is measured in hours.
const holdWatchEvery = time.Minute

// holdRefireRecent bounds "still raising": a hold counts as re-detected only while its latest re-fire from another
// session is this recent. Alert deliveries for a live incident arrive at least on the poller's cadence (minutes);
// an incident that has not produced a session in an hour has gone quiet, and the hold stops being actionable.
const holdRefireRecent = time.Hour

// holdWatchSamples converts one reading into the published gauges. Takes `now` so the age arithmetic is
// deterministic under test.
func holdWatchSamples(open []holds.HoldDecision, redetected map[int64]time.Time, failed []holds.FailedInvestigation, now time.Time) []metrics.Sample {
	sticky, re := 0, 0
	var oldest time.Duration
	for _, h := range open {
		if holds.Sticky(h.ReasonKey) {
			sticky++
		}
		if latest, ok := redetected[h.Seq]; ok && now.Sub(latest) <= holdRefireRecent {
			re++
			if age := now.Sub(h.DecidedAt); age > oldest {
				oldest = age
			}
		}
	}
	out := []metrics.Sample{
		{
			Name: "tg_hold_open", Kind: metrics.Gauge,
			Help: "incidents TG has decided NOT to heal and still remembers (hold_decision rows inside their " +
				"re-detection window). ALWAYS emitted, including at 0: an absent series is the watcher being " +
				"gone, never an empty hold list.",
			Value: float64(len(open)),
		},
		{
			Name: "tg_hold_sticky_open", Kind: metrics.Gauge,
			Help: "the open holds whose reason the prior-hold rule carries to the next detection (a person's " +
				"change, an unknown actor, a stateful or destructive target, an operator's denial). The rest " +
				"are judged afresh when the incident re-fires.",
			Value: float64(sticky),
		},
		{
			Name: "tg_hold_redetected_open", Kind: metrics.Gauge,
			Help: "open holds whose incident fired AGAIN from another session within the last hour — still " +
				"raising. These are the ones a person must act on: the alert keeps coming back and TG is " +
				"deliberately not touching it. An incident quiet for an hour drops out.",
			Value: float64(re),
		},
		{
			Name: "tg_hold_watch_last_success_timestamp_seconds", Kind: metrics.Gauge,
			Help: "when the hold reading was last refreshed from the database. A read error keeps the previous " +
				"reading rather than zeroing it, so this is how a frozen reading shows its age.",
			Value: float64(now.Unix()),
		},
		{
			Name: "tg_failed_investigation_unretried", Kind: metrics.Gauge,
			Help: "sessions in the last 7 days that ended failed:investigate with no later non-failed session on " +
				"the same (host, rule): incidents TG tried to look at, crashed on, and never came back to.",
			Value: float64(len(failed)),
		},
	}
	// Emitted only when a re-detected hold exists: with none there is nothing to measure an age from, and a 0
	// here would read as "held this instant", the one value that must never trip the aging rule.
	if re > 0 {
		out = append(out, metrics.Sample{
			Name: "tg_hold_redetected_oldest_seconds", Kind: metrics.Gauge,
			Help: "how long the longest-standing STILL-RAISING hold has been held (measured from the decision; " +
				"counted only while the incident re-fired within the last hour). An incident that keeps raising " +
				"while TG keeps declining is a person's decision waiting to be made — GET /v1/holds names it.",
			Value: oldest.Seconds(),
		})
	}
	return out
}

// newHoldWatchJob builds the reader and its drivable refresh, publishing one reading immediately so the gauges
// exist before the first tick. Returns (reader, nil) when there is no store: nothing to tick, the reader emits
// nothing, and the boot log says the holds are unmeasured.
func newHoldWatchJob(ctx context.Context, store holdWatchReader) (func() []metrics.Sample, func()) {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("hold watch: no store — HOLDS ARE UNMEASURED. An incident TG has decided not to heal is " +
			"invisible to monitoring; only the ledger and GET /v1/holds know (TG-667).")
		return reader, nil
	}
	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		now := time.Now().UTC()
		open, err := store.OpenHolds(rctx, now)
		if err != nil {
			log.Printf("hold watch: open-holds read failed, keeping the previous reading: %v", err)
			return
		}
		re, err := store.HoldRedetections(rctx, now)
		if err != nil {
			log.Printf("hold watch: redetection read failed, keeping the previous reading: %v", err)
			return
		}
		failed, err := store.FailedInvestigationsUnretried(rctx, now.Add(-httpapi.FailedInvestigationLookback))
		if err != nil {
			log.Printf("hold watch: failed-investigation read failed, keeping the previous reading: %v", err)
			return
		}
		s := holdWatchSamples(open, re, failed, now)
		held.Store(&s)
	}
	refresh()
	return reader, refresh
}

// startHoldWatchJob refreshes the hold gauges on a cadence and hands the samples to the admin surface through
// an atomic pointer. A nil store yields a reader that emits nothing, so an un-pooled worker degrades to silence
// rather than panicking — and rather than letting an absent series read as "no holds".
func startHoldWatchJob(ctx context.Context, store holdWatchReader, every time.Duration) func() []metrics.Sample {
	reader, refresh := newHoldWatchJob(ctx, store)
	if refresh == nil {
		return reader
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
	log.Printf("hold watch: publishing open / sticky / re-detected holds and unretried failed investigations every %s (TG-667)", every)
	return reader
}

// holdWatchStoreOrNil adapts the pool to the job's seam; a nil pool is a nil reader, never a typed nil.
func holdWatchStoreOrNil(pool *db.Pool) holdWatchReader {
	if pool == nil {
		return nil
	}
	return db.NewHoldStore(pool)
}
