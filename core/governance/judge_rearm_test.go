package governance

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A fail-closed control with no reachable recovery is a one-way door.
//
// `JudgeDeadMan.Rearm` documents itself as "the ONLY path back", and until 2026-08-06 nothing in the tree
// called it outside a test: no console route, no worker admin route (that surface deliberately refuses
// enable-shaped actions), no CLI, no migration. The MUTATION breaker's Rearm is bound to the owner-gated
// mode chokepoint; this one was bound to nothing. So the first real halt was permanent — measured live,
// the dead-man was OPEN for every sample Prometheus held and skill_version showed no graduation since
// 2026-07-31 while drafts and trials kept being produced the same morning.
//
// The release is the SAME measurement that trips the halt, read the other way: judge-independent
// denominator, same minimum sample, higher fraction.

type rearmSpy struct {
	calls int
	err   error
}

func (r *rearmSpy) Rearm(context.Context) error { r.calls++; return r.err }

type haltSpy struct{ calls int }

func (h *haltSpy) Halt(context.Context, string) error { h.calls++; return nil }

func livenessFixture(judgedOf int, total int, halt *haltSpy, rearm *rearmSpy) (*JudgeLivenessMonitor, time.Time) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	sessions := make([]Session, 0, total)
	judged := map[string]bool{}
	for i := 0; i < total; i++ {
		id := string(rune('a' + i))
		sessions = append(sessions, Session{SessionID: id, EndedAt: now.Add(-6 * time.Hour)})
		if i < judgedOf {
			judged[id] = true
		}
	}
	m := &JudgeLivenessMonitor{
		Sessions:  fixtureSessions(sessions),
		Judgments: fixtureJudgments(judged),
		Window:    24 * time.Hour,
	}
	if halt != nil {
		m.Halt = halt
	}
	if rearm != nil {
		m.Rearm = rearm
	}
	return m, now
}

type fixtureSessions []Session

func (f fixtureSessions) RecentlyEnded(context.Context) ([]Session, error) { return f, nil }

type fixtureJudgments map[string]bool

func (f fixtureJudgments) HasRealJudgment(_ context.Context, id string) bool { return f[id] }

// KILLING MUTATION: delete the Rearm branch in Run (the pre-2026-08-06 state). RED — the halt is never
// released, which is precisely how the flywheel sat stopped for days.
func TestAProvenLiveJudgeReleasesTheHalt(t *testing.T) {
	rearm := &rearmSpy{}
	halt := &haltSpy{}
	m, now := livenessFixture(8, 8, halt, rearm) // fraction 1.00 over 8 eligible
	res, err := m.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rearm.calls != 1 {
		t.Errorf("a judge scoring %d of %d recent sessions did not release the halt (Rearm calls=%d) — "+
			"JudgeDeadMan.Rearm is the ONLY path back and nothing else in the tree calls it",
			res.Judged, res.Eligible, rearm.calls)
	}
	if !res.Rearmed {
		t.Error("the release is not reported in LivenessResult, so it is invisible in the Temporal run history")
	}
	if halt.calls != 0 {
		t.Errorf("a live judge was ALSO halted (%d calls) — the two branches must be exclusive", halt.calls)
	}
}

// The halt direction is unchanged. A fix that released the halt on a dead judge would pass the test above.
//
// KILLING MUTATION: make the Rearm branch unconditional. RED.
func TestADeadJudgeStillHaltsAndIsNotReleased(t *testing.T) {
	rearm := &rearmSpy{}
	halt := &haltSpy{}
	m, now := livenessFixture(1, 8, halt, rearm) // fraction 0.125 over 8 eligible
	res, err := m.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if halt.calls != 1 || !res.Halted {
		t.Fatalf("a judged fraction of %.3f did not halt accrual (halt calls=%d, Halted=%v)", res.Fraction, halt.calls, res.Halted)
	}
	if rearm.calls != 0 {
		t.Errorf("the halt was RELEASED on a dead judge (Rearm calls=%d) — recovery must require proof of "+
			"life, not merely a run of the monitor", rearm.calls)
	}
}

