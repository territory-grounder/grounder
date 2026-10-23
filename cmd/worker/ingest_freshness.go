package main

// THE DEAD-MAN ON TG'S OWN INPUT (TG-336).
//
// Measured 2026-08-05: ingest_alert went 564 rows/day (2026-07-30) -> 6 (2026-08-01) -> 0 (2026-08-05),
// and session_triage followed it from 522/day to zero. That state had held for FIVE DAYS. Nothing fired,
// because nothing watched. The workers were healthy, the pipelines were green, docker ps was clean, and a
// platform whose entire purpose is triaging estate alerts was deaf.
//
// TG-250 built exactly the right instrument for this shape — offered beside produced, so a seam that is
// running and yielding nothing is visible — and pointed it at the internal seams only. This applies the
// same pattern to the front door.
//
// WHY A PAIR, NOT AN AGE. `last seen 9 hours ago` is unjudgeable on its own: a source that alerts weekly
// is fine, and a source that alerted 500 times yesterday is not. Publishing the baseline count beside the
// age is what lets a rule say "this one used to speak and has stopped" instead of "this one is quiet".

import (
	"context"
	"log"
	"sort"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/modules"
)

// ingestFreshnessReader is the seam the job depends on — an interface so the job is testable without a
// database and so a nil store is a no-op rather than a panic.
type ingestFreshnessReader interface {
	Sources(ctx context.Context, window time.Duration) ([]db.IngestFreshness, error)
	// SourcesNeverSeen answers the question the freshness pair structurally cannot: which DECLARED
	// sources have never delivered anything at all. A source with no rows has no row to go stale, so it
	// is invisible to an age/denominator pair rather than merely quiet (TG-291: CrowdSec is advertised in
	// the boot log and has never appeared in ingest_alert, ever).
	SourcesNeverSeen(ctx context.Context, declared []string) ([]db.DeclaredButSilent, error)
}

// ingestFreshnessSamples converts one reading into the published gauges.
//
// It takes `now` rather than calling time.Now() so the age arithmetic is deterministic under test — the
// codebase's standing rule for anything that computes an elapsed time.
// declaredSilentSamples publishes one gauge per DECLARED source, 1 when it has never delivered.
//
// Emitted for every declared source (not only the silent ones) so the series exists at 0 for healthy
// sources: a rule written over a metric that only appears when broken cannot distinguish "healthy" from
// "the exporter stopped emitting", which is the failure this whole family exists to avoid.
func declaredSilentSamples(declared []string, never []db.DeclaredButSilent) []metrics.Sample {
	silent := map[string]bool{}
	for _, n := range never {
		silent[n.SourceID] = true
	}
	out := make([]metrics.Sample, 0, len(declared)+1)
	for _, d := range declared {
		v := 0.0
		if silent[d] {
			v = 1
		}
		out = append(out, metrics.Sample{
			Name: "tg_ingest_source_never_delivered", Kind: metrics.Gauge,
			Help: "1 when a source TYPE this deployment DECLARES has never delivered a single alert. " +
				"Labelled by source_type, matching the module registry — the per-site source_id " +
				"(librenms-dc1) belongs to the freshness gauges, and comparing the two marks every " +
				"multi-site source as never-delivered. Distinct from a stale last_seen: this source has " +
				"no rows at all, so it is invisible to the freshness pair rather than quiet.",
			Value: v, Labels: map[string]string{"source_type": d},
		})
	}
	// The declared-set size, always emitted — the vacuity floor for the gauges above. Zero declared
	// sources and every declared source healthy must not publish identically.
	out = append(out, metrics.Sample{
		Name: "tg_ingest_sources_declared", Kind: metrics.Gauge,
		Help: "how many alert source TYPES this deployment declares. NOT directly comparable to " +
			"tg_ingest_sources_known, which counts per-site source IDs — one declared type can produce " +
			"several observed ids. Use tg_ingest_source_never_delivered for the real gap.",
		Value: float64(len(declared)),
	})
	return out
}

