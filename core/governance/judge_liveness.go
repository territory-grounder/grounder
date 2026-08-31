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
	// JudgeLifeFraction: the judged fraction a live judge must CLEAR before the halt is released. It sits
	// deliberately ABOVE JudgeDeathFraction so the pair has hysteresis: a fraction hovering on the death
	// threshold would otherwise halt and re-arm on alternate runs, and a graduation gate that flaps is
	// worse than one that stays shut.
	JudgeLifeFraction = 0.75
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

// Rearmer is the RECOVERY counterpart to Halter: it releases a judged-accrual halt (REQ-308).
//
// ★ WHY THIS SEAM EXISTS. JudgeDeadMan.Rearm documents itself as "the ONLY path back", and until
// 2026-08-06 NOTHING in the tree called it outside a test — not the console, not the worker's admin
// surface (which deliberately has no enable-shaped route), not a CLI, not a migration. The mutation
// breaker's Rearm is bound to the owner-gated mode chokepoint; this one was bound to nothing. So the
// first real halt was permanent: measured live, the dead-man had been OPEN for every sample Prometheus
// held and the skill-store flywheel had graduated nothing since 2026-07-31, while drafts and trials kept
// being produced.
//
// A fail-closed control with no reachable recovery is not conservative, it is a one-way door.
type Rearmer interface {
	Rearm(ctx context.Context) error
}

// Escalator routes a judge-death warning through the escalation module.
type Escalator interface {
	Warn(ctx context.Context, kind, detail string) error
}

// HaltStateReader is the OPTIONAL half of the Halter that lets the monitor tell a NEW judge-death from an
// ONGOING one. When the injected Halt implements it (JudgeDeadMan does, via its durable breaker snapshot),
// the human-facing WARN fires ONCE on the false->true transition instead of every tick — the ongoing state
// is already carried by the AnyCircuitBreakerOpen Prometheus alert (circuit_breaker_state==2), whose
// Alertmanager repeat-interval is the correct home for "still dead" reminders (TG-425). A Halt that does not
// implement it keeps the pre-TG-425 warn-every-tick behaviour, so this is purely additive.
type HaltStateReader interface {
	Halted(ctx context.Context) (bool, string)
}

// LivenessResult is the monitor's reading.
type LivenessResult struct {
	Eligible int
	Judged   int
	Fraction float64
	Warned   bool
	// Halted records that this run forced the judged-accrual halt (REQ-308). It rides in the activity's
	// serializable result so the stop is visible in the Temporal run history, not only in a log line.
	Halted bool
	// Rearmed records that this run RELEASED the halt because the judge measured alive. It rides in the
	// same serializable result as Halted for the same reason: a recovery nobody can see in the run history
	// is indistinguishable from a control that never recovers, which is exactly the state this fixes.
	Rearmed bool
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
	// Halt is the ARMED actuator (REQ-308): a judge-death finding stops judged accrual, it does not merely
	// warn. A warning is a message someone must read; the halt is a state a gate must consult. nil ⇒
	// detection-only, which is the pre-TG-222 posture and is honest for a boot with no breaker store.
	Halt Halter
	// Rearm releases the halt when this SAME measurement proves the judge alive. It is not a cooldown and
	// not a timer: the dead-man's own rationale is that it must never "resume accruing on a judge nobody
	// proved alive", and this is that proof — computed from the judge-INDEPENDENT denominator, over a
	// sample large enough to have tripped the halt in the first place, above a threshold set higher than
	// the one that trips it. nil ⇒ the halt is never released here (the pre-2026-08-06 posture, in which
	// it was never released anywhere).
	Rearm Rearmer
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
		// Page the operator ONCE, on the false→true transition — not on every ~hourly tick for the outage's
		// whole duration (desensitising spam; TG-425 measured ~33h of it on 2026-08-08). The ongoing "still
		// dead" state is already carried by the AnyCircuitBreakerOpen Prometheus alert on
		// circuit_breaker_state{name="judge-death"}==2, whose Alertmanager repeat-interval is the right place
		// for reminders. HALT FIRST, PAGE SECOND (REQ-308): the halt is the load-bearing consequence and must
		// not be reachable only through a working notifier — an escalation error returns from this method, so a
		// warn-first ordering would let a broken alerting path swallow the very stop the finding requires. The
		// halt is idempotent and only ever tightens, so it runs EVERY tick; only the human page is deduped, and
		// that dedup is ATOMIC across monitors and sibling workers (TG-432) — haltJudgeAndPage opens the shared
		// breaker and reports whether THIS tick is the transition, replacing the old separate (racy) Halted read.
		halted, page, err := haltJudgeAndPage(ctx, m.Halt, "judge-liveness: judged fraction below threshold — judged accrual halted")
		if err != nil {
			return res, err
		}
		res.Halted = halted
		if m.Escalation != nil && page {
			if err := m.Escalation.Warn(ctx, "judge-death", "judged fraction below threshold"); err != nil {
				return res, err
			}
			res.Warned = true
		}
		return res, nil
	}
	// PROVEN ALIVE — release the halt (REQ-308 recovery). Deliberately gated on the SAME sample-size
	// condition as the halt: a thin or empty population is not evidence of life. "No data is a problem,
	// not everything passed" is the doctrine that put this control here, and it applies in both
	// directions — a judge writing nothing at all must never re-arm itself by writing nothing.
	if m.Rearm != nil && eligible > JudgeDeathMinSample && res.Fraction >= JudgeLifeFraction {
		if err := m.Rearm.Rearm(ctx); err != nil {
			return res, err
		}
		res.Rearmed = true
	}
	return res, nil
}
