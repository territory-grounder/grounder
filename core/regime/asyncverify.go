package regime

// asyncverify.go — the GLOBAL deferred-verify channel (REQ-1709/1710/1711/1712). It is the "Did the effect
// match the prediction?" row of the autonomy loop for lanes whose effect is NOT synchronously observable.
//
// The load-bearing idea (from the GitOps-MR research, threat-model.md DoS/async-gap): an async lane's launch
// returns a JOB HANDLE — and a handle is a PREDICTION, not a success. An actuation whose effect is not
// synchronously observable is NOT verified at launch; it is verified only when this channel polls the job to a
// TERMINAL outcome and diffs that outcome against the committed prediction. Until then the launch is
// pending-verification and counts as NO clean run for graduation. A job launched but never verified fails
// closed for trust, never open (a failed or unobserved mutation is never silently trusted).
//
// This channel is GLOBAL by design: any lane whose effect defers (awx-job today; gitops-mr later) reserves a
// deferred verify here rather than declaring success at launch. It is a VERIFY/observe component — it launches
// NOTHING and actuates NOTHING (the launch is the T-017-3 actuator through the spec/013 interceptor). It only
// (1) reserves the launch idempotently by action_id (REQ-1712), (2) records the async handle (REQ-1709), (3)
// polls the job to a terminal AWX status through a READ-ONLY JobPoller, (4) computes the spec/002 mechanical
// verdict against the prediction (REQ-1710) and feeds it to the graduation ladder (spec/015 REQ-1514), and (5)
// bounds the wait so a deferred verify that never terminates fails safe to a VISIBLE `unverified` record
// (REQ-1711) — never a silent success. It calls no actuator's Exec; the deterministic verifier (core/verify)
// remains the sole author of the verdict, and the acting model never adjudicates its own outcome (INV-10).
//
// Provenance: [O] INV-10 (deterministic deferred verdict; the sole verdict author) · [O] INV-11 (a launched
// effect is a prediction until verified) · [O] INV-07/INV-09 (bound to action_id; fail closed), spec/017
// REQ-1709/1710/1711/1712 (TG-110). Phase 2: the mutating launch stays OFF (mode Shadow) until the flip; this
// verify channel is Phase-safe (it observes, it never launches).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// The JobStatus type and its four TERMINAL constants (JobSuccessful / JobFailed / JobError / JobCanceled) are
// declared in audit.go (the T-017-6 persistence sibling, which records the terminal status on a
// deferred_verdict row). This channel OBSERVES the full poll lifecycle, so it adds the NON-TERMINAL statuses
// AWX transitions through before a terminal (`pending → waiting → running → { terminal }`, design.md AWX
// shapes) plus the Terminal / Succeeded predicates the poll loop reads. The zero value is the empty status
// (not yet observed) — never terminal, so an unobserved job is never mistaken for a finished one.
const (
	JobPending JobStatus = "pending" // queued in AWX; not yet started (non-terminal)
	JobWaiting JobStatus = "waiting" // dependencies pending (non-terminal)
	JobRunning JobStatus = "running" // executing (non-terminal)
)

// Terminal reports whether s is a terminal AWX status (REQ-1709). Only a terminal status ends the deferred
// verify; every other value (including the empty/unknown one, and the non-terminal poll states above) means
// keep polling. (JobStatus.Valid in audit.go is the STRICTER "is a terminal-verdict slug" check; this
// predicate is the poll-loop's "may I stop?" — they agree on the four terminal values.)
func (s JobStatus) Terminal() bool {
	switch s {
	case JobSuccessful, JobFailed, JobError, JobCanceled:
		return true
	default:
		return false
	}
}

// Succeeded reports whether the job reached the one terminal status that means the predicted effect actually
// fired. Only a `successful` job's post-state is adjudicated against the prediction as a possible clean run;
// every other terminal (or a timeout) is never a clean run.
func (s JobStatus) Succeeded() bool { return s == JobSuccessful }

