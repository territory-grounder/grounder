package db

// TG-378: the guest_liveness projection (migration 0079) and its fail-closed reader. Real Postgres
// deliberately — a fake would prove the test author believes in the schema, not that it exists.
//
// KILLING MUTATION (executed 2026-08-11): in Running, change the default branch to `return false, true,
// nil` (unknown collapses into "not running") — TestRunningRefusesTheOpenVocabulary fails on "paused" and
// on the aged row, because that collapse is EXACTLY the guess that sealed start-guest manifests for
// running VMs during the pve03 cascade (unknown != not-running). Restore → green.

import (
	"context"
	"testing"
	"time"
)

func TestGuestLivenessRoundTripAndLatestWins(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	s := NewGuestLivenessStore(pool)

	if err := s.Upsert(ctx, []GuestLivenessState{
		{Guest: "tg378-g1", Node: "pve01", Status: "stopped"},
		{Guest: "tg378-g2", Node: "pve04", Status: "running"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// latest-wins: g1 starts.
	if err := s.Upsert(ctx, []GuestLivenessState{{Guest: "tg378-g1", Node: "pve01", Status: "running"}}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	running, ok, err := s.Running(ctx, "tg378-g1", time.Minute)
	if err != nil || !ok || !running {
		t.Fatalf("g1 must read observed-running: running=%v ok=%v err=%v", running, ok, err)
	}
	running, ok, err = s.Running(ctx, "tg378-g2", time.Minute)
	if err != nil || !ok || !running {
		t.Fatalf("g2 must read observed-running: running=%v ok=%v err=%v", running, ok, err)
	}
	// NEGATIVE CONTROL: a stopped guest answers ok with running=false — otherwise the refusals below
	// prove only that the reader is broken.
	if err := s.Upsert(ctx, []GuestLivenessState{{Guest: "tg378-g3", Node: "pve02", Status: "stopped"}}); err != nil {
		t.Fatalf("g3 upsert: %v", err)
	}
	running, ok, err = s.Running(ctx, "tg378-g3", time.Minute)
	if err != nil || !ok || running {
		t.Fatalf("g3 must read observed-NOT-running: running=%v ok=%v err=%v", running, ok, err)
	}
}

// TestRunningRefusesTheOpenVocabulary: everything outside the two closed states is UNKNOWN (ok=false) —
// paused/suspended/empty statuses, a guest never observed, and a reading older than the freshness bound.
func TestRunningRefusesTheOpenVocabulary(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	s := NewGuestLivenessStore(pool)

	if err := s.Upsert(ctx, []GuestLivenessState{
		{Guest: "tg378-paused", Node: "pve02", Status: "paused"},
		{Guest: "tg378-blank", Node: "pve02", Status: ""},
		{Guest: "tg378-aged", Node: "pve03", Status: "running"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, guest := range []string{"tg378-paused", "tg378-blank", "tg378-never-observed"} {
		if running, ok, err := s.Running(ctx, guest, time.Minute); err != nil || ok || running {
			t.Fatalf("%s must read UNKNOWN (false,false): running=%v ok=%v err=%v", guest, running, ok, err)
		}
	}
	// Age the row below the reader's bound: a reading the sweep stopped vouching for is unknown — the
	// pve03 shape is a dead node's guests VANISHING from the sweep mid-incident, not reporting stopped.
	if _, err := pool.Pool.Exec(ctx,
		`UPDATE guest_liveness SET observed_at = now() - interval '1 hour' WHERE guest = $1`, "tg378-aged"); err != nil {
		t.Fatalf("age: %v", err)
	}
	if running, ok, err := s.Running(ctx, "tg378-aged", time.Minute); err != nil || ok || running {
		t.Fatalf("an aged reading must be UNKNOWN: running=%v ok=%v err=%v", running, ok, err)
	}
	// A non-positive staleAfter disables the age check (the tests-only escape hatch must actually work).
	if running, ok, err := s.Running(ctx, "tg378-aged", 0); err != nil || !ok || !running {
		t.Fatalf("staleAfter<=0 must trust the stored reading: running=%v ok=%v err=%v", running, ok, err)
	}
}

// TestUpsertEmptySweepIsANoOp is the TG-365 emptiness arm: a sweep with zero states writes nothing and
// errors nothing — vanished guests age out rather than being deleted or invented.
func TestUpsertEmptySweepIsANoOp(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	s := NewGuestLivenessStore(pool)
	if err := s.Upsert(ctx, nil); err != nil {
		t.Fatalf("empty sweep must be a no-op, got %v", err)
	}
	if err := s.Upsert(ctx, []GuestLivenessState{}); err != nil {
		t.Fatalf("empty slice must be a no-op, got %v", err)
	}
}

// TestUpsertIsMonotoneOnObservationTime (TG-496): with two writers on guest_liveness (the 5-min estate sweep
// and the ~37s pve-liveness detector) the winner must be the newest OBSERVATION, not the newest WRITE. An
// older-observed RUNNING (the stale sweep, fetched before the guest died) must NOT clobber a newer-observed
// STOPPED (the detector) even though it writes later — the exact down-transition flap the deterministic heal
// would otherwise suffer — while a genuinely newer observation DOES win.
//
// KILLING MUTATION (execute, watch RED, restore): drop the `WHERE guest_liveness.observed_at <=
// EXCLUDED.observed_at` guard from Upsert — the older-observed RUNNING then clobbers the newer STOPPED and
// the first assertion below fails.
func TestUpsertIsMonotoneOnObservationTime(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	s := NewGuestLivenessStore(pool)
	guest := "tg496-mono"
	tNew := time.Now().UTC()
	tOld := tNew.Add(-2 * time.Minute)

	// The detector observes STOPPED at the newer time and writes first.
	if err := s.Upsert(ctx, []GuestLivenessState{{Guest: guest, Node: "pve01", Status: "stopped", ObservedAt: tNew}}); err != nil {
		t.Fatalf("detector upsert: %v", err)
	}
	// The slow sweep, having fetched RUNNING 2 min earlier, writes LATER — it must NOT win the monotone guard.
	if err := s.Upsert(ctx, []GuestLivenessState{{Guest: guest, Node: "pve01", Status: "running", ObservedAt: tOld}}); err != nil {
		t.Fatalf("stale sweep upsert: %v", err)
	}
	if running, ok, err := s.Running(ctx, guest, time.Hour); err != nil || !ok || running {
		t.Fatalf("an OLDER-observed running write clobbered a NEWER-observed stopped row (the down-transition hazard): running=%v ok=%v err=%v", running, ok, err)
	}
	// A genuinely newer observation (a real recovery) DOES win.
	if err := s.Upsert(ctx, []GuestLivenessState{{Guest: guest, Node: "pve01", Status: "running", ObservedAt: tNew.Add(time.Minute)}}); err != nil {
		t.Fatalf("recovery upsert: %v", err)
	}
	if running, ok, err := s.Running(ctx, guest, time.Hour); err != nil || !ok || !running {
		t.Fatalf("a NEWER-observed running write must win (a real recovery): running=%v ok=%v err=%v", running, ok, err)
	}
}
