package breaker

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TG-432 — CompareAndOpen / TripOpen is the CROSS-PROCESS atomic open that lets a safety monitor dedup a
// human page across sibling workers. These oracles drive the real MemStore (the process-shared twin of the
// pgx row) and the real Breaker.

// TestCompareAndOpen_ReportsWhoFlippedIt proves the basic contract: the FIRST open returns true, and a second
// open on the already-open row returns false without disturbing the original OpenedAt.
func TestCompareAndOpen_ReportsWhoFlippedIt(t *testing.T) {
	store := NewMemStore()
	t0 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

	openedNow, err := store.CompareAndOpen(context.Background(), "judge-death", t0)
	if err != nil {
		t.Fatalf("first CompareAndOpen: %v", err)
	}
	if !openedNow {
		t.Fatal("first CompareAndOpen returned openedNow=false — the fresh open must report it flipped the breaker")
	}
	// A second open, later, must report it did NOT flip it and must NOT overwrite the first trip's timestamp.
	openedNow, err = store.CompareAndOpen(context.Background(), "judge-death", t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("second CompareAndOpen: %v", err)
	}
	if openedNow {
		t.Fatal("second CompareAndOpen returned openedNow=true — an already-open breaker was reported as newly opened (double-page bug)")
	}
	rec, ok, _ := store.Load(context.Background(), "judge-death")
	if !ok || rec.State != StateOpen {
		t.Fatalf("breaker not open after CompareAndOpen: ok=%v state=%v", ok, rec.State)
	}
	if !rec.OpenedAt.Equal(t0) {
		t.Fatalf("OpenedAt was overwritten by the second (idempotent) open: got %v want the first trip %v", rec.OpenedAt, t0)
	}
}

// TestCompareAndOpen_ConcurrentTripsElectOneOpener is the killing oracle for the cross-monitor race (finding
// 1). Many goroutines — standing in for sibling worker PROCESSES sharing one store row — race to open the same
// breaker for the same death. EXACTLY ONE must see openedNow=true, or two monitors both page. If the check
// and the write were not atomic under the store lock, several would read "closed" and several would flip it.
func TestCompareAndOpen_ConcurrentTripsElectOneOpener(t *testing.T) {
	store := NewMemStore()
	const racers = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		openers int
		start   = make(chan struct{})
	)
	t0 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines at once to maximize the contention window
			openedNow, err := store.CompareAndOpen(context.Background(), "judge-death", t0)
			if err != nil {
				t.Errorf("CompareAndOpen: %v", err)
				return
			}
			if openedNow {
				mu.Lock()
				openers++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if openers != 1 {
		t.Fatalf("CompareAndOpen elected %d openers among %d concurrent trips — want exactly 1. A non-atomic "+
			"check-then-write lets multiple monitors each believe they opened the breaker and each page (TG-432).", openers, racers)
	}
}

// TestTripOpen_UsesAtomicOpenerAndDedups proves Breaker.TripOpen threads the atomic capability through: the
// first trip opens (true), a second is a no-op (false). Two Breaker values sharing one store — the sibling
// worker model — elect exactly one opener.
func TestTripOpen_UsesAtomicOpener(t *testing.T) {
	store := NewMemStore()
	clock := func() time.Time { return time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC) }
	b1, err := New("judge-death", store, WithThreshold(1), WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := New("judge-death", store, WithThreshold(1), WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	first, err := b1.TripOpen(context.Background())
	if err != nil {
		t.Fatalf("b1.TripOpen: %v", err)
	}
	second, err := b2.TripOpen(context.Background()) // sibling instance, same shared row
	if err != nil {
		t.Fatalf("b2.TripOpen: %v", err)
	}
	if !first || second {
		t.Fatalf("TripOpen dedup across sibling breakers broken: first=%v second=%v (want true,false)", first, second)
	}
	snap, err := b2.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != StateOpen {
		t.Fatalf("breaker not open after TripOpen: %v", snap.State)
	}
}
