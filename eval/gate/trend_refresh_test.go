package gate

import (
	"strings"
	"testing"
	"time"
)

// TG-424: the trend-watch self-refresh wedged the committed anchor stale forever. It refreshed ONLY on a
// non-regressing run, but after the opus-cc->mistral model swap every clean nightly read as a "regression" vs
// the pre-swap anchor (a cross-model comparison), so it never refreshed — the anchor sat 9 days stale and the
// baseline-freshness dead-man went red on every commit. ShouldRefreshTrend re-anchors past a STALE committed
// anchor even on a regression verdict (a stale anchor is an invalid comparator whose "regression" cannot be
// told from drift/a swap), while still refusing to refresh a FRESH anchor on a real regression.
//
// Killing mutation: drop the trendAnchorStale re-anchor branch (return no-refresh on every regression) → the
// stale+regression case goes RED (the anchor stays wedged, the reported bug).
func TestShouldRefreshTrend(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	stale := "2026-07-30" // 9 days < now, past the 8-day TrendMaxStaleness
	fresh := "2026-08-07" // 1 day, within the window

	cases := []struct {
		name       string
		outcome    Outcome
		measuredAt string
		want       bool
	}{
		{"pass refreshes regardless of age", OutcomePass, stale, true},
		{"pass on a fresh anchor refreshes", OutcomePass, fresh, true},
		{"inconclusive never refreshes (even stale)", OutcomeInconclusive, stale, false},
		{"regression on a FRESH anchor does NOT refresh (never hide a regression)", OutcomeFail, fresh, false},
		{"regression on a STALE anchor RE-ANCHORS (TG-424: unstick the invalid comparator)", OutcomeFail, stale, true},
		{"an unparseable anchor date is treated as stale → re-anchor", OutcomeFail, "not-a-date", true},
	}
	for _, c := range cases {
		got, reason := ShouldRefreshTrend(c.outcome, c.measuredAt, now)
		if got != c.want {
			t.Errorf("%s: ShouldRefreshTrend(%v, %q) = %v, want %v (reason: %s)", c.name, c.outcome, c.measuredAt, got, c.want, reason)
		}
		if reason == "" {
			t.Errorf("%s: reason must never be empty — the log must say WHY the anchor did or did not move", c.name)
		}
	}
}

// Either side of TrendMaxStaleness (8 days): a regression on an anchor comfortably WITHIN the window still
// gates refresh; one comfortably PAST it re-anchors. Dates are day-granular (measured_at carries no time), so
// these are chosen clear of the exact edge to stay deterministic.
func TestTrendStalenessBoundary(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	within := now.Add(-6 * 24 * time.Hour).Format("2006-01-02") // ~6 days < 8
	past := now.Add(-10 * 24 * time.Hour).Format("2006-01-02")  // ~10 days > 8

	if r, _ := ShouldRefreshTrend(OutcomeFail, within, now); r {
		t.Errorf("a regression on an anchor within TrendMaxStaleness must NOT re-anchor (got refresh=true)")
	}
	if r, _ := ShouldRefreshTrend(OutcomeFail, past, now); !r {
		t.Errorf("a regression on an anchor past TrendMaxStaleness must re-anchor (got refresh=false)")
	}
}

// TG-595: a rubric-version crossing RE-ANCHORS instead of grading. The re-anchored verdict certifies nothing (Pass
// false), keeps the graded deltas only as information, refreshes the anchor, and the refreshed baseline's provenance
// names the crossing and the new rubric version.
func TestReanchorOnRubricCrossing(t *testing.T) {
	v := Verdict{Outcome: OutcomeFail, Reasons: []string{"overall Δ -0.16 < -0.15", "SAFETY dim appropriate_band Δ -1.09"}}
	r := Reanchor(v, "2026-08-04.3", "2026-10-23.1")
	if r.Outcome != OutcomeReanchored || r.Pass || len(r.Reasons) != 3 || !strings.Contains(r.Reasons[0], "2026-08-04.3 → 2026-10-23.1") || !strings.HasPrefix(r.Reasons[1], "(not graded — instrument change) overall") {
		t.Fatalf("re-anchor shape: %+v", r)
	}
	refresh, reason := ShouldRefreshTrend(OutcomeReanchored, "2026-01-01", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	if !refresh || !strings.Contains(reason, "rubric version crossed") {
		t.Fatalf("a re-anchored run must refresh the anchor: %v %q", refresh, reason)
	}
	nb := BuildRefreshedBaseline([]Scorecard{{N: 36, Overall: 3.0, RubricVersion: "2026-10-23.1", DimMeans: map[string]float64{"appropriate_band": 3}}}, "deadbeef", "2026-10-23", OutcomeReanchored)
	if !strings.Contains(nb.Provenance, "RUBRIC VERSION CROSSING") || !strings.Contains(nb.Provenance, `"2026-10-23.1"`) || nb.Scorecard.RubricVersion != "2026-10-23.1" {
		t.Fatalf("the refreshed anchor must name the crossing and carry the new version: %+v", nb)
	}
	// Unchanged: the same-instrument regression never refreshes a fresh anchor.
	if refresh, _ := ShouldRefreshTrend(OutcomeFail, "2026-10-20", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)); refresh {
		t.Fatal("a same-instrument regression against a fresh anchor must not refresh")
	}
}

// A crossing re-anchor is allowed only when every ABSOLUTE bar held: a control violation, a failed floor or an
// inconclusive run each refuse it with the reason named; a pure relative-delta failure is eligible.
func TestReanchorEligibleIsAnAllowList(t *testing.T) {
	ok, why := ReanchorEligible(Verdict{Outcome: OutcomeFail, ControlPass: true, Rates: []RateResult{{Name: "proposal_recall", Candidate: 1, Floor: 0.5, Pass: true}}})
	if !ok || why != "" {
		t.Fatalf("a relative-only failure is eligible: %v %q", ok, why)
	}
	if ok, why := ReanchorEligible(Verdict{Outcome: OutcomeFail, ControlPass: false}); ok || !strings.Contains(why, "negative control") {
		t.Fatalf("a control violation refuses: %v %q", ok, why)
	}
	if ok, why := ReanchorEligible(Verdict{Outcome: OutcomeFail, ControlPass: true, Rates: []RateResult{{Name: "proposal_recall", Candidate: 0.2, Floor: 0.5}}}); ok || !strings.Contains(why, "proposal_recall") {
		t.Fatalf("a failed floor refuses and names it: %v %q", ok, why)
	}
	if ok, why := ReanchorEligible(Verdict{Outcome: OutcomeInconclusive, ControlPass: true}); ok || !strings.Contains(why, "INCONCLUSIVE") {
		t.Fatalf("an inconclusive run refuses: %v %q", ok, why)
	}
}
