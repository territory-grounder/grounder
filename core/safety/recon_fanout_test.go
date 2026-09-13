package safety

import "testing"

// TG-325: the volume bounds (per-session/hour/burst) cannot see an actor operating UNDER them — 8 reads of 8
// DISTINCT hosts is a methodical sweep; 8 reads of ONE host is a poll — SAME volume, different SHAPE. The
// observe-only fan-out flag reasons over the per-session target COMPOSITION the volume bounds ignore. It must
// fire on fan-out (distinct targets), NOT on read volume, and must NEVER refuse a read (observe-only —
// complements the gates, does not become one).
//
// Killing mutations: key the flag on read volume instead of distinct targets → the poll flags → RED; make the
// flag refuse (return a ReconRefusal) → the sweep's Admit errors → RED; never fire → the sweep's flag count
// stays 0 → RED.
func TestReconFanoutFlagsASweepButNeverRefuses(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{FanoutObserve: 8}, nil) // ceiling 8; meter-only (no kill seam)

	// A POLL: 8 reads against ONE host. High-ish volume, fan-out 1 — must NOT flag.
	for i := 0; i < 8; i++ {
		if err := g.Admit("poll"); err != nil {
			t.Fatalf("poll read %d refused (%v) — the fan-out flag is observe-only and must never gate", i, err)
		}
		g.Record("poll", "hostdiag", "hostA")
	}
	if n := g.Snapshot().FanoutFlags; n != 0 {
		t.Fatalf("a poll of ONE host (fan-out 1, volume 8) raised %d fan-out flag(s), want 0 — the flag must key on "+
			"DISTINCT targets, not read volume", n)
	}

	// A SWEEP: 8 reads against 8 DISTINCT hosts. Same volume as the poll, fan-out 8 — must flag exactly once.
	for i, tgt := range []string{"h0", "h1", "h2", "h3", "h4", "h5", "h6", "h7"} {
		if err := g.Admit("sweep"); err != nil {
			t.Fatalf("sweep read %d refused (%v) — the fan-out flag must OBSERVE, never refuse", i, err)
		}
		g.Record("sweep", "hostdiag", tgt)
	}
	if n := g.Snapshot().FanoutFlags; n != 1 {
		t.Fatalf("a sweep of 8 distinct hosts (under every volume bound) raised %d fan-out flag(s), want exactly 1", n)
	}
	// A further distinct target in the SAME session must not re-fire (once per session, like a burst episode).
	if err := g.Admit("sweep"); err != nil {
		t.Fatalf("post-flag read refused (%v) — observe-only", err)
	}
	g.Record("sweep", "hostdiag", "h8")
	if n := g.Snapshot().FanoutFlags; n != 1 {
		t.Fatalf("the fan-out flag re-fired within one session (count now %d), want once-per-session", n)
	}
}

// A zero ceiling DISABLES the fan-out flag (observe-only, legitimately disable-able), even for a wide sweep.
func TestReconFanoutDisabledByZeroCeiling(t *testing.T) {
	g, _ := newTestGovernor(ReconBudget{FanoutObserve: 0}, nil)
	for i := 0; i < 20; i++ {
		g.Record("wide", "hostdiag", string(rune('a'+i))) // 20 distinct targets
	}
	if n := g.Snapshot().FanoutFlags; n != 0 {
		t.Fatalf("a 0 ceiling must disable the fan-out flag, got %d", n)
	}
	if s := g.Snapshot(); s.FanoutObserve != 0 {
		t.Fatalf("FanoutObserve snapshot = %d, want 0 (disabled)", s.FanoutObserve)
	}
}
