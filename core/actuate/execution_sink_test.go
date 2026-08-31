package actuate

import (
	"context"
	"errors"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/manifest"
)

// recordingExecutionSink captures every per-execution record so a test can assert on the sequence.
type recordingExecutionSink struct {
	rows []execRow
	err  error
}

type execRow struct {
	actionID    string
	externalRef string
	verdict     safety.Verdict
	verified    bool
	inverts     string
}

func (s *recordingExecutionSink) Record(_ context.Context, actionID, externalRef, _, _ string, v safety.Verdict, verified bool, invertsActionID string) error {
	s.rows = append(s.rows, execRow{actionID: actionID, externalRef: externalRef, verdict: v, verified: verified, inverts: invertsActionID})
	return s.err
}

// firstWinsVerdictSink models action_verdict's real semantics: keyed by action_id, ON CONFLICT DO NOTHING.
// Re-committing the same id is silently a no-op — the behaviour that made repeated executions invisible.
type firstWinsVerdictSink struct{ seen map[string]safety.Verdict }

func (s *firstWinsVerdictSink) Commit(_ context.Context, actionID, _, _, _ string, v safety.Verdict) error {
	if s.seen == nil {
		s.seen = map[string]safety.Verdict{}
	}
	if _, exists := s.seen[actionID]; exists {
		return nil // first-wins: the later verdict is DROPPED
	}
	s.seen[actionID] = v
	return nil
}

// THE DEFECT THIS EXISTS FOR. One action shape executed N times leaves exactly ONE durable verdict, because
// action_id is content-addressed over the operation alone. Measured live: 113 executions -> 28 outcomes. The
// per-execution sink must record all N, with each run's OWN verdict.
func TestExecutionSink_RecordsEveryRunWhileTheVerdictStoreKeepsOnlyTheFirst(t *testing.T) {
	const sameShape = "action-abc"
	verdicts := &firstWinsVerdictSink{}
	execs := &recordingExecutionSink{}

	// Three runs of the SAME action shape with genuinely different outcomes.
	runs := []safety.Verdict{safety.VerdictMatch, safety.VerdictDeviation, safety.VerdictMatch}
	for i, v := range runs {
		if err := verdicts.Commit(context.Background(), sameShape, "plan", "host", "site", v); err != nil {
			t.Fatalf("run %d: verdict commit: %v", i, err)
		}
		if err := execs.Record(context.Background(), sameShape, "incident-1", "host", "site", v, true, ""); err != nil {
			t.Fatalf("run %d: execution record: %v", i, err)
		}
	}

	if got := len(verdicts.seen); got != 1 {
		t.Fatalf("action_verdict holds %d rows for one shape, want 1 (first-wins is the documented contract)", got)
	}
	if verdicts.seen[sameShape] != safety.VerdictMatch {
		t.Fatalf("the per-shape row must keep the FIRST outcome, got %q", verdicts.seen[sameShape])
	}
	if len(execs.rows) != len(runs) {
		t.Fatalf("per-execution rows = %d, want %d — repeated heals of one shape must each be recordable", len(execs.rows), len(runs))
	}
	// Each run's own verdict must survive, including the deviation the per-shape row dropped.
	for i, want := range runs {
		if execs.rows[i].verdict != want {
			t.Errorf("run %d verdict = %q, want %q", i, execs.rows[i].verdict, want)
		}
	}
	if execs.rows[1].verdict != safety.VerdictDeviation {
		t.Error("the second run's DEVIATION was lost — that is precisely the outcome the first-wins row hides")
	}
}