// NO DATA IS NOT PROOF OF LIFE. A judge writing nothing at all drives the eligible population to zero, and
// a zero-sample fraction of 0 must neither halt (too thin to page on, REQ-306) nor release.
//
// KILLING MUTATION: drop the `eligible > JudgeDeathMinSample` conjunct from the Rearm branch. RED — an
// empty population yields Fraction 0, which fails the fraction test, so make it `judged == eligible`
// instead and it re-arms on nothing at all.
func TestAThinOrEmptySampleNeitherHaltsNorReleases(t *testing.T) {
	for _, tc := range []struct {
		name          string
		judged, total int
	}{
		{"empty", 0, 0},
		{"thin-and-perfect", 3, 3},  // 100% judged but only 3 eligible — at the minimum, not above it
		{"thin-and-terrible", 0, 3}, // 0% judged, same population — too thin to page on
	} {
		t.Run(tc.name, func(t *testing.T) {
			rearm := &rearmSpy{}
			halt := &haltSpy{}
			m, now := livenessFixture(tc.judged, tc.total, halt, rearm)
			if _, err := m.Run(context.Background(), now); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rearm.calls != 0 {
				t.Errorf("a %d-session sample released the graduation halt — no data is a problem, not a pass", tc.total)
			}
			if halt.calls != 0 {
				t.Errorf("a %d-session sample halted accrual — below the minimum sample this must not page", tc.total)
			}
		})
	}
}

// HYSTERESIS. The release bar sits above the halt bar so a fraction hovering between them leaves the halt
// standing rather than flapping the graduation gate open and shut on alternate runs.
//
// KILLING MUTATION: set JudgeLifeFraction = JudgeDeathFraction. RED.
func TestAFractionBetweenTheThresholdsChangesNothing(t *testing.T) {
	if JudgeLifeFraction <= JudgeDeathFraction {
		t.Fatalf("the release bar (%.2f) must sit ABOVE the halt bar (%.2f) or the pair has no hysteresis",
			JudgeLifeFraction, JudgeDeathFraction)
	}
	rearm := &rearmSpy{}
	halt := &haltSpy{}
	m, now := livenessFixture(5, 8, halt, rearm) // 0.625: above 0.50, below 0.75
	res, err := m.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Fraction <= JudgeDeathFraction || res.Fraction >= JudgeLifeFraction {
		t.Fatalf("fixture is not in the dead band: fraction %.3f vs [%.2f, %.2f)", res.Fraction, JudgeDeathFraction, JudgeLifeFraction)
	}
	if halt.calls != 0 || rearm.calls != 0 {
		t.Errorf("a fraction of %.3f in the dead band moved the gate (halt=%d rearm=%d) — it must leave the "+
			"existing posture alone", res.Fraction, halt.calls, rearm.calls)
	}
}

// A release that cannot persist must surface, not be swallowed — the same fail-safe direction Halt has.
//
// KILLING MUTATION: ignore the error from Rearm. RED.
func TestAFailedReleaseIsReturned(t *testing.T) {
	boom := errors.New("store unavailable")
	rearm := &rearmSpy{err: boom}
	m, now := livenessFixture(8, 8, nil, rearm)
	if _, err := m.Run(context.Background(), now); !errors.Is(err, boom) {
		t.Errorf("a failed re-arm was swallowed (err=%v) — the caller would read a halt as released when it "+
			"is still standing", err)
	}
}

// Vacuity floor: the fixture must actually produce the populations these tests claim, or every assertion
// above is about a monitor that saw nothing.
func TestLivenessFixtureProducesTheClaimedPopulation(t *testing.T) {
	m, now := livenessFixture(6, 8, nil, nil)
	res, err := m.Run(context.Background(), now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Eligible != 8 || res.Judged != 6 {
		t.Fatalf("fixture produced eligible=%d judged=%d, want 8/6 — the other tests are measuring something else",
			res.Eligible, res.Judged)
	}
}
