package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
)

// The observed-cascade-latency read round-trips against a real Postgres (spec/002 REQ-110, TG-220): the
// durable ingest ledger is the ONLY source of the p95 the learned falsifiability window is built on, so the
// SQL that derives a propagation delay from it is worth pinning against the real planner rather than a
// hand-written twin. Skipped in CI (no DB); runs under compose when TG_TEST_POSTGRES_DSN points at a
// migrated database.
//
// It asserts the four properties the window rule depends on:
//
//	(a) ONE sample per (primary raise, dependent) — the FIRST subsequent raise, so a chatty dependent
//	    contributes a propagation delay rather than a vote;
//	(b) the maxLag bound excludes a raise too far after the primary to be a cascade;
//	(c) samples come back oldest→newest, which is what makes falsify.EdgeWindow's trailing SAMPLE_CAP
//	    the MOST RECENT evidence rather than an arbitrary slice;
//	(d) the edge key is DIRECTED — (a→b) and (b→a) are different edges.
func TestCascadeLatencyIntegration(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the pgx cascade-latency integration test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	primary := "lat-primary-" + uniq
	dependent := "lat-dependent-" + uniq
	far := "lat-far-" + uniq
	defer func() {
		_, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host IN ($1, $2, $3)`, primary, dependent, far)
	}()

	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	seed := func(host string, at time.Time, n int) {
		_, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1, 'test', '', 'HostDown', 'critical', $2, $3, $3)`,
			fmt.Sprintf("ref-lat-%s-%d-%s", host, n, uniq), host, at)
		if err != nil {
			t.Fatalf("seed %s: %v", host, err)
		}
	}

	// Cascade 1: the dependent follows 60s later — and again at 200s (a repeat that must NOT become a
	// second sample for this primary raise).
	seed(primary, base, 1)
	seed(dependent, base.Add(60*time.Second), 2)
	seed(dependent, base.Add(200*time.Second), 3)
	// Cascade 2, an hour later: the dependent follows 900s later (the slow observation that widens the window).
	seed(primary, base.Add(time.Hour), 4)
	seed(dependent, base.Add(time.Hour+900*time.Second), 5)
	// A host that alerts far outside the lag bound is NOT a cascade of this primary.
	seed(far, base.Add(5*time.Hour), 6)

	store := NewCascadeLatencyStore(p)
	got, err := store.EdgeLatencies(ctx, []string{primary}, base.Add(-time.Minute), 30*time.Minute, falsify.LatencySampleCap)
	if err != nil {
		t.Fatalf("EdgeLatencies: %v", err)
	}
	edge := falsify.CascadeEdge{Primary: primary, Dependent: dependent}
	samples := got[edge]
	// (a) + (c): exactly two samples, one per primary raise, oldest first.
	if len(samples) != 2 || samples[0] != 60*time.Second || samples[1] != 900*time.Second {
		t.Fatalf("samples = %v, want [60s 900s] — one per primary raise, oldest first", samples)
	}
	// (b): the far host is outside the 30m lag bound and is not an edge at all.
	if s, ok := got[falsify.CascadeEdge{Primary: primary, Dependent: far}]; ok {
		t.Fatalf("a raise %v after the primary must not read as a cascade, got %v", 4*time.Hour, s)
	}
	// (d): the edge is DIRECTED — nothing was learned about (dependent → primary).
	if s, ok := got[falsify.CascadeEdge{Primary: dependent, Dependent: primary}]; ok {
		t.Fatalf("the reverse edge must not be populated from a forward observation, got %v", s)
	}
	// And the whole point: the durable samples drive the ported window rule to max(900s, 2 x p95=900s).
	if w := falsify.EdgeWindow(samples, falsify.DefaultWindowFloor, falsify.DefaultWindowCap); w != 1800*time.Second {
		t.Fatalf("learned window from the durable ledger = %s, want 1800s", w)
	}

	// The maxLag bound is load-bearing: tightened below the slow observation, only the fast sample survives
	// and the window falls back to the floor. (MUTATION CONTROL for (b): if the bound did nothing, this would
	// still read two samples and 1800s.)
	tight, err := store.EdgeLatencies(ctx, []string{primary}, base.Add(-time.Minute), 5*time.Minute, falsify.LatencySampleCap)
	if err != nil {
		t.Fatalf("EdgeLatencies (tight bound): %v", err)
	}
	if s := tight[edge]; len(s) != 1 || s[0] != 60*time.Second {
		t.Fatalf("under a 5m lag bound the samples = %v, want [60s] only", s)
	}
	if w := falsify.EdgeWindow(tight[edge], falsify.DefaultWindowFloor, falsify.DefaultWindowCap); w != falsify.DefaultWindowFloor {
		t.Fatalf("a fast-only edge must keep the floor window, got %s", w)
	}

	// The error→ok mapping fails SAFE: an unreadable read is (nil, false), which leaves the scorer on the
	// floor. Proven here by closing the pool underneath the adapter.
	p2, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool 2: %v", err)
	}
	reader := CascadeLatencies(NewCascadeLatencyStore(p2), 30*time.Minute, falsify.LatencySampleCap)
	p2.Close()
	if _, ok := reader(ctx, []string{primary}, base); ok {
		t.Fatal("an unreadable durable record must report ok=false so the scorer falls back to the 900s floor")
	}
}
