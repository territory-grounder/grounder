package db

// ORACLES FOR ESTATE-SNAPSHOT RETENTION (TG-355).
//
// Against a REAL Postgres deliberately: the whole mechanism is a window function over (plane, captured_at)
// inside a SECURITY DEFINER function, plus three floors expressed in SQL. A fake would return whatever it
// was told and would prove that the author believes in the predicate.
//
// The table was 84 MB of a 140 MB database when this was written — 60% of everything, growing 334.6
// rows/day with no reaper. What must NOT happen is a retention path that can be talked into emptying it.

import (
	"context"
	"testing"
	"time"
)

func snapReapFixture(ctx context.Context, t *testing.T) (*EstateSnapshotReapStore, *Pool, func()) {
	t.Helper()
	dsn := skipWithoutDB(t)
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// This reaper's predicate ranks over the WHOLE table, so the fixture must own it — the same lesson the
	// ledger chain fixture learned: a rank cannot be partially owned any more than a hash chain can.
	clean := func() {
		_, _ = p.Exec(ctx, `DELETE FROM estate_snapshot`)
		_, _ = p.Exec(ctx, `DELETE FROM estate_snapshot_reap`)
	}
	clean()
	return NewEstateSnapshotReapStore(p), p, func() { clean(); p.Close() }
}

// seedSnap inserts one snapshot for a plane at an age.
func seedSnap(ctx context.Context, t *testing.T, p *Pool, plane string, age time.Duration) {
	t.Helper()
	if _, err := p.Exec(ctx, `
		INSERT INTO estate_snapshot (captured_at, node_count, edge_count, source_count, graph_json, plane, schema_version)
		VALUES (now() - $1::interval, 1, 1, 1, '{}'::jsonb, $2, 1)`, age.String(), plane); err != nil {
		t.Fatalf("seed %s @%s: %v", plane, age, err)
	}
}

