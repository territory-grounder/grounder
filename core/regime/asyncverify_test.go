package regime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// prediction builds a committed launch prediction naming a target host (its own alerting is the expected
// direct effect) and, optionally, a predicted cascade host.
func prediction(target string, cascade ...string) verify.Prediction {
	hosts := map[string]struct{}{}
	for _, h := range cascade {
		hosts[h] = struct{}{}
	}
	return verify.Prediction{
		ActionID:       "action-pred",
		PlanHash:       "plan#1",
		TargetHost:     target,
		Site:           "nl",
		PredictedHosts: hosts,
		PredictedRules: map[string]struct{}{},
	}
}

func newChannel(t *testing.T, opts ...Option) (*AsyncVerify, *MemPendingStore, *MemJobPoller, *MemGraduationSink) {
	t.Helper()
	store := NewMemPendingStore()
	poller := NewMemJobPoller()
	grad := NewMemGraduationSink()
	base := append([]Option{WithGraduationSink(grad)}, opts...)
	av, err := NewAsyncVerify(store, poller, base...)
	if err != nil {
		t.Fatalf("NewAsyncVerify: %v", err)
	}
	return av, store, poller, grad
}

// TestConstructorFailsLoudWhenUnwired proves a channel that cannot verify refuses at construction rather than
// leaving a launch silently unverified.
func TestConstructorFailsLoudWhenUnwired(t *testing.T) {
	if _, err := NewAsyncVerify(nil, NewMemJobPoller()); !errors.Is(err, ErrChannelUnwired) {
		t.Fatalf("nil store must fail loud with ErrChannelUnwired, got %v", err)
	}
	if _, err := NewAsyncVerify(NewMemPendingStore(), nil); !errors.Is(err, ErrChannelUnwired) {
		t.Fatalf("nil poller must fail loud with ErrChannelUnwired, got %v", err)
	}
}

// TestLaunchIsPredictionNotSuccess proves REQ-1709/1710: a reserved+bound launch is StatePending and is NOT a
// clean run; it becomes a clean verified run ONLY when the deferred verify polls the job to a `successful`
// terminal and the mechanical verdict is `match`.
func TestLaunchIsPredictionNotSuccess(t *testing.T) {
	ctx := context.Background()
	av, store, poller, grad := newChannel(t)

	intent := LaunchIntent{ActionID: "a1", OpClass: "restart-service", Lane: RegimeAWXJob, Prediction: prediction("web01")}
	if err := av.Reserve(ctx, intent); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := av.BindHandle(ctx, "a1", "job-77"); err != nil {
		t.Fatalf("BindHandle: %v", err)
	}

	// At launch time the record is pending — the handle is a prediction, not a success.
	rec, _ := store.Get(ctx, "a1")
	if rec.State != StatePending || rec.JobID != "job-77" {
		t.Fatalf("after bind: want pending with job handle, got %+v", rec)
	}
	if rec.CleanRun() {
		t.Fatal("a bound-but-unverified launch must NOT be a clean run (launch is a prediction)")
	}

	// The job is still running: a Verify step keeps it pending — still no clean run, nothing fed to graduation.
	poller.Script("job-77", JobRunning, JobRunning, JobSuccessful)
	res, err := av.Verify(ctx, "a1")
	if err != nil {
		t.Fatalf("Verify(running): %v", err)
	}
	if res.State != StatePending || res.CleanRun || res.GradFed {
		t.Fatalf("a running job must stay pending, not clean, not fed: %+v", res)
	}
	if len(grad.Recorded()) != 0 {
		t.Fatalf("no graduation evidence may be fed before terminal, got %+v", grad.Recorded())
	}

	// It reaches `successful`: NOW it is a verified clean run and the verdict is fed to the graduation ladder.
	res, err = av.Verify(ctx, "a1")
	if err != nil {
		t.Fatalf("Verify(running #2): %v", err) // second poll still running per script
	}
	if res.State != StatePending {
		t.Fatalf("second poll should still be running/pending per script, got %+v", res)
	}
	res, err = av.Verify(ctx, "a1")
	if err != nil {
		t.Fatalf("Verify(successful): %v", err)
	}
	if res.State != StateVerified || res.TerminalStatus != JobSuccessful {
		t.Fatalf("want verified/successful, got %+v", res)
	}
	if res.Verdict != safety.VerdictMatch || !res.Verified || !res.CleanRun {
		t.Fatalf("a successful job with no observed cascade is a clean match, got %+v", res)
	}
	feeds := grad.Recorded()
	if len(feeds) != 1 || feeds[0].OpClass != "restart-service" || feeds[0].Verdict != safety.VerdictMatch || !feeds[0].Verified {
		t.Fatalf("exactly one clean verdict must be fed to graduation, got %+v", feeds)
	}
}

