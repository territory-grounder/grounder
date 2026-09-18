package actuate

// TG-146 A3, the ≥2-deep half. The classify-time floor already sees the params (temporal/runner
// actionSafetyParts → manifest.Action.SafetyParts — the A3 first half, verified fixed on-ticket); the
// ADAPTER floor did not: IsDestructiveOp saw only (Op, OpClass) and NO stateful check existed here at
// all, so for the awx/k8s/mcp/proxmox lanes (no leaf check of their own — only the ssh leaf reads the
// unit) a mis-banded request's only pre-effect depth was params-blind. The exact measured shape:
// Target "dc1app01" / Op "restart-service" / Params{"unit":"mariadb.service"} — the database appears
// ONLY in the params.
//
// KILLING MUTATIONS (each executed 2026-08-11, RED, then reverted):
//  1. delete the stateful-floor arm → TestAStatefulParamUnderANonVotedBandRefusesAtTheAdapter fails
//     ("REACHED THE LEAF") — the pre-fix behavior, a database restart executing under an AUTO band.
//  2. revert IsDestructiveOp(floorParts...) to (Op, OpClass) → TestADestructiveParamValueRefusesAtTheFloor
//     fails — a destructive token hiding in a param value sails past the adapter floor.

import (
	"context"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/safety"
)

// paramsRequest builds a request whose ONLY identity-bearing content is in Params — the A3 shape.
func paramsRequest(t *testing.T, band safety.Band, params map[string]string) Request {
	t.Helper()
	a := manifest.Action{Target: "dc1app01", OpClass: "restart-service", Op: "restart", Params: params, Reversible: true}
	m, err := manifest.New(a, band, "plan-146", "pred-146")
	if err != nil {
		t.Fatal(err)
	}
	r := goodRequest(t)
	r.Manifest = m
	r.Band = band
	r.Argv = []string{"systemctl", "restart", params["unit"]}
	return r
}

func TestAStatefulParamUnderANonVotedBandRefusesAtTheAdapter(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	out, err := i.Do(context.Background(), paramsRequest(t, safety.BandAuto, map[string]string{"unit": "mariadb.service"}))
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 || out.Executed {
		t.Fatalf("a MARIADB restart under an AUTO band REACHED THE LEAF (%d exec) — the database appeared "+
			"only in the params and the adapter floor never looked there (the measured A3 shape)", act.execs)
	}
	if !out.Refused || !strings.Contains(out.Reason, "stateful floor") {
		t.Fatalf("the refusal must come from the adapter's stateful floor with its band diagnosis, got %q", out.Reason)
	}
}

func TestADestructiveParamValueRefusesAtTheFloor(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	// Benign Op/OpClass; the destructive token hides in a param VALUE. Band AUTO so the admission gate
	// (which requires a recorded approval for POLL_PAUSE, upstream of the floor) does not mask the floor.
	out, err := i.Do(context.Background(), paramsRequest(t, safety.BandAuto, map[string]string{"unit": "cleanup", "cmd": "dropdb production"}))
	if err != nil {
		t.Fatalf("failed loud instead of refusing: %v", err)
	}
	if act.execs != 0 || !out.Refused || !strings.Contains(out.Reason, "never-auto floor") {
		t.Fatalf("a destructive token in a param value must hit the adapter's never-auto floor: %+v execs=%d", out, act.execs)
	}
}

// TestAVotedStatefulMutationStillExecutes is the anti-over-block control: POLL_PAUSE is the human-voted
// band — the classify-time floor put it there and the vote happened — so the adapter's stateful floor
// must NOT re-refuse it (a stateful mutation that can never execute even voted would be the over-block
// that gets a safety control reverted in anger).
func TestAVotedStatefulMutationStillExecutes(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	r := paramsRequest(t, safety.BandPollPause, map[string]string{"unit": "mariadb.service"})
	r.Approved = true // the human vote — without it the ADMISSION gate refuses upstream and this control
	// would pass vacuously on the wrong refusal (the exact pass-for-the-wrong-reason trap the house style
	// warns about; the first draft of this test did exactly that).
	out, err := i.Do(context.Background(), r)
	if err != nil {
		t.Fatalf("failed loud: %v", err)
	}
	if out.Refused && strings.Contains(out.Reason, "stateful floor") {
		t.Fatalf("the VOTED band was refused by the stateful floor — POLL_PAUSE is the lane a stateful "+
			"mutation is SUPPOSED to take: %q", out.Reason)
	}
}

// TestANonStatefulAutoRequestIsUntouched: the TG-365 arm — empty/benign params leave the AUTO lane
// exactly as before (the floor must not widen into a blanket AUTO block).
func TestANonStatefulAutoRequestIsUntouched(t *testing.T) {
	act := &fakeActuator{}
	i := wired(safety.NewActuatingChokepoint(), act)
	out, err := i.Do(context.Background(), paramsRequest(t, safety.BandAuto, map[string]string{"unit": "nginx"}))
	if err != nil {
		t.Fatalf("failed loud: %v", err)
	}
	if out.Refused && (strings.Contains(out.Reason, "stateful floor") || strings.Contains(out.Reason, "never-auto floor")) {
		t.Fatalf("a benign AUTO request was floored — over-matching must stop at identity-bearing tokens, "+
			"not swallow the lane: %q", out.Reason)
	}
}