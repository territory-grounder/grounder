package governance

import (
	"context"
	"testing"
)

// deathPairs builds n pairs the local judge left unscored but the frontier scored — a confirmed DEATH.
func deathPairs(n int) []CrossCheckPair {
	var pairs []CrossCheckPair
	for i := 0; i < n; i++ {
		pairs = append(pairs, CrossCheckPair{SessionID: "s", LocalScored: false, FrontierScored: true, FrontierVerdict: "match"})
	}
	return pairs
}

// TestFrontierCrossCheckDeathPagesOnceOnTransition is the frontier half of TG-425. The frontier monitor also
// halted + warned "judge-death" on EVERY scheduled run of an ongoing death. It now pages only on the
// false->true transition, reading the SHARED judge-death breaker (the same one judge-liveness drives), so the
// death pages once ACROSS both monitors. The HALT still runs every tick.
//
//	Killing mutation: drop the `!alreadyDead` guard on the frontier judge-death Warn — K pages, not 1 → RED.
func TestFrontierCrossCheckDeathPagesOnceOnTransition(t *testing.T) {
	esc := &fakeEscalator{}
	dm := &fakeDeadman{}
	m := &FrontierCrossCheckMonitor{Pairs: fakePairs{deathPairs(6)}, Escalation: esc, Halt: dm}

	const K = 4
	for i := 0; i < K; i++ {
		r, err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !r.Death || !r.Halted {
			t.Fatalf("run %d: a confirmed death must halt every tick, got %+v", i, r)
		}
	}
	if esc.warned != 1 {
		t.Fatalf("frontier judge-death paged %d times over %d ongoing-death ticks — want exactly 1 (the "+
			"transition; the ongoing state is on the shared judge-death breaker's Prometheus alert, TG-425)", esc.warned, K)
	}
	if dm.halts != K {
		t.Fatalf("the HALT must run every death tick (only the page is deduped), got %d want %d", dm.halts, K)
	}
}

// TestFrontierDeathIsSilentWhenLivenessAlreadyPaged proves the CROSS-MONITOR dedup: if the shared judge-death
// breaker is already open (e.g. judge-liveness already paged this death), the frontier monitor still HALTS
// (idempotent) but does NOT re-page — one death, one page, across both monitors.
func TestFrontierDeathIsSilentWhenLivenessAlreadyPaged(t *testing.T) {
	esc := &fakeEscalator{}
	dm := &fakeDeadman{open: true} // already halted by a sibling monitor
	m := &FrontierCrossCheckMonitor{Pairs: fakePairs{deathPairs(6)}, Escalation: esc, Halt: dm}

	r, err := m.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !r.Death || !r.Halted {
		t.Fatalf("a confirmed death must still be detected and halt (idempotent) even when already open: %+v", r)
	}
	if esc.warned != 0 {
		t.Fatalf("the death was already paged (breaker already open) — the frontier monitor must NOT re-page, got warned=%d want 0", esc.warned)
	}
}
