package regime

// TG-122 slice 0 — the deferred-verify PRODUCER's launch dispatch (ApplyReserved). These oracles pin the
// three properties the seam exists for: an async launch's handle is CAPTURED onto the Outcome (else the
// deferred channel can never poll it), the plain Apply refusal is UNCHANGED even for a fully-wired async
// lane (the refusal is about the PATH, not the wiring), and ApplyReserved adds no bypass (the mode
// chokepoint still refuses at Shadow before the leaf runs; a synchronous lane is refused outright).

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// handleFake is a fakeActuator whose Exec answers with a job handle on stdout — the awxjob/gitopsmr launch
// shape (the job id / MR ref, trailing newline included to prove the capture trims).
type handleFake struct {
	fakeActuator
	stdout string
}

func (h *handleFake) Exec(context.Context, []string, []byte) (actuation.Result, error) {
	h.execs++
	return actuation.Result{Stdout: []byte(h.stdout)}, nil
}

// KILLING MUTATION: drop the handleCaptureActuator wrap (or the post-fill) in ApplyReserved → AsyncHandle
// comes back empty and this reddens — the exact silent gap effect.go:78 documented ("the job id is
// discarded at this boundary").
func TestApplyReservedLaunchesAsyncLaneAndCapturesHandle(t *testing.T) {
	leaf := &handleFake{fakeActuator: fakeActuator{cap: "awx-job", ro: false}, stdout: "job-42\n"}
	lane := NewAWXJobLane(WithAWXActuator(leaf))
	req := goodRequest(t)
	// The producer's contract: the observer answers (nil, false) — TG-182 UNVERIFIABLE, so the launch
	// executes but no inline verdict is minted; the deferred channel adjudicates. (A NIL observer would
	// refuse pre-execute: "cannot verify ⇒ will not execute".)
	req.Observe = func(context.Context) ([]verify.ObservedAlert, bool) { return nil, false }
	// The HOST arm of the pre-action baseline: with the deferred observer answering false, the baseline
	// gate stands on this arm (as on any pool-bearing deployment).
	req.PreAnomalous = func(context.Context) (map[string]bool, bool) { return map[string]bool{"web01": true}, true }
	out, err := actuatingSeam().ApplyReserved(context.Background(), lane, req)
	if err != nil {
		t.Fatalf("ApplyReserved: %v", err)
	}
	if !out.Executed || out.Refused {
		t.Fatalf("a reserved async launch through an actuating chain must execute (launch), got %+v", out)
	}
	if leaf.execs != 1 {
		t.Fatalf("the leaf must Exec exactly once, got %d", leaf.execs)
	}
	if out.AsyncHandle != "job-42" {
		t.Fatalf("the launch handle must be captured (trimmed) onto the Outcome for BindHandle, got %q", out.AsyncHandle)
	}
}

// The PLAIN Apply refusal must be unchanged even for a fully-wired async lane: the structural guard is
// about the synchronous PATH adjudicating a handle, not about whether an actuator happens to be injected.
// KILLING MUTATION: route Apply's async branch through ApplyReserved's launch → this reddens.
func TestApplyStillRefusesAsyncLaneEvenWithActuatorInjected(t *testing.T) {
	leaf := &handleFake{fakeActuator: fakeActuator{cap: "awx-job", ro: false}, stdout: "job-1"}
	out, err := actuatingSeam().Apply(context.Background(), NewAWXJobLane(WithAWXActuator(leaf)), goodRequest(t))
	if err != nil {
		t.Fatalf("Apply must refuse cleanly: %v", err)
	}
	if out.Executed || !out.Refused || leaf.execs != 0 {
		t.Fatalf("Apply must keep the structural async refusal (no launch), got %+v execs=%d", out, leaf.execs)
	}
	if out.AsyncHandle != "" {
		t.Fatalf("a refused launch must carry no handle, got %q", out.AsyncHandle)
	}
}

// ApplyReserved is the ASYNC dispatch only — a synchronous lane routed at it is refused (governed) before
// the interceptor is even built, so the two entry points stay disjoint.
func TestApplyReservedRefusesSynchronousLane(t *testing.T) {
	var built int
	seam := NewLaneEffect(func(actuation.Actuator) *actuate.Interceptor { built++; return nil })
	leaf := &fakeActuator{cap: "ssh", ro: false}
	out, err := seam.ApplyReserved(context.Background(), NewNativeSSHLane(leaf), goodRequest(t))
	if err != nil {
		t.Fatalf("must refuse cleanly: %v", err)
	}
	if out.Executed || !out.Refused || built != 0 || leaf.execs != 0 {
		t.Fatalf("a synchronous lane must be refused by ApplyReserved before any build/exec, got %+v built=%d execs=%d", out, built, leaf.execs)
	}
}

// ApplyReserved adds NO bypass: at Shadow the mode chokepoint refuses before the leaf runs, exactly as on
// every other path. A reserved launch is still a GOVERNED launch.
func TestApplyReservedDormantUnderShadow(t *testing.T) {
	leaf := &handleFake{fakeActuator: fakeActuator{cap: "awx-job", ro: false}, stdout: "job-9"}
	seam := NewLaneEffect(func(l actuation.Actuator) *actuate.Interceptor {
		return withPermissivePolicy(actuate.NewInterceptor(safety.NewReadOnlyChokepoint(), l, audit.NewLedger()))
	})
	req := goodRequest(t)
	req.Observe = nil
	out, err := seam.ApplyReserved(context.Background(), NewAWXJobLane(WithAWXActuator(leaf)), req)
	if err != nil {
		t.Fatalf("ApplyReserved at Shadow: %v", err)
	}
	if out.Executed || leaf.execs != 0 {
		t.Fatalf("ApplyReserved must not launch at Shadow (mode chokepoint), got %+v execs=%d", out, leaf.execs)
	}
	if out.AsyncHandle != "" {
		t.Fatalf("no launch ⇒ no handle, got %q", out.AsyncHandle)
	}
	if !strings.Contains(strings.ToLower(out.Reason), "mode") && !strings.Contains(strings.ToLower(out.Reason), "shadow") && !strings.Contains(strings.ToLower(out.Reason), "actuat") {
		t.Logf("refusal reason (informational): %q", out.Reason)
	}
}
