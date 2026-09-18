package actuate

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/trace"
)

// TestAdmitReportsRateBudgetHeadroom is the TG-178 computation oracle for the actuation-limit gate margin: each
// admitted lease carries the tightest RATE-budget slack that remained AFTER it was charged — the minimum, over
// the session and target scopes, of (per-window cap − trailing-window count). It deliberately does NOT track
// the in-flight concurrency cap (a binary mutex, always zero slack on the pass path), so with a small window
// budget the headroom counts DOWN to zero as the window fills, and zero is the last actuation before the throttle.
//
// Killing mutations: change the `-1` in Admit to `-0` (off by one) → the sequence reads 3,2,1 not 2,1,0 → RED;
// compute the margin from the in-flight caps instead of the per-window caps → the sequence stops counting down → RED.
func TestAdmitReportsRateBudgetHeadroom(t *testing.T) {
	l := NewActuationLimiter(nil).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 3, TargetPerWindow: 5, SessionInFlight: 1, TargetInFlight: 1,
	})
	// The session budget (3) is tighter than the target budget (5), so it binds the headroom throughout.
	want := []int{2, 1, 0}
	for i, w := range want {
		lease, refusal := l.Admit("sess-A", "web01")
		if refusal != "" {
			t.Fatalf("admit %d: unexpected refusal %q", i+1, refusal)
		}
		if lease.headroom != w {
			t.Fatalf("admit %d: rate-budget headroom = %d, want %d (min(session %d-%d-1, target %d-%d-1))",
				i+1, lease.headroom, w, 3, i, 5, i)
		}
		lease.Release() // frees the in-flight slot; does NOT refund the window count, so the budget keeps filling
	}
	// The fourth admission has spent the session per-window budget (3/3) and must be throttled, not admitted.
	if lease, refusal := l.Admit("sess-A", "web01"); refusal == "" {
		lease.Release()
		t.Fatal("the fourth admission exhausted the session per-window budget and must be refused, not admitted")
	}
}

// TestActuationLimitGateEmitsRateBudgetMargin is the TG-178 REACHABILITY oracle: the interceptor's actuation-limit
// gate must actually STAMP the lease's rate-budget headroom onto its observe-only verdict row, so the boundary
// case is visible on the trail rather than merely computed. A first admission against a 3-per-window session
// budget leaves headroom 2 (min(3-0-1, 5-0-1)).
//
// Killing mutation: revert the interceptor's actuation-limit emit from emitGateMargin back to plain emitGate
// (drop the margin) → the row's Margin is nil → RED. This is the "implemented but unreachable" guard: the
// computation oracle above still passes while the wiring is dead.
func TestActuationLimitGateEmitsRateBudgetMargin(t *testing.T) {
	act := &fakeActuator{}
	sink := &marginCaptureSink{}
	limiter := NewActuationLimiter(nil).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 3, TargetPerWindow: 5, SessionInFlight: 1, TargetInFlight: 1,
	})
	i := wired(safety.NewActuatingChokepoint(), act).
		WithActuationLimiter(limiter).
		WithGateVerdictSink(sink)
	out, err := i.Do(context.Background(), goodRequest(t))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Refused {
		t.Fatalf("a good request within budget must not be refused: %+v", out)
	}
	var row *trace.GateVerdict
	for idx := range sink.rows {
		if sink.rows[idx].Gate == "actuation-limit" {
			row = &sink.rows[idx]
		}
	}
	if row == nil {
		t.Fatal("no actuation-limit gate row was emitted")
	}
	if row.Verdict != "pass" {
		t.Fatalf("actuation-limit verdict = %q, want pass", row.Verdict)
	}
	if row.Margin == nil {
		t.Fatal("the actuation-limit gate must stamp its rate-budget margin on the verdict trail (TG-178)")
	}
	if *row.Margin != 2 {
		t.Fatalf("actuation-limit margin = %v, want 2 (rate-budget slack after the first admission)", *row.Margin)
	}
}
