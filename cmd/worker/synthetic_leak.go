package main

import (
	"context"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// The live-DB-leak register (TG-190a, CONSTITUTION §4.9 — "synthetic canaries against an isolated
// throwaway DB (live-DB-leak counter must stay 0)").
//
// The counter is built BEFORE the canary injector on purpose: the hazard a synthetic-incident canary
// carries is not that it fails, it is that one of its rows reaches the LIVE database, where the judge
// scores it and the flywheel learns from it. An injector shipped without this would run that hazard with
// nothing pointed at it.

// syntheticLeakReader is the one read this register needs, as a seam so the oracles drive it without a
// database. A nil reader degrades to a REFUSAL sample rather than silence — see below.
type syntheticLeakReader interface {
	LeakCount(ctx context.Context) (db.SyntheticLeak, error)
}

// syntheticLeakSamples publishes the tripwire.
//
// THE ABSENT CASE IS THE WHOLE DESIGN PROBLEM, and it is why this emits three series rather than one. A
// gauge whose healthy value is 0 reads identically when it is healthy, when its store is unwired, and when
// its query is broken — and the reassuring reading wins by default. So:
//
//	tg_synthetic_rows_live       the leak itself; MUST be 0
//	tg_synthetic_scan_population the denominator it was counted from — 0-of-3383 and 0-of-0 differ
//	tg_synthetic_scan_ok         1 only when the count was actually taken
//
// An alert may only trust tg_synthetic_rows_live == 0 while tg_synthetic_scan_ok == 1 and the population is
// non-zero. That is the same vacuity floor the actuation-guard coverage check learned the hard way: a
// denominator that can shrink to fit the failure is a coverage claim, not a check.
func syntheticLeakSamples(leak db.SyntheticLeak, scanned bool) []metrics.Sample {
	ok := 0.0
	if scanned {
		ok = 1
	}
	return []metrics.Sample{
		{
			Name: "tg_synthetic_rows_live", Kind: metrics.Gauge,
			Help: "session_triage rows marked synthetic on the LIVE database. CONSTITUTION 4.9 requires " +
				"this to stay 0: a non-zero value is a synthetic canary that escaped its throwaway " +
				"database into the corpus the judge scores and the flywheel learns from. Read ONLY " +
				"alongside tg_synthetic_scan_ok — an unwired store also reads 0.",
			Value: float64(leak.Leaked),
		},
		{
			Name: "tg_synthetic_scan_population", Kind: metrics.Gauge,
			Help: "rows the leak count was taken over — the DENOMINATOR. '0 leaked of 3383' is evidence; " +
				"'0 leaked' alone is a number that a broken query, an empty table and a healthy system " +
				"all produce identically.",
			Value: float64(leak.Total),
		},
		{
			Name: "tg_synthetic_scan_ok", Kind: metrics.Gauge,
			Help: "1 = the leak count was actually taken this scrape; 0 = no store wired, or the read " +
				"failed, or the database predates migration 0069 and structurally CANNOT record a leak. " +
				"A canary must never be armed while this reads 0.",
			Value: ok,
		},
	}
}

// collectSyntheticLeak takes one reading. A nil reader or a failed read yields scanned=false, which drives
// tg_synthetic_scan_ok to 0 — the register still emits, so its silence is never mistaken for its zero.
func collectSyntheticLeak(ctx context.Context, r syntheticLeakReader) []metrics.Sample {
	if r == nil {
		return syntheticLeakSamples(db.SyntheticLeak{}, false)
	}
	leak, err := r.LeakCount(ctx)
	if err != nil {
		return syntheticLeakSamples(db.SyntheticLeak{}, false)
	}
	return syntheticLeakSamples(leak, true)
}

// syntheticLeakStoreOrNil mirrors the other registers' store seam: a nil pool yields a nil reader, which
// collectSyntheticLeak reports as scan_ok=0 rather than as a clean zero.
func syntheticLeakStoreOrNil(pool *db.Pool) syntheticLeakReader {
	if pool == nil {
		return nil
	}
	return db.NewTriageStore(pool)
}