// AsyncObservable reports whether a lane's effect is NOT synchronously observable and therefore requires a
// deferred verify (REQ-1709). native-ssh verifies SYNCHRONOUSLY inline in the spec/013 interceptor, so it
// never defers; every other lane's effect (awx-job job template, gitops-mr merge request, k8s-declarative
// reconcile, api) completes asynchronously and MUST be verified by this channel rather than trusted at launch.
func AsyncObservable(r Regime) bool { return r.Valid() && r != RegimeNativeSSH }

// ---------------------------------------------------------------------------------------------------------
// Read-only poll seam + graduation feed seam + in-memory fakes for the oracles.
// ---------------------------------------------------------------------------------------------------------

// JobPoller reads the current status of an async job by its handle. It is READ-ONLY: the deferred verify
// OBSERVES, it never launches or mutates (the launch is T-017-3's actuator through the interceptor). The AWX
// net/http client (T-017-3) satisfies it via `GET /api/v2/jobs/{id}/`; this package defines only the seam +
// an in-memory fake so the oracles run without AWX. (The method is deliberately not named Exec — this channel
// never reaches an effect; the regime-composition standing check forbids an Exec call in the package.)
type JobPoller interface {
	// PollJob returns the current JobStatus of jobID. A transient read error leaves the deferred verify pending
	// (it retries), so an unreachable AWX never fabricates a terminal outcome.
	PollJob(ctx context.Context, jobID string) (JobStatus, error)
}

// GraduationSink receives the earned-trust evidence a completed deferred verify produces: the op-class of the
// launched action, the mechanical Verdict, and whether a terminal let the verifier ADJUDICATE the post-state
// (verified). The main.go wave adapts this to the spec/015 graduation ladder — `policy.OutcomeFromVerdict(v,
// verified)` then `Ladder.Record(opClass, outcome)` — so THIS package never imports core/policy and neither
// re-adjudicates the verdict nor couples to the ladder's implementation. A nil sink is permitted (the verdict
// still rides the pending record + the audit row); a wired sink is how an op-class earns autonomy on VERIFIED
// deferred runs (REQ-1710).
type GraduationSink interface {
	// RecordDeferredVerdict feeds one completed deferred verify to the graduation ladder. verified==false
	// (a timeout or an indeterminate terminal) must NEVER count as clean — the adapter maps it to the ladder's
	// unverified outcome (spec/015 REQ-1515).
	RecordDeferredVerdict(ctx context.Context, opClass string, verdict safety.Verdict, verified bool) error
}

// MemJobPoller is a scripted in-memory JobPoller fake for the oracles. Script(jobID, seq...) queues the
// statuses returned on successive polls (the last is repeated once exhausted, so a terminal status is sticky);
// Fail(jobID, err) makes every poll of that job return a transient error (to exercise the fail-safe path).
type MemJobPoller struct {
	mu      sync.Mutex
	scripts map[string][]JobStatus
	fails   map[string]error
	calls   map[string]int
}

// NewMemJobPoller returns an empty scripted poller (an unknown job polls as JobPending — non-terminal).
func NewMemJobPoller() *MemJobPoller {
	return &MemJobPoller{scripts: map[string][]JobStatus{}, fails: map[string]error{}, calls: map[string]int{}}
}

// Script queues the statuses jobID returns on successive PollJob calls (last repeated). Chainable.
func (p *MemJobPoller) Script(jobID string, seq ...JobStatus) *MemJobPoller {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[jobID] = append([]JobStatus(nil), seq...)
	return p
}

// Fail makes every poll of jobID return err (a transient read failure). Chainable.
func (p *MemJobPoller) Fail(jobID string, err error) *MemJobPoller {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails[jobID] = err
	return p
}

// PollJob returns the next scripted status for jobID (repeating the last), or a primed transient error.
func (p *MemJobPoller) PollJob(_ context.Context, jobID string) (JobStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fails[jobID]; err != nil {
		return "", err
	}
	seq := p.scripts[jobID]
	i := p.calls[jobID]
	p.calls[jobID]++
	switch {
	case len(seq) == 0:
		return JobPending, nil // an unknown / unscripted job is still queued — non-terminal, keep polling.
	case i < len(seq):
		return seq[i], nil
	default:
		return seq[len(seq)-1], nil // exhausted → the last status is sticky (a terminal stays terminal).
	}
}

