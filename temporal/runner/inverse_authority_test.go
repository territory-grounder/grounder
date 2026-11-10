package runner

import (
	"context"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/temporal/wfauth"
)

type inverseExecReader struct {
	row   db.ForwardExecution
	found bool
	prior []db.InverseExecution
}

func (r inverseExecReader) ExecutionFor(context.Context, string, string) (db.ForwardExecution, bool, error) {
	return r.row, r.found, nil
}

func (r inverseExecReader) InversesOf(context.Context, string, string) ([]db.InverseExecution, error) {
	return r.prior, nil
}

var _ interface {
	InversesOf(context.Context, string, string) ([]db.InverseExecution, error)
} = (*db.ActionExecutionStore)(nil)

type inverseNonces struct{}

func (inverseNonces) Claim(context.Context, string, string, time.Time) (bool, error) {
	return true, nil
}

var inverseKey = []byte("inverse-authority-test-key-0123456789abcdef")

type inverseManifests struct {
	m   *manifest.ActionManifest
	err error
}

func (r inverseManifests) Get(context.Context, string) (*manifest.ActionManifest, bool, error) {
	return r.m, r.m != nil, r.err
}

var inverseForward = manifest.Action{OpClass: "start-guest", Target: "guest01", Params: map[string]string{"guest": "guest01"}, Reversible: true}

func inverseForwardID() string {
	id, err := inverseForward.ID()
	if err != nil {
		panic(err)
	}
	return id
}

func inverseSealedForward(t *testing.T) *manifest.ActionManifest {
	t.Helper()
	m, err := manifest.New(inverseForward, safety.BandPollPause, "plan", "prediction")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func inverseActivities(t *testing.T, row db.ForwardExecution, found bool) *Activities {
	t.Helper()
	v, err := wfauth.NewVerifier(inverseKey, inverseNonces{})
	if err != nil {
		t.Fatal(err)
	}
	return &Activities{D: Deps{Executions: inverseExecReader{row: row, found: found}, WorkflowAuth: v, Manifests: inverseManifests{m: inverseSealedForward(t)}}}
}

func TestUndoFactsCarryTheSealedForwardAction(t *testing.T) {
	a := inverseActivities(t, db.ForwardExecution{TargetHost: "guest01", ExecutedAt: time.Now().UTC()}, true)
	f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true})
	if !ok || f.Forward == nil || f.Forward.Params["guest"] != "guest01" {
		t.Fatalf("facts = %+v ok=%v", f, ok)
	}
}

func TestUndoFactsDropAForwardWhoseContentDoesNotMatchItsID(t *testing.T) {
	a := inverseActivities(t, db.ForwardExecution{TargetHost: "guest01", ExecutedAt: time.Now().UTC()}, true)
	in := inverseOrder()
	in.ForwardActionID = "not-the-sealed-id"
	f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: in, AutoFired: true})
	if !ok || f.Forward != nil {
		t.Fatalf("facts = %+v ok=%v, want readable with no forward", f, ok)
	}
}

func TestUndoFactsAreUnreadableWhenTheManifestStoreFails(t *testing.T) {
	a := inverseActivities(t, db.ForwardExecution{TargetHost: "guest01", ExecutedAt: time.Now().UTC()}, true)
	a.D.Manifests = inverseManifests{err: context.DeadlineExceeded}
	if _, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true}); ok {
		t.Fatal("a failed manifest read must read as unreadable")
	}
	a.D.Manifests = nil
	if _, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true}); ok {
		t.Fatal("a missing manifest store must read as unreadable")
	}
}

func signedRollback(t *testing.T, in RollbackInput) RollbackInput {
	t.Helper()
	s, err := wfauth.NewSigner(inverseKey)
	if err != nil {
		t.Fatal(err)
	}
	env, err := s.Sign(RollbackAuthKind, in.AuthFields()...)
	if err != nil {
		t.Fatal(err)
	}
	in.Auth = env
	return in
}

func inverseOrder() RollbackInput {
	return RollbackInput{ForwardActionID: inverseForwardID(), ForwardExternalRef: "ref", ForwardTarget: "guest01", Operator: "op", RollbackExternalRef: "rb"}
}

func TestUndoFactsCarryTheActuationRecord(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	a := inverseActivities(t, db.ForwardExecution{TargetHost: "guest01", ExecutedAt: at, Verdict: "deviation", Unverifiable: true}, true)
	f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true})
	if !ok || !f.Found || f.TargetHost != "guest01" || !f.ExecutedAt.Equal(at) || f.Verdict != "deviation" || !f.Unverifiable || !f.AutoFired {
		t.Fatalf("facts = %+v ok=%v", f, ok)
	}
}

func TestUndoFactsMarkOnlyAValidManualSignature(t *testing.T) {
	a := inverseActivities(t, db.ForwardExecution{TargetHost: "guest01", ExecutedAt: time.Now().UTC()}, true)
	if f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder()}); !ok || f.Signed {
		t.Fatalf("unsigned: signed=%v ok=%v", f.Signed, ok)
	}
	if f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: signedRollback(t, inverseOrder())}); !ok || !f.Signed {
		t.Fatalf("signed: signed=%v ok=%v", f.Signed, ok)
	}
	tampered := signedRollback(t, inverseOrder())
	tampered.ForwardTarget = "guest02"
	if f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: tampered}); !ok || f.Signed {
		t.Fatalf("tampered: signed=%v ok=%v", f.Signed, ok)
	}
}

func TestUndoFactsAreUnreadableWithoutTheRecordStore(t *testing.T) {
	a := &Activities{}
	if _, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder()}); ok {
		t.Fatal("a missing execution reader must read as unreadable")
	}
}

func TestUndoFactsAllowARetryAfterAnIndeterminateUndo(t *testing.T) {
	v, err := wfauth.NewVerifier(inverseKey, inverseNonces{})
	if err != nil {
		t.Fatal(err)
	}
	a := &Activities{D: Deps{Executions: inverseExecReader{found: true, prior: []db.InverseExecution{{Verdict: db.VerdictIndeterminate}}}, WorkflowAuth: v, Manifests: inverseManifests{m: inverseSealedForward(t)}}}
	f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true})
	if !ok || f.AlreadyUndone {
		t.Fatalf("an undo whose acknowledgement was lost must stay retryable: %+v ok=%v", f, ok)
	}
}

func TestUndoFactsSeeAPriorUndo(t *testing.T) {
	v, err := wfauth.NewVerifier(inverseKey, inverseNonces{})
	if err != nil {
		t.Fatal(err)
	}
	a := &Activities{D: Deps{Executions: inverseExecReader{found: true, prior: []db.InverseExecution{{Verdict: "match"}}}, WorkflowAuth: v, Manifests: inverseManifests{m: inverseSealedForward(t)}}}
	f, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true})
	if !ok || !f.AlreadyUndone {
		t.Fatalf("already undone: %+v ok=%v", f, ok)
	}
}

type recordOnlyReader struct{}

func (recordOnlyReader) ExecutionFor(context.Context, string, string) (db.ForwardExecution, bool, error) {
	return db.ForwardExecution{}, true, nil
}

func TestUndoFactsAreUnreadableWithoutAPriorUndoReader(t *testing.T) {
	a := &Activities{D: Deps{Executions: recordOnlyReader{}}}
	if _, ok := a.undoFacts(context.Background(), RollbackExecuteInput{In: inverseOrder(), AutoFired: true}); ok {
		t.Fatal("no way to read prior undos must read as unreadable")
	}
}
