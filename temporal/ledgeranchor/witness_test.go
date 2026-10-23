package ledgeranchor

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"

	"github.com/territory-grounder/grounder/core/audit"
)

func sampleAnchor() audit.Anchor {
	return audit.ComputeAnchor(audit.HeadState{
		Seq: 6, Hash: "h6",
		Recent: []audit.RowRef{{Seq: 5, Hash: "h5"}, {Seq: 6, Hash: "h6"}},
	})
}

// TestWitnessWorkflowID_DeterministicAndDistinct pins the addressing scheme TemporalWitness.Record and
// ReadWitness both depend on: the SAME (domain, seq) always maps to the SAME Workflow ID (so a verifier can
// look one up without a List/Visibility query), and any change to either input changes the id — two chains,
// or two seqs of the same chain, must never collide onto the same witness.
func TestWitnessWorkflowID_DeterministicAndDistinct(t *testing.T) {
	a := WitnessWorkflowID("governance-ledger", 42)
	b := WitnessWorkflowID("governance-ledger", 42)
	if a != b {
		t.Fatalf("WitnessWorkflowID is not deterministic: %q != %q", a, b)
	}
	if got := WitnessWorkflowID("governance-ledger", 43); got == a {
		t.Fatalf("a different seq produced the same id: %q", got)
	}
	if got := WitnessWorkflowID("knowledge-corpus", 42); got == a {
		t.Fatalf("a different domain produced the same id: %q", got)
	}
}

// TestWitnessAnchorActivity_EchoesInput proves the activity performs no I/O and mutates nothing: whatever
// audit.Anchor it is handed is exactly what it returns. It is invoked as a bare Go function — no Temporal
// machinery required, because that is precisely the interface Temporal's SDK expects an activity to satisfy.
func TestWitnessAnchorActivity_EchoesInput(t *testing.T) {
	in := sampleAnchor()
	out, err := WitnessAnchorActivity(context.Background(), in)
	if err != nil {
		t.Fatalf("WitnessAnchorActivity: %v", err)
	}
	if out != in {
		t.Fatalf("activity must echo its input unchanged: got %+v, want %+v", out, in)
	}
}

