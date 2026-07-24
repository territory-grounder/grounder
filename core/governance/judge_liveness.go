package governance

import (
	"context"
	"time"
)

const (
	// JudgeDeathFraction: a judged fraction below this over a large-enough sample trips a warning.
	JudgeDeathFraction = 0.5
	// JudgeDeathMinSample: the eligible population must EXCEED this (i.e. > 3) for a warning — a thinner
	// sample is too small to page on (REQ-306).
	JudgeDeathMinSample = 3
)

// Session is a recently-ended session as read from the judge-INDEPENDENT session-outcome tables (the
// denominator population). The judge holds no write grant on this source, so it cannot enlarge or shrink
// its own eligibility set (REQ-305, judge-independence).
type Session struct {
	SessionID string
	EndedAt   time.Time
	Synthetic bool // synthetic canary rows are excluded from the liveness sample
}

// SessionStore is the judge-INDEPENDENT denominator source.
type SessionStore interface {
	RecentlyEnded(ctx context.Context) ([]Session, error)
}

// JudgmentStore reads whether a real local judgment (a non-negative overall_score) exists for a session.
// This is the ONLY judge-written table the monitor touches, and only for numerator existence — never for
// the denominator.
type JudgmentStore interface {
	HasRealJudgment(ctx context.Context, sessionID string) bool
}

// Escalator routes a judge-death warning through the escalation module.
type Escalator interface {
	Warn(ctx context.Context, kind, detail string) error
}

// LivenessResult is the monitor's reading.
type LivenessResult struct {
	Eligible int
	Judged   int
	Fraction float64
	Warned   bool
}

// JudgeLivenessMonitor measures whether the local judge is scoring recent sessions.
type JudgeLivenessMonitor struct {
	Sessions   SessionStore
	Judgments  JudgmentStore
	Escalation Escalator
	Window     time.Duration // recency-eligibility UPPER bound (ended recently enough to still be judged)
	// Lag is the recency-eligibility LOWER bound: a session that ended within Lag has not yet had time for
	// the judge's cadence to run, so it is NOT counted as un-judged (counting it would falsely depress the
	// fraction and page a healthy judge as dead). Zero disables the lower bound.
	Lag time.Duration
}

// Run computes the judged fraction: the denominator is the eligible recently-ended, non-synthetic
// sessions drawn from the judge-independent store; the numerator is those with a real judgment. A
// session outside the recency window or a synthetic row is excluded. When the eligible population
// EXCEEDS the minimum sample and the fraction is below the threshold, it raises a judge-death warning
// through the escalation module (REQ-305/306). A dead judge drives the fraction down and trips the
// warning rather than reporting itself healthy.
func (m *JudgeLivenessMonitor) Run(ctx context.Context, now time.Time) (LivenessResult, error) {
	sessions, err := m.Sessions.RecentlyEnded(ctx)
	if err != nil {
		return LivenessResult{}, err
	}
	eligible, judged := 0, 0
	for _, s := range sessions {
		if s.Synthetic {
			continue
		}
		if m.Window > 0 && now.Sub(s.EndedAt) > m.Window {
			continue // too old — outside the recency window, not eligible
		}
		if m.Lag > 0 && now.Sub(s.EndedAt) < m.Lag {
			continue // too recent — the judge's cadence has not run yet, so not-yet-judgeable is not "un-judged"
		}
		eligible++
		if m.Judgments.HasRealJudgment(ctx, s.SessionID) {
			judged++
		}
	}
	res := LivenessResult{Eligible: eligible, Judged: judged}
	if eligible > 0 {
		res.Fraction = float64(judged) / float64(eligible)
	}
	if eligible > JudgeDeathMinSample && res.Fraction < JudgeDeathFraction {
		if m.Escalation != nil {
			if err := m.Escalation.Warn(ctx, "judge-death", "judged fraction below threshold"); err != nil {
				return res, err
			}
		}
		res.Warned = true
	}
	return res, nil
}
