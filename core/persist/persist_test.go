package persist

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

var pNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func TestScheduledRebootIsBiTemporalWithKillSwitch(t *testing.T) {
	reg := NewMemScheduledReboots()
	in := ScheduledReboot{
		Host:       "nl-frr01",
		Cron:       "0 3 * * 0",
		Kind:       "os-patch",
		KillSwitch: false,
		ValidFrom:  pNow,
		ValidUntil: pNow.Add(90 * 24 * time.Hour),
	}
	out, err := reg.Register(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.ValidFrom.IsZero() || out.ValidUntil.IsZero() {
		t.Fatalf("row must carry a validity window: %+v", out)
	}
	if out.State != SRObserving {
		t.Fatalf("a freshly discovered schedule must default to observing, got %s", out.State)
	}
	if out.SchemaVersion <= 0 {
		t.Fatalf("row must be schema-stamped, got %d", out.SchemaVersion)
	}
	got, ok, _ := reg.Get(context.Background(), "nl-frr01", "os-patch")
	if !ok || got.Host != "nl-frr01" {
		t.Fatalf("registered row not retrievable: %+v ok=%v", got, ok)
	}
}

// TestScheduledRebootRediscoveryPreservesPromotion proves a periodic discovery sweep that re-registers an
// already-promoted schedule does NOT demote it or clear its kill switch (the predecessor's ON CONFLICT
// intent): promotion state (State, Observations, KillSwitch) is preserved while the window is refreshed.
// Before the fix, the wholesale overwrite un-promoted every live schedule on every sweep.
func TestScheduledRebootRediscoveryPreservesPromotion(t *testing.T) {
	reg := NewMemScheduledReboots()
	base := ScheduledReboot{Host: "nl-frr01", Cron: "0 3 * * 0", Kind: "os-patch", ValidFrom: pNow, ValidUntil: pNow.Add(90 * 24 * time.Hour)}

	// A schedule that has been promoted to live (2 observed boots) and later killed by an operator.
	promoted := base
	promoted.State = SRLive
	promoted.Observations = 2
	promoted.KillSwitch = true
	if _, err := reg.Register(context.Background(), promoted); err != nil {
		t.Fatal(err)
	}

	// A weekly discovery sweep re-finds it as a fresh candidate (observing, 0 boots, not killed) with a moved window.
	rediscover := base
	rediscover.State = SRObserving
	rediscover.Observations = 0
	rediscover.KillSwitch = false
	rediscover.ValidUntil = pNow.Add(180 * 24 * time.Hour)
	out, err := reg.Register(context.Background(), rediscover)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != SRLive || out.Observations != 2 || !out.KillSwitch {
		t.Fatalf("re-discovery must PRESERVE promotion state (live/2/killed), got state=%s obs=%d kill=%v", out.State, out.Observations, out.KillSwitch)
	}
	if !out.ValidUntil.Equal(pNow.Add(180 * 24 * time.Hour)) {
		t.Fatalf("re-discovery must refresh the validity window, got %v", out.ValidUntil)
	}
	got, ok, _ := reg.Get(context.Background(), "nl-frr01", "os-patch")
	if !ok || got.State != SRLive || !got.KillSwitch {
		t.Fatalf("the stored row must stay live+killed after re-discovery: %+v", got)
	}
}

// A discovery sweep that finds a SHIFTED cron (maintenance moved from 03:00 to 04:00) is a NEW, unverified
// schedule that must observe before it suppresses — it does NOT inherit the old cron's promotion. The old
// promoted row still exists on its own (host,kind,cron) key. Get returns the most-recently first-registered
// row, matching the pgx twin (ORDER BY created_at DESC) and the predecessor (key = host,kind,cron).
func TestScheduledRebootChangedCronIsNewObservingSchedule(t *testing.T) {
	reg := NewMemScheduledReboots()
	base := ScheduledReboot{Host: "nl-frr01", Kind: "os-patch", ValidFrom: pNow, ValidUntil: pNow.Add(90 * 24 * time.Hour)}

	// a promoted (live, 2 boots) schedule at 03:00 Sunday
	live := base
	live.Cron = "0 3 * * 0"
	live.State = SRLive
	live.Observations = 2
	if _, err := reg.Register(context.Background(), live); err != nil {
		t.Fatal(err)
	}

	// a sweep finds the maintenance shifted to 04:00 — a fresh observing candidate
	shifted := base
	shifted.Cron = "0 4 * * 0"
	shifted.State = SRObserving
	shifted.Observations = 0
	out, err := reg.Register(context.Background(), shifted)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != SRObserving || out.Observations != 0 {
		t.Fatalf("a shifted cron must be a NEW observing schedule, not inherit promotion: %+v", out)
	}

	// Get returns the most-recently first-registered cron (the 04:00 observing one), matching the pgx twin.
	got, ok, _ := reg.Get(context.Background(), "nl-frr01", "os-patch")
	if !ok || got.Cron != "0 4 * * 0" || got.State != SRObserving {
		t.Fatalf("Get must return the most-recent (04:00 observing) schedule, got %+v ok=%v", got, ok)
	}
}

func TestScheduledRebootRejectsInvertedWindow(t *testing.T) {
	reg := NewMemScheduledReboots()
	_, err := reg.Register(context.Background(), ScheduledReboot{
		Host: "h", Kind: "k", ValidFrom: pNow, ValidUntil: pNow.Add(-time.Hour),
	})
	if !errors.Is(err, ErrInvalidWindow) {
		t.Fatalf("inverted window must be rejected, got %v", err)
	}
}

func TestEscalationQueueAppendOnlyAndReEnters(t *testing.T) {
	ctx := context.Background()
	q := NewEscalationQueue()
	if _, err := q.Enqueue(ctx, "TG-1", 1, pNow.Add(-time.Minute)); err != nil { // already eligible
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, "TG-2", 1, pNow.Add(time.Hour)); err != nil { // not yet eligible
		t.Fatal(err)
	}
	if q.Len() != 2 {
		t.Fatalf("both enqueues must append, len=%d", q.Len())
	}

	// Only the eligible row is due; marking it fired transitions status but never deletes (append-only).
	due, err := q.DuePending(ctx, pNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ExternalRef != "TG-1" {
		t.Fatalf("only the eligible row should be due, got %+v", due)
	}
	if err := q.MarkFired(ctx, due[0].Seq); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 2 {
		t.Fatalf("firing must not delete rows (append-only), len=%d", q.Len())
	}
	items := q.Items()
	if items[0].Status != EscalFired || items[1].Status != EscalPending {
		t.Fatalf("status transitions wrong: %+v", items)
	}
	if items[0].SchemaVersion <= 0 {
		t.Fatalf("rows must be schema-stamped")
	}
	// MarkFired is idempotent: re-firing the same seq, or a missing seq, is a no-op — not an error.
	if err := q.MarkFired(ctx, due[0].Seq); err != nil {
		t.Fatalf("re-firing a fired row must be a no-op, got %v", err)
	}
	if err := q.MarkFired(ctx, 9999); err != nil {
		t.Fatalf("firing a missing seq must be a no-op, got %v", err)
	}
	// After the eligible row fired, nothing is due (TG-2 is not yet eligible).
	if again, _ := q.DuePending(ctx, pNow); len(again) != 0 {
		t.Fatalf("no row should be due after the eligible one fired, got %+v", again)
	}
}

func TestEscalationRejectsEmptyRef(t *testing.T) {
	q := NewEscalationQueue()
	if _, err := q.Enqueue(context.Background(), "", 1, pNow); !errors.Is(err, ErrEmptyRef) {
		t.Fatalf("empty ref must fail closed, got %v", err)
	}
}

// ADVERSARIAL (INV-22, concurrent inputs): the shared registries must be race-free under concurrent writers.
func TestSharedStoresConcurrent(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := from.AddDate(1, 0, 0)
	sr := NewMemScheduledReboots()
	q := NewEscalationQueue()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			h := "h" + string(rune('a'+n%26))
			_, _ = sr.Register(context.Background(), ScheduledReboot{Host: h, Kind: "reboot", Cron: "0 3 * * *", ValidFrom: from, ValidUntil: until})
			_, _, _ = sr.Get(context.Background(), h, "reboot")
			_, _ = q.Enqueue(context.Background(), "ref", 0, from)
			_ = q.Len()
		}(i)
	}
	wg.Wait()
	if q.Len() != 200 {
		t.Fatalf("all enqueues must land: got %d want 200", q.Len())
	}
}
