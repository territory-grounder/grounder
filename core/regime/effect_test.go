package regime

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/actuate"
)

// AN ASYNC LANE MUST NEVER REACH THE SYNCHRONOUS VERIFY PATH.
//
// The awx-job leaf answers a launch with a JOB HANDLE: the job is queued and the estate is untouched when the
// call returns. Driven through the synchronous chain, that handle is adjudicated as a finished mutation — exit
// 0 gives `execute: pass`, an unchanged estate verifies `match`, and `match` is the only promoting graduation
// outcome — so a mutating op-class would climb toward AUTO on launches that later FAILED, with nothing ever
// reading the terminal job status. The deferred-verify channel built for exactly this has no producer wired.
//
// This is inert today only because the AWX lane is unconfigured; that is a config accident, not a control.
// Two env vars would arm it silently. The refusal is therefore structural.
func TestAsyncLaneIsRefusedOnTheSynchronousPath(t *testing.T) {
	var built int
	seam := NewLaneEffect(func(actuation.Actuator) *actuate.Interceptor { built++; return nil })
	out, err := seam.Apply(context.Background(), NewAWXJobLane(), actuate.Request{})
	if err != nil {
		t.Fatalf("the seam must REFUSE cleanly, not fail loud: %v", err)
	}
	if out.Executed {
		t.Fatalf("an async launch must never be reported executed: %+v", out)
	}
	if !out.Refused {
		t.Fatalf("want a governed refusal so the runner records it permanently and does not retry; got %+v", out)
	}
	if built != 0 {
		t.Errorf("the interceptor must not even be built for an async lane, got %d build(s)", built)
	}
	if !strings.Contains(out.Reason, "handle") {
		t.Errorf("the refusal must say WHY, so it is auditable; got %q", out.Reason)
	}
}

// THE COUNTERPART, and the reason this predicate is NOT the neighbouring AsyncObservable. That one is
// `Valid() && != native-ssh`, a REQ-1709 classification that counts PROXMOX as async — but the Proxmox leaf
// polls its task to completion before returning, so its effect HAS landed and its post-state IS immediately
// observable. Gating on AsyncObservable would refuse every start-guest heal: the only op-class at LevelAuto
// and the whole actuation evidence base. This asserts the synchronous lanes stay open.
func TestSynchronousLanesAreNotRefusedByTheAsyncGate(t *testing.T) {
	for _, r := range []Regime{RegimeNativeSSH, RegimeProxmox} {
		if returnsHandleNotOutcome(r) {
			t.Errorf("regime %q completes its effect before returning and MUST stay on the synchronous path — "+
				"refusing it would kill a live heal path", r)
		}
	}
	if !returnsHandleNotOutcome(RegimeAWXJob) {
		t.Error("awx-job returns a launch handle and must be gated")
	}
}
