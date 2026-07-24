package safety_test

// Drift-guard (integration-audit A2 hardening): the self-protected control-plane veto is fed only the terse
// (op, op_class) pair via safety.IsRestartClass, so restartClassRE MUST recognize every op-class the effect
// leaves can actually actuate — or the veto silently goes dead for the un-listed class (exactly the A2 bug:
// restart-container / reload-service were added to the opschema registry but restartClassRE was not extended).
// This test binds restartClassRE to the SINGLE source of truth for the actuatable set (opschema.Specs), so a
// future op-class added to opschema.json cannot re-introduce the lag without a failing test. If a genuinely
// non-restart actuatable op-class is ever added, that failure is the FORCING FUNCTION: decide consciously
// whether the control-plane veto should cover it (add its token to restartClassRE), or exclude it here with a
// documented rationale — never let it lag silently.

import (
	"testing"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/safety"
)

// nonRestartExempt is the CONSCIOUS classification demanded by this test's forcing function: an actuatable
// op-class that is genuinely NOT a service restart, for which the RESTART-based self-protected control-plane
// veto does not apply, WITH the documented reason it is safe on a control-plane target. Adding a class here is
// a deliberate safety decision reviewed in the MR that adds it — never a silent lag.
var nonRestartExempt = map[string]string{
	// disk-grow GROWS a filesystem/volume (never restarts a service, never removes data). On a control-plane
	// host a disk-grow is SELF-HEALING (it relieves a full disk that would otherwise take TG down), not
	// self-harming, so the restart-veto (which stops TG restarting/killing its own services) does not apply.
	// Its safety story is instead: the awx-job lane's operator template allowlist + op-class/template
	// cross-check, the AWX play's own pool-floor/rate-cap/sentinel guards, the mode chokepoint, and the policy
	// engine — none of which this restart-specific veto duplicates.
	"disk-grow": "non-restart, non-destructive disk GROW — self-healing on a control-plane target, so the restart-based veto does not apply",
	// start-guest brings a DOWN guest UP (never restarts/kills a running service). On a control-plane guest a
	// start is SELF-HEALING (it restores infra that would otherwise stay down), not self-harming, so the
	// restart-veto (which stops TG restarting/killing its OWN running services) does not apply. Its safety story
	// is the proxmox actuator's per-guest allowlist + the never-auto floor on reboot/shutdown/reset/destroy +
	// reversibility (stop is the inverse) + the incident context (a device-down alert ⇒ the guest is expected-up).
	"start-guest": "non-restart guest START (brings a down guest up) — self-healing on a control-plane target, so the restart-based veto does not apply",
}

func TestSelfProtectedRestartCoversEveryActuatableOpClass(t *testing.T) {
	specs := opschema.Specs()
	if len(specs) == 0 {
		t.Fatal("opschema registry is empty — cannot validate the self-protected-restart coverage invariant")
	}
	for _, s := range specs {
		if _, exempt := nonRestartExempt[s.OpClass]; exempt {
			continue // consciously classified as non-restart above (the forcing function is satisfied)
		}
		// Exactly as temporal/runner/activities.go feeds it: IsRestartClass(in.Op, in.OpClass).
		if !safety.IsRestartClass(s.Op, s.OpClass) {
			t.Errorf("actuatable op-class %q (op %q) is NOT recognized by safety.IsRestartClass — the self-protected "+
				"control-plane veto would silently never fire for it (the A2 lag). Extend restartClassRE in "+
				"core/safety/safety.go to cover it, or add it to nonRestartExempt here with a documented rationale.", s.OpClass, s.Op)
		}
	}
}
