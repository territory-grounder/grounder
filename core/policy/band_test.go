package policy

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// TestComposeRespectMoreRestrictive proves respect band_mode emits the MORE-RESTRICTIVE of {policy verdict,
// risk band} across a representative matrix of verdict × band (REQ-1509). The band can only RAISE scrutiny;
// it never loosens the policy verdict.
func TestComposeRespectMoreRestrictive(t *testing.T) {
	cases := []struct {
		name string
		pv   Verdict
		band safety.Band
		want Verdict
	}{
		// auto policy under each band: POLL_PAUSE (→approve floor) VETOES the permissive auto (REQ-1509);
		// AUTO / AUTO_NOTICE permit autonomy, so auto stands.
		{"auto_under_pollpause_vetoed", VerdictAuto, safety.BandPollPause, VerdictApprove},
		{"auto_under_autonotice_stands", VerdictAuto, safety.BandAutoNotice, VerdictAuto},
		{"auto_under_auto_stands", VerdictAuto, safety.BandAuto, VerdictAuto},
		// approve policy: never loosened; a permissive band cannot pull it down to auto.
		{"approve_under_pollpause", VerdictApprove, safety.BandPollPause, VerdictApprove},
		{"approve_under_autonotice_not_loosened", VerdictApprove, safety.BandAutoNotice, VerdictApprove},
		{"approve_under_auto_not_loosened", VerdictApprove, safety.BandAuto, VerdictApprove},
		// deny ALWAYS stays deny — the most restrictive verdict, no band loosens it.
		{"deny_under_pollpause", VerdictDeny, safety.BandPollPause, VerdictDeny},
		{"deny_under_autonotice", VerdictDeny, safety.BandAutoNotice, VerdictDeny},
		{"deny_under_auto", VerdictDeny, safety.BandAuto, VerdictDeny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, rec := ComposeBand(c.pv, c.band, BandRespect, false)
			if got != c.want {
				t.Fatalf("respect(%q, %s) = %q, want %q", c.pv, c.band, got, c.want)
			}
			if rec.Composed != c.want {
				t.Fatalf("record.Composed = %q, want %q", rec.Composed, c.want)
			}
			if rec.BandMode != BandRespect {
				t.Fatalf("record.BandMode = %q, want respect", rec.BandMode)
			}
			if rec.Overridden || rec.DoubleWarn || rec.FloorClamped {
				t.Fatalf("respect must not set Overridden/DoubleWarn/FloorClamped: %+v", rec)
			}
			if rec.SafetyBand != c.band || rec.PolicyVerdict != c.pv {
				t.Fatalf("record did not carry the inputs: %+v", rec)
			}
		})
	}
}

// TestComposeDenyAlwaysDeny is the standing property that a policy deny survives EVERY composition — every
// band under both modes (deny-overrides at the composition layer, REQ-1509/1510).
func TestComposeDenyAlwaysDeny(t *testing.T) {
	for _, band := range []safety.Band{safety.BandAuto, safety.BandAutoNotice, safety.BandPollPause} {
		for _, mode := range []BandMode{BandRespect, BandForce, "", "bogus"} {
			for _, floor := range []bool{false, true} {
				got, rec := ComposeBand(VerdictDeny, band, mode, floor)
				if got != VerdictDeny || rec.Composed != VerdictDeny {
					t.Fatalf("deny under band=%s mode=%q floor=%v = %q, want deny", band, mode, floor, got)
				}
			}
		}
	}
}

// TestComposeForceOverride proves force band_mode applies the POLICY verdict even when it is LESS restrictive
// than the risk band, stamps the double-confirmation warning, and RECORDS the override with the band it
// overrode — the double-audit (REQ-1510). This is the negative control proving the override is auditable.
func TestComposeForceOverride(t *testing.T) {
	// policy auto beneath a POLL_PAUSE band (whose floor is approve): force LOOSENS the outcome to auto.
	got, rec := ComposeBand(VerdictAuto, safety.BandPollPause, BandForce, false)
	if got != VerdictAuto {
		t.Fatalf("force(auto, POLL_PAUSE) = %q, want the policy verdict auto", got)
	}
	if !rec.Overridden {
		t.Fatalf("force loosening below the band must set Overridden: %+v", rec)
	}
	if rec.OverriddenBand != safety.BandPollPause {
		t.Fatalf("record must name the overridden band POLL_PAUSE, got %s", rec.OverriddenBand)
	}
	if !rec.DoubleWarn {
		t.Fatalf("force must stamp the double-confirmation warning (REQ-1510): %+v", rec)
	}
	if rec.FloorClamped {
		t.Fatalf("no constitutional floor applied here — FloorClamped must be false: %+v", rec)
	}
}

