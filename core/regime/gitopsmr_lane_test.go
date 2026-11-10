package regime

// TG-122: the gitops-mr lane joins awx-job as the second ASYNC lane — an opened MR is a handle, not a completed
// effect (the estate is untouched until a human merges and Atlantis/Argo reconciles). These oracles pin the two
// structural guards that keep it fail-closed and OFF the synchronous verify path, mirroring the awx-job oracles.

import (
	"context"
	"errors"
	"strings"
	"testing"

	actuation "github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
)

// TestGitOpsMRLaneIsAsyncRefusedOnTheSynchronousPath: LaneEffect.Apply must REFUSE the gitops-mr lane on the
// synchronous verify path (never build the interceptor, never report executed) until the deferred-verify
// producer is wired. KILLING MUTATION: drop RegimeGitOpsMR from returnsHandleNotOutcome → Apply would
// adjudicate a not-yet-merged MR as a finished mutation → this goes RED.
func TestGitOpsMRLaneIsAsyncRefusedOnTheSynchronousPath(t *testing.T) {
	var built int
	seam := NewLaneEffect(func(actuation.Actuator) *actuate.Interceptor { built++; return nil })
	out, err := seam.Apply(context.Background(), NewGitOpsMRLane(), actuate.Request{})
	if err != nil {
		t.Fatalf("the seam must REFUSE cleanly, not fail loud: %v", err)
	}
	if out.Executed {
		t.Fatalf("an opened MR must never be reported executed (estate untouched until human merge+reconcile): %+v", out)
	}
	if !out.Refused {
		t.Fatalf("want a governed refusal the runner records permanently and does not retry; got %+v", out)
	}
	if built != 0 {
		t.Errorf("the interceptor must not even be built for an async lane, got %d build(s)", built)
	}
	if !strings.Contains(out.Reason, "handle") {
		t.Errorf("the refusal must say WHY, so it is auditable; got %q", out.Reason)
	}
}

// TestGitOpsMRLaneMembership: gitops-mr MUST be async-gated; the synchronous lanes MUST NOT be.
func TestGitOpsMRLaneMembership(t *testing.T) {
	if !returnsHandleNotOutcome(RegimeGitOpsMR) {
		t.Error("gitops-mr returns an MR handle and MUST be gated onto the deferred-verify path")
	}
	for _, r := range []Regime{RegimeNativeSSH, RegimeProxmox} {
		if returnsHandleNotOutcome(r) {
			t.Errorf("regime %q completes its effect before returning and must stay synchronous", r)
		}
	}
}

// TestGitOpsMRLaneFailsClosedUntilWired: the lane's default leaf is the fail-closed pendingActuator (read-only,
// refuses ErrLaneNotWired), and the engine registers it under RegimeGitOpsMR for kind-routing. Injecting no
// actuator leaves it refusing — the arm-live actuator is a later slice.
func TestGitOpsMRLaneFailsClosedUntilWired(t *testing.T) {
	l := NewGitOpsMRLane()
	if l.Regime() != RegimeGitOpsMR {
		t.Fatalf("gitops-mr lane Regime()=%q, want %q", l.Regime(), RegimeGitOpsMR)
	}
	e := NewEngine(nil, []Lane{NewGitOpsMRLane()})
	lane, ok := e.LaneForRegime(RegimeGitOpsMR)
	if !ok || lane == nil {
		t.Fatal("engine must register the gitops-mr lane for kind-routing")
	}
	leaf := lane.effectLeaf()
	if !leaf.ReadOnly() {
		t.Fatal("the unwired gitops-mr lane's leaf must be read-only (pendingActuator) — mutation stays OFF")
	}
	if _, err := leaf.Exec(context.Background(), []string{"gitops-mr-open"}, []byte("{}")); !errors.Is(err, ErrLaneNotWired) {
		t.Fatalf("unwired gitops-mr leaf Exec err=%v, want ErrLaneNotWired", err)
	}
	if !RegimeGitOpsMR.Valid() {
		t.Fatal("RegimeGitOpsMR must be a Valid regime")
	}
}

// TestWithGitOpsMRActuatorInjectsTheLeaf: WithGitOpsMRActuator replaces the pending leaf; a nil actuator is
// ignored (keeps the refusing placeholder — never an unwired live leaf).
func TestWithGitOpsMRActuatorInjectsTheLeaf(t *testing.T) {
	l := NewGitOpsMRLane(WithGitOpsMRActuator(stubActuator{capability: "gitops-mr"}))
	if got := l.effectLeaf().Capability(); got != "gitops-mr" {
		t.Fatalf("injected leaf Capability()=%q, want gitops-mr", got)
	}
	l2 := NewGitOpsMRLane(WithGitOpsMRActuator(nil))
	if _, err := l2.effectLeaf().Exec(context.Background(), nil, nil); !errors.Is(err, ErrLaneNotWired) {
		t.Fatalf("a nil actuator must be ignored (keep pendingActuator); Exec err=%v", err)
	}
}

type stubActuator struct{ capability string }

func (s stubActuator) Capability() string { return s.capability }
func (s stubActuator) ReadOnly() bool     { return false }
func (s stubActuator) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	return actuation.Result{}, nil
}
