package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
)

// TG-432 — the judge-death PAGE is deduped ATOMICALLY across both monitors and every sibling worker, via a
// real JudgeDeadMan over a shared breaker store (which implements AtomicHalter/AtomicOpener). These oracles
// drive the production types, not the fakeDeadman fallback the TG-425 tests use.

// TestJudgeDeathPagesOnceAcrossMonitorsViaAtomicBreaker is the finding-1 killing oracle at the monitor level.
// One death, seen by BOTH the liveness monitor and the frontier monitor sharing one dead-man, pages exactly
// once: the first to run opens the shared breaker (openedNow=true → pages); the second finds it already open
// (openedNow=false → halts idempotently but does NOT re-page). The decision is the atomic CompareAndOpen, not
// a separate racy Halted() read, so two monitors in different worker processes cannot both page.
//
//	Killing mutation: make haltJudgeAndPage's atomic branch return (true, true) — page regardless of who
//	opened it — and the frontier monitor re-pages the already-open death: escF.warned becomes 1, RED.
func TestJudgeDeathPagesOnceAcrossMonitorsViaAtomicBreaker(t *testing.T) {
	store := breaker.NewMemStore() // the process-shared row both monitors' dead-man coordinate through
	dm, err := NewJudgeDeadMan(store, nil)
	if err != nil {
		t.Fatal(err)
	}

	escL := &fakeEscalator{}
	liveness := &JudgeLivenessMonitor{
		Sessions: fakeSessions{sessions(10, time.Hour)}, Judgments: fakeJudgments{map[string]bool{}},
		Escalation: escL, Halt: dm, Window: 24 * time.Hour,
	}
	escF := &fakeEscalator{}
	frontier := &FrontierCrossCheckMonitor{Pairs: fakePairs{deathPairs(6)}, Escalation: escF, Halt: dm}

	// Liveness sees the death first: it opens the shared breaker and pages once.
	rL, err := liveness.Run(context.Background(), gNow)
	if err != nil {
		t.Fatal(err)
	}
	if !rL.Halted || escL.warned != 1 {
		t.Fatalf("the first monitor to confirm the death must halt and page once: halted=%v warned=%d", rL.Halted, escL.warned)
	}

	// The frontier monitor sees the SAME death, but the shared breaker is already open: it halts (idempotent)
	// and must NOT re-page.
	rF, err := frontier.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rF.Death || !rF.Halted {
		t.Fatalf("the frontier monitor must still detect and halt the ongoing death (idempotent): %+v", rF)
	}
	if escF.warned != 0 {
		t.Fatalf("the death was already paged by liveness (shared breaker open) — the frontier monitor must NOT "+
			"re-page; got warned=%d want 0 (TG-432 atomic cross-monitor dedup)", escF.warned)
	}
}

// flakyOpenStore is a breaker store whose atomic open fails its first `failN` calls, modelling a transient DB
// blip at the exact instant of a NEW death. Load/Save/List are inherited from the embedded MemStore.
type flakyOpenStore struct {
	*breaker.MemStore
	failN int
}

func (s *flakyOpenStore) CompareAndOpen(ctx context.Context, name string, now time.Time) (bool, error) {
	if s.failN > 0 {
		s.failN--
		return false, errors.New("transient breaker-store failure")
	}
	return s.MemStore.CompareAndOpen(ctx, name, now)
}