// MemGraduationSink is an in-memory GraduationSink fake that records every deferred verdict fed to it — the
// oracle inspects it to prove a verdict was fed to the graduation ladder (REQ-1710) and that an unverified run
// was fed as not-clean.
type MemGraduationSink struct {
	mu    sync.Mutex
	Feeds []GraduationFeed
}

// GraduationFeed is one recorded feed into the graduation sink.
type GraduationFeed struct {
	OpClass  string
	Verdict  safety.Verdict
	Verified bool
}

// NewMemGraduationSink returns an empty recording sink.
func NewMemGraduationSink() *MemGraduationSink { return &MemGraduationSink{} }

// RecordDeferredVerdict records the feed.
func (s *MemGraduationSink) RecordDeferredVerdict(_ context.Context, opClass string, v safety.Verdict, verified bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Feeds = append(s.Feeds, GraduationFeed{OpClass: opClass, Verdict: v, Verified: verified})
	return nil
}

// Recorded returns a copy of the feeds (deterministic order of recording).
func (s *MemGraduationSink) Recorded() []GraduationFeed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]GraduationFeed(nil), s.Feeds...)
}

var (
	_ JobPoller      = (*MemJobPoller)(nil)
	_ GraduationSink = (*MemGraduationSink)(nil)
)

// ---------------------------------------------------------------------------------------------------------
// The channel.
// ---------------------------------------------------------------------------------------------------------

// DefaultVerificationBound is the fallback operator-declared window (REQ-1711) within which a launched job
// must reach a terminal status or be recorded `unverified`. Conservative: a mutation whose effect we could not
// confirm within the bound never counts toward graduation. An operator may configure a per-deployment bound.
const DefaultVerificationBound = 30 * time.Minute

var (
	// ErrChannelUnwired is returned when the channel is constructed without its required collaborators (store /
	// poller) — a control that cannot verify must fail LOUD, never leave a launch silently unverified.
	ErrChannelUnwired = errors.New("regime: async-verify channel is unwired (nil store or poller) — refusing")
	// ErrDuplicateLaunch is the fail-closed refusal Reserve returns for an action_id that already carries a
	// launch (live OR terminal). It is the idempotency guard (REQ-1712): the async lane MUST NOT launch a second
	// job when Reserve refuses, so a retry / re-poll / redelivery never double-actuates.
	ErrDuplicateLaunch = errors.New("regime: action_id already carries a launch (live or terminal) — refusing a second launch (idempotency)")
	// ErrSynchronousLane is returned by Reserve for a lane whose effect IS synchronously observable (native-ssh):
	// such a launch is verified inline by the spec/013 interceptor and must not use the deferred channel.
	ErrSynchronousLane = errors.New("regime: lane effect is synchronously observable — no deferred verify (use the inline interceptor verdict)")
	// ErrHandleAlreadyBound is returned by BindHandle when a job handle was already recorded for the action_id —
	// a launch is bound exactly once (a second bind would imply a second launch).
	ErrHandleAlreadyBound = errors.New("regime: a job handle is already bound for this action_id — a launch is bound exactly once")
)

// AsyncVerify is the GLOBAL deferred-verify channel. It holds the pending-verification store (the durable
// idempotency + fail-safe record), a read-only JobPoller, an optional GraduationSink and alert observer, and
// the operator-declared verification bound + a clock seam (for deterministic tests). It launches nothing.
//
// Concurrency contract: Verify must be driven SERIALLY per action_id — a Temporal workflow keyed on the
// action_id (inherently single-writer) or a scheduler that never runs two Verify steps for the same action
// concurrently. Reserve is already race-safe across action_ids (its atomicity is the store's Insert, which
// fails closed on a duplicate). The per-action_id single-writer discipline for Verify is enforced at the
// STORE boundary by the durable pgx impl (T-017-6) via a state-guarded Update (compare-and-set on State), so
// two concurrent terminal transitions cannot both adjudicate — the in-memory fake here does not add that
// guard and relies on the serial-scheduler contract. AsyncVerify itself keeps no shared mutable state.
type AsyncVerify struct {
	store   PendingStore
	poller  JobPoller
	grad    GraduationSink                                                    // optional: the graduation-ladder feed (REQ-1710)
	observe func(context.Context, verify.Prediction) []verify.ObservedAlert   // optional: the observed-alert source for the verdict
	bound   time.Duration                                                     // the verification bound (REQ-1711)
	now     func() time.Time                                                  // clock seam (default time.Now)
	logf    func(format string, args ...any)                                  // optional structured log
}

