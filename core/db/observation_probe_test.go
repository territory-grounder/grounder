package db

import (
	"context"
	"testing"
	"time"
)

// probeFixture is a MINIMAL DB fixture — a connected pool, nothing else. It deliberately does NOT pull in the
// gold-axis fixture (openFixture) the observation-probe tests have no use for; seeding it would only expose
// them to that fixture's fixed-id collisions with a concurrent worktree. Every id these tests seed is
// namespaced with gx(...) so inserts cannot collide and cleanup (gxLike) cannot reach another run's rows.
func probeFixture(t *testing.T) (context.Context, *Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return ctx, p, func() { p.Close() }
}

// The observation_probe ledger round-trip (TG-180 part 2). Runs against a REAL Postgres (TG_TEST_DSN) with the
// 0093 migration applied, because every risk here is SQL/constraint semantics a fake cannot reproduce: the
// pending-only partial read, the terminal-verdict confirmed set, and the single-writer verdict transition.
func TestObservationProbe_LedgerRoundTrip(t *testing.T) {
	ctx, p, done := probeFixture(t)
	defer done()

	wipe := func() { _, _ = p.Exec(ctx, `DELETE FROM observation_probe WHERE host LIKE $1`, gxLike()) }
	wipe()
	defer wipe()

	s := NewObservationProbeStore(p)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	gapHost, incHost := gx("probe-gap"), gx("probe-inc")

	// Two probes: one we will confirm a gap on, one we will decide inconclusive (never ran).
	gapID, err := s.RecordProbe(ctx, gapHost, "device-down", base, base.Add(10*time.Minute), true, "t")
	if err != nil {
		t.Fatalf("record gap probe: %v", err)
	}
	incID, err := s.RecordProbe(ctx, incHost, "device-down", base, base.Add(10*time.Minute), false, "t")
	if err != nil {
		t.Fatalf("record inconclusive probe: %v", err)
	}

	// Both start pending.
	pend, err := s.PendingProbes(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if n := countHostPrefix(pend, gxLikePrefix()); n != 2 {
		t.Fatalf("pending (this run) = %d, want 2", n)
	}

	// Decide the gap terminal, the other inconclusive.
	if err := s.SetProbeVerdict(ctx, gapID, "unobservable_confirmed"); err != nil {
		t.Fatalf("set gap verdict: %v", err)
	}
	if err := s.SetProbeVerdict(ctx, incID, "inconclusive"); err != nil {
		t.Fatalf("set inconclusive verdict: %v", err)
	}

	// Neither is pending now.
	pend, _ = s.PendingProbes(ctx)
	if n := countHostPrefix(pend, gxLikePrefix()); n != 0 {
		t.Fatalf("pending after decide (this run) = %d, want 0", n)
	}

	// ONLY the terminal one is confirmed — the inconclusive host stays re-probeable (excluded from the numerator).
	confirmed, err := s.ProbeConfirmedHosts(ctx)
	if err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	if !confirmed[gapHost] {
		t.Fatal("gap host not in confirmed set — a terminal verdict must count toward coverage")
	}
	if confirmed[incHost] {
		t.Fatal("inconclusive host IS in confirmed set — an inconclusive probe must NOT count as coverage (it never ran)")
	}

	// SINGLE-WRITER: re-deciding an already-decided row is a no-op, not an overwrite.
	if err := s.SetProbeVerdict(ctx, gapID, "observable"); err != nil {
		t.Fatalf("second verdict write errored: %v", err)
	}
	if got := verdictOf(ctx, t, p, gapID); got != "unobservable_confirmed" {
		t.Fatalf("verdict after second write = %q, want the first verdict to stand (single-writer)", got)
	}
}

// The alert-times reader filters to the INCLUSIVE window and excludes out-of-window alerts.
func TestObservationProbe_AlertTimesWindow(t *testing.T) {
	ctx, p, done := probeFixture(t)
	defer done()

	host := gx("probe-host")
	wipe := func() { _, _ = p.Exec(ctx, `DELETE FROM ingest_alert WHERE host = $1`, host) }
	wipe()
	defer wipe()

	inj := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	end := inj.Add(10 * time.Minute)
	seed := func(at time.Time) {
		if _, err := p.Exec(ctx, `
			INSERT INTO ingest_alert (external_ref, source_type, source_id, alert_rule, severity, host, received_at, observed_at)
			VALUES ($1,'librenms','lnms','Device-Down','critical',$2,$3,$3)`,
			gx("probe-"+at.Format("150405.000000")), host, at); err != nil {
			t.Fatalf("seed alert: %v", err)
		}
	}
	seed(inj.Add(-time.Minute))    // before the window — excluded
	seed(inj)                      // exactly at injection — included (inclusive)
	seed(inj.Add(5 * time.Minute)) // in window — included
	seed(end)                      // exactly at window close — included (inclusive)
	seed(end.Add(time.Minute))     // after the window — excluded

	got, err := NewObservationProbeStore(p).ProbeAlertTimes(ctx, host, inj, end)
	if err != nil {
		t.Fatalf("ProbeAlertTimes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("in-window alerts = %d, want 3 (at-injection, mid, at-close; the two outside excluded)", len(got))
	}
}

// A verdict outside the CHECK set is rejected by the migration constraint — the closed enumeration is enforced
// at the database, not just in Go.
func TestObservationProbe_VerdictConstraintEnforced(t *testing.T) {
	ctx, p, done := probeFixture(t)
	defer done()
	host := gx("probe-bad")
	defer func() { _, _ = p.Exec(ctx, `DELETE FROM observation_probe WHERE host = $1`, host) }()

	_, err := p.Exec(ctx, `
		INSERT INTO observation_probe (host, fault_class, window_end, verdict, decided_at)
		VALUES ($1,'device-down', now(), 'nonsense', now())`, host)
	if err == nil {
		t.Fatal("inserted a verdict outside the CHECK set — the closed enumeration is not enforced at the DB")
	}
}

// gxLikePrefix is this run's namespace prefix, used to count only the rows this test binary seeded.
func gxLikePrefix() string { return fixtureNS + "-" }

func countHostPrefix(ps []PendingProbe, prefix string) int {
	n := 0
	for _, p := range ps {
		if len(p.Host) >= len(prefix) && p.Host[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func verdictOf(ctx context.Context, t *testing.T, p *Pool, id int64) string {
	t.Helper()
	var v string
	if err := p.QueryRow(ctx, `SELECT verdict FROM observation_probe WHERE id=$1`, id).Scan(&v); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	return v
}
