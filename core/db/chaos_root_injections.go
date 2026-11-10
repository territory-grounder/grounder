package db

import (
	"context"
	"fmt"
	"time"
)

// ChaosRootInjections counts, per root host, the DISTINCT injected faults since `since` — the DENOMINATOR the
// chaos tier's disproof decay divides by (TG-673, deep-review F13). ChaosCascades (chaos_read.go) counts only
// the injections a downstream FOLLOWED; on its own that number can only grow, so a pair that cascaded once
// and then failed to cascade on the next nine injections of the same root still read as 0.90 ground truth.
// With this count beside it the source can read "1 of 10" and let the edge decay toward irrelevance.
//
// Deliberately coarse: it counts every fault class injected on the root, not only the classes that could
// cascade (a service-down on a guest does not take its dependents with it the way a device-down does), so the
// ratio UNDER-states a real dependency rather than over-stating it — the safe direction for a tier that
// feeds blast-radius prediction. Bound parameters only.
func (s *AxisReadStore) ChaosRootInjections(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT host, count(DISTINCT id)
		FROM injected_fault
		WHERE injected_at >= $1 AND host <> ''
		GROUP BY host`, since)
	if err != nil {
		return nil, fmt.Errorf("db: chaos root injections: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var host string
		var n int
		if err := rows.Scan(&host, &n); err != nil {
			return nil, fmt.Errorf("db: chaos root injections scan: %w", err)
		}
		out[host] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: chaos root injections rows: %w", err)
	}
	return out, nil
}