// An UNVERIFIABLE execution must still be recorded, with verified=false and no verdict. An execution that
// left no trace is indistinguishable from one that never happened, which is worse for an audit surface than
// recording "we ran it and could not check".
func TestExecutionSink_RecordsUnverifiableRunsHonestly(t *testing.T) {
	execs := &recordingExecutionSink{}
	if err := execs.Record(context.Background(), "a1", "inc", "host", "site", safety.VerdictMatch, false, ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(execs.rows) != 1 {
		t.Fatal("an unverifiable execution must still be recorded")
	}
	if execs.rows[0].verified {
		t.Error("an unverifiable execution must be marked unverified, never laundered as a clean result")
	}
}

// The recorder is EVIDENCE, not a gate: the action already executed and cannot be un-run, so a write failure
// must not change the outcome. It is audited as a control gap instead.
func TestExecutionSink_RecordFailureIsNotFatal(t *testing.T) {
	sink := &recordingExecutionSink{err: errors.New("db down")}
	err := sink.Record(context.Background(), "a1", "inc", "host", "site", safety.VerdictMatch, true, "")
	if err == nil {
		t.Fatal("the sink must surface its error so the interceptor can audit the control gap")
	}
	if len(sink.rows) != 1 {
		t.Fatal("the attempt must still be observable to the caller")
	}
}

// TestInterceptor_AcceptsAnExecutionSink — the chainable setter must ATTACH the sink, not merely typecheck.
//
// Until TG-258 this test's entire body was `var _ ExecutionSink = (*recordingExecutionSink)(nil)`: a
// compile-time assertion that the TEST'S OWN FAKE satisfies the interface. It named the interceptor and
// never mentioned it, and it could not fail at run time under any change to the interceptor at all — while
// its comment claimed the setter "accepts the sink, so wiring is a composition-root decision and a lane
// cannot silently be left dark". Gutting WithExecutionSink to a bare `return i` left it green, and that is
// precisely the failure the sink exists to prevent: the composition root wires a recorder, the interceptor
// drops it, i.executions stays nil, and every per-execution row is silently discarded — 113 executions
// collapsing back into 28 durable outcomes, so "N independent hands-off heals of this class" becomes
// unprovable with no error anywhere and every caller unchanged.
//
// KILLING MUTATION (executed 2026-08-03, reverted green): delete `i.executions = e` from WithExecutionSink.
func TestInterceptor_AcceptsAnExecutionSink(t *testing.T) {
	var _ ExecutionSink = (*recordingExecutionSink)(nil) // the fake really is the seam under test
	sink := &recordingExecutionSink{}
	ic := NewInterceptor(safety.NewReadOnlyChokepoint(), nil, nil).WithExecutionSink(sink)
	if ic == nil {
		t.Fatal("WithExecutionSink returned nil — the chain the composition root builds cannot continue")
	}
	got, ok := ic.executions.(*recordingExecutionSink)
	if !ok || got != sink {
		t.Fatalf("WithExecutionSink did not attach the sink it was handed (interceptor holds %#v): the "+
			"per-execution recorder is then nil at the one place Do consults it, so only the FIRST run of "+
			"each content-addressed action shape ever leaves a durable outcome and every repeat heal is "+
			"invisible — the defect this file's first test measures", ic.executions)
	}
}

// TestInterceptor_RecordsEveryExecutionThroughTheAttachedSink closes the hop the setter test cannot see.
//
// Attachment is necessary and NOT sufficient, and the gap between the two is where this file's whole subject
// actually lives: every other test above drives the FAKE directly (execs.Record(...)), and the setter test
// drives only WithExecutionSink, so before this test existed nothing in the repository ever ran the
// interceptor with a sink attached. Deleting the entire `if i.executions != nil { i.executions.Record(...) }`
// block from Do — the ONE call site, the actual per-execution write — left `go test ./core/actuate/...`
// green (executed 2026-08-03). That is the recorded production defect restored in full: 113 executions
// collapsing to 28 durable outcomes because action_verdict is content-addressed first-wins, with the sink
// wired, attached, type-correct and never called.
//
// Two runs of the SAME action shape, because one run cannot distinguish the fix from the bug: the first-wins
// verdict row exists either way, and only the SECOND row proves a repeat heal is recordable at all.
//
// KILLING MUTATION (executed 2026-08-03, reverted green): replace Do's execution-record block with
// `_ = i.executions` — RED with "recorded 0 executions after 1 run".
func TestInterceptor_RecordsEveryExecutionThroughTheAttachedSink(t *testing.T) {
	ctx := context.Background()
	sink := &recordingExecutionSink{}
	act := &fakeActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithExecutionSink(sink)

	// A FRESH Request per run, exactly as two separate incidents produce it. The manifest carries a lifecycle
	// chain that the first Do advances, so re-submitting the same Request VALUE is a replay rather than a
	// second heal; a fresh one over the same action yields the identical content-addressed action_id, which is
	// the whole premise of the collapse being measured here.
	var actionID string
	for run := 1; run <= 2; run++ {
		out, err := i.Do(ctx, goodRequest(t))
		if err != nil {
			t.Fatalf("run %d: Do failed loud: %v", run, err)
		}
		if !out.Executed {
			t.Fatalf("run %d: the request must execute for this test to say anything about recording it: %+v",
				run, out)
		}
		if actionID == "" {
			actionID = out.ActionID
		} else if out.ActionID != actionID {
			t.Fatalf("run %d produced action id %q, not %q: the two runs are not the SAME content-addressed "+
				"shape, so this test is no longer measuring the first-wins collapse it claims to",
				run, out.ActionID, actionID)
		}
		// Asserted INSIDE the loop, so the failure names which run went missing rather than only the total.
		if len(sink.rows) != run {
			t.Fatalf("the interceptor recorded %d executions after %d run(s): Do is not writing through the "+
				"attached ExecutionSink, so the per-shape action_verdict row (content-addressed, first-wins) "+
				"is the only durable trace and repeated heals of one shape are invisible — 113 executions "+
				"reported as 28 outcomes, the exact live measurement this file exists for", len(sink.rows), run)
		}
	}
	if act.execs != 2 {
		t.Fatalf("the actuator ran %d times, want 2 — the premise of this test (two real executions of one "+
			"shape) did not hold, so its recording assertions prove nothing", act.execs)
	}
	for n, row := range sink.rows {
		if row.actionID != actionID {
			t.Errorf("row %d carries action id %q, want %q: a row that cannot be tied back to the action it "+
				"records is not evidence", n, row.actionID, actionID)
		}
		if row.verdict != safety.VerdictMatch {
			t.Errorf("row %d verdict = %q, want %q: the row must carry the verdict computed against THIS "+
				"execution's post-state, not a default", n, row.verdict, safety.VerdictMatch)
		}
		if !row.verified {
			t.Errorf("row %d is marked unverified though the post-state verified: recording a checked run as "+
				"unchecked understates the evidence and resets the graduation streak", n)
		}
		// A forward action carries NO inverse reference (TG-404). The inverse case is the next test.
		if row.inverts != "" {
			t.Errorf("row %d marks a forward action as inverting %q — only a compensating rollback may carry "+
				"inverts_action_id", n, row.inverts)
		}
	}
}

// TestInterceptor_RecordsAnInverseWithItsForwardReference — TG-404: when Do executes a request marked as the
// INVERSE of a forward action, the per-execution record carries inverts_action_id, so "did the rollback run,
// and how did it go?" is a query rather than a log-string parse. This is the interceptor half (the sink got
// the reference); action_execution_inverse_test.go is the DB half (the reference is durable).
//
// KILLING MUTATION: drop `r.InvertsActionID` from the Do execution-record call (pass "") — RED here, because
// the recorded inverse is then indistinguishable from a forward action, which is exactly defect #2 of the
// three the ticket's guard pins.
func TestInterceptor_RecordsAnInverseWithItsForwardReference(t *testing.T) {
	ctx := context.Background()
	sink := &recordingExecutionSink{}
	act := &fakeActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithExecutionSink(sink)

	const forwardID = "forward-action-being-undone"
	req := goodRequest(t)
	req.InvertsActionID = forwardID

	out, err := i.Do(ctx, req)
	if err != nil {
		t.Fatalf("Do failed loud: %v", err)
	}
	if !out.Executed {
		t.Fatalf("the inverse must execute for this test to say anything about recording it: %+v", out)
	}
	if len(sink.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1 — the inverse execution was not recorded at all (defect #1)", len(sink.rows))
	}
	row := sink.rows[0]
	if row.inverts != forwardID {
		t.Errorf("the inverse row carries inverts=%q, want %q — an inverse indistinguishable from a forward "+
			"action means the loop-closure register can never count it and TG-82 has no revert evidence", row.inverts, forwardID)
	}
	// The inverse has its OWN content-addressed action_id (the rollback argv's hash), distinct from the forward
	// it undoes: the reference names what it reverses, it is NOT the reverted action's identity.
	if row.actionID == forwardID {
		t.Error("the inverse row's action_id equals the forward id — an inverse is its own execution with its " +
			"own action_id; inverts_action_id is a REFERENCE, not the identity")
	}
}

// spec/029 T-029-3 review finding #1 (the CRITICAL): a CHAIN-GAP execution — the path where the
// manifest lifecycle chain fails to record/verify on an action that ALREADY ran — used to return
// early BEFORE the per-run execution record was written, leaving an executed (and
// breaker-tripping) mutation with NO durable executed-trace at all: StageExecuted unrecorded AND
// the 7b row skipped. The commit-confirm consult then read the run as never-executed and resolved
// its armed window over a live mutation. This drill drives the REAL interceptor into that exact
// branch and demands the sink row anyway.
//
// KILLING MUTATION (executed 2026-08-14): remove the i.executions.Record call from the chain-gap
// early return (restoring the pre-fix shape) — this drill goes red on rows=0 while the breaker
// still trips, which is precisely the reviewed defect. Restored, green.
func TestChainGapExecutionStillWritesThePerRunRecord(t *testing.T) {
	ctx := context.Background()
	sink := &recordingExecutionSink{}
	act := &fakeActuator{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithExecutionSink(sink)

	req := goodRequest(t)
	// Corrupt the sealed lifecycle chain so the post-execute VerifyChain() fails: a bogus stage
	// with a wrong payload hash breaks the link order. The corruption is INVISIBLE to the
	// pre-execute gates (they assert the action identity, not the full chain), so the effect
	// genuinely runs and the interceptor lands in the chain-integrity-gap branch.
	req.Manifest.Stages = append(req.Manifest.Stages, manifest.Stage{
		Stage: manifest.StageName("bogus-tamper"), ActionID: req.Manifest.ActionID, PayloadHash: "tampered", Seq: 999,
	})

	out, err := i.Do(ctx, req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Executed {
		t.Fatalf("precondition: the effect must have EXECUTED for this drill to say anything (out=%+v, execs=%d)", out, act.execs)
	}
	if out.Reason != "manifest lifecycle chain gap" {
		t.Fatalf("precondition: the run must land in the CHAIN-GAP branch, got reason %q — the injection no longer reaches the reviewed path", out.Reason)
	}
	if len(sink.rows) != 1 {
		t.Fatalf("a chain-gap execution left %d per-run record(s), want 1 — an executed mutation with no durable "+
			"executed-trace is invisible to the commit-confirm consult, which would stand its armed revert down "+
			"over a live mutation (the reviewed CRITICAL)", len(sink.rows))
	}
	if sink.rows[0].actionID != out.ActionID {
		t.Fatalf("the chain-gap record must bind the executed action (%q), got %q", out.ActionID, sink.rows[0].actionID)
	}
}
