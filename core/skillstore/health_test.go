package skillstore

import (
	"context"
	"strings"
	"testing"
	"time"
)

// spec/014 REQ-1313 — "The trial dashboard exposes arm health and assignment staleness".
//
// The scenario these oracles drive: given an active trial with assignments, reading the trial state
// exposes per-arm samples, means, the test statistic, projected completion, and newest-assignment age.

// baseTrial is a 2-arm trial (control + one candidate) ending 30 days out.
func healthTestBaseTrial(now time.Time) Trial {
	return Trial{
		ID:               1,
		SkillName:        "alert-class-playbooks",
		CandidateIDs:     []int64{77},
		Dimension:        "falsifiable_prediction",
		MinSamplesPerArm: 15,
		PThreshold:       0.05,
		EndsAt:           now.Add(30 * 24 * time.Hour),
		Status:           "active",
	}
}

func TestTrialHealthExposesEveryFieldREQ1313(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tr := healthTestBaseTrial(now)
	last := now.Add(-90 * time.Minute)

	scores := map[int][]float64{
		-1: {3, 3, 4, 3, 4},
		0:  {4, 5, 4, 5, 4},
	}
	h := AssessTrial(tr, map[int]int{-1: 20, 0: 22}, scores, 4.0, &last, now)

	if len(h.Arms) != 2 {
		t.Fatalf("want an arm row per arm (control + 1 candidate), got %d", len(h.Arms))
	}
	ctrl, cand := h.Arms[0], h.Arms[1]
	if ctrl.Arm != -1 || cand.Arm != 0 {
		t.Fatalf("arms out of order: %d, %d", ctrl.Arm, cand.Arm)
	}

	// per-arm sample counts — assigned and scored are BOTH reported, and they differ.
	if ctrl.Assigned != 20 || ctrl.Scored != 5 {
		t.Errorf("control: want assigned=20 scored=5, got assigned=%d scored=%d", ctrl.Assigned, ctrl.Scored)
	}
	if cand.Assigned != 22 || cand.Scored != 5 {
		t.Errorf("candidate: want assigned=22 scored=5, got assigned=%d scored=%d", cand.Assigned, cand.Scored)
	}
	// the shortfall is derived from SCORED, not assigned: 15 needed, 5 scored ⇒ 10 short per arm.
	if ctrl.Short != 10 || cand.Short != 10 || h.ShortSamples != 20 {
		t.Errorf("want 10 short per arm and 20 total, got %d/%d total %d", ctrl.Short, cand.Short, h.ShortSamples)
	}

	// means
	if ctrl.Mean == nil || cand.Mean == nil {
		t.Fatalf("both arms scored, so both means must be present: %v %v", ctrl.Mean, cand.Mean)
	}
	if *ctrl.Mean != 3.4 || *cand.Mean != 4.4 {
		t.Errorf("means: want 3.4/4.4, got %v/%v", *ctrl.Mean, *cand.Mean)
	}

	// the test statistic — on the candidate, never on the control (nothing to compare it against).
	if ctrl.PVsControl != nil {
		t.Errorf("the control arm has no p against itself, got %v", *ctrl.PVsControl)
	}
	if cand.PVsControl == nil {
		t.Fatal("the candidate arm has 5 samples on each side — the test statistic must be exposed")
	}
	if *cand.PVsControl <= 0 || *cand.PVsControl >= 1 {
		t.Errorf("p out of range: %v", *cand.PVsControl)
	}

	// projected completion: 20 short at 4/day ⇒ 5 days, well inside the 30-day window.
	if h.ProjectedCompletion == nil {
		t.Fatal("a positive fill rate with samples outstanding must yield a projection")
	}
	if got, want := *h.ProjectedCompletion, now.Add(5*24*time.Hour); !got.Equal(want) {
		t.Errorf("projection: want %s, got %s", want, got)
	}
	if !h.CompletesBeforeEnd {
		t.Error("a projection 5 days out on a trial ending in 30 completes before the end")
	}

	// assignment staleness
	if h.NewestAssignment == nil || !h.NewestAssignment.Equal(last) {
		t.Fatalf("newest assignment: want %s, got %v", last, h.NewestAssignment)
	}
	if h.AssignmentAgeSecond == nil || *h.AssignmentAgeSecond != 5400 {
		t.Errorf("staleness: want 5400s, got %v", h.AssignmentAgeSecond)
	}
}

// The defect this whole read model exists for, in its live form: 32 assignments, control already at
// the per-arm minimum, and ZERO scored samples. Trial 14 on the running system, 2026-08-07.
func TestAssignedAtMinimumWithNoScoredSampleIsNotHealthy(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tr := healthTestBaseTrial(now)
	last := now.Add(-8 * time.Hour)

	h := AssessTrial(tr, map[int]int{-1: 15, 0: 17}, map[int][]float64{}, 0.43, &last, now)

	if h.Arms[0].Assigned != 15 {
		t.Fatalf("precondition: control is at the 15-per-arm assignment count, got %d", h.Arms[0].Assigned)
	}
	for _, a := range h.Arms {
		if a.Scored != 0 {
			t.Fatalf("arm %d: precondition is zero scored samples, got %d", a.Arm, a.Scored)
		}
		if a.Short != 15 {
			t.Errorf("arm %d: an arm at 0 scored samples is 15 short whatever its assignment count, got %d", a.Arm, a.Short)
		}
		// Absent is not zero. A mean of 0 renders as the worst attainable score.
		if a.Mean != nil {
			t.Errorf("arm %d: an unmeasured mean must be absent, got %v", a.Arm, *a.Mean)
		}
		if a.PVsControl != nil {
			t.Errorf("arm %d: an unmeasured p must be absent, got %v", a.Arm, *a.PVsControl)
		}
	}
	// 30 short at 0.43/day ⇒ ~70 days; the trial ends in 30.
	if h.ProjectedCompletion == nil {
		t.Fatal("a positive rate must still project, even when the projection is bad news")
	}
	if h.CompletesBeforeEnd {
		t.Errorf("30 samples at 0.43/day is ~70 days on a trial ending in 30 — this must not read as completing (projected %s, ends %s)",
			h.ProjectedCompletion, tr.EndsAt)
	}
}

