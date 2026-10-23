package policy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateGovernor_ClampsNPlusOne — the (N+1)th auto in the window clamps to approve; the first N are admitted.
func TestRateGovernor_ClampsNPlusOne(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewRateGovernor(func() time.Time { return base })
	const n = 3
	for i := 0; i < n; i++ {
		if v, rec := g.Clamp(VerdictAuto, "svc.restart", n); v != VerdictAuto || rec.Clamped {
			t.Fatalf("auto %d = %q clamped=%v, want admitted auto", i, v, rec.Clamped)
		}
	}
	v, rec := g.Clamp(VerdictAuto, "svc.restart", n)
	if v != VerdictApprove || !rec.Clamped || rec.CountInWindow != n {
		t.Fatalf("(N+1)th auto = %q rec=%+v, want rate-clamped approve with count=%d", v, rec, n)
	}
}

// TestRateGovernor_WindowRollsOver — advancing the injected clock past the window frees the budget again.
func TestRateGovernor_WindowRollsOver(t *testing.T) {
	var now atomic.Int64
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now.Store(base.UnixNano())
	g := NewRateGovernor(func() time.Time { return time.Unix(0, now.Load()) })
	const n = 2

	for i := 0; i < n; i++ {
		g.Clamp(VerdictAuto, "k", n)
	}
	// At the cap now: next is clamped.
	if v, _ := g.Clamp(VerdictAuto, "k", n); v != VerdictApprove {
		t.Fatalf("at-cap auto = %q, want clamped approve", v)
	}
	// Advance the clock past the window — the earlier events fall out and the budget is free again.
	now.Store(base.Add(90 * time.Second).UnixNano())
	if v, rec := g.Clamp(VerdictAuto, "k", n); v != VerdictAuto || rec.CountInWindow != 0 {
		t.Fatalf("after window roll-over = %q rec=%+v, want admitted auto with count=0", v, rec)
	}
}

// TestRateGovernor_PerKeyIsolation — separate keys (op-classes) have separate budgets.
func TestRateGovernor_PerKeyIsolation(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewRateGovernor(func() time.Time { return base })
	g.Clamp(VerdictAuto, "class.a", 1) // exhausts class.a
	if v, _ := g.Clamp(VerdictAuto, "class.a", 1); v != VerdictApprove {
		t.Fatalf("class.a second auto not clamped")
	}
	if v, _ := g.Clamp(VerdictAuto, "class.b", 1); v != VerdictAuto {
		t.Fatalf("class.b was charged against class.a's budget")
	}
}

// TestRateGovernor_NoGovernor — a zero/unset limit never clamps; a non-auto verdict is never charged.
func TestRateGovernor_NoGovernor(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewRateGovernor(func() time.Time { return base })
	for i := 0; i < 100; i++ {
		if v, rec := g.Clamp(VerdictAuto, "k", 0); v != VerdictAuto || rec.Clamped {
			t.Fatalf("limit 0 clamped an auto")
		}
	}
	// approve/deny are never charged (they never auto-execute).
	if v, _ := g.Clamp(VerdictApprove, "k2", 1); v != VerdictApprove {
		t.Fatalf("approve altered by governor")
	}
}

// TestRateGovernor_NilSafe — a nil governor degrades to a no-op (never panics, never clamps).
func TestRateGovernor_NilSafe(t *testing.T) {
	var g *RateGovernor
	if v, _ := g.Clamp(VerdictAuto, "k", 5); v != VerdictAuto {
		t.Fatalf("nil governor altered the verdict")
	}
}

// TestRateGovernor_ConcurrencySafe — concurrent Clamp calls admit EXACTLY the limit and never over-admit
// (run under -race). The window is large so no roll-over interferes.
func TestRateGovernor_ConcurrencySafe(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := NewRateGovernor(func() time.Time { return base })
	const limit = 50
	const goroutines = 500

	var admitted int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if v, _ := g.Clamp(VerdictAuto, "hot", limit); v == VerdictAuto {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	wg.Wait()
	if admitted != limit {
		t.Fatalf("admitted %d autos, want exactly the limit %d (no over/under-admit under contention)", admitted, limit)
	}
}
