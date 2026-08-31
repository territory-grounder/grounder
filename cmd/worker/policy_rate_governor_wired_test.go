package main

// THE POLICY RATE GOVERNOR MUST BE ATTACHED IN PRODUCTION (TG-316).
//
// `policy.Engine.WithRateGovernor` had exactly ONE caller in the entire tree, and it was
// spec/015-policy-engine/acceptance/acceptance_test.go. The worker built its engine as
// `policy.NewEngine(...).WithGraduation(...)` and never attached a governor, so `Engine.rateGov` was nil
// in every production worker and `Refine` degraded to the confidence clamp alone.
//
// The consequence is the one this repository keeps producing: `"rate_limit": 30` in
// core/policy/templates/conservative.json, and any operator rule that sets rate_limit, read like an armed
// control and counted nothing. A configured limit that silently counts nothing is worse than having no
// limit at all, because it answers the question "is this bounded?" with yes.
//
// The acceptance test passing is exactly why this survived: the control WORKS, it was just never wired,
// and a test that constructs its own engine cannot tell the difference. So this guard reads the
// composition root — the same technique core/seal/composition_root_test.go uses for the same reason.

import (
	"os"
	"strings"
	"testing"
)

func TestPolicyEngineIsBuiltWithARateGovernor(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read composition root: %v", err)
	}
	// Comment lines do not count. Twice today a guard of this shape passed because prose ABOUT the control
	// contained the token it was looking for (TG-326, TG-143), so the exclusion is deliberate.
	var built, governed bool
	for _, line := range strings.Split(string(src), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "policy.NewEngine(") {
			built = true
		}
		if strings.Contains(code, "WithRateGovernor(") {
			governed = true
		}
	}
	// Vacuity floor: if the worker stopped building a policy engine at all, "no governor" would be true for
	// a reason this test is not about, and its message would send a reader hunting the wrong thing.
	if !built {
		t.Fatal("cmd/worker/main.go no longer calls policy.NewEngine — this guard is reading a file that no " +
			"longer describes the policy wiring, and would otherwise pass by checking nothing")
	}
	if !governed {
		t.Error("the production policy engine is built WITHOUT WithRateGovernor, so Engine.rateGov is nil and " +
			"Refine degrades to the confidence clamp alone. Every `rate_limit` in a ruleset — including the " +
			"30 in core/policy/templates/conservative.json — then reads like an armed control while counting " +
			"nothing, which answers \"is this bounded?\" with a yes it has not earned.")
	}
}
