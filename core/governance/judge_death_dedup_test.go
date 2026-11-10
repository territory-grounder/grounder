package governance

import (
	"context"
	"testing"
	"time"
)

// fakeDeadman is a stateful Halter that also implements HaltStateReader + Rearmer, mirroring JudgeDeadMan's
// durable open/closed state so the monitor can tell a NEW judge-death from an ONGOING one.
type fakeDeadman struct {
	open  bool
	halts int
}

func (f *fakeDeadman) Halt(_ context.Context, _ string) error  { f.open = true; f.halts++; return nil }
func (f *fakeDeadman) Halted(_ context.Context) (bool, string) { return f.open, "" }
func (f *fakeDeadman) Rearm(_ context.Context) error           { f.open = false; return nil }

// TestJudgeDeathWarnsOnceOnTheTransitionNotEveryTick is the TG-425 guard.
//
// The judge-death page re-fired on EVERY ~hourly monitor tick for the whole outage (~33h on 2026-08-08),
// spamming the matrix room — the desensitiser the owner asked about. The ongoing "still dead" state is
// already carried by the AnyCircuitBreakerOpen Prometheus alert (circuit_breaker_state{name="judge-death"}==2),
// so the human page should fire ONCE on the false->true transition. The HALT still runs every tick.
//
//	Killing mutation: drop the `!alreadyDead` guard on the Warn (warn whenever below threshold) — the page
//	count becomes K, not 1, and this test goes RED on the page count while the halt count is unchanged.
func TestJudgeDeathWarnsOnceOnTheTransitionNotEveryTick(t *testing.T) {
	ss := sessions(10, time.Hour) // 10 eligible (> MinSample)
	judged := map[string]bool{}   // none judged -> fraction 0 -> below the death threshold
	esc := &fakeEscalator{}
	dm := &fakeDeadman{}
	m := &JudgeLivenessMonitor{
		Sessions: fakeSessions{ss}, Judgments: fakeJudgments{judged},
		Escalation: esc, Halt: dm, Rearm: dm, Window: 24 * time.Hour,
	}

	const K = 5 // five consecutive ticks of the SAME ongoing death
	for i := 0; i < K; i++ {
		r, err := m.Run(context.Background(), gNow)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !r.Halted {
			t.Fatalf("run %d: the accrual HALT must run every tick (it is load-bearing, never deduped)", i)
		}
	}
	if esc.warned != 1 {
		t.Fatalf("judge-death paged %d times over %d ticks of ONE ongoing death — want exactly 1 (the "+
			"false->true transition); the ongoing state is covered by the AnyCircuitBreakerOpen Prometheus "+
			"alert, so re-paging every tick is spam (TG-425)", esc.warned, K)
	}
	if dm.halts != K {
		t.Fatalf("the HALT must run all %d ticks (only the human page is deduped, never the stop), got %d", K, dm.halts)
	}

	// VACUITY / DIRECTION GUARD: a NEW death AFTER a recovery must page again — the dedup is transition-based,
	// not a permanent gag. Rearm (breaker closes), then a below-threshold run reads not-halted and pages.
	if err := dm.Rearm(context.Background()); err != nil {
		t.Fatalf("rearm: %v", err)
	}
	if _, err := m.Run(context.Background(), gNow); err != nil {
		t.Fatalf("post-rearm run: %v", err)
	}
	if esc.warned != 2 {
		t.Fatalf("a fresh death after recovery must page again (a new transition), got warned=%d want 2", esc.warned)
	}
}

// TestJudgeDeathWithoutHaltStateReaderStillWarns keeps the pre-TG-425 behaviour for a Halt that cannot report
// its state (or no Halt at all): the dedup is purely additive and must never silence a monitor that has no
// durable transition signal to dedup against.
func TestJudgeDeathWithoutHaltStateReaderStillWarns(t *testing.T) {
	ss := sessions(5, time.Hour)
	esc := &fakeEscalator{}
	// No Halt wired -> no HaltStateReader -> alreadyDead stays false -> warns every run (as before).
	m := &JudgeLivenessMonitor{Sessions: fakeSessions{ss}, Judgments: fakeJudgments{map[string]bool{}}, Escalation: esc, Window: 24 * time.Hour}
	for i := 0; i < 3; i++ {
		if _, err := m.Run(context.Background(), gNow); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if esc.warned != 3 {
		t.Fatalf("with no HaltStateReader the monitor must warn every tick (additive dedup), got %d want 3", esc.warned)
	}
}
