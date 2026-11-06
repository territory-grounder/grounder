package main

// TG-505: core/policy.SelfTest is the REQ-1501 pipeline mode-invariant guard, documented (pipeline_guard.go)
// as a boot preflight that MUST refuse to start a worker whose DECISION pipeline is mode-dependent — exactly
// as the actuation interceptor's SelfTest refuses to boot an unwired chain. It was present-not-reaching:
// defined + unit-tested, but never CALLED in main() (deadcode reported SelfTest/AssertModeInvariant
// unreachable from both binaries), so the documented boot refusal did not exist. This source guard pins the
// boot call site so the wiring cannot silently regress, in the guest_liveness_wire_test.go house pattern
// (workerMainSource strips comments + enforces a vacuity floor; the stripper self-test lives there).
//
// KILLING MUTATION: delete the `policy.SelfTest(context.Background())` preflight line from main() — this test
// fails naming the missing site. Restore → green. (A dropped call site fails SAFE in the narrow sense that
// the worker still boots — but the REQ-1501 boot refusal is then a dead capability, the exact
// "present, not reaching" shape TG-505 was filed about.)

import (
	"strings"
	"testing"
)

func TestPolicyPipelineGuardSelfTestIsWiredAtBoot(t *testing.T) {
	src := workerMainSource(t)
	if !strings.Contains(src, "policy.SelfTest(context.Background())") {
		t.Error("core/policy.SelfTest is not CALLED in main() — the REQ-1501 pipeline mode-invariant boot " +
			"preflight is present-not-reaching: a mode-dependent decision pipeline would boot without the " +
			"documented fail-closed refusal (TG-505). Wire it beside the interceptor SelfTest / ProvePreflight.")
	}
	// It must fail the boot CLOSED, like the actuation preflight — a call that swallowed the error would let a
	// mode-dependent pipeline boot anyway. Anchored on the preflight's unique fatal message so the assertion
	// cannot be satisfied by some other log.Fatalf in main().
	if !strings.Contains(src, "policy pipeline-guard: boot self-test failed") {
		t.Error("the policy pipeline-guard preflight does not FAIL CLOSED on a REQ-1501 violation — the boot " +
			"self-test must log.Fatalf and refuse to start, not merely log the error (TG-505).")
	}
}
