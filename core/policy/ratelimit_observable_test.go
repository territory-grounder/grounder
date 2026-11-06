package policy

import (
	"testing"
	"time"
)

// TG-339. The policy rate governor was DARK for its whole life — `WithRateGovernor` had one caller and it
// was a spec acceptance test, so `rateGov` was nil in every production worker while
// core/policy/templates/conservative.json advertised `"rate_limit": 30`. It is wired now
// (cmd/worker/main.go, pinned by policy_rate_governor_wired_test.go) and published NOTHING, so there was
// still no way to answer from outside the process whether it had ever governed anything.
//
// The counter that matters is `ungoverned`, and these oracles exist mostly to defend it. Counting only
// clamps cannot distinguish "nothing needed clamping" from "no limit was ever in force" — and the second
// is the state this control was actually in.

// Named distinctly: core/policy already has a fixedClock() with a different signature. Six separate
// duplicate-test-helper collisions in one day across this repo, one of which reddened main.
func atClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// TestAnUngovernedCallIsCountedSeparately is the finding. A limit that never reaches clamp() leaves the
// verdict untouched, and a clamp-only counter reports that as health.
func TestAnUngovernedCallIsCountedSeparately(t *testing.T) {
	g := NewRateGovernor(atClock(time.Now()))

	for i := 0; i < 5; i++ {
		if v, _ := g.Clamp(VerdictAuto, "restart-service", 0); v != VerdictAuto {
			t.Fatalf("an unset limit changed the verdict to %q — the governor must be a no-op there", v)
		}
	}

	st := g.Stats()
	if st.Ungoverned != 5 {
		t.Errorf("ungoverned=%d after 5 calls with rate_limit=0, want 5. Without this counter, a template "+
			"that advertises a rate_limit which never reaches the governor is INDISTINGUISHABLE from a "+
			"governor with nothing to clamp — which is exactly how this control stayed dark.", st.Ungoverned)
	}
	if st.Governed != 0 {
		t.Errorf("governed=%d — no limit was in force, so nothing was governed", st.Governed)
	}
	if st.Clamped != 0 {
		t.Errorf("clamped=%d — an ungoverned call must never count as a clamp", st.Clamped)
	}
}

// TestAdmittedAndClampedAreBothCounted pins the governed path: the denominator moves on every governed
// call, not only on the ones that clamp.
func TestAdmittedAndClampedAreBothCounted(t *testing.T) {
	g := NewRateGovernor(atClock(time.Now()))
	const limit = 2

	// Two admitted, then two clamped (the window never advances under a fixed clock).
	for i := 0; i < 4; i++ {
		g.Clamp(VerdictAuto, "restart-service", limit)
	}

	st := g.Stats()
	if st.Governed != 4 {
		t.Errorf("governed=%d after 4 governed calls, want 4 — the denominator must count admissions AND "+
			"clamps, or a clamp rate cannot be computed", st.Governed)
	}
	if st.Clamped != 2 {
		t.Errorf("clamped=%d, want 2 (calls 3 and 4 exceeded the cap of %d)", st.Clamped, limit)
	}
	if st.Ungoverned != 0 {
		t.Errorf("ungoverned=%d — a positive limit was in force on every call", st.Ungoverned)
	}
}

// TestANonAutoVerdictIsNotCountedAsGoverned. approve/deny are already at or above the human bar and are
// never charged; counting them would inflate the denominator and make the clamp rate meaningless.
func TestANonAutoVerdictIsNotCountedAsGoverned(t *testing.T) {
	g := NewRateGovernor(atClock(time.Now()))

	g.Clamp(VerdictApprove, "restart-service", 5)
	g.Clamp(VerdictDeny, "restart-service", 5)

	st := g.Stats()
	if st.Governed != 0 || st.Clamped != 0 {
		t.Errorf("governed=%d clamped=%d — a non-auto verdict is never governed and never charged, so it "+
			"must not move either counter", st.Governed, st.Clamped)
	}
}

// TestANilGovernorReportsZerosWithoutPanicking. main() may legitimately have no governor (no pool ⇒ no
// engine), and Stats is called from the metrics path on every scrape.
func TestANilGovernorReportsZerosWithoutPanicking(t *testing.T) {
	var g *RateGovernor
	st := g.Stats()
	if st.Governed != 0 || st.Ungoverned != 0 || st.Clamped != 0 {
		t.Errorf("a nil governor reported %+v, want zeros", st)
	}
}

// TestTheWindowIsReported — the counts above are meaningless without their denominator's denominator. A
// governor silently running a 1-second window would clamp almost nothing and look healthy.
func TestTheWindowIsReported(t *testing.T) {
	g := NewRateGovernor(atClock(time.Now())).WithWindow(90 * time.Second)
	if got := g.Stats().Window; got != 90*time.Second {
		t.Errorf("Stats().Window = %v, want 90s — without the window published, a misconfigured budget is "+
			"invisible: the counts look calm because the window is too short to accumulate anything", got)
	}
}