// TestJudgeDeathGenesisPageSurvivesAStoreBlip is the finding-2 killing oracle. The pre-TG-432 code read the
// breaker state SEPARATELY from the halt: a read blip fails CLOSED to "already dead", so the one genesis page
// was suppressed while the halt (a separate write) still landed — losing that page forever. The atomic op has
// no separate read: a store failure surfaces as an error (HALT-FIRST) and leaves the breaker CLOSED, so the
// death is still owed a page and the very next healthy tick fires it. The page is DEFERRED, never swallowed.
//
//	Killing mutation: make HaltOpen swallow the store error (return (false, nil)); tick 1 no longer errors —
//	the `err == nil` assertion below goes RED.
func TestJudgeDeathGenesisPageSurvivesAStoreBlip(t *testing.T) {
	store := &flakyOpenStore{MemStore: breaker.NewMemStore(), failN: 1}
	dm, err := NewJudgeDeadMan(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	esc := &fakeEscalator{}
	m := &JudgeLivenessMonitor{
		Sessions: fakeSessions{sessions(10, time.Hour)}, Judgments: fakeJudgments{map[string]bool{}},
		Escalation: esc, Halt: dm, Window: 24 * time.Hour,
	}

	// Tick 1 — the store blips on the atomic open. The tick fails (halt-first), nothing is paged, and the
	// breaker is left CLOSED (the failed open wrote nothing).
	if _, err := m.Run(context.Background(), gNow); err == nil {
		t.Fatal("tick 1: a store failure on the atomic open must SURFACE as an error (halt-first), not be swallowed")
	}
	if esc.warned != 0 {
		t.Fatalf("tick 1: nothing may page on a failed open, got warned=%d", esc.warned)
	}
	if open, _ := dm.Halted(context.Background()); open {
		t.Fatal("tick 1: a FAILED atomic open must leave the breaker CLOSED — the death is still owed its page")
	}

	// Tick 2 — the store has recovered. The death is still new (breaker never opened), so this tick opens it
	// and fires the deferred genesis page: deferred by one tick, never lost.
	r, err := m.Run(context.Background(), gNow)
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if !r.Halted || esc.warned != 1 {
		t.Fatalf("tick 2 (store recovered) must open the breaker and fire the DEFERRED genesis page: halted=%v warned=%d want warned=1", r.Halted, esc.warned)
	}
}

// driftPairs builds n comparable pairs (both judges scored) where all but the first DISAGREE — a low
// agreement rate over the >5 floor, i.e. DRIFT with no death (nothing is locally-unscored).
func driftPairs(n int) []CrossCheckPair {
	pairs := make([]CrossCheckPair, 0, n)
	for i := range n {
		lv, fv := "deviation", "match"
		if i == 0 {
			lv, fv = "match", "match"
		}
		pairs = append(pairs, CrossCheckPair{SessionID: "s", LocalScored: true, LocalVerdict: lv, FrontierScored: true, FrontierVerdict: fv})
	}
	return pairs
}

// TestFrontierDriftPagesEveryRunAtRunLevel is the finding-3 regression test: the DRIFT→Warn branch had a
// Run()-level coverage gap (only Evaluate() was exercised). DRIFT is deliberately NOT transition-deduped like
// death — a local↔frontier disagreement is a standing human-adjudication signal that pages and keeps paging —
// so over K runs it pages K times, even with a real (atomic) dead-man wired for the death path.
//
//	Killing mutation: dedup drift like death (page once) → warned=1 not K, RED; or drop the res.Drift→Warn
//	branch → warned=0, RED.
func TestFrontierDriftPagesEveryRunAtRunLevel(t *testing.T) {
	dm, err := NewJudgeDeadMan(breaker.NewMemStore(), nil) // real dead-man: proves drift is independent of it
	if err != nil {
		t.Fatal(err)
	}
	esc := &fakeEscalator{}
	m := &FrontierCrossCheckMonitor{Pairs: fakePairs{driftPairs(6)}, Escalation: esc, Halt: dm}

	const K = 3
	for i := range K {
		r, err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !r.Drift {
			t.Fatalf("run %d: low local↔frontier agreement over >5 pairs must raise DRIFT through Run(): %+v", i, r)
		}
		if r.Death {
			t.Fatalf("run %d: pure drift (no locally-unscored pairs) must NOT read as a death: %+v", i, r)
		}
	}
	if esc.warned != K {
		t.Fatalf("DRIFT must page on EVERY run (it is not transition-deduped like death) — got %d over %d runs, want %d (TG-432 finding 3)", esc.warned, K, K)
	}
}
