package predict

// TG-378: the seal-time STATE PRECONDITION. During the 2026-08-06 pve03 cascade, three of the four sealed
// start-guest manifests targeted guests that were RUNNING the whole time (897h/2,103h uptimes); nothing
// between "the model proposed start" and "seal the manifest" asked, and the only gate that stopped them
// was an unrelated global band. These oracles pin the closed contract: an op-class declaring
// requires_target_state seals ONLY over an observation that satisfies it, records that observation on the
// manifest, and REFUSES — leaving no committed prediction behind — when the state is violated, unknown,
// or the reader is unwired. Unknown is not not-running.
//
// KILLING MUTATIONS (each executed 2026-08-11, RED, then reverted — see the MR record):
//  1. delete the checkStatePrecondition call in Commit → TestAStartOnARunningGuestRefusesAtSeal seals and
//     fails with "sealed a start for a RUNNING guest".
//  2. treat ok=false as not-running (`if !ok { running = false }` fall-through) →
//     TestAnUnestablishablePreconditionRefuses fails: an unobservable target sealed.
//  3. treat a nil reader as "no precondition" → TestAnUnwiredStateReaderRefuses fails.
//  4. return the observation for EVERY class (drop the RequiresTargetState=="" early return) →
//     TestClassesWithoutAPreconditionAreUntouched fails (restart-service refused with no reader wired —
//     the over-block direction, which is how a safety check gets reverted in anger).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
)

func startGuestProposal() proposal.Proposal {
	p, err := proposal.ParseProposal([]byte(`{"external_ref":"TG-378","target":"dc1nc01","op_class":"start-guest","op":"start","params":{"guest":"dc1nc01"},"confidence":0.9}`))
	if err != nil {
		panic(err)
	}
	return p
}

// fixed returns a reader that always answers (running, ok) with a fixed provenance.
func fixedReader(running, ok bool) func(context.Context, string) (bool, string, bool) {
	return func(context.Context, string) (bool, string, bool) { return running, "test reader", ok }
}

func TestAStartOnARunningGuestRefusesAtSeal(t *testing.T) {
	g := testGate(ModeEnforce)
	g.GuestRunning = fixedReader(true, true)
	ctx := context.Background()

	_, err := g.Commit(ctx, startGuestProposal(), "plan-378-run", "dc1", safety.BandPollPause, false)
	if !errors.Is(err, ErrPreconditionViolated) {
		t.Fatalf("sealed a start for a RUNNING guest (err=%v) — the pve03 defect intact", err)
	}
	// The refusal must be BEFORE the prediction commits: a refused action leaves no authorizing artifact.
	if has, _ := g.Store.Has(ctx, "plan-378-run"); has {
		t.Fatal("a refused seal left a committed prediction behind — the refusal ran too late")
	}
}

func TestAnUnestablishablePreconditionRefuses(t *testing.T) {
	g := testGate(ModeEnforce)
	g.GuestRunning = fixedReader(false, false) // reader answers: could not establish
	_, err := g.Commit(context.Background(), startGuestProposal(), "plan-378-unk", "dc1", safety.BandPollPause, false)
	if !errors.Is(err, ErrPreconditionUnestablished) {
		t.Fatalf("an unobservable target must refuse (unknown != not-running), got %v", err)
	}
}

func TestAnUnwiredStateReaderRefuses(t *testing.T) {
	g := testGate(ModeEnforce) // GuestRunning deliberately nil
	_, err := g.Commit(context.Background(), startGuestProposal(), "plan-378-nil", "dc1", safety.BandPollPause, false)
	if !errors.Is(err, ErrPreconditionUnestablished) {
		t.Fatalf("an UNWIRED reader must refuse a state-preconditioned class — a gate whose seam nothing "+
			"fills must not become a gate that checks nothing; got %v", err)
	}
}

func TestAnObservedStoppedGuestSealsAndRecordsTheObservation(t *testing.T) {
	g := testGate(ModeEnforce)
	g.GuestRunning = fixedReader(false, true) // observed stopped
	ctx := context.Background()

	gp, err := g.Commit(ctx, startGuestProposal(), "plan-378-ok", "dc1", safety.BandPollPause, false)
	if err != nil {
		t.Fatalf("an observed-stopped guest is the legitimate start case and must seal: %v", err)
	}
	obs := gp.Manifest().PreconditionObservation
	if !strings.Contains(obs, "not-running") || !strings.Contains(obs, "dc1nc01") {
		t.Fatalf("the satisfying observation must be recorded on the manifest, got %q", obs)
	}
	if has, _ := g.Store.Has(ctx, "plan-378-ok"); !has {
		t.Fatal("the sealed path must still commit its prediction")
	}
}

// TestClassesWithoutAPreconditionAreUntouched is the anti-over-block control: restart-service declares no
// requires_target_state, so it seals with NO reader wired and records no observation — the precondition
// gate must never widen itself into a blanket state check nobody declared.
func TestClassesWithoutAPreconditionAreUntouched(t *testing.T) {
	g := testGate(ModeEnforce) // nil reader
	gp, err := g.Commit(context.Background(), testProposal(), "plan-378-rs", "dc1", safety.BandPollPause, true)
	if err != nil {
		t.Fatalf("a class with no declared precondition must be untouched by the state gate: %v", err)
	}
	if gp.Manifest().PreconditionObservation != "" {
		t.Fatalf("a class with no precondition must record no observation, got %q", gp.Manifest().PreconditionObservation)
	}
}