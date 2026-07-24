package acceptance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/regime"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// T-017-4 (REQ-1709/1710/1711/1712) — the GLOBAL async-verify (deferred-verify) channel. Registered from this
// file's init() as a step-registrar slice so the shared acceptance harness (acceptance_test.go) is never
// edited by parallel task work — the same pattern the regime-core (T-017-1/2) and awxplaybooks (T-017-5)
// slices use.
func init() {
	stepRegistrars = append(stepRegistrars, registerAsyncVerifySteps)
}

// ladderSink adapts the spec/017 async-verify GraduationSink onto the REAL spec/015 graduation ladder — the
// EXACT wiring main.go performs: a completed deferred verify's verdict is mapped to a graduation RunOutcome
// (policy.OutcomeFromVerdict) and recorded on the per-op-class ladder (Ladder.Record). This proves the
// deferred verdict is fed to the graduation ladder (REQ-1710) without core/regime importing core/policy.
type ladderSink struct{ l *policy.Ladder }

func (s ladderSink) RecordDeferredVerdict(ctx context.Context, opClass string, v safety.Verdict, verified bool) error {
	_, err := s.l.Record(ctx, opClass, policy.OutcomeFromVerdict(v, verified))
	return err
}

// asyncClock is a manually-advanced clock so the verification-bound (REQ-1711) is deterministic.
type asyncClock struct{ t time.Time }

func (c *asyncClock) now() time.Time { return c.t }

type asyncVerifyWorld struct {
	av      *regime.AsyncVerify
	store   *regime.MemPendingStore
	poller  *regime.MemJobPoller
	ladder  *policy.Ladder
	clock   *asyncClock
	opClass string

	// results captured across steps.
	handleBound bool
	res         regime.DeferredResolution
	verifyErr   error
	dupErr      error
	polledSeq   []regime.JobStatus
}

// build wires a fresh channel over the in-memory store + scripted poller + a REAL graduation ladder (threshold
// 1, so a single clean verified run promotes the op-class — the concrete "fed to the ladder" proof).
func (w *asyncVerifyWorld) build(bound time.Duration) {
	w.store = regime.NewMemPendingStore()
	w.poller = regime.NewMemJobPoller()
	w.clock = &asyncClock{t: time.Unix(1_700_000_000, 0)}
	w.ladder = policy.NewLadder(1, policy.NewMemGraduationStore(), nil)
	av, err := regime.NewAsyncVerify(w.store, w.poller,
		regime.WithGraduationSink(ladderSink{l: w.ladder}),
		regime.WithVerificationBound(bound),
		regime.WithClock(w.clock.now),
	)
	if err != nil {
		panic(fmt.Sprintf("async-verify build: %v", err))
	}
	w.av = av
}

func asyncPrediction(target string) verify.Prediction {
	return verify.Prediction{
		ActionID:       "acc-async",
		PlanHash:       "plan#async",
		TargetHost:     target,
		Site:           "nl",
		PredictedHosts: map[string]struct{}{},
		PredictedRules: map[string]struct{}{},
	}
}

