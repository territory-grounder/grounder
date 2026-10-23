package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TG-188 organic recovery: RecoveryEventsSince is the learner's clear feed — recovery transitions strictly
// after a (received_at, id) cursor, in cursor order, capped, with the cursor handed back so a row is never
// re-fed (ObserveClear is not idempotent) and never lost (the id tiebreaker resumes INSIDE a tied group).
func TestRecoveryEventsSinceFeedsStrictlyAfterTheCursor(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("recfeed-%d", os.Getpid())
	h1, h2 := uniq+"-h1", uniq+"-h2"
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	defer func() {
		for _, h := range []string{h1, h2} {
			_, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE host = $1`, h)
		}
	}()
	seed := func(host, kind string, at time.Time) {
		t.Helper()
		if _, err := p.Exec(ctx, `INSERT INTO ingest_transition (external_ref, kind, host, alert_rule, received_at)
			VALUES ($1,$2,$3,'Device-Down',$4)`, fmt.Sprintf("%s-%s-%d", uniq, host, at.UnixNano()), kind, host, at); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(h1, "recovery", base.Add(1*time.Minute))
	seed(h2, "recovery", base.Add(2*time.Minute))
	seed(h1, "raise", base.Add(3*time.Minute)) // a non-recovery transition must never feed a clear
	seed(h2, "recovery", base.Add(4*time.Minute))

	store := NewTransitionLogStore(p)
	ours := func(host string) bool { return host == h1 || host == h2 }

	// First pull from before the fixture: our three recoveries, in cursor order; the raise excluded.
	got, cur, err := store.RecoveryEventsSince(ctx, RecoveryCursor{At: base})
	if err != nil {
		t.Fatalf("RecoveryEventsSince: %v", err)
	}
	var mine []time.Time
	for _, c := range got {
		if ours(c.Host) {
			mine = append(mine, c.At)
		}
	}
	if len(mine) != 3 {
		t.Fatalf("got %d fixture clears, want 3 (the raise row must not feed)", len(mine))
	}
	for i := 1; i < len(mine); i++ {
		if mine[i].Before(mine[i-1]) {
			t.Fatal("clears not in cursor order")
		}
	}
	if cur.At.Before(base.Add(4 * time.Minute)) {
		t.Errorf("cursor.At = %s, want >= the latest read recovery %s", cur.At, base.Add(4*time.Minute))
	}

	// KILLING MUTATION (strictly-after): a second pull from the returned cursor must re-feed NOTHING of
	// ours — weaken the row-value comparison to >= and the last clear re-feeds, reddening this.
	got2, _, err := store.RecoveryEventsSince(ctx, cur)
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	for _, c := range got2 {
		if ours(c.Host) {
			t.Errorf("row re-fed across the cursor: %s@%s", c.Host, c.At)
		}
	}

	// Empty-input: a cursor past everything yields no rows and an UNCHANGED cursor.
	future := RecoveryCursor{At: base.Add(24 * time.Hour)}
	got3, cur3, err := store.RecoveryEventsSince(ctx, future)
	if err != nil {
		t.Fatalf("future pull: %v", err)
	}
	for _, c := range got3 {
		if ours(c.Host) {
			t.Errorf("phantom clear: %+v", c)
		}
	}
	if !cur3.At.Equal(future.At) || cur3.ID != future.ID {
		t.Errorf("empty pull moved the cursor: %+v → %+v", future, cur3)
	}
}

// THE REVIEW FINDING'S ORACLE: a bulk INSERT stamps MANY rows with ONE received_at (now() evaluates once
// per statement), and a timestamp-only watermark landing inside such a tied group at the LIMIT boundary
// would PERMANENTLY drop the unread remainder. The (received_at, id) cursor resumes inside the group:
// every row is fed exactly once across pulls. Killing mutation: drop the id tiebreaker from the cursor
// comparison/ORDER BY (timestamp-only) and the "fed exactly once" count reddens (rows lost).
func TestRecoveryEventsSinceSurvivesTiedTimestampsAtTheCapBoundary(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("rectie-%d", os.Getpid())
	host := uniq + "-host"
	tied := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	total := recoveryFeedCap + 52 // strictly more than one capped pull, ALL at one timestamp
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_transition WHERE host = $1`, host) }()
	if _, err := p.Exec(ctx, `
		INSERT INTO ingest_transition (external_ref, kind, host, alert_rule, received_at)
		SELECT $1 || '-' || g, 'recovery', $2, 'Device-Down', $3 FROM generate_series(1, $4) g`,
		uniq, host, tied, total); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}

	store := NewTransitionLogStore(p)
	fed := 0
	cur := RecoveryCursor{At: tied.Add(-time.Second)}
	for pulls := 0; pulls < 4; pulls++ { // 2 pulls suffice; the bound proves termination
		got, next, err := store.RecoveryEventsSince(ctx, cur)
		if err != nil {
			t.Fatalf("pull %d: %v", pulls, err)
		}
		n := 0
		for _, c := range got {
			if c.Host == host {
				n++
			}
		}
		if n == 0 && len(got) == 0 {
			break
		}
		fed += n
		cur = next
	}
	if fed != total {
		t.Errorf("fed %d of %d tied-timestamp rows — a timestamp-only watermark silently drops the remainder past the cap boundary", fed, total)
	}
}