// TestComposeForceAutoNotice binds the acceptance scenario "Force band_mode overrides the risk band and
// double-warns" at the unit level: force whose verdict is auto over an AUTO_NOTICE band applies the policy
// verdict and records the double-confirmation warning (REQ-1510).
func TestComposeForceAutoNotice(t *testing.T) {
	got, rec := ComposeBand(VerdictAuto, safety.BandAutoNotice, BandForce, false)
	if got != VerdictAuto {
		t.Fatalf("force(auto, AUTO_NOTICE) = %q, want the policy verdict auto", got)
	}
	if !rec.DoubleWarn {
		t.Fatalf("force must record the double-confirmation warning: %+v", rec)
	}
	if rec.BandMode != BandForce {
		t.Fatalf("record.BandMode = %q, want force", rec.BandMode)
	}
}

// TestComposeForceCannotBreachNeverAutoFloor is the load-bearing safety proof (INV-09): even an operator's
// force override CANNOT make a floor-class action resolve to auto. A force→auto composition is clamped back
// to approve when the constitutional never-auto floor applies, and the clamp is recorded.
func TestComposeForceCannotBreachNeverAutoFloor(t *testing.T) {
	// force whose policy verdict is auto, under the least-restrictive AUTO band, WITH the floor applied.
	got, rec := ComposeBand(VerdictAuto, safety.BandAuto, BandForce, true)
	if got != VerdictApprove {
		t.Fatalf("force under the never-auto floor = %q, want the clamped approve (INV-09)", got)
	}
	if !rec.FloorClamped {
		t.Fatalf("the floor clamp must be recorded (double-audit): %+v", rec)
	}
	if rec.Composed != VerdictApprove {
		t.Fatalf("record.Composed = %q, want approve", rec.Composed)
	}
	// The floor also holds beneath respect: policy auto + band auto + floor → approve, never auto.
	if r, rr := ComposeBand(VerdictAuto, safety.BandAuto, BandRespect, true); r != VerdictApprove || !rr.FloorClamped {
		t.Fatalf("respect under the floor = %q (clamped=%v), want approve clamped", r, rr.FloorClamped)
	}
}

// TestComposeModeFailClosed proves an unset ("" inherit) and any INVALID band_mode both resolve to respect
// (the safer, more-restrictive composition) — determinism + fail-closed (REQ-1509 default).
func TestComposeModeFailClosed(t *testing.T) {
	for _, mode := range []BandMode{"", "bogus", "FORCE", "Respect"} {
		got, rec := ComposeBand(VerdictAuto, safety.BandPollPause, mode, false)
		if got != VerdictApprove {
			t.Fatalf("band_mode %q must fail closed to respect (→approve), got %q", mode, got)
		}
		if rec.BandMode != BandRespect {
			t.Fatalf("band_mode %q must resolve to respect, got %q", mode, rec.BandMode)
		}
		if rec.DoubleWarn {
			t.Fatalf("a non-force mode must not stamp the double-warn: %q", mode)
		}
	}
}

// TestComposeUnmappedVerdictIsMostRestrictive proves an unmapped/invalid policy verdict is treated as the
// most restrictive (deny) — fail closed (INV-09).
func TestComposeUnmappedVerdictIsMostRestrictive(t *testing.T) {
	got, _ := ComposeBand(Verdict("weird"), safety.BandAuto, BandRespect, false)
	if got != VerdictDeny {
		t.Fatalf("unmapped verdict must compose to deny, got %q", got)
	}
}

// TestComposeDeterministic proves the composition is a pure, deterministic function: identical inputs yield
// identical outputs across repeated calls.
func TestComposeDeterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		got, rec := ComposeBand(VerdictAuto, safety.BandPollPause, BandForce, false)
		if got != VerdictAuto || !rec.Overridden || !rec.DoubleWarn {
			t.Fatalf("non-deterministic composition on iteration %d: %q %+v", i, got, rec)
		}
	}
}

// TestNeverAutoApplies proves the read-only floor derivation over core/safety: a floor op-class, an
// irreversible action, or a destructive op each trip the floor; an ordinary reversible action does not.
func TestNeverAutoApplies(t *testing.T) {
	if !NeverAutoApplies(EvalInput{OpClass: "reboot", Reversible: true}) {
		t.Fatal("a never-auto op-class must trip the floor")
	}
	if !NeverAutoApplies(EvalInput{OpClass: "restart-service", Reversible: false}) {
		t.Fatal("an irreversible action must trip the floor")
	}
	if !NeverAutoApplies(EvalInput{OpClass: "restart-service", Argv: "dropdb prod", Reversible: true}) {
		t.Fatal("a destructive op must trip the floor regardless of the declared class")
	}
	if NeverAutoApplies(EvalInput{OpClass: "restart-service", Argv: "systemctl restart nginx", Reversible: true}) {
		t.Fatal("an ordinary reversible restart must NOT trip the floor")
	}
}
