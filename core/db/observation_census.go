package db

import (
	"context"
	"fmt"
	"time"
)

// LastAlertByHost returns, for every host that has EVER produced an admitted alert in the retained
// ingest_alert history, the most recent time it did (TG-180 observation census). A host ABSENT from the result
// has never fired — the census reads that absence as "structurally unobservable": no alert rule has ever
// matched it, so TG has no evidence it can see the host at all.
//
// It reads the FRONT-DOOR log (ingest_alert = what TG admitted), not a raw provider feed, so the claim is
// exactly "TG has never triaged a signal for this host", which is the honest, TG-grounded reading. Bound query,
// no string interpolation; grouped on the host index, O(rows).
func (s *AxisReadStore) LastAlertByHost(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT host, max(received_at) FROM ingest_alert WHERE host <> '' GROUP BY host`)
	if err != nil {
		return nil, fmt.Errorf("db: last-alert-by-host census: %w", err)
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var host string
		var last time.Time
		if err := rows.Scan(&host, &last); err != nil {
			return nil, fmt.Errorf("db: last-alert-by-host scan: %w", err)
		}
		out[host] = last
	}
	return out, rows.Err()
}