// TestIdempotencyRefusesSecondLaunch proves REQ-1712: a second Reserve for an action_id that already carries a
// launch is refused (ErrDuplicateLaunch), whether the first is still live OR already terminal — so a retry /
// re-poll / redelivery never double-actuates.
func TestIdempotencyRefusesSecondLaunch(t *testing.T) {
	ctx := context.Background()
	av, _, poller, _ := newChannel(t)
	intent := LaunchIntent{ActionID: "dup", OpClass: "restart-service", Lane: RegimeAWXJob, Prediction: prediction("web01")}

	if err := av.Reserve(ctx, intent); err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	// live duplicate.
	if err := av.Reserve(ctx, intent); !errors.Is(err, ErrDuplicateLaunch) {
		t.Fatalf("a second live launch must refuse ErrDuplicateLaunch, got %v", err)
	}
	// drive the first to a terminal, then a duplicate is STILL refused (a terminal action_id never relaunches).
	_ = av.BindHandle(ctx, "dup", "job-dup")
	poller.Script("job-dup", JobSuccessful)
	if _, err := av.Verify(ctx, "dup"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := av.Reserve(ctx, intent); !errors.Is(err, ErrDuplicateLaunch) {
		t.Fatalf("a second launch for a TERMINAL action_id must still refuse, got %v", err)
	}
}

// TestBindHandleBoundExactlyOnce proves a launch's handle is recorded exactly once — a second bind (which would
// imply a second launch) is refused.
func TestBindHandleBoundExactlyOnce(t *testing.T) {
	ctx := context.Background()
	av, _, _, _ := newChannel(t)
	_ = av.Reserve(ctx, LaunchIntent{ActionID: "b1", OpClass: "oc", Lane: RegimeAWXJob, Prediction: prediction("h")})
	if err := av.BindHandle(ctx, "b1", "job-1"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := av.BindHandle(ctx, "b1", "job-2"); !errors.Is(err, ErrHandleAlreadyBound) {
		t.Fatalf("a second bind must refuse ErrHandleAlreadyBound, got %v", err)
	}
	// binding an unreserved action fails closed (a handle can never invent a launch).
	if err := av.BindHandle(ctx, "never", "job-x"); !errors.Is(err, ErrNoPending) {
		t.Fatalf("binding an unreserved action must fail ErrNoPending, got %v", err)
	}
}

// TestNonTerminatingVerifyFailsSafe proves REQ-1711: a deferred verify that never terminates fails safe. Past
// the operator bound the launch is recorded UNVERIFIED — visible in the escalation queue, fed to graduation as
// a NON-clean run, and never a silent success.
func TestNonTerminatingVerifyFailsSafe(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	av, _, poller, grad := newChannel(t, WithVerificationBound(10*time.Minute), WithClock(clock.now))

	_ = av.Reserve(ctx, LaunchIntent{ActionID: "stuck", OpClass: "reboot", Lane: RegimeAWXJob, Prediction: prediction("web01")})
	_ = av.BindHandle(ctx, "stuck", "job-stuck")
	poller.Script("job-stuck", JobRunning) // never terminates

	// within the bound: stays pending (still no clean run).
	clock.advance(5 * time.Minute)
	res, err := av.Verify(ctx, "stuck")
	if err != nil {
		t.Fatalf("Verify(within bound): %v", err)
	}
	if res.State != StatePending || res.CleanRun {
		t.Fatalf("within the bound a non-terminal job stays pending, got %+v", res)
	}
	// It is VISIBLE in the pending-verification queue.
	if q, _ := av.PendingQueue(ctx); len(q) != 1 || q[0].ActionID != "stuck" {
		t.Fatalf("the launch must be visible in the pending queue, got %+v", q)
	}

	// past the bound: recorded unverified (fail-safe), never a silent success.
	clock.advance(6 * time.Minute) // now 11m > 10m bound
	res, err = av.Verify(ctx, "stuck")
	if err != nil {
		t.Fatalf("Verify(past bound): %v", err)
	}
	if res.State != StateUnverified || res.CleanRun || res.Verified {
		t.Fatalf("past the bound the job must be unverified and never clean, got %+v", res)
	}
	// visible for escalation, not in the live pending queue.
	if u, _ := av.Unverified(ctx); len(u) != 1 || u[0].ActionID != "stuck" {
		t.Fatalf("the timed-out launch must be visible in the unverified/escalation queue, got %+v", u)
	}
	if q, _ := av.PendingQueue(ctx); len(q) != 0 {
		t.Fatalf("a resolved launch must leave the pending queue, got %+v", q)
	}
	// fed to graduation as a NON-clean run (verified==false → the ladder never promotes on it).
	feeds := grad.Recorded()
	if len(feeds) != 1 || feeds[0].Verified {
		t.Fatalf("an unverified run must feed graduation as not-verified, got %+v", feeds)
	}
}

// TestFailedJobIsDeviation proves a failed/error terminal is a VERIFIED deviation — a failed mutation is never
// a clean run and demotes the op-class (fed verified==true, deviation).
func TestFailedJobIsDeviation(t *testing.T) {
	ctx := context.Background()
	for _, status := range []JobStatus{JobFailed, JobError} {
		av, _, poller, grad := newChannel(t)
		id := "f-" + string(status)
		_ = av.Reserve(ctx, LaunchIntent{ActionID: id, OpClass: "oc", Lane: RegimeAWXJob, Prediction: prediction("web01")})
		_ = av.BindHandle(ctx, id, "job-"+id)
		poller.Script("job-"+id, status)
		res, err := av.Verify(ctx, id)
		if err != nil {
			t.Fatalf("Verify(%s): %v", status, err)
		}
		if res.State != StateVerified || res.Verdict != safety.VerdictDeviation || res.CleanRun {
			t.Fatalf("%s must be a verified deviation, never a clean run, got %+v", status, res)
		}
		if f := grad.Recorded(); len(f) != 1 || f[0].Verdict != safety.VerdictDeviation || !f[0].Verified {
			t.Fatalf("%s must feed a verified deviation to graduation, got %+v", status, f)
		}
	}
}

// TestCanceledJobIsIndeterminate proves a canceled terminal is fail-safe UNVERIFIED (indeterminate post-state):
// no verdict, not clean, fed as not-verified so it neither promotes nor demotes.
func TestCanceledJobIsIndeterminate(t *testing.T) {
	ctx := context.Background()
	av, _, poller, grad := newChannel(t)
	_ = av.Reserve(ctx, LaunchIntent{ActionID: "c1", OpClass: "oc", Lane: RegimeAWXJob, Prediction: prediction("web01")})
	_ = av.BindHandle(ctx, "c1", "job-c1")
	poller.Script("job-c1", JobCanceled)
	res, err := av.Verify(ctx, "c1")
	if err != nil {
		t.Fatalf("Verify(canceled): %v", err)
	}
	if res.State != StateVerified || res.Verdict != "" || res.Verified || res.CleanRun {
		t.Fatalf("a canceled job must be indeterminate (no verdict, not verified, not clean), got %+v", res)
	}
	if f := grad.Recorded(); len(f) != 1 || f[0].Verified {
		t.Fatalf("a canceled job must feed graduation as not-verified, got %+v", f)
	}
}

// TestSuccessfulWithSurpriseCascadeIsDeviation proves the deferred verdict is the REAL spec/002 mechanical
// verdict: a `successful` job whose observed cascade surprises the prediction is a deviation, not a clean run.
func TestSuccessfulWithSurpriseCascadeIsDeviation(t *testing.T) {
	ctx := context.Background()
	observe := func(context.Context, verify.Prediction) []verify.ObservedAlert {
		return []verify.ObservedAlert{{Host: "db99", Rule: "down", Site: "nl"}} // unpredicted surprise host
	}
	av, _, poller, _ := newChannel(t, WithObserver(observe))
	_ = av.Reserve(ctx, LaunchIntent{ActionID: "s1", OpClass: "oc", Lane: RegimeAWXJob, Prediction: prediction("web01")})
	_ = av.BindHandle(ctx, "s1", "job-s1")
	poller.Script("job-s1", JobSuccessful)
	res, err := av.Verify(ctx, "s1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Verdict != safety.VerdictDeviation || res.CleanRun {
		t.Fatalf("a successful job with a surprise cascade must be a deviation, not clean, got %+v", res)
	}
}

// TestVerifyIsIdempotent proves REQ-1712 (re-poll safety): once terminal, re-running Verify returns the stored
// resolution and never re-polls or re-feeds graduation.
func TestVerifyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	av, _, poller, grad := newChannel(t)
	_ = av.Reserve(ctx, LaunchIntent{ActionID: "i1", OpClass: "oc", Lane: RegimeAWXJob, Prediction: prediction("web01")})
	_ = av.BindHandle(ctx, "i1", "job-i1")
	poller.Script("job-i1", JobSuccessful)
	if _, err := av.Verify(ctx, "i1"); err != nil {
		t.Fatalf("Verify #1: %v", err)
	}
	for i := 0; i < 3; i++ {
		res, err := av.Verify(ctx, "i1")
		if err != nil {
			t.Fatalf("Verify re-poll: %v", err)
		}
		if res.State != StateVerified || res.GradFed {
			t.Fatalf("a re-poll must return the stored resolution and re-feed nothing, got %+v", res)
		}
	}
	if len(grad.Recorded()) != 1 {
		t.Fatalf("graduation must be fed exactly once across re-polls, got %+v", grad.Recorded())
	}
}

// TestReserveRefusesSynchronousLane proves the deferred channel refuses a synchronously-observable lane
// (native-ssh): its effect is verified inline by the spec/013 interceptor, not here.
func TestReserveRefusesSynchronousLane(t *testing.T) {
	ctx := context.Background()
	av, _, _, _ := newChannel(t)
	err := av.Reserve(ctx, LaunchIntent{ActionID: "n1", OpClass: "oc", Lane: RegimeNativeSSH, Prediction: prediction("web01")})
	if !errors.Is(err, ErrSynchronousLane) {
		t.Fatalf("native-ssh (synchronous) must refuse ErrSynchronousLane, got %v", err)
	}
}

// TestTransientPollErrorStaysPending proves a transient poll failure within the bound leaves the launch pending
// and never fabricates a terminal outcome.
func TestTransientPollErrorStaysPending(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	av, _, poller, grad := newChannel(t, WithVerificationBound(10*time.Minute), WithClock(clock.now))
	_ = av.Reserve(ctx, LaunchIntent{ActionID: "e1", OpClass: "oc", Lane: RegimeAWXJob, Prediction: prediction("web01")})
	_ = av.BindHandle(ctx, "e1", "job-e1")
	poller.Fail("job-e1", errors.New("awx unreachable"))
	res, err := av.Verify(ctx, "e1")
	if err == nil {
		t.Fatal("a transient poll error within the bound must surface an error for retry")
	}
	if res.State != StatePending {
		t.Fatalf("a transient poll error must leave the launch pending, got %+v", res)
	}
	if len(grad.Recorded()) != 0 {
		t.Fatal("a transient poll error must not fabricate a graduation feed")
	}
	// past the bound, an unreadable job fails safe to unverified.
	clock.advance(11 * time.Minute)
	res, err = av.Verify(ctx, "e1")
	if err != nil {
		t.Fatalf("Verify(past bound): %v", err)
	}
	if res.State != StateUnverified {
		t.Fatalf("past the bound an unreadable job must be unverified, got %+v", res)
	}
}

// TestMemPendingStoreIdempotency proves the store fake enforces the same fail-closed idempotency as the durable
// store: a duplicate Insert is refused and a transition on an unknown action fails closed.
func TestMemPendingStoreIdempotency(t *testing.T) {
	ctx := context.Background()
	s := NewMemPendingStore()
	rec := PendingVerification{ActionID: "x", OpClass: "oc", Lane: RegimeAWXJob, State: StatePending}
	if err := s.Insert(ctx, rec); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if err := s.Insert(ctx, rec); !errors.Is(err, ErrDuplicatePending) {
		t.Fatalf("duplicate Insert must fail ErrDuplicatePending, got %v", err)
	}
	if err := s.Update(ctx, PendingVerification{ActionID: "ghost"}); !errors.Is(err, ErrNoPending) {
		t.Fatalf("updating an absent record must fail ErrNoPending, got %v", err)
	}
	if err := s.Insert(ctx, PendingVerification{}); err == nil {
		t.Fatal("an empty action_id must be rejected")
	}
}

// fakeClock is a manually-advanced clock for the bound tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time     { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
