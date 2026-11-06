package skilltrial

// TG-222 / spec/004 REQ-308 — a CONFIRMED DEAD JUDGE halts judged accrual at the graduation choke point.
//
// This drives the REAL FinalizeActivity with the REAL core/governance.JudgeDeadMan over a REAL
// core/breaker — the same objects cmd/worker wires — against the in-memory trial store CI already uses
// (no Postgres, constraint D5). The load-bearing assertion is that a HALTED dead-man leaves the trial store
// completely untouched: nothing swept, nothing graduated, nothing rejected. A "halt" that still mutates
// trial rows has not halted accrual, it has only complained about it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/governance"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// expiredTrial seeds one trial whose window has closed, so an unhalted finalize pass HAS work to do —
// without that, "nothing changed" would prove nothing.
func expiredTrial(t *testing.T, st *skillstore.MemTrialStore, now time.Time) skillstore.Trial {
	t.Helper()
	tr, err := st.CreateTrial(context.Background(), skillstore.Trial{
		SkillName: "triage-protocol", CandidateIDs: []int64{2}, ControlVersionID: 1,
		Dimension: "correct_diagnosis", MinSamplesPerArm: 4, MinLift: 0.2,
		EndsAt: now.Add(-time.Hour), Status: "active",
	})
	if err != nil {
		t.Fatalf("seed trial: %v", err)
	}
	return tr
}

func TestHaltedJudgeDeadManRefusesTheGraduationPass(t *testing.T) {
	now := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	st := skillstore.NewMemTrialStore(10)
	tr := expiredTrial(t, st, now)

	dm, err := governance.NewJudgeDeadMan(breaker.NewMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	acts := &Activities{D: Deps{Trials: st, Store: skillstore.NewMemStore(), JudgeHealth: dm}}

	// Control: with a healthy judge the pass RUNS and does its work (so the halted case is a real difference,
	// not a vacuous "nothing happened either way").
	if _, rerr := acts.FinalizeActivity(context.Background(), now); rerr != nil {
		t.Fatalf("a healthy judge must not block the finalize pass: %v", rerr)
	}
	if stillActive(context.Background(), t, st, tr.ID) {
		t.Fatal("the control pass did no work — the expired trial is still active, so this oracle cannot " +
			"distinguish a halt from a no-op")
	}

	// Now the judge is confirmed dead. A NEW expired trial is seeded so there is again work available.
	st2 := skillstore.NewMemTrialStore(10)
	tr2 := expiredTrial(t, st2, now)
	if herr := dm.Halt(context.Background(), "frontier cross-check: local judge scored nothing the frontier could"); herr != nil {
		t.Fatal(herr)
	}
	acts2 := &Activities{D: Deps{Trials: st2, Store: skillstore.NewMemStore(), JudgeHealth: dm}}
	out, err := acts2.FinalizeActivity(context.Background(), now)
	if err == nil {
		t.Fatal("a confirmed dead judge must make the finalize pass FAIL LOUDLY — a quiet skip is a warning " +
			"nobody reads, which is how a judge stayed dead for three weeks")
	}
	if !strings.Contains(err.Error(), "REFUSING to graduate") {
		t.Fatalf("the refusal must say what it refused and why, got %q", err)
	}
	if out.Graduated != 0 || out.Swept != 0 || out.NoWinner != 0 || out.StillOpen != 0 {
		t.Fatalf("a halted pass must report NO work at all, got %+v", out)
	}
	// THE ACCRUAL ASSERTION: the trial row is untouched, so the next pass reconsiders it whole once the
	// judge is proven alive and an operator re-arms.
	if !stillActive(context.Background(), t, st2, tr2.ID) {
		t.Fatal("a halted pass mutated the trial — accrual was not actually halted")
	}

	// Re-arm and the pass resumes: the halt is recoverable, never a permanent kill.
	if rerr := dm.Rearm(context.Background()); rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := acts2.FinalizeActivity(context.Background(), now); rerr != nil {
		t.Fatalf("after a re-arm the pass must run again: %v", rerr)
	}
}

// An unwired dead-man is an honest no-op: identical behaviour to the pre-TG-222 finalizer, so a boot with no
// breaker store is unaffected rather than silently half-guarded.
func TestNilJudgeHealthIsTransparent(t *testing.T) {
	now := time.Date(2026, 7, 31, 4, 0, 0, 0, time.UTC)
	st := skillstore.NewMemTrialStore(10)
	expiredTrial(t, st, now)
	acts := &Activities{D: Deps{Trials: st, Store: skillstore.NewMemStore()}}
	if _, err := acts.FinalizeActivity(context.Background(), now); err != nil {
		t.Fatalf("an unwired dead-man must not change behaviour: %v", err)
	}
}

// stillActive reports whether the trial is still in the store's ACTIVE set — the only judged-accrual state
// change FinalizeTrials can make, read through the same TrialStore surface production uses.
func stillActive(ctx context.Context, t *testing.T, st skillstore.TrialStore, id int64) bool {
	t.Helper()
	trials, err := st.ActiveTrials(ctx)
	if err != nil {
		t.Fatalf("active trials: %v", err)
	}
	for _, tr := range trials {
		if tr.ID == id {
			return true
		}
	}
	return false
}
