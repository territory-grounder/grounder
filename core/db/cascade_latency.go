package db

import (
	"context"
	"fmt"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
)

// CascadeLatencyStore is the DURABLE EVIDENCE behind the learned falsifiability window (spec/002 REQ-110,
// TG-220): the observed propagation delay per causal edge, read straight out of TG's own front-door ledger
// (ingest_alert, migration 0033).
//
// WHY THIS LEDGER AND NOT SOMETHING NEW. The window has to be learned from what ACTUALLY happened, and
// ingest_alert is the one durable, append-only record of when each alert arrived. It needs no new table, no
// new write on the ingest path, and no migration — the same "compute it from data already stored" discipline
// as AlertHistoryStore.ShadowSuppressionSince. It is also the SAME stream the estate's self-learning
// confidence tier is fed from (core/learn.CoOccurrenceLearner pairs earlier-host → later-host inside a
// cascade window), so an edge's learned CONFIDENCE and its learned LATENCY are derived from one body of
// evidence and keyed identically: the ordered (primary → dependent) host pair.
//
// WHAT ONE OBSERVATION IS. For each raise of a primary host at t, and each OTHER host that raised in
// (t, t+maxLag], the FIRST such raise contributes one sample of (dependent_at − primary_at). "First" is what
// makes it a propagation delay rather than a count of how noisy the dependent is: a dependent that alerts
// five times after one primary event contributes ONE latency, not five.
//
// The percentile itself is deliberately NOT computed here. It is pure, deterministic Go in
// falsify.Percentile / falsify.EdgeWindow, exercised by the real scorer path — this store only supplies the
// samples, ordered oldest→newest so the trailing SAMPLE_CAP is the most recent evidence.
type CascadeLatencyStore struct{ p *Pool }

// NewCascadeLatencyStore returns the Postgres-backed observed-cascade-latency reader.
func NewCascadeLatencyStore(p *Pool) *CascadeLatencyStore { return &CascadeLatencyStore{p: p} }

// maxRootsPerPrimary multiplies the per-edge sample cap to bound how many of a primary's OWN raises feed the
// join. Generous on purpose: an edge keeps at most perEdge samples, so 8x leaves ample headroom for a
// dependent that follows only intermittently while still bounding a flapping host's contribution.
const maxRootsPerPrimary = 8

// EdgeLatencies returns the observed cascade-latency samples for every edge whose PRIMARY is one of
// primaries, over raises at-or-after since, keeping at most perEdge of the MOST RECENT samples per edge and
// returning each edge's samples oldest→newest.
//
// Bounded FOUR ways so a periodic cron read can never run away: the primary host list (only the targets of
// the due batch), the `since` horizon, maxLag, and perEdge. maxLag bounds what may count as a cascade at all
// — a dependent alerting hours after the primary is a coincidence, not a propagation delay, and a sample
// longer than the window cap could not widen the window past the cap anyway.
//
// The fourth bound is on the ROOT raises: only the most recent maxRootsPerPrimary x perEdge raises of each
// primary feed the join. Without it a chatty (flapping) target host makes the join's input grow with its own
// alert count times the estate size. It is the same RECENCY principle as the trailing sample cap applied one
// level up, and it cannot bias the percentile toward "faster": dropping the OLDEST root raises can only
// remove old samples, which the trailing cap would discard anyway.
//
// Every value is a BOUND parameter ($1..$5); nothing is string-built (INV-03).
func (s *CascadeLatencyStore) EdgeLatencies(ctx context.Context, primaries []string, since time.Time, maxLag time.Duration, perEdge int) (map[falsify.CascadeEdge][]time.Duration, error) {
	out := map[falsify.CascadeEdge][]time.Duration{}
	if len(primaries) == 0 || perEdge <= 0 || maxLag <= 0 {
		return out, nil
	}
	rows, err := s.p.Query(ctx, `
		WITH root AS (
		    SELECT host, received_at FROM (
		        SELECT host, received_at,
		               row_number() OVER (PARTITION BY host ORDER BY received_at DESC) AS rn
		          FROM ingest_alert
		         WHERE host = ANY($1) AND host <> '' AND received_at >= $2
		    ) r WHERE rn <= $5
		), lagged AS (
		    -- ONE sample per (primary raise, dependent): the FIRST dependent raise inside the lag bound, so a
		    -- chatty dependent contributes a propagation delay, not a vote.
		    SELECT r.host AS primary_host, d.host AS dependent_host, r.received_at AS root_at,
		           MIN(d.received_at - r.received_at) AS lag
		      FROM root r
		      JOIN ingest_alert d
		        ON d.host <> '' AND d.host <> r.host
		       AND d.received_at > r.received_at
		       AND d.received_at <= r.received_at + $3::interval
		     GROUP BY r.host, r.received_at, d.host
		), ranked AS (
		    SELECT primary_host, dependent_host, root_at, lag,
		           row_number() OVER (PARTITION BY primary_host, dependent_host ORDER BY root_at DESC) AS rn
		      FROM lagged
		)
		SELECT primary_host, dependent_host, EXTRACT(EPOCH FROM lag)::float8
		  FROM ranked
		 WHERE rn <= $4
		 ORDER BY primary_host, dependent_host, root_at ASC`,
		primaries, since, maxLag, perEdge, perEdge*maxRootsPerPrimary)
	if err != nil {
		return nil, fmt.Errorf("db: cascade latencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var primary, dependent string
		var seconds float64
		if err := rows.Scan(&primary, &dependent, &seconds); err != nil {
			return nil, fmt.Errorf("db: cascade latency scan: %w", err)
		}
		if seconds <= 0 {
			continue // not a propagation delay
		}
		k := falsify.CascadeEdge{Primary: primary, Dependent: dependent}
		out[k] = append(out[k], time.Duration(seconds*float64(time.Second)))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: cascade latencies iterate: %w", err)
	}
	return out, nil
}

// CascadeLatencies adapts the store into the scorer's falsify.LatencyReader seam, and it is the ONE place the
// error→ok mapping lives for the learned window. A read error is (nil, false) — the scorer then leaves every
// edge on the 900s FLOOR, which is the fail-safe direction: a database blip must never SHORTEN an observation
// window and manufacture the very misses TG-220 exists to stop recording.
func CascadeLatencies(s *CascadeLatencyStore, maxLag time.Duration, perEdge int) falsify.LatencyReader {
	return func(ctx context.Context, primaries []string, since time.Time) (map[falsify.CascadeEdge][]time.Duration, bool) {
		m, err := s.EdgeLatencies(ctx, primaries, since, maxLag, perEdge)
		if err != nil {
			return nil, false
		}
		return m, true
	}
}