// Option configures an AsyncVerify.
type Option func(*AsyncVerify)

// WithGraduationSink wires the graduation-ladder feed (REQ-1710). nil is ignored (the verdict still rides the
// pending record). main.go passes an adapter over the spec/015 policy.Ladder.
func WithGraduationSink(g GraduationSink) Option {
	return func(a *AsyncVerify) {
		if g != nil {
			a.grad = g
		}
	}
}

// WithObserver wires the observed-alert source the deterministic verifier diffs against the prediction
// (spec/002). nil (the default) yields an empty observation, under which a successful job with no observed
// cascade is a clean `match` (ComputeVerdictDetail's quiet case) — exactly the fail-direction we want.
func WithObserver(observe func(context.Context, verify.Prediction) []verify.ObservedAlert) Option {
	return func(a *AsyncVerify) {
		if observe != nil {
			a.observe = observe
		}
	}
}

// WithVerificationBound sets the operator-declared window a job must reach terminal within or be recorded
// `unverified` (REQ-1711). A non-positive bound clamps to DefaultVerificationBound.
func WithVerificationBound(d time.Duration) Option {
	return func(a *AsyncVerify) {
		if d > 0 {
			a.bound = d
		}
	}
}

// WithClock overrides the clock (for deterministic bound tests). nil is ignored.
func WithClock(now func() time.Time) Option {
	return func(a *AsyncVerify) {
		if now != nil {
			a.now = now
		}
	}
}

// WithLogger attaches a structured logger. nil is ignored.
func WithLogger(logf func(string, ...any)) Option {
	return func(a *AsyncVerify) {
		if logf != nil {
			a.logf = logf
		}
	}
}