func ingestFreshnessSamples(rows []db.IngestFreshness, now time.Time) []metrics.Sample {
	// The FLEET-level pair first. A per-source rule cannot see "every source stopped at once", which is
	// exactly what happened here: four sources degraded over four days and the estate went silent.
	var newest time.Time
	var fleetRecent int64
	out := make([]metrics.Sample, 0, len(rows)*2+3)

	for _, r := range rows {
		lbl := map[string]string{"source_id": r.SourceID}
		if r.LastSeen.After(newest) {
			newest = r.LastSeen
		}
		fleetRecent += r.RecentTotal

		// A source that has NEVER delivered has no meaningful age. Emitting a huge number there would
		// make it indistinguishable from a source that died, and only one of those is an incident.
		if !r.LastSeen.IsZero() {
			out = append(out, metrics.Sample{
				Name: "tg_ingest_source_last_seen_seconds", Kind: metrics.Gauge,
				Help: "seconds since this alert source last delivered. Read beside " +
					"tg_ingest_source_recent_total: silence from a source that never speaks is not an " +
					"incident, silence from one that delivered 500 alerts this week is the estate going deaf.",
				Value: now.Sub(r.LastSeen).Seconds(), Labels: lbl,
			})
		}
		out = append(out, metrics.Sample{
			Name: "tg_ingest_source_recent_total", Kind: metrics.Gauge,
			Help:  "alerts this source delivered in the baseline window — the DENOMINATOR for its silence.",
			Value: float64(r.RecentTotal), Labels: lbl,
		})
		// The NUMERATOR over that denominator (TG-373): incidents that named no machine at all — neither a
		// hostname nor a subject address. Emitted for every source, including at 0, so a source that
		// attributes everything is distinguishable from one that stopped being measured.
		//
		// Measured 2026-08-06 before this existed: 48 of 165 prometheus-alertmanager rows (29.1%) named no
		// machine, and the number was reachable only by querying Postgres by hand. Among those 48 were the
		// three alerts TG received about its own AWX outage.
		out = append(out, metrics.Sample{
			Name: "tg_ingest_source_recent_unattributed", Kind: metrics.Gauge,
			Help: "alerts in the baseline window that named NO MACHINE — no hostname and no subject IP. " +
				"Read as a FRACTION of tg_ingest_source_recent_total: an unattributed incident cannot be " +
				"blast-radius reasoned, deduped against the estate, or matched to its own ticket. Non-zero " +
				"is not automatically a defect — a workload-only alert legitimately names no machine — so " +
				"it is the ratio, and its change over time, that is worth watching.",
			Value: float64(r.RecentUnattributed), Labels: lbl,
		})
	}

	// VACUITY FLOOR, PUBLISHED. If the query returns no rows at all, every per-source series above is
	// absent and a rule written over them goes quiet — silence that reads exactly like health. This gauge
	// is the one series that is ALWAYS emitted, so "TG can see no sources whatsoever" is itself alertable.
	out = append(out, metrics.Sample{
		Name: "tg_ingest_sources_known", Kind: metrics.Gauge,
		Help: "how many alert sources TG has ever received from. ZERO means the intake table is empty or " +
			"unreadable — every per-source series is then absent, and absent must not read as healthy.",
		Value: float64(len(rows)),
	})
	out = append(out, metrics.Sample{
		Name: "tg_ingest_recent_total", Kind: metrics.Gauge,
		Help: "alerts received across ALL sources in the baseline window. A per-source rule cannot see " +
			"every source stopping at once, which is what happened on 2026-07-31.",
		Value: float64(fleetRecent),
	})
	if !newest.IsZero() {
		out = append(out, metrics.Sample{
			Name: "tg_ingest_last_seen_seconds", Kind: metrics.Gauge,
			Help:  "seconds since ANY alert arrived from any source. This is the estate-deaf gauge.",
			Value: now.Sub(newest).Seconds(),
		})
	}
	return out
}

// startIngestFreshnessJob polls the intake liveness on a cadence and hands the samples to the admin
// surface through an atomic pointer — the same shape the wiring registers use, and for the same reason: a
// /metrics scrape must never trigger a database query.
//
// Returns the reader the admin surface should call. A nil store yields a reader that emits nothing, so an
// un-pooled worker degrades to silence rather than panicking.
func startIngestFreshnessJob(ctx context.Context, store ingestFreshnessReader, declared []string, every, window time.Duration) func() []metrics.Sample {
	var held atomic.Pointer[[]metrics.Sample]
	reader := func() []metrics.Sample {
		if p := held.Load(); p != nil {
			return *p
		}
		return nil
	}
	if store == nil {
		log.Print("ingest freshness: no store — TG's OWN INPUT is unwatched. The intake can collapse " +
			"silently, which is exactly what went unnoticed for five days in TG-336.")
		return reader
	}

	refresh := func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		rows, err := store.Sources(rctx, window)
		if err != nil {
			// Deliberately does NOT clear the held samples. A transient DB error must not silently zero
			// the intake gauges — that would look identical to the estate going deaf, and would page.
			log.Printf("ingest freshness: read failed, keeping the previous reading: %v", err)
			return
		}
		s := ingestFreshnessSamples(rows, time.Now().UTC())
		// The declared-set comparison rides the same refresh: one read cadence, one hand-off, so the two
		// halves can never disagree about which sources exist.
		if never, nerr := store.SourcesNeverSeen(rctx, declared); nerr == nil {
			s = append(s, declaredSilentSamples(declared, never)...)
		} else {
			log.Printf("ingest freshness: declared-source check failed, freshness still published: %v", nerr)
		}
		held.Store(&s)
	}

	refresh() // publish immediately, so the gauges exist before the first tick
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
	log.Printf("ingest freshness: watching TG's own input every %s over a %s baseline — a source that "+
		"used to deliver and has stopped is now visible", every, window)
	return reader
}

// ingestFreshnessStoreOrNil keeps the composition root readable and, more importantly, keeps a typed-nil
// out of the interface. `var p *db.Pool = nil; var r ingestFreshnessReader = NewStore(p)` produces a
// NON-nil interface holding a nil pointer, so the `store == nil` guard above would not fire and the first
// query would panic. This returns an untyped nil in that case.
func ingestFreshnessStoreOrNil(pool *db.Pool) ingestFreshnessReader {
	if pool == nil {
		return nil
	}
	return db.NewIngestFreshnessStore(pool)
}

// declaredIngestSources lists the source ids this deployment CONFIGURES, from the module registry.
//
// The registry is the authority here, not the database: the whole point is to compare what the deployment
// believes it ingests against what has actually arrived. Only ENABLED capabilities count — a described but
// disabled module is not a promise, and flagging it would train an operator to ignore the gauge.
func declaredIngestSources(reg *modules.Registry) []string {
	if reg == nil {
		return nil
	}
	var out []string
	for _, cp := range reg.Capabilities() {
		if cp.Surface == modules.SurfaceIngest && cp.Enabled {
			out = append(out, cp.SourceType)
		}
	}
	sort.Strings(out) // deterministic, so the emitted series order is diff-stable
	return out
}
