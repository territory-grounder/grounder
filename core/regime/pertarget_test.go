package regime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// hostBoundFake is a fakeActuator that also declares its bound host — the interceptor's HostBound capability
// (spec/013 REQ-1219) — so a per-target leaf whose ActuationHost()==target can be proven to pass gate 4g.
type hostBoundFake struct {
	fakeActuator
	host string
}

func (h *hostBoundFake) ActuationHost() string { return h.host }

func actuatingSeam() *LaneEffect {
	return NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(safety.NewActuatingChokepoint(), l, audit.NewLedger())
	})
}

// REQ-1717 (P3-B2): the per-target lane builds the effect leaf from the ACTION's own target host and, under an
// actuating chokepoint, drives it through the full spec/013 chain to Exec exactly once — on a leaf bound to
// that target, so the B1 host-match gate (REQ-1219) passes by construction.
func TestPerTargetLaneBuildsLeafForActionTarget(t *testing.T) {
	var gotTarget string
	leaf := &hostBoundFake{fakeActuator: fakeActuator{cap: "ssh", ro: false}}
	lane := NewNativeSSHLaneFunc(func(_ context.Context, target string) (actuation.Actuator, error) {
		gotTarget = target
		leaf.host = target // a correct per-target leaf binds to the target ⇒ ActuationHost()==target
		return leaf, nil
	})
	out, err := actuatingSeam().Apply(context.Background(), lane, goodRequest(t)) // goodRequest targets web01
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if gotTarget != "web01" {
		t.Fatalf("the builder must be called with the action's target host, got %q", gotTarget)
	}
	if !out.Executed || out.Refused || leaf.execs != 1 {
		t.Fatalf("a per-target leaf bound to the target must execute exactly once (host-match gate passes), got %+v execs=%d", out, leaf.execs)
	}
}

// REQ-1717 + INV-09: the per-target lane is DORMANT under Shadow — the mode chokepoint refuses before any
// per-target Exec, so merely BUILDING the per-target leaf arms nothing.
func TestPerTargetLaneDormantUnderShadow(t *testing.T) {
	leaf := &hostBoundFake{fakeActuator: fakeActuator{cap: "ssh", ro: false}}
	lane := NewNativeSSHLaneFunc(func(_ context.Context, target string) (actuation.Actuator, error) {
		leaf.host = target
		return leaf, nil
	})
	seam := NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return actuate.NewInterceptor(safety.NewReadOnlyChokepoint(), l, audit.NewLedger()) // Shadow
	})
	out, err := seam.Apply(context.Background(), lane, goodRequest(t))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Executed || leaf.execs != 0 {
		t.Fatalf("under Shadow the per-target leaf must NOT execute (dormant), got %+v execs=%d", out, leaf.execs)
	}
}

// REQ-1717: a per-target build refusal (no actuation identity, empty target) is a GOVERNED refusal —
// Refused/Executed=false with a NIL error (so the runner records a clean, permanent refusal and never retries
// on a permanent resolution failure), and the reason names the target.
func TestPerTargetLaneBuildErrorIsGovernedRefusal(t *testing.T) {
	lane := NewNativeSSHLaneFunc(func(_ context.Context, _ string) (actuation.Actuator, error) {
		return nil, errors.New("no actuation identity configured")
	})
	out, err := actuatingSeam().Apply(context.Background(), lane, goodRequest(t))
	if err != nil {
		t.Fatalf("a build refusal must be a GOVERNED refusal (nil error, no retry), got err=%v", err)
	}
	if !out.Refused || out.Executed {
		t.Fatalf("a per-target build refusal must be Refused/Executed=false, got %+v", out)
	}
	if !strings.Contains(out.Reason, "web01") {
		t.Fatalf("the refusal reason must name the target, got %q", out.Reason)
	}
}

// The static lane (no per-target build) is unchanged — it uses effectLeaf() and never invokes a per-target
// builder (the behaviour-preserving default when TG_ACTUATION_SSH_PER_TARGET is off).
func TestStaticNativeLaneStillUsesEffectLeaf(t *testing.T) {
	leaf := &fakeActuator{cap: "ssh", ro: false}
	out, err := actuatingSeam().Apply(context.Background(), NewNativeSSHLane(leaf), goodRequest(t))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !out.Executed || leaf.execs != 1 {
		t.Fatalf("the static native lane must still execute via its effect leaf, got %+v execs=%d", out, leaf.execs)
	}
}

// REQ-1717 + B1 (spec/013 REQ-1219): a MIS-built per-target leaf — one whose ActuationHost() does NOT equal
// the action target — is refused by the interceptor's host-match gate BEFORE execute. The per-target lane
// relies on the leaf binding to the target; B1 is the defense-in-depth floor if a builder ever yields the
// wrong host (e.g. a name-form skew), so a mis-bound leaf fails safe rather than mis-actuating.
func TestPerTargetMisboundLeafRefusedByHostMatch(t *testing.T) {
	leaf := &hostBoundFake{fakeActuator: fakeActuator{cap: "ssh", ro: false}, host: "wrong-host"} // != web01
	lane := NewNativeSSHLaneFunc(func(_ context.Context, _ string) (actuation.Actuator, error) {
		return leaf, nil // returns a leaf bound to the WRONG host
	})
	out, err := actuatingSeam().Apply(context.Background(), lane, goodRequest(t)) // goodRequest targets web01
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !out.Refused || out.Executed || leaf.execs != 0 {
		t.Fatalf("a leaf bound to host != the action target must be refused by the host-match gate before execute, got %+v execs=%d", out, leaf.execs)
	}
}
