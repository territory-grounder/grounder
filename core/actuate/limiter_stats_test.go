package actuate

// THE GOVERNOR'S SILENCE MUST STOP BEING AMBIGUOUS.
//
// The actuation frequency limiter has always been on the real path — interceptor.go calls Admit before
// every effect — and published nothing at all. For a rate governor that is a specific kind of blind: it is
// SUPPOSED to be quiet, so "has never needed to refuse" and "is admitting everything because its window is
// misconfigured" produce identical evidence, which is none.
//
// A leaked lease is the third invisible state and the worst: Admit without Release holds an in-flight slot
// forever, so the lane wedges and every other reading still looks healthy.

import (
	"testing"
	"time"
)

func TestStatsCountsAdmissionsAndRefusalsSeparately(t *testing.T) {
	fixed := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	l := NewActuationLimiter(func() time.Time { return fixed }).
		WithLimits(ActuationLimits{Window: time.Hour, SessionPerWindow: 1, TargetPerWindow: 9,
			SessionInFlight: 9, TargetInFlight: 9})

	if _, refusal := l.Admit("s1", "hostA"); refusal != "" {
		t.Fatalf("first actuation refused: %s", refusal)
	}
	if _, refusal := l.Admit("s1", "hostB"); refusal == "" {
		t.Fatal("the session budget of 1 did not refuse the second actuation — this test cannot prove " +
			"the refusal counter works if nothing is ever refused")
	}

	st := l.Stats()
	if st.Admitted != 1 {
		t.Errorf("Admitted = %d, want 1", st.Admitted)
	}
	if st.Refused != 1 {
		t.Errorf("Refused = %d, want 1. A governor that cannot report a refusal is indistinguishable "+
			"from one that never refuses, which is the state this instrumentation exists to end", st.Refused)
	}
	// The budget must be echoed, or the counts above are unreadable: "3 refusals" means nothing without
	// knowing whether the cap was 1 or 100.
	if st.Window != time.Hour || st.SessionPerWindow != 1 {
		t.Errorf("the budget was not echoed alongside the counts: %+v", st)
	}
}

// In-flight must count each live actuation ONCE. Admit charges both the session and target scopes, so a
// naive sum over the in-flight map double-counts and a reader chasing a leaked lease chases a bookkeeping
// artefact instead.
func TestInFlightCountsEachActuationOnceNotOncePerScope(t *testing.T) {
	fixed := time.Date(2026, time.August, 5, 12, 0, 0, 0, time.UTC)
	l := NewActuationLimiter(func() time.Time { return fixed }).
		WithLimits(ActuationLimits{Window: time.Hour, SessionPerWindow: 9, TargetPerWindow: 9,
			SessionInFlight: 9, TargetInFlight: 9})

	lease, refusal := l.Admit("s1", "hostA")
	if refusal != "" {
		t.Fatalf("admit refused: %s", refusal)
	}
	if got := l.Stats().InFlight; got != 1 {
		t.Fatalf("InFlight = %d for ONE live actuation, want 1. Admit charges both the session and the "+
			"target scope, so summing the map without halving reports double and makes a real leaked "+
			"lease indistinguishable from normal operation.", got)
	}
	lease.Release()
	if got := l.Stats().InFlight; got != 0 {
		t.Errorf("InFlight = %d after Release, want 0 — a lease that does not decrement is a leak, and "+
			"this gauge is the only thing that would show it", got)
	}
}

// A nil limiter must report zeroes rather than panic: /metrics is scraped on a schedule and must never be
// the thing that takes the worker down.
func TestStatsOnANilLimiterDoesNotPanic(t *testing.T) {
	var l *ActuationLimiter
	if got := l.Stats(); got.Admitted != 0 || got.Refused != 0 || got.InFlight != 0 {
		t.Errorf("a nil limiter reported %+v, want zeroes", got)
	}
}