func snapCount(ctx context.Context, t *testing.T, p *Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM estate_snapshot `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// KILLING MUTATION: drop the `recency > keep_per_plane` clause. RED — the reaper deletes recent snapshots,
// and EstateReadStore.Latest() reads exactly those.
func TestTheNewestPerPlaneAreKept(t *testing.T) {
	ctx := context.Background()
	st, p, done := snapReapFixture(ctx, t)
	defer done()

	// 80 old rows per plane. With keep=50 the newest 50 of each must survive on rank alone.
	for i := 0; i < 80; i++ {
		age := time.Duration(48+i) * time.Hour
		seedSnap(ctx, t, p, "triage", age)
		seedSnap(ctx, t, p, "actuation", age)
	}
	if _, err := st.Reap(ctx, MinKeepPerPlane, 10000); err != nil {
		t.Fatalf("reap: %v", err)
	}
	for _, plane := range []string{"triage", "actuation"} {
		got := snapCount(ctx, t, p, `WHERE plane = $1`, plane)
		if got < MinKeepPerPlane {
			t.Errorf("plane %s kept %d rows, fewer than the %d floor — Latest() reads the newest row for a "+
				"plane, so reaping into that window blinds the estate view it exists to serve",
				plane, got, MinKeepPerPlane)
		}
	}
}

// PER PLANE, NOT GLOBALLY. The two planes publish at different rates; a global "newest N" lets the chattier
// one evict the other's only recent snapshot — the exact conflation migration 0061 added the plane column
// to end.
//
// KILLING MUTATION: remove `PARTITION BY plane` from the recency window. RED.
func TestTheQuieterPlaneIsNotEvictedByTheChattierOne(t *testing.T) {
	ctx := context.Background()
	st, p, done := snapReapFixture(ctx, t)
	defer done()

	// triage publishes 300 old rows; actuation only 60, all OLDER than triage's.
	for i := 0; i < 300; i++ {
		seedSnap(ctx, t, p, "triage", time.Duration(48+i)*time.Hour)
	}
	for i := 0; i < 60; i++ {
		seedSnap(ctx, t, p, "actuation", time.Duration(400+i)*time.Hour)
	}
	if _, err := st.Reap(ctx, MinKeepPerPlane, 10000); err != nil {
		t.Fatalf("reap: %v", err)
	}
	act := snapCount(ctx, t, p, `WHERE plane = 'actuation'`)
	if act < MinKeepPerPlane {
		t.Fatalf("the actuation plane kept %d rows, below the %d floor. Its snapshots are all older than "+
			"triage's, so a globally-ranked reaper deletes them first — and the actuation plane's graph is "+
			"the one the mutation gate reasons over", act, MinKeepPerPlane)
	}
}

// THE 24-HOUR FLOOR. Whatever the parameters say, the recent window survives — the window that would explain
// whatever just went wrong.
//
// KILLING MUTATION: drop the `captured_at < now() - interval '24 hours'` clause. RED.
func TestNothingInsideTwentyFourHoursIsEverReaped(t *testing.T) {
	ctx := context.Background()
	st, p, done := snapReapFixture(ctx, t)
	defer done()

	// 200 rows inside the floor, all for one plane, far beyond keep_per_plane.
	for i := 0; i < 200; i++ {
		seedSnap(ctx, t, p, "triage", time.Duration(i)*time.Minute)
	}
	if _, err := st.Reap(ctx, MinKeepPerPlane, 10000); err != nil {
		t.Fatalf("reap: %v", err)
	}
	recent := snapCount(ctx, t, p, `WHERE captured_at > now() - interval '24 hours'`)
	if recent != 200 {
		t.Fatalf("kept %d of 200 rows inside the 24h floor — a retention reaper never needs a cutoff of "+
			"now(), and the recent window is the one that explains an incident", recent)
	}
}

// THE DAILY SAMPLE. One row per plane per UTC day survives forever, so coarse history stays answerable at
// ~24 KB/day instead of ~3.9 MB.
//
// KILLING MUTATION: drop the `day_rank > 1` clause. RED — history collapses to the retained window.
func TestTheFirstSnapshotOfEachDayIsKept(t *testing.T) {
	ctx := context.Background()
	st, p, done := snapReapFixture(ctx, t)
	defer done()

	// Ten days back, twelve rows per day, one plane. keep=50 covers ~4 days; the rest must leave a daily trace.
	for d := 3; d < 13; d++ {
		for h := 0; h < 12; h++ {
			seedSnap(ctx, t, p, "triage", time.Duration(d*24+h*2)*time.Hour)
		}
	}
	if _, err := st.Reap(ctx, MinKeepPerPlane, 10000); err != nil {
		t.Fatalf("reap: %v", err)
	}
	var days int
	if err := p.QueryRow(ctx,
		`SELECT count(DISTINCT date_trunc('day', captured_at)) FROM estate_snapshot WHERE plane='triage'`).
		Scan(&days); err != nil {
		t.Fatalf("count days: %v", err)
	}
	if days < 10 {
		t.Errorf("only %d distinct days survived of the 10 seeded — the daily sample is what makes 'what did "+
			"the estate look like last week' answerable after retention", days)
	}
}

// A MISCONFIGURED CALL CANNOT EMPTY THE TABLE. The floor is in the DATABASE, so passing 0 through the Go
// default and passing 0 directly to SQL must both be clamped.
//
// KILLING MUTATION: remove the keep_per_plane clamp from the SQL function. RED.
func TestAZeroKeepCannotEmptyTheTable(t *testing.T) {
	ctx := context.Background()
	st, p, done := snapReapFixture(ctx, t)
	defer done()

	for i := 0; i < 300; i++ {
		seedSnap(ctx, t, p, "triage", time.Duration(48+i)*time.Hour)
	}
	// Straight at the SQL, bypassing the Go default entirely — the Go side must not be the only floor.
	var deleted int64
	if err := p.QueryRow(ctx, `SELECT reap_estate_snapshot(0, 10000)`).Scan(&deleted); err != nil {
		t.Fatalf("reap(0): %v", err)
	}
	left := snapCount(ctx, t, p, ``)
	if left < MinKeepPerPlane {
		t.Fatalf("reap_estate_snapshot(0, …) left %d rows — the clamp is in Go only, so anything that calls "+
			"the function directly can empty the largest table in the database", left)
	}
	_ = st
}

// THE PURGE IS JOURNALLED IN THE SAME TRANSACTION. A deletion that leaves no record is the shape TG-295
// built its journal against.
//
// KILLING MUTATION: remove the INSERT from the function body. RED.
func TestEveryReapIsJournalled(t *testing.T) {
	ctx := context.Background()
	st, p, done := snapReapFixture(ctx, t)
	defer done()

	for i := 0; i < 300; i++ {
		seedSnap(ctx, t, p, "triage", time.Duration(48+i)*time.Hour)
	}
	deleted, err := st.Reap(ctx, MinKeepPerPlane, 10000)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if deleted == 0 {
		t.Fatal("the reaper deleted nothing from 300 old rows with keep=50 — this test cannot observe a " +
			"journal entry for a purge that did not happen")
	}
	var journalled int64
	if err := p.QueryRow(ctx,
		`SELECT rows_deleted FROM estate_snapshot_reap ORDER BY id DESC LIMIT 1`).Scan(&journalled); err != nil {
		t.Fatalf("no journal row for a purge that deleted %d rows: %v", deleted, err)
	}
	if journalled != deleted {
		t.Errorf("journal says %d rows, the call returned %d — the record and the deletion disagree",
			journalled, deleted)
	}
}

// The Go constant and the SQL clamp must not drift: a caller reading MinKeepPerPlane to decide what is safe
// would otherwise be reading a number the database does not enforce.
//
// ASSERTED BY EQUIVALENCE, not by an absolute count, and the first version of this test got that wrong.
// Counting survivors after asking for 49 gave 57 — because the daily-sample clause keeps first-of-day rows
// on top of the recency window, so the total is (floor + however many days the fixture spans). That number
// says nothing about where the clamp sits. Asking for one BELOW the floor and exactly AT it must produce the
// identical outcome; that is the clamp, and nothing else is.
//
// KILLING MUTATION: remove the keep_per_plane clamp from the SQL. RED — 49 then keeps one row fewer than 50.
func TestTheGoFloorMatchesTheDatabaseFloor(t *testing.T) {
	ctx := context.Background()

	run := func(keep int) int {
		t.Helper()
		_, p, done := snapReapFixture(ctx, t)
		defer done()
		// A single UTC day, so the daily-sample clause contributes exactly one row and cannot vary between
		// the two runs being compared.
		for i := 0; i < 200; i++ {
			seedSnap(ctx, t, p, "triage", 25*time.Hour+time.Duration(i)*time.Second)
		}
		if _, err := p.Exec(ctx, `SELECT reap_estate_snapshot($1::int, 10000)`, keep); err != nil {
			t.Fatalf("reap(%d): %v", keep, err)
		}
		return snapCount(ctx, t, p, `WHERE plane='triage'`)
	}

	below, at := run(MinKeepPerPlane-1), run(MinKeepPerPlane)
	if below != at {
		t.Errorf("asking for %d kept %d rows and asking for %d kept %d — they must be identical, because "+
			"the database clamps anything under the floor. MinKeepPerPlane is what a caller reads to decide "+
			"what is safe, and it has drifted from the SQL", MinKeepPerPlane-1, below, MinKeepPerPlane, at)
	}
	if at < MinKeepPerPlane {
		t.Errorf("asking for exactly the floor kept %d rows, fewer than %d", at, MinKeepPerPlane)
	}
}