// TestWitnessAnchorWorkflow_RoundTrips proves the workflow completes and its RESULT — the fact Temporal's
// server durably records as WorkflowExecutionCompleted — is exactly the anchor it was given. Uses Temporal's
// own WorkflowTestSuite (the same pattern temporal/manifestwrite/manifestwrite_test.go uses), which executes
// the real workflow+activity code without a live server.
func TestWitnessAnchorWorkflow_RoundTrips(t *testing.T) {
	var wts testsuite.WorkflowTestSuite
	env := wts.NewTestWorkflowEnvironment()
	env.RegisterActivity(WitnessAnchorActivity)

	in := sampleAnchor()
	env.ExecuteWorkflow(WitnessAnchorWorkflow, in)

	if !env.IsWorkflowCompleted() {
		t.Fatal("witness workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("witness workflow returned an error: %v", err)
	}
	var out audit.Anchor
	if err := env.GetWorkflowResult(&out); err != nil {
		t.Fatalf("read workflow result: %v", err)
	}
	// Field-by-field, not out != in: Go's time.Time == is documented as unreliable across a serialization
	// boundary (the workflow result travels through Temporal's data converter), even when both values are the
	// zero time — comparing the content that matters avoids a spurious failure on the At field alone.
	if out.Seq != in.Seq || out.Hash != in.Hash || out.WindowSize != in.WindowSize || out.Digest != in.Digest {
		t.Fatalf("the witnessed result must equal the anchor recorded: got %+v, want %+v", out, in)
	}
	if !out.At.IsZero() {
		t.Fatalf("sampleAnchor never sets At — the round trip must not invent one: got %v", out.At)
	}
}

// fakeWorkflowRun is a minimal client.WorkflowRun fake — just enough for TemporalWitness's Get() calls.
// found=false makes Get report NotFound, the same shape client.GetWorkflow's real result gives for a
// Workflow ID nothing has ever started (or that has aged out of retention).
type fakeWorkflowRun struct {
	result audit.Anchor
	found  bool
	err    error
}

func (f *fakeWorkflowRun) GetID() string    { return "fake" }
func (f *fakeWorkflowRun) GetRunID() string { return "fake-run" }
func (f *fakeWorkflowRun) Get(_ context.Context, valuePtr interface{}) error {
	if f.err != nil {
		return f.err
	}
	if !f.found {
		return &serviceerror.NotFound{Message: "workflow execution not found"}
	}
	if p, ok := valuePtr.(*audit.Anchor); ok {
		*p = f.result
	}
	return nil
}

// GetWithOptions completes the client.WorkflowRun interface (unused by TemporalWitness, which only ever
// calls Get) — delegates to it so the fake satisfies the real SDK interface exactly.
func (f *fakeWorkflowRun) GetWithOptions(ctx context.Context, valuePtr interface{}, _ client.WorkflowRunGetOptions) error {
	return f.Get(ctx, valuePtr)
}

// fakeTemporalClient is a minimal temporalClient fake — the whole point of narrowing TemporalWitness.Client
// to temporalClient (witness.go) rather than the full ~30-method client.Client: this fake only needs the two
// methods Record/ReadWitness actually call, which is what makes the squat-detection path (below) testable at
// all without a live Temporal server.
type fakeTemporalClient struct {
	executeErr error            // ExecuteWorkflow's error (a *serviceerror.WorkflowExecutionAlreadyStarted simulates a squat)
	executeRun *fakeWorkflowRun // ExecuteWorkflow's WorkflowRun on success (nil error)
	squatted   *fakeWorkflowRun // what GetWorkflow returns — the "already there" execution a squat pre-registered
	executed   []audit.Anchor   // every anchor ExecuteWorkflow was called with, for assertions
}

func (f *fakeTemporalClient) ExecuteWorkflow(_ context.Context, _ client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	if len(args) == 1 {
		if a, ok := args[0].(audit.Anchor); ok {
			f.executed = append(f.executed, a)
		}
	}
	if f.executeErr != nil {
		return nil, f.executeErr
	}
	return f.executeRun, nil
}

func (f *fakeTemporalClient) GetWorkflow(_ context.Context, _ string, _ string) client.WorkflowRun {
	if f.squatted != nil {
		return f.squatted
	}
	return &fakeWorkflowRun{found: false}
}

// TestTemporalWitness_Record_DetectsSquattedMismatch is the killing oracle for the finding: an attacker with
// tg_runtime's (unauthenticated, see the package doc) network reach pre-registers WitnessWorkflowID(domain,
// seq) with FORGED content before the legitimate recorder gets there. ExecuteWorkflow then returns
// AlreadyStarted exactly as it would for a benign duplicate pass — the ONLY way to tell the two apart is to
// read back what is actually sitting under that id and compare it to what was intended. Before the fix,
// AlreadyStarted was treated as unconditional success and this attack was invisible; after the fix, a content
// mismatch is surfaced, never silently swallowed.
func TestTemporalWitness_Record_DetectsSquattedMismatch(t *testing.T) {
	intended := sampleAnchor()
	squatted := intended
	squatted.Hash = "forged-hash-attacker-planted"
	squatted.Digest = "forged-digest-attacker-planted"

	fc := &fakeTemporalClient{
		executeErr: &serviceerror.WorkflowExecutionAlreadyStarted{Message: "already started"},
		squatted:   &fakeWorkflowRun{found: true, result: squatted},
	}
	w := &TemporalWitness{Client: fc, TaskQueue: "tg.runner", Domain: "governance-ledger"}

	err := w.Record(context.Background(), intended)
	if err == nil {
		t.Fatal("a squatted witness with DIFFERENT content must surface an error — silent success is exactly the false-clean the finding named")
	}
	if !errors.Is(err, ErrTemporalWitnessMismatch) {
		t.Fatalf("the squat mismatch must be ErrTemporalWitnessMismatch-shaped so callers can errors.Is it: got %v", err)
	}
}

// TestTemporalWitness_Record_TrueDuplicateStaysIdempotent proves the fix did not overcorrect: a genuinely
// benign AlreadyStarted (the SAME recorder re-witnessing a seq it already recorded — a restart, a retried
// pass) with MATCHING content must still succeed silently, exactly as the pre-fix behavior intended for the
// case it was actually meant to cover.
func TestTemporalWitness_Record_TrueDuplicateStaysIdempotent(t *testing.T) {
	a := sampleAnchor()
	fc := &fakeTemporalClient{
		executeErr: &serviceerror.WorkflowExecutionAlreadyStarted{Message: "already started"},
		squatted:   &fakeWorkflowRun{found: true, result: a}, // identical content — a genuine prior witness of the SAME anchor
	}
	w := &TemporalWitness{Client: fc, TaskQueue: "tg.runner", Domain: "governance-ledger"}

	if err := w.Record(context.Background(), a); err != nil {
		t.Fatalf("matching content on AlreadyStarted must stay idempotent success: %v", err)
	}
}

// TestTemporalWitness_Record_SquatReadBackFailureIsNotSwallowed: if AlreadyStarted fires but the read-back
// itself fails (a transient Temporal error, not NotFound), Record must surface that failure rather than fall
// back to either "clean" or "tamper" — an unverifiable squat is not a verified one.
func TestTemporalWitness_Record_SquatReadBackFailureIsNotSwallowed(t *testing.T) {
	fc := &fakeTemporalClient{
		executeErr: &serviceerror.WorkflowExecutionAlreadyStarted{Message: "already started"},
		squatted:   &fakeWorkflowRun{err: errors.New("temporal unavailable")},
	}
	w := &TemporalWitness{Client: fc, TaskQueue: "tg.runner", Domain: "governance-ledger"}

	if err := w.Record(context.Background(), sampleAnchor()); err == nil {
		t.Fatal("a read-back failure after AlreadyStarted must surface as an error, not silent success")
	}
}

// TestTemporalWitness_Record_SquatNotFoundIsNotSwallowed: AlreadyStarted but the read-back reports NotFound
// (an inconsistent view — e.g. the racing execution has not completed) must also surface, not pass.
func TestTemporalWitness_Record_SquatNotFoundIsNotSwallowed(t *testing.T) {
	fc := &fakeTemporalClient{
		executeErr: &serviceerror.WorkflowExecutionAlreadyStarted{Message: "already started"},
		squatted:   &fakeWorkflowRun{found: false},
	}
	w := &TemporalWitness{Client: fc, TaskQueue: "tg.runner", Domain: "governance-ledger"}

	if err := w.Record(context.Background(), sampleAnchor()); err == nil {
		t.Fatal("AlreadyStarted with an unreadable existing witness must surface as an error, not silent success")
	}
}

// TestTemporalWitness_Record_NormalPathUnaffected proves the ordinary, non-squat, non-duplicate path (a fresh
// seq, ExecuteWorkflow succeeds outright) still works after narrowing Client to temporalClient and adding the
// compare branch — the fix must not regress the common case.
func TestTemporalWitness_Record_NormalPathUnaffected(t *testing.T) {
	a := sampleAnchor()
	fc := &fakeTemporalClient{executeRun: &fakeWorkflowRun{found: true, result: a}}
	w := &TemporalWitness{Client: fc, TaskQueue: "tg.runner", Domain: "governance-ledger"}

	if err := w.Record(context.Background(), a); err != nil {
		t.Fatalf("the normal (non-squat) path must still succeed: %v", err)
	}
	if len(fc.executed) != 1 || fc.executed[0] != a {
		t.Fatalf("ExecuteWorkflow must have been called with the intended anchor: got %+v", fc.executed)
	}
}
