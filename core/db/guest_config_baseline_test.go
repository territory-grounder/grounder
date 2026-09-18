package db

// TG-466 slice 1: the guest_config_baseline projection (migration 0091) against REAL Postgres — a
// fake has already hidden a field-drop in this repository once, and this store's compare-and-roll is
// exactly the read-modify-write shape a fake blesses wrongly.
//
// The SQL-layer half of the ticket's NEGATIVE DRILL lives here: identical hashes re-Recorded sweep
// after sweep (what an organic crash/stop/start produces — the config bytes do not move) must NEVER
// set changed_at, so ChangedWithin stays false at any window; one rolled hash (a deliberate edit)
// sets it exactly once.
//
// KILLING MUTATIONS (executed 2026-08-14, each restored):
//   - Record: invert the compare (`stored != obs.Hash` on the refresh branch) → RED:
//     TestGuestConfigBaselineLifecycle fails at "sweep 0: same hash must not read as changed".
//   - ChangedWithin: drop the window clause (`AND changed_at >= …` neutralized) → RED: the aged-change
//     check fails ("a change outside the window must not read as in-window").
//   - Record: allow the blank hash through the Go guard → stayed GREEN, and the reason is the finding:
//     migration 0091's `CHECK (length(config_hash) > 0)` refuses it at the schema, so the refusal
//     property is enforced at TWO layers and this oracle proves the property, not the Go line. The Go
//     guard remains for fail-fast (before a transaction) and for an error a sweep can read; the vmid
//     and guest arms of TestGuestConfigBaselineRefusesMalformed still bind it (schema CHECKs cover
//     those too — same two-layer shape).

import (
	"context"
	"testing"
	"time"
)

func guestConfigFixtureCleanup(ctx context.Context, t *testing.T, p *Pool) func() {
	t.Helper()
	cleanup := func() {
		_, _ = p.Exec(ctx, `DELETE FROM guest_config_baseline WHERE guest LIKE $1`, "tg466-%")
	}
	cleanup()
	return cleanup
}

func TestGuestConfigBaselineLifecycle(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	defer guestConfigFixtureCleanup(ctx, t, pool)()
	s := NewGuestConfigBaselineStore(pool)

	obs := GuestConfigObservation{VMID: 466201, Guest: "tg466-web01", Node: "pve-a", Kind: "lxc", Hash: "ch1:aaa"}

	// First sighting: a baseline, never a change.
	out, err := s.Record(ctx, obs)
	if err != nil || !out.FirstSighting || out.Changed {
		t.Fatalf("first sighting must record a baseline and no change: %+v err=%v", out, err)
	}

	// The ORGANIC mirror: three sweeps across a crash/stop/start present the SAME hash. changed_at
	// must never be set — this is the SQL half of the INV-09 drill.
	for i := 0; i < 3; i++ {
		out, err = s.Record(ctx, obs)
		if err != nil || out.FirstSighting || out.Changed || out.PreviousHash != "ch1:aaa" {
			t.Fatalf("sweep %d: same hash must not read as changed: %+v err=%v", i, out, err)
		}
	}
	if changed, err := s.ChangedWithin(ctx, "tg466-web01", 24*time.Hour); err != nil || changed {
		t.Fatalf("INV-09 VIOLATED at the store: organic re-records read as an in-window change (changed=%v err=%v)", changed, err)
	}

	// A deliberate EDIT rolls the baseline exactly once.
	obs.Hash = "ch1:bbb"
	out, err = s.Record(ctx, obs)
	if err != nil || !out.Changed || out.PreviousHash != "ch1:aaa" {
		t.Fatalf("an edit must read changed with the prior baseline preserved: %+v err=%v", out, err)
	}
	out, err = s.Record(ctx, obs)
	if err != nil || out.Changed {
		t.Fatalf("a change must fire once, not on every later sweep: %+v err=%v", out, err)
	}
	if changed, err := s.ChangedWithin(ctx, "tg466-web01", time.Hour); err != nil || !changed {
		t.Fatalf("the edit must be readable in-window: changed=%v err=%v", changed, err)
	}
	// Fail-closed edges of the reader: another guest, a never-changed guest, and a non-positive window.
	if changed, err := s.ChangedWithin(ctx, "tg466-nobody", time.Hour); err != nil || changed {
		t.Fatalf("an unobserved guest must read false: changed=%v err=%v", changed, err)
	}
	if changed, err := s.ChangedWithin(ctx, "tg466-web01", 0); err != nil || changed {
		t.Fatalf("a non-positive window asks nothing and must answer false: changed=%v err=%v", changed, err)
	}

	// Age the change out of the window: in-window means IN WINDOW, not "ever".
	if _, err := pool.Exec(ctx,
		`UPDATE guest_config_baseline SET changed_at = now() - interval '2 hours' WHERE guest = $1`, "tg466-web01"); err != nil {
		t.Fatalf("age: %v", err)
	}
	if changed, err := s.ChangedWithin(ctx, "tg466-web01", time.Hour); err != nil || changed {
		t.Fatalf("a change outside the window must not read as in-window: changed=%v err=%v", changed, err)
	}
	if changed, err := s.ChangedWithin(ctx, "tg466-web01", 3*time.Hour); err != nil || !changed {
		t.Fatalf("a change inside a wider window must read true: changed=%v err=%v", changed, err)
	}
}

func TestGuestConfigBaselineRefusesMalformed(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	defer guestConfigFixtureCleanup(ctx, t, pool)()
	s := NewGuestConfigBaselineStore(pool)

	// An empty hash stored today is a fabricated "changed" tomorrow — refuse at the door.
	for _, obs := range []GuestConfigObservation{
		{VMID: 466301, Guest: "tg466-x", Hash: ""},
		{VMID: 0, Guest: "tg466-x", Hash: "ch1:x"},
		{VMID: 466302, Guest: "", Hash: "ch1:x"},
	} {
		if _, err := s.Record(ctx, obs); err == nil {
			t.Fatalf("malformed observation must be refused, not stored: %+v", obs)
		}
	}
}
