package gate

import (
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
