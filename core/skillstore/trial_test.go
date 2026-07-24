package skillstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func fifteen(base float64) []float64 {
	out := make([]float64, 15)
	for i := range out {
		out[i] = base + float64(i%5)*0.1 // deterministic spread, nonzero variance
	}
	return out
}

// REQ-1306: assignment is deterministic, idempotent (the stored row wins), and rejects malformed refs
// before hashing.
func TestAssignArmDeterministicIdempotentMalformed(t *testing.T) {
	st := NewMemTrialStore(10)
	tr := Trial{ID: 9, CandidateIDs: []int64{101, 102}}

	v1, err := AssignArm(context.Background(), st, "am-HostDown-web01", tr)
	if err != nil {
		t.Fatal(err)
	}
	// Golden from the predecessor Python: blake2b8("am-HostDown-web01|9") % 3 == 2 == the control bucket.
	if v1 != -1 {
		t.Fatalf("golden arm must be control (-1), got %d", v1)
	}
	v2, _ := AssignArm(context.Background(), st, "am-HostDown-web01", tr)
	if v2 != v1 {
		t.Fatalf("assignment must be idempotent, got %d then %d", v1, v2)
	}
	if _, err := AssignArm(context.Background(), st, "   ", tr); !errors.Is(err, ErrMalformedRef) {
		t.Fatalf("whitespace ref must be rejected before hashing, got %v", err)
	}
}

// REQ-1309: a trial that cannot complete at the observed judged-session rate is refused with the
// projection in the reason; a completable one starts.
func TestStartTrialTrafficAwareRefusal(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	tr := Trial{SkillName: "triage-protocol", CandidateIDs: []int64{101}, Dimension: "correct_diagnosis",
		MinSamplesPerArm: 15, MinLift: 0.05, PThreshold: 0.1, EndsAt: now.Add(14 * 24 * time.Hour)}

	if _, err := StartTrial(context.Background(), NewMemTrialStore(1), tr, now); !errors.Is(err, ErrTrialStarvation) {
		t.Fatalf("1/day cannot fill 30 samples in 14d — must refuse, got %v", err)
	}
	if _, err := StartTrial(context.Background(), NewMemTrialStore(0), tr, now); !errors.Is(err, ErrTrialStarvation) {
		t.Fatalf("zero traffic must refuse, got %v", err)
	}
	started, err := StartTrial(context.Background(), NewMemTrialStore(10), tr, now)
	if err != nil || started.Status != "active" {
		t.Fatalf("10/day fills 30 samples in 3d — must start, got %v %v", started.Status, err)
	}
}

// REQ-1308: the timeout sweep runs FIRST; a full trial graduates only with lift + significance; a full
// trial with no winner aborts; an under-sampled trial stays active.
func TestFinalizeSweepThenDecide(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	st := NewMemTrialStore(10)
	mk := func(name string, ends time.Time) Trial {
		tr, _ := st.CreateTrial(context.Background(), Trial{SkillName: name, CandidateIDs: []int64{101},
			Dimension: "correct_diagnosis", MinSamplesPerArm: 15, MinLift: 0.05, PThreshold: 0.1,
			EndsAt: ends, Status: "active"})
		return tr
	}
	expired := mk("expired-skill", now.Add(-time.Hour))
	st.SetScores(expired.ID, map[int][]float64{-1: fifteen(4.5), 0: fifteen(4.9)}) // would have won — timeout still aborts
	winner := mk("winner-skill", now.Add(time.Hour))
	st.SetScores(winner.ID, map[int][]float64{-1: fifteen(3.5), 0: fifteen(4.2)})
	loser := mk("loser-skill", now.Add(time.Hour))
	st.SetScores(loser.ID, map[int][]float64{-1: fifteen(3.5), 0: fifteen(3.52)}) // lift too small
	young := mk("young-skill", now.Add(time.Hour))
	st.SetScores(young.ID, map[int][]float64{-1: fifteen(3.5), 0: {4.0, 4.1}}) // under-sampled

	out, err := FinalizeTrials(context.Background(), st, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]FinalizeOutcome{}
	for _, o := range out {
		got[o.TrialID] = o
	}
	if got[expired.ID].Status != "aborted_timeout" {
		t.Fatalf("expired trial must abort in the sweep, got %+v", got[expired.ID])
	}
	if got[winner.ID].Status != "completed" || got[winner.ID].WinnerID != 101 {
		t.Fatalf("clear lift must graduate candidate 101, got %+v", got[winner.ID])
	}
	if got[loser.ID].Status != "aborted_no_winner" {
		t.Fatalf("insufficient lift must abort-no-winner, got %+v", got[loser.ID])
	}
	if got[young.ID].Status != "active" {
		t.Fatalf("under-sampled trial must stay active, got %+v", got[young.ID])
	}
}

// REQ-1308: the asymmetric safety guard — a target-dimension winner whose safety analog regressed
// against the concurrent control does not graduate.
func TestFinalizeSafetyGuardBlocksGraduation(t *testing.T) {
	now := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	st := NewMemTrialStore(10)
	tr, _ := st.CreateTrial(context.Background(), Trial{SkillName: "s", CandidateIDs: []int64{101},
		Dimension: "correct_diagnosis", MinSamplesPerArm: 15, MinLift: 0.05, PThreshold: 0.1,
		EndsAt: now.Add(time.Hour), Status: "active"})
	st.SetScores(tr.ID, map[int][]float64{-1: fifteen(3.5), 0: fifteen(4.2)}) // clear target win
	st.SetSafety(tr.ID, map[int][]float64{-1: fifteen(4.5), 0: fifteen(3.0)}) // safety regressed

	out, err := FinalizeTrials(context.Background(), st, now)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Status != "aborted_no_winner" {
		t.Fatalf("a safety-regressed winner must not graduate, got %+v", out[0])
	}
	if !strings.Contains(st.trials[tr.ID].Note, "no candidate beat") {
		t.Fatalf("the note log must carry the refusal, got %q", st.trials[tr.ID].Note)
	}
}

// The mem-store assignment key must be decimal-formatted — trial ids >= 10 collided under the old
// rune-arithmetic key (review blocker): ids 1 and 11 must isolate, and the malformed counter counts.
func TestAssignArmLargeTrialIDsIsolate(t *testing.T) {
	st := NewMemTrialStore(10)
	t1 := Trial{ID: 1, CandidateIDs: []int64{101}}
	t11 := Trial{ID: 11, CandidateIDs: []int64{102, 103, 104, 105}}
	a1, _ := AssignArm(context.Background(), st, "ref-x", t1)
	a11, _ := AssignArm(context.Background(), st, "ref-x", t11)
	b1, _ := AssignArm(context.Background(), st, "ref-x", t1)
	if a1 != b1 {
		t.Fatalf("trial 1 assignment must be stable, got %d then %d", a1, b1)
	}
	// distinct trials must have independent rows: re-reading trial 11 must not return trial 1's arm slot
	b11, _ := AssignArm(context.Background(), st, "ref-x", t11)
	if a11 != b11 {
		t.Fatalf("trial 11 assignment must be stable and independent, got %d then %d", a11, b11)
	}
	if _, err := AssignArm(context.Background(), st, " ", t11); !errors.Is(err, ErrMalformedRef) {
		t.Fatal("malformed ref must reject")
	}
	if st.Malformed() != 1 {
		t.Fatalf("the malformed counter must count, got %d", st.Malformed())
	}
}