// A zero fill rate is not a distant completion date. Rendering "cannot be projected" is the whole
// difference between an operator acting and an operator waiting.
func TestZeroFillRateProjectsNothingRatherThanForever(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	h := AssessTrial(healthTestBaseTrial(now), map[int]int{-1: 3, 0: 3}, map[int][]float64{}, 0, nil, now)

	if h.ProjectedCompletion != nil {
		t.Errorf("no measurable supply cannot yield a completion date, got %s", h.ProjectedCompletion)
	}
	if h.CompletesBeforeEnd {
		t.Error("an unprojectable trial must never read as completing before its end")
	}
	// never assigned is distinct from assigned long ago; neither collapses to an age of zero.
	if h.NewestAssignment != nil || h.AssignmentAgeSecond != nil {
		t.Errorf("a trial with no assignment has no staleness to report, got %v / %v", h.NewestAssignment, h.AssignmentAgeSecond)
	}
}

func TestFullTrialCompletesWithoutAProjection(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	full := make([]float64, 15)
	for i := range full {
		full[i] = 4
	}
	h := AssessTrial(healthTestBaseTrial(now), map[int]int{-1: 40, 0: 41},
		map[int][]float64{-1: full, 0: full}, 0, nil, now)

	if h.ShortSamples != 0 {
		t.Fatalf("both arms at the minimum leaves nothing short, got %d", h.ShortSamples)
	}
	if !h.CompletesBeforeEnd {
		t.Error("a full trial is decidable by the next finalize pass")
	}
	if h.ProjectedCompletion != nil {
		t.Errorf("a full trial has nothing left to project, got %s", h.ProjectedCompletion)
	}
}

// ---- REQ-1309: the start guard projects against the population its arms fill from -------------------

// The live case, as a refusal. falsifiable_prediction supplied 0.43 filling samples/day while all
// judged sessions ran at 207.57/day; a 2x15 trial needs 30 ⇒ ~70 days against a 49-day window.
func TestStartRefusesOnTheDimensionsOwnSupplyNotOverallTraffic(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	st := NewMemTrialStore(207.57) // what the old global rate reported
	st.SetDimensionRate("falsifiable_prediction", 0.43)

	tr := Trial{
		SkillName:        "alert-class-playbooks",
		CandidateIDs:     []int64{77},
		Dimension:        "falsifiable_prediction",
		MinSamplesPerArm: 15,
		EndsAt:           now.Add(49 * 24 * time.Hour),
	}
	_, err := StartTrial(context.Background(), st, tr, now)
	if err == nil {
		t.Fatal("30 samples at 0.43/day is ~70 days into a 49-day window — the trial must be refused")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("want a starvation refusal, got %v", err)
	}
	// The refusal must name the dimension it measured, so an operator can tell WHICH supply was short.
	if !strings.Contains(err.Error(), "falsifiable_prediction") {
		t.Errorf("the refusal must name the dimension it projected against: %v", err)
	}
}

// The same trial on a dimension with real supply starts — the guard scopes, it does not just refuse.
func TestStartAcceptsWhenTheDimensionsOwnSupplySuffices(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	st := NewMemTrialStore(0.01) // a global rate that would refuse everything
	st.SetDimensionRate("correct_diagnosis", 12.36)

	tr := Trial{
		SkillName:        "triage-protocol",
		CandidateIDs:     []int64{88},
		Dimension:        "correct_diagnosis",
		MinSamplesPerArm: 15,
		EndsAt:           now.Add(49 * 24 * time.Hour),
	}
	got, err := StartTrial(context.Background(), st, tr, now)
	if err != nil {
		t.Fatalf("30 samples at 12.36/day is ~2.4 days into a 49-day window — must start: %v", err)
	}
	if got.Status != "active" {
		t.Errorf("want an active trial, got %q", got.Status)
	}
}

// The guard must ASK for the trial's dimension, not any dimension. A store that answers per-dimension
// while the engine asks globally is the drift this argument exists to prevent.
func TestStartAsksTheStoreForTheTrialsOwnDimension(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	st := NewMemTrialStore(500)
	st.SetDimensionRate("correct_diagnosis", 500)
	st.SetDimensionRate("falsifiable_prediction", 0.001)

	tr := Trial{
		SkillName:        "alert-class-playbooks",
		CandidateIDs:     []int64{77},
		Dimension:        "falsifiable_prediction",
		MinSamplesPerArm: 15,
		EndsAt:           now.Add(49 * 24 * time.Hour),
	}
	if _, err := StartTrial(context.Background(), st, tr, now); err == nil {
		t.Fatal("the guard read a rate other than falsifiable_prediction's — 0.001/day cannot fill 30 samples in 49 days")
	}
}
