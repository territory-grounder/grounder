package db

import (
	"context"
	"fmt"
)

// The LIVE-DB-LEAK COUNTER (TG-190a, CONSTITUTION §4.9: "synthetic canaries against an isolated throwaway
// DB (live-DB-leak counter must stay 0)").
//
// WHY THIS EXISTS BEFORE THE CANARY DOES. A synthetic-incident injector drives known traffic through the
// real loop against a THROWAWAY database. The hazard it carries is not that the canary fails — it is that a
// canary row reaches the LIVE database, where the judge scores it, the flywheel learns from it, and every
// rate computed over the corpus silently measures TG's own test traffic. Shipping the injector first would
// mean running that hazard with no instrument pointed at it.
//
// So this is the tripwire, and its correct reading is ZERO — forever, on the live database. It is one of the
// few gauges here whose healthy value is the absence of its subject, which makes the vacuity question sharp:
// a counter that reads 0 because nothing can ever set it is indistinguishable from one reading 0 because
// nothing has leaked. LeakCount answers that by returning the TOTAL alongside, so 0-of-3383 and 0-of-0 are
// different readings rather than the same number.

// SyntheticLeak is one reading of the live database's synthetic contamination.
type SyntheticLeak struct {
	// Leaked is the number of session_triage rows marked synthetic. On the LIVE database this must be 0.
	Leaked int64
	// Total is the population Leaked was counted out of. It is the denominator, published so a zero is
	// readable: "0 of 3,383 rows are synthetic" is evidence, "0" alone is a number that a broken query, an
	// empty table, and a healthy system all produce identically.
	Total int64
}

// Clean reports whether the live database is free of synthetic contamination. It is FALSE for an empty
// population as well as for a leak: a database with no sessions at all has not demonstrated cleanliness,
// it has demonstrated nothing, and a canary's safety argument may not rest on an unmeasured population.
func (l SyntheticLeak) Clean() bool { return l.Total > 0 && l.Leaked == 0 }

// LeakCount reads the synthetic-marker contamination of the live corpus.
//
// A database that predates migration 0069 has no such column; that is reported as an ERROR rather than as
// zero. "The marker does not exist yet" and "the marker exists and nothing is marked" are different facts,
// and only the second one is a safety statement — collapsing them would let the counter certify a database
// that structurally cannot record a leak.
func (s *TriageStore) LeakCount(ctx context.Context) (SyntheticLeak, error) {
	var out SyntheticLeak
	err := s.p.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE synthetic), count(*)
		FROM session_triage`).Scan(&out.Leaked, &out.Total)
	if err != nil {
		return SyntheticLeak{}, fmt.Errorf("db: synthetic leak count (a database without migration 0069 "+
			"cannot record a canary leak, so this is an error and never a clean zero): %w", err)
	}
	return out, nil
}
