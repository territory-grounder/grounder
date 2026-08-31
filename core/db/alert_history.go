package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/verify"
)

// AlertHistoryStore reads the raise/clear history for one alert key, which is the durable state
// ingest.DecideSuppress needs and the batch-local Pipeline deliberately does not keep.
//
// THE KEY IS (host, alert_rule), NOT (source, host, alert_rule). A recovery is recorded in
// ingest_transition with a host and a rule but NO source id (verified live: 1,364 recovery rows, every one
// carrying host and alert_rule, none carrying a source). That is not a schema gap to work around — it is
// correct. The incident belongs to the HOST, so a clear for "Device-Down on dc1mealie01" closes that
// incident regardless of which monitoring system happened to report it. Keying on the source as well would
// let a recovery from LibreNMS fail to close an incident raised by the liveness poller, and the second
// detector's alerts would then be suppressed against an incident nothing could ever clear.
type AlertHistoryStore struct{ p *Pool }

// NewAlertHistoryStore builds the reader.
func NewAlertHistoryStore(p *Pool) *AlertHistoryStore { return &AlertHistoryStore{p: p} }

// KeyHistory returns the merged raise/clear timeline for (host, alertRule) at or after since, ascending.
//
// Raises come from ingest_alert (what the front door admitted) and clears from ingest_transition
// (kind='recovery'). They are merged in ONE query with a UNION ALL and ordered by the database rather than
// stitched in Go: two separately-sorted lists zipped by hand is where an off-by-one puts a clear before the
// raise it closed, and DecideSuppress reads that ordering as "the incident is already closed" — the failure
// direction that suppresses a live incident.
//
// `since` bounds the scan. It should be at least ingest.MaxOpenIncident, or an incident older than the
// window would look unseen and its repeats would be admitted as new.
func (s *AlertHistoryStore) KeyHistory(ctx context.Context, host, alertRule string, since time.Time) ([]ingest.Fire, error) {
	if host == "" || alertRule == "" {
		// A key that identifies nothing must not match everything: an empty host would otherwise collapse
		// the whole estate into one incident and suppress across unrelated machines.
		return nil, fmt.Errorf("db: alert history needs both host and alert_rule (got host=%q rule=%q)", host, alertRule)
	}
	rows, err := s.p.Query(ctx, `
		SELECT at, recovered FROM (
		    SELECT received_at AS at, false AS recovered
		      FROM ingest_alert
		     WHERE host = $1 AND alert_rule = $2 AND received_at >= $3
		    UNION ALL
		    SELECT received_at AS at, true AS recovered
		      FROM ingest_transition
		     WHERE host = $1 AND alert_rule = $2 AND received_at >= $3 AND kind = 'recovery'
		) h
		ORDER BY at ASC`, host, alertRule, since)
	if err != nil {
		return nil, fmt.Errorf("db: alert key history: %w", err)
	}
	defer rows.Close()

	var out []ingest.Fire
	for rows.Next() {
		var f ingest.Fire
		if err := rows.Scan(&f.At, &f.Recovered); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ShadowSummary is what incident-scoped suppression WOULD have done over a window, computed entirely from
// data already stored. No new table, no write on the ingest path, no runtime cost — the evidence needed to
// decide whether to enable suppression is a pure read over ingest_alert + ingest_transition.
type ShadowSummary struct {
	Accepted      int // alerts admitted by the front door in the window
	WouldSuppress int // of those, repeats of a still-OPEN incident
	Keys          int // distinct (host, alert_rule) incidents seen
}

// ShadowSuppressionSince computes ShadowSummary over the window.
//
// ★ THIS IS A SECOND, INDEPENDENT IMPLEMENTATION OF ingest.DecideSuppress, in SQL, and that is deliberate.
// The Go version walks a key's history backwards and stops at the first event; LAG over the same merged
// timeline partitioned by key IS that walk — the immediately preceding event. An oracle asserts the two
// agree row-for-row on the golden fixture, so a divergence in either is caught rather than averaged away.
// Two implementations that must agree is a stronger check than either alone, and it is the only way to
// trust a shadow number that will be used to justify dropping real alerts.
//
// The verdict per raise: suppress iff a prior event exists for the key, it was NOT a recovery, and it falls
// within the staleness bound. Recoveries are timeline entries only — they are never counted as accepted
// alerts, because they are not alerts.
func (s *AlertHistoryStore) ShadowSuppressionSince(ctx context.Context, since time.Time, maxOpen time.Duration) (ShadowSummary, error) {
	var out ShadowSummary
	err := s.p.QueryRow(ctx, `
		WITH timeline AS (
		    SELECT host, alert_rule, received_at AS at, false AS recovered
		      FROM ingest_alert
		     WHERE received_at >= $1 AND host <> '' AND alert_rule <> ''
		    UNION ALL
		    SELECT host, alert_rule, received_at AS at, true AS recovered
		      FROM ingest_transition
		     WHERE received_at >= $1 AND kind = 'recovery' AND host <> '' AND alert_rule <> ''
		), walked AS (
		    SELECT host, alert_rule, at, recovered,
		           LAG(recovered) OVER w AS prev_recovered,
		           LAG(at)        OVER w AS prev_at
		      FROM timeline
		    WINDOW w AS (PARTITION BY host, alert_rule ORDER BY at)
		)
		SELECT count(*) FILTER (WHERE NOT recovered),
		       count(*) FILTER (WHERE NOT recovered AND prev_at IS NOT NULL
		                          AND prev_recovered = false AND at - prev_at <= $2),
		       count(DISTINCT (host, alert_rule))
		  FROM walked`, since, maxOpen).
		Scan(&out.Accepted, &out.WouldSuppress, &out.Keys)
	if err != nil {
		return ShadowSummary{}, fmt.Errorf("db: shadow suppression summary: %w", err)
	}
	return out, nil
}

// OpenIncidentHosts reports EVERY host that held an OPEN incident as of asOf — a raise at-or-before asOf
// with no recovery between it and asOf — bounded by staleAfter exactly as ActiveByOpenIncident below.
//
// This is the actuation verifier's PRE-ANOMALOUS BASELINE (the 2026-07-28 false deviation, ledger 5153-5155):
// a host that was already broken BEFORE an action executed cannot be that action's cascade, so the verifier
// subtracts this set from its surprise candidates. Three properties make this the right instrument where the
// LibreNMS point-read is not:
//
//   - DURABLE. It reads TG's own ingest ledger, so it does not share a failure mode with the live monitoring
//     HTTP surface the post-state read uses. The false deviation was manufactured in the one second where the
//     point-read baseline was unestablished and the post-read succeeded.
//   - ANCHORED. Both arms cut at received_at <= asOf, so an alert raised by the action itself (arriving after
//     asOf) can NEVER launder into the baseline and hide a real cascade. The caller anchors asOf at the
//     moment before the effect fired.
//   - ESTATE-WIDE. The verifier does not know pre-execution which hosts will appear in the post-state, so the
//     baseline must cover every host, not a candidate list.
//
// The verdict consumer treats this at HOST granularity deliberately: an already-broken host firing a NEW rule
// label is overwhelmingly the same incident evolving (or a monitoring relabel), not a fresh cascade; the
// residual real-cascade-onto-already-broken-host case is caught by the settle-window reconcile, which
// re-observes after recovery.
func (s *AlertHistoryStore) OpenIncidentHosts(ctx context.Context, asOf time.Time, staleAfter time.Duration) (map[string]bool, error) {
	rows, err := s.p.Query(ctx, `
		SELECT host FROM (
		    SELECT DISTINCT ON (host) host, at, recovered
		      FROM (
		          SELECT host, received_at AS at, false AS recovered
		            FROM ingest_alert WHERE received_at <= $1 AND host <> ''
		          UNION ALL
		          SELECT host, received_at AS at, true AS recovered
		            FROM ingest_transition WHERE received_at <= $1 AND kind = 'recovery' AND host <> ''
		      ) t
		     ORDER BY host, at DESC
		) latest
		WHERE NOT recovered AND at >= ($1::timestamptz - $2::interval)`, asOf, staleAfter)
	if err != nil {
		return nil, fmt.Errorf("db: open incident hosts as of %s: %w", asOf.Format(time.RFC3339), err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// OpenIncidentPairs reports every (host, alert_rule) pair that held an OPEN incident as of asOf — the same
// merged raise/clear walk as OpenIncidentHosts, at PAIR granularity: a raise for the pair at-or-before asOf
// with no recovery between it and asOf, bounded by staleAfter.
//
// This is the falsifiability scorer's PAIR-arm baseline (Phase C4): the (host,rule) alerts that were ALREADY
// firing when a prediction was committed, so an ambient alert predating the forecast cannot read as its
// failed cascade. It answers the same question as the interceptor's live pre-execution snapshot (REQ-1228's
// pair arm) but from TG's own durable ledger, which is what makes it reconstructible at SCORING time anchored
// at the prediction's CommittedAt: received_at <= asOf cuts both arms, so nothing that fired after the
// commit can launder into the baseline. Pair granularity deliberately complements the host arm — the host
// arm swallows an open incident evolving its rule label; the pair arm records exactly which alert was live.
func (s *AlertHistoryStore) OpenIncidentPairs(ctx context.Context, asOf time.Time, staleAfter time.Duration) ([]verify.ObservedAlert, error) {
	rows, err := s.p.Query(ctx, `
		SELECT host, alert_rule FROM (
		    SELECT DISTINCT ON (host, alert_rule) host, alert_rule, at, recovered
		      FROM (
		          SELECT host, alert_rule, received_at AS at, false AS recovered
		            FROM ingest_alert WHERE received_at <= $1 AND host <> '' AND alert_rule <> ''
		          UNION ALL
		          SELECT host, alert_rule, received_at AS at, true AS recovered
		            FROM ingest_transition WHERE received_at <= $1 AND kind = 'recovery' AND host <> '' AND alert_rule <> ''
		      ) t
		     ORDER BY host, alert_rule, at DESC
		) latest
		WHERE NOT recovered AND at >= ($1::timestamptz - $2::interval)`, asOf, staleAfter)
	if err != nil {
		return nil, fmt.Errorf("db: open incident pairs as of %s: %w", asOf.Format(time.RFC3339), err)
	}
	defer rows.Close()
	var out []verify.ObservedAlert
	for rows.Next() {
		var h, r string
		if err := rows.Scan(&h, &r); err != nil {
			return nil, err
		}
		out = append(out, verify.ObservedAlert{Host: h, Rule: r})
	}
	return out, rows.Err()
}

// OpenIncidentsBaseline adapts OpenIncidentHosts into the (set, ok) shape the actuation interceptor and the
// async verifier consume, and it is the ONE place the error→ok mapping lives. A read error is (nil, false) —
// never (empty, true): an empty map asserts "no host was anomalous", which on a failed read is exactly the
// manufactured-deviation defect this baseline exists to close, reproduced at a new seam. The mutation control
// for that property lives on THIS function.
func OpenIncidentsBaseline(s *AlertHistoryStore, staleAfter time.Duration) func(context.Context, time.Time) (map[string]bool, bool) {
	return func(ctx context.Context, asOf time.Time) (map[string]bool, bool) {
		m, err := s.OpenIncidentHosts(ctx, asOf, staleAfter)
		if err != nil {
			return nil, false
		}
		return m, true
	}
}

// ActiveByOpenIncident reports which of hosts currently have an OPEN incident — a raise with no recovery
// after it — rather than "a row arrived in the last N minutes".
//
// WHY THIS EXISTS, measured on the live estate: 11 hosts currently hold an open incident and only 2 of them
// fall inside the 15-minute window AlertLogStore.ActiveHosts uses (temporal/runner/activities.go:1212).
// NINE — 82% — are genuinely down and invisible to common-cause corroboration, because a host that has been
// failing for twenty minutes simply stops producing rows. Corroboration exists to tell an ISOLATED
// host-down from a shared-parent failure, and it is answering that question while blind to most of the
// evidence.
//
// It is also IMMUNE TO SUPPRESSION BY CONSTRUCTION, which the row-recency definition is not. Incident-scoped
// dedup keeps the FIRST alert and drops repeats; the first alert of an open incident is often hours old, so
// under the recency definition suppression removes exactly the rows that keep a host visible — measured at
// 140 of 203 suppressed alerts leaving no in-window evidence at all. Asking "is there an open incident"
// cannot be affected by how many times that incident re-announced itself.
//
// staleAfter bounds an incident whose recovery never arrived: past it the incident is treated as closed, on
// the same reasoning as ingest.MaxOpenIncident — a lost recovery is a monitoring gap, and without the bound
// a host would corroborate forever.
func (s *AlertHistoryStore) ActiveByOpenIncident(ctx context.Context, hosts []string, now time.Time, staleAfter time.Duration) (map[string]bool, error) {
	out := map[string]bool{}
	if len(hosts) == 0 {
		return out, nil
	}
	rows, err := s.p.Query(ctx, `
		SELECT host FROM (
		    SELECT DISTINCT ON (host) host, at, recovered
		      FROM (
		          SELECT host, received_at AS at, false AS recovered
		            FROM ingest_alert WHERE host = ANY($1)
		          UNION ALL
		          SELECT host, received_at AS at, true AS recovered
		            FROM ingest_transition WHERE host = ANY($1) AND kind = 'recovery'
		      ) t
		     ORDER BY host, at DESC
		) latest
		WHERE NOT recovered AND at >= ($2::timestamptz - $3::interval)`, hosts, now, staleAfter)
	if err != nil {
		return nil, fmt.Errorf("db: active by open incident: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// AlertedHosts returns the distinct hosts TG has been alerted about inside the window — the denominator for
// host-diagnostic reach (TG-271).
//
// Deliberately UNFILTERED. It returns every distinct `host` value including the Kubernetes component names
// Alertmanager puts there (cilium-agent, coredns, node-exporter…), because deciding which of those is a
// real host is the CALLER's job and doing it here would bury the judgement inside a SQL string. The caller
// splits them by resolvability and publishes both numbers; a query that quietly dropped the non-hosts would
// make the coverage figure look better without anyone being able to see why.
//
// Empty host values are excluded — they are not a host TG failed to cover, they are an alert with no host.
func (s *AlertHistoryStore) AlertedHosts(ctx context.Context, window time.Duration) ([]string, error) {
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}
	rows, err := s.p.Query(ctx, `
		SELECT DISTINCT host FROM ingest_alert
		WHERE host <> '' AND received_at > now() - $1::interval
		ORDER BY host`, window.String())
	if err != nil {
		return nil, fmt.Errorf("db: alerted hosts: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("db: alerted host scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
