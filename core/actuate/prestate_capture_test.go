package actuate

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// recordingPreStateSink captures every pre-mutation snapshot the interceptor persists, so a test can assert the
// chokepoint captured it at the last pre-effect instant and bound it to the executed action.
type recordingPreStateSink struct {
	rows []preStateRow
	err  error
}

type preStateRow struct {
	actionID string
	state    PreState
}

func (s *recordingPreStateSink) RecordPreState(_ context.Context, actionID string, st PreState) error {
	s.rows = append(s.rows, preStateRow{actionID: actionID, state: st})
	return s.err
}

// TG-58: a wired CaptureState hook + PreStateSink snapshot the target's pre-mutation state at the last pre-effect
// instant and persist it bound to the EXECUTED action, so a Phase-2 applied-undo has a concrete restore point.
func TestPreStateCapture_RecordsSnapshotForAnExecutedMutation(t *testing.T) {
	act := &fakeActuator{}
	sink := &recordingPreStateSink{}
	i := wired(safety.NewActuatingChokepoint(), act).WithPreStateSink(sink)

	req := goodRequest(t)
	want := PreState{Kind: "service", Data: []byte("nginx=active,enabled")}
	req.CaptureState = func(context.Context) (PreState, bool) { return want, true }

	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("expected a real executed mutation, got %+v execs=%d", out, act.execs)
	}
	if len(sink.rows) != 1 {
		t.Fatalf("pre-state sink recorded %d rows, want 1 (captured pre-effect, stored for the executed action)", len(sink.rows))
	}
	if sink.rows[0].actionID != out.ActionID {
		t.Fatalf("pre-state must bind to the executed action id: sink=%q executed=%q", sink.rows[0].actionID, out.ActionID)
	}
	if sink.rows[0].state.Kind != "service" || string(sink.rows[0].state.Data) != "nginx=active,enabled" {
		t.Fatalf("the caller's snapshot must reach the sink verbatim, got %+v", sink.rows[0].state)
	}
}

// Dormant: no CaptureState hook ⇒ nothing captured, nothing recorded — the pre-effect path is byte-identical.
// KILLING MUTATION: capture unconditionally (drop the `if r.CaptureState != nil` guard) → this records a row.
func TestPreStateCapture_NilHookIsDormant(t *testing.T) {
	act := &fakeActuator{}
	sink := &recordingPreStateSink{}
	i := wired(safety.NewActuatingChokepoint(), act).WithPreStateSink(sink)

	out, err := i.Do(context.Background(), goodRequest(t)) // goodRequest wires no CaptureState
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Executed {
		t.Fatalf("expected an executed mutation, got %+v", out)
	}
	if len(sink.rows) != 0 {
		t.Fatalf("no CaptureState hook must record NO pre-state, got %d rows", len(sink.rows))
	}
}

// A failed capture is NON-FATAL — pre-state capture is rollback prep, not a safety gate: the mutation still
// executes (a missing restore point is a Phase-2 arming concern, never a reason to refuse a safe heal now).
func TestPreStateCapture_FailedCaptureIsNonFatal(t *testing.T) {
	act := &fakeActuator{}
	sink := &recordingPreStateSink{}
	i := wired(safety.NewActuatingChokepoint(), act).WithPreStateSink(sink)

	req := goodRequest(t)
	req.CaptureState = func(context.Context) (PreState, bool) { return PreState{}, false } // capture cannot be read

	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !out.Executed || act.execs != 1 {
		t.Fatalf("a failed pre-state capture must NOT block the mutation: %+v execs=%d", out, act.execs)
	}
	if len(sink.rows) != 0 {
		t.Fatalf("a failed capture must record no pre-state, got %d rows", len(sink.rows))
	}
}

// A NO-OP mutation (target already in goal state) returns BEFORE the store block, so no pre-state is persisted
// even with a wired hook+sink — the capture ran at the pre-effect instant, but only a REAL mutation records one.
// This pins the structural guarantee (the no-op early-return sits before the store) against a future refactor.
func TestPreStateCapture_NoOpMutationStoresNoPreState(t *testing.T) {
	sink := &recordingPreStateSink{}
	i := actuatingInterceptor(safety.NewActuatingChokepoint(), &noOpActuator{}, audit.NewLedger()).WithPreStateSink(sink)

	req := goodRequest(t)
	req.CaptureState = func(context.Context) (PreState, bool) { return PreState{Kind: "service", Data: []byte("x")}, true }

	out, err := i.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Executed {
		t.Fatalf("a no-op must report Executed=false, got %+v", out)
	}
	if len(sink.rows) != 0 {
		t.Fatalf("a no-op mutation must persist NO pre-state (only real mutations do), got %d rows", len(sink.rows))
	}
}
