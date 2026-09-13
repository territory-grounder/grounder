package regime

// TG-122 slice 2 — per-lane poller/bound selection on the global deferred-verify channel. The channel stays
// single (one store, one Reserve discipline); only the OBSERVATION differs per tenant, keyed on the Lane the
// record already carries durably.

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/verify"
)

func reserveOn(t *testing.T, av *AsyncVerify, actionID string, lane Regime) {
	t.Helper()
	if err := av.Reserve(context.Background(), LaunchIntent{
		ActionID: actionID, OpClass: "restart-service", Lane: lane, Prediction: verify.Prediction{},
	}); err != nil {
		t.Fatalf("reserve %s: %v", actionID, err)
	}
	if err := av.BindHandle(context.Background(), actionID, "h-"+actionID); err != nil {
		t.Fatalf("bind %s: %v", actionID, err)
	}
}

// KILLING MUTATION: consult a.poller instead of pollerFor(rec.Lane) at the poll site → the gitops record
// polls the awx fake and this reddens.
func TestVerifyRoutesPollByLane(t *testing.T) {
	base := NewMemJobPoller().Script("h-awx1", JobRunning)
	gitops := NewMemJobPoller().Script("h-git1", JobFailed)
	av, err := NewAsyncVerify(NewMemPendingStore(), base,
		WithLanePoller(RegimeGitOpsMR, gitops))
	if err != nil {
		t.Fatal(err)
	}
	reserveOn(t, av, "awx1", RegimeAWXJob)
	reserveOn(t, av, "git1", RegimeGitOpsMR)

	// The awx record polls the channel-wide (base) poller: running, stays pending.
	res, err := av.Verify(context.Background(), "awx1")
	if err != nil || res.State != StatePending {
		t.Fatalf("awx1 via base poller: %+v %v (want pending)", res, err)
	}
	// The gitops record polls ITS lane poller: failed terminal (a rejected MR is never a clean run).
	res, err = av.Verify(context.Background(), "git1")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateVerified || res.TerminalStatus != JobFailed || res.CleanRun {
		t.Fatalf("git1 via lane poller: %+v (want verified terminal failed, no clean run)", res)
	}
}

// A lane with NO registered poller and an erroring base stays pending within the bound and resolves
// `unverified` past it — never a fabricated terminal.
func TestLaneWithoutPollerResolvesUnverifiedAtItsBound(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	base := NewMemJobPoller()
	base.Fail("h-git2", context.DeadlineExceeded)
	av, err := NewAsyncVerify(NewMemPendingStore(), base,
		WithClock(clock),
		WithVerificationBound(10*time.Minute),
		WithLaneVerificationBound(RegimeGitOpsMR, 72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	reserveOn(t, av, "git2", RegimeGitOpsMR)

	// Past the CHANNEL bound but inside the LANE bound: still pending — the gitops lane legitimately rides
	// a human review cycle. KILLING MUTATION: use a.bound instead of boundFor(rec.Lane) → this resolves
	// unverified at 11m and reddens.
	now = now.Add(11 * time.Minute)
	if res, _ := av.Verify(context.Background(), "git2"); res.State != StatePending {
		t.Fatalf("inside the lane bound the record must stay pending, got %+v", res)
	}
	// Past the LANE bound: unverified, visible, no clean run.
	now = now.Add(73 * time.Hour)
	res, err := av.Verify(context.Background(), "git2")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateUnverified || res.CleanRun {
		t.Fatalf("past the lane bound the record must resolve unverified, got %+v", res)
	}
}

// Options are additive-only: nil/invalid registrations leave the safe defaults.
func TestLaneOptionsIgnoreInvalidRegistrations(t *testing.T) {
	base := NewMemJobPoller().Script("h-a1", JobRunning)
	av, err := NewAsyncVerify(NewMemPendingStore(), base,
		WithLanePoller(RegimeAWXJob, nil),
		WithLanePoller(Regime("bogus"), NewMemJobPoller()),
		WithLaneVerificationBound(RegimeAWXJob, -time.Minute),
		WithLaneVerificationBound(Regime("bogus"), time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	reserveOn(t, av, "a1", RegimeAWXJob)
	if res, err := av.Verify(context.Background(), "a1"); err != nil || res.State != StatePending {
		t.Fatalf("invalid options must leave the channel-wide defaults intact, got %+v %v", res, err)
	}
}