func registerAsyncVerifySteps(sc *godog.ScenarioContext) {
	w := &asyncVerifyWorld{}

	// ---- REQ-1709: an async actuation returns a job handle and is polled to a terminal state ----
	sc.Step(`^an AWX job launched asynchronously$`, func() error {
		w.build(30 * time.Minute)
		w.opClass = "restart-service"
		intent := regime.LaunchIntent{ActionID: "req1709", OpClass: w.opClass, Lane: regime.RegimeAWXJob, Prediction: asyncPrediction("web01")}
		if err := w.av.Reserve(context.Background(), intent); err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		// the async launch returns a job handle (the id AWX hands back at POST /launch/).
		if err := w.av.BindHandle(context.Background(), "req1709", "job-1709"); err != nil {
			return fmt.Errorf("bind handle: %w", err)
		}
		w.handleBound = true
		// the job transitions pending → running → successful; the channel must POLL to the terminal.
		w.poller.Script("job-1709", regime.JobPending, regime.JobRunning, regime.JobSuccessful)
		return nil
	})
	sc.Step(`^the deferred-verify channel runs$`, func() error {
		// drive the poll loop until terminal (a scheduler would call Verify repeatedly).
		for i := 0; i < 5; i++ {
			res, err := w.av.Verify(context.Background(), "req1709")
			if err != nil {
				return fmt.Errorf("verify: %w", err)
			}
			w.res = res
			w.polledSeq = append(w.polledSeq, res.TerminalStatus)
			if res.State != regime.StatePending {
				break
			}
		}
		return nil
	})
	sc.Step(`^the lane returns a job handle and the engine polls the job to a terminal state rather than declaring success at launch$`, func() error {
		if !w.handleBound {
			return errors.New("the async lane must return a job handle at launch")
		}
		rec, err := w.store.Get(context.Background(), "req1709")
		if err != nil {
			return err
		}
		if rec.JobID != "job-1709" {
			return fmt.Errorf("the launch must carry its async job handle, got %q", rec.JobID)
		}
		// NOT declared successful at launch: it reached the terminal only via polling.
		if w.res.State != regime.StateVerified || w.res.TerminalStatus != regime.JobSuccessful {
			return fmt.Errorf("the channel must POLL the job to a terminal state, got %+v", w.res)
		}
		return nil
	})

	// ---- REQ-1710: launch is a prediction; verdict computed against terminal + fed to the graduation ladder ----
	sc.Step(`^a launched job treated as a prediction$`, func() error {
		w.build(30 * time.Minute)
		w.opClass = "restart-service"
		intent := regime.LaunchIntent{ActionID: "req1710", OpClass: w.opClass, Lane: regime.RegimeAWXJob, Prediction: asyncPrediction("web01")}
		if err := w.av.Reserve(context.Background(), intent); err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		if err := w.av.BindHandle(context.Background(), "req1710", "job-1710"); err != nil {
			return fmt.Errorf("bind: %w", err)
		}
		// the class has earned NOTHING yet — the launch is a prediction, not a clean run.
		if lvl := w.ladder.LevelOf(context.Background(), w.opClass); lvl != policy.LevelApprove {
			return fmt.Errorf("before the deferred verdict the class must be at approve, got %s", lvl)
		}
		return nil
	})
	sc.Step(`^the job reaches a terminal outcome$`, func() error {
		w.poller.Script("job-1710", regime.JobSuccessful)
		res, err := w.av.Verify(context.Background(), "req1710")
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}
		w.res = res
		return nil
	})
	sc.Step(`^the engine computes the mechanical verdict by comparing the terminal outcome against the prediction and feeds the verdict to the graduation ladder$`, func() error {
		// the mechanical verdict is the deterministic spec/002 verdict (a successful job with no surprise cascade
		// is a clean match) — the acting model never authored it.
		if w.res.Verdict != safety.VerdictMatch || !w.res.Verified || !w.res.CleanRun {
			return fmt.Errorf("a successful launch matching its prediction must be a clean verified match, got %+v", w.res)
		}
		if !w.res.GradFed {
			return errors.New("the deferred verdict must be fed to the graduation ladder")
		}
		// fed to the REAL ladder: with threshold 1, one clean verified deferred run promotes the class to auto.
		if lvl := w.ladder.LevelOf(context.Background(), w.opClass); lvl != policy.LevelAuto {
			return fmt.Errorf("the graduation ladder must record the clean verified run (promote to auto), got %s", lvl)
		}
		return nil
	})

	// ---- REQ-1711: pending counts as no clean run; a non-terminating verify times out to unverified ----
	sc.Step(`^a launched job that has not reached a terminal deferred-verified outcome$`, func() error {
		w.build(10 * time.Minute)
		w.opClass = "reboot"
		intent := regime.LaunchIntent{ActionID: "req1711", OpClass: w.opClass, Lane: regime.RegimeAWXJob, Prediction: asyncPrediction("web01")}
		if err := w.av.Reserve(context.Background(), intent); err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		if err := w.av.BindHandle(context.Background(), "req1711", "job-1711"); err != nil {
			return fmt.Errorf("bind: %w", err)
		}
		w.poller.Script("job-1711", regime.JobRunning) // never terminates
		return nil
	})
	sc.Step(`^graduation reads the run and the verification bound elapses$`, func() error {
		// while pending: it counts as NO clean verified run and is VISIBLE in the pending-verification queue.
		w.clock.t = w.clock.t.Add(5 * time.Minute)
		res, err := w.av.Verify(context.Background(), "req1711")
		if err != nil {
			return fmt.Errorf("verify(pending): %w", err)
		}
		if res.State != regime.StatePending || res.CleanRun {
			return fmt.Errorf("a pending launch must count as no clean verified run, got %+v", res)
		}
		q, _ := w.av.PendingQueue(context.Background())
		if len(q) != 1 || q[0].ActionID != "req1711" {
			return fmt.Errorf("the pending launch must be visible in the pending-verification queue, got %+v", q)
		}
		// the verification bound elapses without a terminal outcome.
		w.clock.t = w.clock.t.Add(6 * time.Minute) // 11m > 10m bound
		w.res, w.verifyErr = w.av.Verify(context.Background(), "req1711")
		return w.verifyErr
	})
	sc.Step(`^the job is recorded pending-verification and counts as no clean verified run and a job that does not reach terminal within the bound is recorded unverified and counts toward no graduation$`, func() error {
		if w.res.State != regime.StateUnverified || w.res.CleanRun || w.res.Verified {
			return fmt.Errorf("a job past its bound must be recorded unverified and never clean, got %+v", w.res)
		}
		// visible for escalation — never a silent success.
		u, _ := w.av.Unverified(context.Background())
		if len(u) != 1 || u[0].ActionID != "req1711" {
			return fmt.Errorf("the timed-out launch must be visible in the unverified/escalation queue, got %+v", u)
		}
		// counts toward NO graduation: the op-class earned no clean run and stays at approve.
		if lvl := w.ladder.LevelOf(context.Background(), w.opClass); lvl != policy.LevelApprove {
			return fmt.Errorf("an unverified run must count toward no graduation (class stays approve), got %s", lvl)
		}
		if st := w.ladder.State(context.Background(), w.opClass); st.CleanRunCount != 0 {
			return fmt.Errorf("an unverified run must not accrue a clean-run count, got %d", st.CleanRunCount)
		}
		return nil
	})

	// ---- REQ-1712: a second launch for the same action_id is refused ----
	sc.Step(`^an action_id that already carries a live or terminal job$`, func() error {
		w.build(30 * time.Minute)
		w.opClass = "restart-service"
		intent := regime.LaunchIntent{ActionID: "req1712", OpClass: w.opClass, Lane: regime.RegimeAWXJob, Prediction: asyncPrediction("web01")}
		if err := w.av.Reserve(context.Background(), intent); err != nil {
			return fmt.Errorf("first reserve: %w", err)
		}
		if err := w.av.BindHandle(context.Background(), "req1712", "job-1712"); err != nil {
			return fmt.Errorf("bind: %w", err)
		}
		return nil
	})
	sc.Step(`^a retry re-poll or redelivery attempts a second launch for that action_id$`, func() error {
		intent := regime.LaunchIntent{ActionID: "req1712", OpClass: w.opClass, Lane: regime.RegimeAWXJob, Prediction: asyncPrediction("web01")}
		w.dupErr = w.av.Reserve(context.Background(), intent)
		return nil
	})
	sc.Step(`^the engine refuses the second launch so the action never double-actuates$`, func() error {
		if !errors.Is(w.dupErr, regime.ErrDuplicateLaunch) {
			return fmt.Errorf("a second launch for an existing action_id must be refused with ErrDuplicateLaunch, got %v", w.dupErr)
		}
		// exactly one record exists for the action_id — no second job was ever reserved.
		all, _ := w.store.List(context.Background())
		count := 0
		for _, r := range all {
			if r.ActionID == "req1712" {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("exactly one launch record must exist for the action_id, got %d", count)
		}
		return nil
	})
}
