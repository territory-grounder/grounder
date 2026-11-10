package skillstore

import "time"

// The trial read model (spec/014 REQ-1313): per-arm sample counts, means, the test statistic,
// projected completion, and assignment staleness.
//
// WHY THIS TYPE EXISTS AT ALL. Until 2026-08-07 the dashboard published per-arm ASSIGNMENT counts and
// nothing else. Assignments and filling samples are DIFFERENT POPULATIONS: a session is assigned the
// instant it starts, but it fills an arm only once the judge has scored the trial's dimension under
// the CURRENT rubric. Measured live that morning, trial 14 (alert-class-playbooks /
// falsifiable_prediction, active, ends 2026-09-25) carried 32 assignments — control already at its
// 15-per-arm minimum — and ZERO scored samples on either arm. The dashboard rendered a trial about to
// complete; the engine saw a trial that had not started accumulating. Publishing the assignment count
// alone is the wrong-denominator defect in its reporting form.

// ArmHealth is one arm's fill state. Arm -1 is the control bucket (the elided last hash bucket).
type ArmHealth struct {
	Arm      int `json:"arm"`
	Assigned int `json:"assigned"`
	Scored   int `json:"scored"`
	// Short is how many further SCORED samples this arm needs to reach MinSamplesPerArm. It is
	// derived from Scored, never from Assigned — the arm minimum FinalizeTrials enforces is a
	// scored-sample minimum.
	Short int `json:"short"`
	// Mean and PVsControl are nil for an arm with no scored sample, and PVsControl is additionally nil
	// for the control arm itself and whenever either side holds fewer than the two samples a variance
	// needs. Absent is not zero: a zero mean renders as the worst attainable score on a surface whose
	// job here is to say "not measured", and a zero p-value renders as decisive significance.
	Mean       *float64 `json:"mean,omitempty"`
	PVsControl *float64 `json:"p_vs_control,omitempty"`
}

// TrialHealth is the whole-trial read model.
type TrialHealth struct {
	Arms []ArmHealth `json:"arms"`
	// ShortSamples totals Short across every arm — the scored samples between this trial and a
	// finalize decision.
	ShortSamples int `json:"short_samples"`
	// FillRatePerDay is the observed supply of filling samples for THIS trial's dimension, per day.
	// It is the same population Arms.Scored is counted from, which is the whole point: a projection
	// made against a wider population is the defect this model reports on.
	FillRatePerDay float64 `json:"fill_rate_per_day"`
	// ProjectedCompletion is nil when the rate is zero — an unmeasurable completion is not a distant
	// one, and nil renders as "cannot be projected" instead of a reassuring date. It is also nil once
	// the trial is already full (nothing left to project).
	ProjectedCompletion *time.Time `json:"projected_completion,omitempty"`
	// CompletesBeforeEnd is true only when a projection exists and lands at or before EndsAt. A trial
	// that is already full is complete-able now, so it is true. Everything else — including an
	// unprojectable trial — is false: this flag never reads optimistically from missing data.
	CompletesBeforeEnd bool `json:"completes_before_end"`
	// NewestAssignment / AssignmentAgeSeconds are the dead-man indicator. Nil means the trial has
	// never been assigned anything, which is a distinct condition from "assigned long ago" and must
	// not collapse into an age of zero.
	NewestAssignment    *time.Time `json:"newest_assignment,omitempty"`
	AssignmentAgeSecond *float64   `json:"newest_assignment_age_seconds,omitempty"`
}

// AssessTrial builds the read model from the trial row, its per-arm assignment counts, its per-arm
// scored samples, the dimension's observed fill rate, and the newest assignment time. Pure: every
// input is measurement data and the function reads no clock but the `now` it is handed.
func AssessTrial(t Trial, assigned map[int]int, scores map[int][]float64, fillRatePerDay float64, newestAssignment *time.Time, now time.Time) TrialHealth {
	h := TrialHealth{FillRatePerDay: fillRatePerDay}
	control := scores[-1]

	for arm := -1; arm < len(t.CandidateIDs); arm++ {
		s := scores[arm]
		a := ArmHealth{Arm: arm, Assigned: assigned[arm], Scored: len(s)}
		if short := t.MinSamplesPerArm - len(s); short > 0 {
			a.Short = short
			h.ShortSamples += short
		}
		if len(s) > 0 {
			m := mean(s)
			a.Mean = &m
		}
		// The test statistic is reported only where it is defined: a candidate arm, both sides at two
		// or more samples. WelchOneSided itself returns (0, 1) below that, and publishing a p of 1
		// there would read as a measured non-result rather than an unmeasured one.
		if arm >= 0 && len(s) >= 2 && len(control) >= 2 {
			_, p := WelchOneSided(s, control)
			a.PVsControl = &p
		}
		h.Arms = append(h.Arms, a)
	}

	switch {
	case h.ShortSamples == 0:
		// Already full: the next FinalizeTrials pass can decide it.
		h.CompletesBeforeEnd = true
	case fillRatePerDay > 0:
		days := float64(h.ShortSamples) / fillRatePerDay
		p := now.Add(time.Duration(days * 24 * float64(time.Hour)))
		h.ProjectedCompletion = &p
		h.CompletesBeforeEnd = !p.After(t.EndsAt)
	}

	if newestAssignment != nil {
		n := *newestAssignment
		h.NewestAssignment = &n
		age := now.Sub(n).Seconds()
		h.AssignmentAgeSecond = &age
	}
	return h
}
