package db

// TG-668 (the observation half; G4A fail-open readers). The clear/necessity reader used to answer "is HOST quiet?"
// from a HOST-granular walk: the latest event on the host, of ANY rule, decided — so a recovery of rule B arriving
// after the raise of rule A read the host as quiet while A never recovered (cross-rule fail-open), and a host the
// ledger had never seen read as quiet because it was absent from the open set (never-monitored fail-open). This
// file scopes the reading to the SUBJECT host the caller names: open-ness is decided PER (host, rule) pair — a pair
// closes only on a recovery of its own rule — and a host with no ingest history at all is UNOBSERVABLE (ok=false),
// never quiet.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// hostScope is one subject host's reading: whether the ledger has ever seen it, and how many of its (host, rule)
// pairs are currently OPEN at pair granularity inside the staleness bound. AgedOpen counts pairs whose latest event
// is a raise/re-fire OLDER than the bound with no recovery after it — recorded for the evidence string and the
// census, not (yet) a verdict: the live ledger carries such pairs on most active hosts because recovery deliveries
// are lost upstream (2026-11-07 census: 27 on one host, 22 on others), so reading them as "unknown" would stall
// every clear on those hosts; see the TG-668 note.
type hostScope struct {
	Observable bool
	FreshOpen  int
	AgedOpen   int
}

// hostScopeFor reads one host's scope as of asOf. Hosts compare case-insensitively.
func (o *LedgerObserver) hostScopeFor(ctx context.Context, host string, asOf time.Time) (hostScope, error) {
	h := strings.ToLower(strings.TrimSpace(host))
	var s hostScope
	if h == "" {
		return s, nil
	}
	if err := o.p.QueryRow(ctx, `
		WITH ev AS (
		    SELECT alert_rule, received_at AS at, false AS recovered
		      FROM ingest_alert WHERE lower(host) = $1 AND received_at <= $2
		    UNION ALL
		    SELECT alert_rule, received_at AS at, false AS recovered
		      FROM ingest_alert_occurrence WHERE lower(host) = $1 AND received_at <= $2
		    UNION ALL
		    SELECT alert_rule, received_at AS at, true AS recovered
		      FROM ingest_transition WHERE lower(host) = $1 AND received_at <= $2 AND kind = 'recovery'
		), latest AS (
		    SELECT DISTINCT ON (alert_rule) alert_rule, at, recovered FROM ev ORDER BY alert_rule, at DESC
		)
		SELECT (SELECT count(*) FROM ev) > 0,
		       count(*) FILTER (WHERE NOT recovered AND at >= ($2::timestamptz - $3::interval)),
		       count(*) FILTER (WHERE NOT recovered AND at <  ($2::timestamptz - $3::interval))
		  FROM latest`, h, asOf, o.staleAfter).Scan(&s.Observable, &s.FreshOpen, &s.AgedOpen); err != nil {
		return hostScope{}, fmt.Errorf("db: host scope for %s: %w", h, err)
	}
	return s, nil
}