// NewAsyncVerify builds the deferred-verify channel over a pending store and a read-only job poller (both
// REQUIRED — a nil either way yields ErrChannelUnwired at construction, never a silent launch left unverified).
func NewAsyncVerify(store PendingStore, poller JobPoller, opts ...Option) (*AsyncVerify, error) {
	if store == nil || poller == nil {
		return nil, ErrChannelUnwired
	}
	a := &AsyncVerify{
		store:  store,
		poller: poller,
		bound:  DefaultVerificationBound,
		now:    time.Now,
	}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// LaunchIntent is what an async lane hands the channel BEFORE it launches (REQ-1709/1712): the action to be
// launched (its manifest action_id + op-class), the async lane, and the committed prediction the terminal
// outcome will be diffed against. The job handle (job_id) is deliberately NOT here — the launch has not fired
// yet; Reserve claims the action_id first so at most one launch can ever fire for it.
type LaunchIntent struct {
	ActionID   string
	OpClass    string
	Lane       Regime
	Prediction verify.Prediction
}

// Reserve is the PRE-LAUNCH idempotency gate (REQ-1712): it atomically claims intent.ActionID by durably
// inserting a StatePending record, so at most ONE launch ever fires for it. If the action_id already carries a
// record (live OR terminal) it returns ErrDuplicateLaunch and the async lane MUST NOT launch — a retry /
// re-poll / redelivery never double-actuates. A synchronously-observable lane (native-ssh) is refused
// (ErrSynchronousLane): its effect is verified inline by the interceptor, not here. Reserve launches NOTHING;
// it only reserves the record the one sanctioned launch then binds its handle to.
func (a *AsyncVerify) Reserve(ctx context.Context, intent LaunchIntent) error {
	if intent.ActionID == "" {
		return fmt.Errorf("regime: async-verify Reserve requires an action_id")
	}
	if intent.OpClass == "" {
		return fmt.Errorf("regime: async-verify Reserve requires an op-class (the graduation bucket)")
	}
	if !intent.Lane.Valid() {
		return fmt.Errorf("regime: async-verify Reserve: invalid lane %q", intent.Lane)
	}
	if !AsyncObservable(intent.Lane) {
		return fmt.Errorf("%w: lane %q", ErrSynchronousLane, intent.Lane)
	}
	rec := PendingVerification{
		ActionID:   intent.ActionID,
		OpClass:    intent.OpClass,
		Lane:       intent.Lane,
		State:      StatePending,
		Prediction: intent.Prediction,
		LaunchedAt: a.now(),
	}
	if err := a.store.Insert(ctx, rec); err != nil {
		if errors.Is(err, ErrDuplicatePending) {
			// The action_id is already claimed — the fail-closed no-double-launch guard (REQ-1712).
			return fmt.Errorf("%w: %s", ErrDuplicateLaunch, intent.ActionID)
		}
		return fmt.Errorf("regime: async-verify Reserve: %w", err)
	}
	a.log("async-verify: reserved deferred verify for action %s (op-class %q, lane %s) — pending", short(intent.ActionID), intent.OpClass, intent.Lane)
	return nil
}

// BindHandle records the async job handle (job_id) the ONE sanctioned launch returned (REQ-1709). The handle
// is a PREDICTION, not a success: the record stays StatePending until Verify reaches a terminal. It fails with
// ErrNoPending if the action was never Reserved, ErrHandleAlreadyBound if a handle was already recorded (a
// launch is bound exactly once), and refuses to bind onto an already-terminal record.
func (a *AsyncVerify) BindHandle(ctx context.Context, actionID, jobID string) error {
	if actionID == "" || jobID == "" {
		return fmt.Errorf("regime: async-verify BindHandle requires an action_id and a job_id")
	}
	rec, err := a.store.Get(ctx, actionID)
	if err != nil {
		return fmt.Errorf("regime: async-verify BindHandle: %w", err)
	}
	if rec.State != StatePending {
		return fmt.Errorf("regime: async-verify BindHandle: action %s is already %s — cannot bind a handle", short(actionID), rec.State)
	}
	if rec.JobID != "" {
		return fmt.Errorf("%w: action %s", ErrHandleAlreadyBound, short(actionID))
	}
	rec.JobID = jobID
	if err := a.store.Update(ctx, rec); err != nil {
		return fmt.Errorf("regime: async-verify BindHandle: %w", err)
	}
	a.log("async-verify: bound job handle %s to action %s — still pending (a handle is a prediction, not a success)", jobID, short(actionID))
	return nil
}

// DeferredResolution is the outcome of one Verify step — a NON-SECRET projection of the pending record. It
// carries the launch handle, the state, and (once terminal) the mechanical verdict + whether the run counted
// as clean. CleanRun is the ONE signal that a launched effect matched its prediction; it is false for every
// pending, timed-out, failed, or indeterminate launch (launch-as-prediction, REQ-1710/1711).
type DeferredResolution struct {
	ActionID       string
	OpClass        string
	Lane           Regime
	JobID          string
	State          VerifyState
	TerminalStatus JobStatus
	Verdict        safety.Verdict
	Verified       bool // a terminal let the verifier adjudicate the post-state (feeds graduation semantics)
	CleanRun       bool // Verified && Verdict == match — the sole clean-run signal for graduation
	// GradFed reports whether THIS Verify call fed the graduation sink. It is true only on the ONE call that
	// transitions the run to a terminal (verified / unverified); an idempotent re-poll of an already-resolved
	// run returns GradFed=false because the evidence was fed once, on the transition — not because it was never
	// fed. A nil sink also yields false.
	GradFed bool
	Reason  string
}

func (a *AsyncVerify) resolution(rec PendingVerification, gradFed bool, reason string) DeferredResolution {
	return DeferredResolution{
		ActionID:       rec.ActionID,
		OpClass:        rec.OpClass,
		Lane:           rec.Lane,
		JobID:          rec.JobID,
		State:          rec.State,
		TerminalStatus: rec.TerminalStatus,
		Verdict:        rec.Verdict,
		Verified:       rec.Verified,
		CleanRun:       rec.CleanRun(),
		GradFed:        gradFed,
		Reason:         reason,
	}
}

// Verify is ONE single-shot deferred-verify step for a launched action (REQ-1709/1710/1711). A scheduler (a
// Temporal poll loop / cron in main.go) drives it repeatedly; each call is idempotent and restart-safe:
//
//   - Already terminal (verified / unverified): return the stored resolution WITHOUT re-polling or re-feeding
//     graduation — a re-poll / redelivery never double-adjudicates (REQ-1712).
//   - No handle bound yet: if within the bound stay pending (the launch has not reported a job_id); if the
//     bound elapsed, record `unverified` (fail-safe — a launch that never produced a handle is not trusted).
//   - Handle bound, poll the job:
//   - terminal → StateVerified: compute the spec/002 mechanical verdict against the committed prediction
//     (REQ-1710) and feed it to the graduation ladder; a `successful` job is adjudicated against the observed
//     cascade, a failed/error job is a deviation (a failed mutation is never a clean run), a canceled job is
//     indeterminate (unverified).
//   - non-terminal within bound → stays pending (still NO clean run).
//   - non-terminal past bound → StateUnverified (REQ-1711): recorded unverified, visible, counts toward NO
//     graduation. This is the fail-safe for a deferred verify that never terminates — never a silent success.
//
// A transient poll error within the bound leaves the record pending and is returned wrapped (retry later); it
// never fabricates a terminal outcome.
func (a *AsyncVerify) Verify(ctx context.Context, actionID string) (DeferredResolution, error) {
	rec, err := a.store.Get(ctx, actionID)
	if err != nil {
		return DeferredResolution{}, fmt.Errorf("regime: async-verify Verify: %w", err)
	}

	// Idempotent short-circuit: a re-poll of an already-adjudicated launch never re-grades or double-feeds.
	if rec.State == StateVerified || rec.State == StateUnverified {
		return a.resolution(rec, false, "already resolved — deferred verify is idempotent"), nil
	}

	overBound := a.now().Sub(rec.LaunchedAt) > a.bound

	// A launch that never reported a job handle: stay pending within the bound; time out to unverified past it.
	if rec.JobID == "" {
		if overBound {
			return a.resolveUnverified(ctx, rec, "verification bound elapsed before a job handle was bound")
		}
		return a.resolution(rec, false, "reserved but no job handle bound yet — pending"), nil
	}

	status, perr := a.poller.PollJob(ctx, rec.JobID)
	if perr != nil {
		// A transient read failure: fail safe. Past the bound, a job we still cannot confirm is `unverified`;
		// within it, stay pending and surface the error so the scheduler retries.
		if overBound {
			return a.resolveUnverified(ctx, rec, fmt.Sprintf("verification bound elapsed; job status unreadable: %v", perr))
		}
		return a.resolution(rec, false, "job status unreadable — pending (will retry)"), fmt.Errorf("regime: async-verify poll: %w", perr)
	}

	if !status.Terminal() {
		if overBound {
			return a.resolveUnverified(ctx, rec, fmt.Sprintf("verification bound elapsed; job still %s (non-terminal)", status))
		}
		a.log("async-verify: action %s job %s is %s (non-terminal) — still pending, no clean run", short(rec.ActionID), rec.JobID, status)
		return a.resolution(rec, false, fmt.Sprintf("job %s (non-terminal) — pending", status)), nil
	}

	// Terminal: adjudicate. The deterministic verifier is the sole verdict author; the acting model never is.
	verdict, verified := a.adjudicate(ctx, rec, status)
	rec.State = StateVerified
	rec.TerminalStatus = status
	rec.Verdict = verdict
	rec.Verified = verified
	rec.ResolvedAt = a.now()
	if err := a.store.Update(ctx, rec); err != nil {
		return DeferredResolution{}, fmt.Errorf("regime: async-verify Verify: persist terminal verdict: %w", err)
	}
	gradFed := a.feedGraduation(ctx, rec)
	a.log("async-verify: action %s job %s terminal=%s verdict=%s verified=%t clean=%t — deferred verify complete",
		short(rec.ActionID), rec.JobID, status, verdictStr(verdict), verified, rec.CleanRun())
	return a.resolution(rec, gradFed, fmt.Sprintf("terminal %s → verdict %s", status, verdictStr(verdict))), nil
}

// adjudicate maps a TERMINAL job status to the mechanical verdict + the verify-on-auto bit (REQ-1710):
//
//   - successful → the effect fired; diff the observed cascade against the committed prediction with the
//     deterministic spec/002 verifier (verify.ComputeVerdictDetail). verified == true.
//   - failed / error → the predicted successful effect did NOT happen. A failed mutation is a VERIFIED
//     deviation (verified == true, deviation) — it demotes the op-class; a failure is never silently trusted.
//   - canceled → the run was aborted; the post-state is INDETERMINATE. Fail safe: verified == false, no verdict
//     — it neither promotes nor demotes, only breaks the clean streak.
func (a *AsyncVerify) adjudicate(ctx context.Context, rec PendingVerification, status JobStatus) (safety.Verdict, bool) {
	switch {
	case status.Succeeded():
		var observed []verify.ObservedAlert
		if a.observe != nil {
			observed = a.observe(ctx, rec.Prediction)
		}
		return verify.ComputeVerdictDetail(rec.Prediction, observed).Verdict, true
	case status == JobFailed || status == JobError:
		return safety.VerdictDeviation, true
	default: // canceled — indeterminate post-state; fail safe as unverified (no clean run, no demote).
		return "", false
	}
}

// resolveUnverified records the fail-safe timeout state (REQ-1711): the bound elapsed before a terminal
// outcome. The record is marked StateUnverified (visible, never a silent success) and fed to graduation as a
// non-clean run (verified == false → the ladder's unverified outcome), so it counts toward NO graduation.
func (a *AsyncVerify) resolveUnverified(ctx context.Context, rec PendingVerification, reason string) (DeferredResolution, error) {
	rec.State = StateUnverified
	rec.Verified = false
	rec.Verdict = ""
	rec.ResolvedAt = a.now()
	if err := a.store.Update(ctx, rec); err != nil {
		return DeferredResolution{}, fmt.Errorf("regime: async-verify resolveUnverified: %w", err)
	}
	gradFed := a.feedGraduation(ctx, rec)
	a.log("async-verify: action %s job %s recorded UNVERIFIED (%s) — no clean run, visible for escalation", short(rec.ActionID), rec.JobID, reason)
	return a.resolution(rec, gradFed, "unverified: "+reason), nil
}

// feedGraduation feeds a resolved run's earned-trust evidence to the graduation ladder (REQ-1710). It is the
// single translation point to the graduation sink; a nil sink is a no-op (the verdict still rides the record).
// A sink error is logged and NOT propagated as a verify failure — the deferred verdict itself is already
// durably recorded; the ladder feed is downstream evidence, and failing safe means the run simply does not
// promote.
func (a *AsyncVerify) feedGraduation(ctx context.Context, rec PendingVerification) bool {
	if a.grad == nil {
		return false
	}
	if err := a.grad.RecordDeferredVerdict(ctx, rec.OpClass, rec.Verdict, rec.Verified); err != nil {
		a.log("async-verify: graduation feed for op-class %q failed (%v) — evidence not recorded, run does not promote", rec.OpClass, err)
		return false
	}
	return true
}

// PendingQueue returns the launched-but-unresolved records (StatePending) — the visible pending-verification
// queue the console (REQ-1716) and escalation read. A launch in this queue counts as NO clean run.
func (a *AsyncVerify) PendingQueue(ctx context.Context) ([]PendingVerification, error) {
	return a.store.List(ctx, StatePending)
}

// Unverified returns the launches that timed out without a terminal outcome (StateUnverified) — the fail-safe
// escalation queue (REQ-1711). These are visible, never silently trusted, and count toward no graduation.
func (a *AsyncVerify) Unverified(ctx context.Context) ([]PendingVerification, error) {
	return a.store.List(ctx, StateUnverified)
}

func (a *AsyncVerify) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// verdictStr renders a possibly-empty verdict for logs/reasons (an unadjudicated terminal has no verdict).
func verdictStr(v safety.Verdict) string {
	if v == "" {
		return "none"
	}
	return string(v)
}

// short renders a compact prefix of a content hash / action_id for logs and reasons (never the full id).
func short(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
