package risk

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

// THE BAND FLOOR SEAM (spec/028 REQ-2809, spec/026 REQ-2611).
//
// One field, two producers: the graduation ladder's AUTO_NOTICE floor and the actor-evidence policy's
// POLL_PAUSE floor. The property that matters for both is the same and is the only one that makes a floor
// safe to accept from a runtime source at all: IT MAY ONLY EVER RAISE THE BAR.

// autoEligibleInput is the one input shape that reaches BandAuto — every mechanical gate satisfied. It is
// built here rather than reused from a fixture so the floor oracles cannot be silently defanged by an
// unrelated change to a shared fixture that stops it reaching AUTO.
func autoEligibleInput() GatedInput {
	return GatedInput{
		ActionID:      "act-1",
		PlanHash:      "plan-1",
		OpClass:       "restart-service",
		Reversible:    Reversible,
		HasPrediction: true,
		Signals:       map[string]string{},
	}
}

// TestFloorOnlyEverRaisesTheBar is the load-bearing oracle. It sweeps every (computed band, declared floor)
// pair and asserts the resulting band is never LESS restrictive than what the gate computed on its own.
//
// RED CONTROL EXECUTED: replaced the `in.BandFloor >= d.Band` guard with an unconditional assignment
// `d.Band = in.BandFloor` ->
//
//	"a declared floor LOWERED a computed poll_pause to auto — a floor that can lower the bar is not a
//	 floor, it is a runtime-writable path to autonomy"
func TestFloorOnlyEverRaisesTheBar(t *testing.T) {
	// Three inputs whose UNFLOORED outcomes span all three bands.
	computed := []struct {
		name string
		in   GatedInput
		want safety.Band
	}{
		{"auto", autoEligibleInput(), safety.BandAuto},
		{"auto_notice", func() GatedInput { g := autoEligibleInput(); g.CriticalityTier = true; return g }(), safety.BandAutoNotice},
		{"poll_pause", func() GatedInput { g := autoEligibleInput(); g.Jailbreak = true; return g }(), safety.BandPollPause},
	}
	floors := []safety.Band{safety.BandPollPause, safety.BandAutoNotice, safety.BandAuto}

	for _, c := range computed {
		if got := Classify(c.in).Band; got != c.want {
			t.Fatalf("fixture %q classified as %v without a floor, want %v — the sweep below would be "+
				"testing the wrong band", c.name, got, c.want)
		}
		for _, f := range floors {
			in := c.in
			in.BandFloor = f
			in.BandFloorApplies = true
			in.BandFloorReason = "oracle"
			got := Classify(in).Band

			// The ordering IS the safety property: BandPollPause(0) < BandAutoNotice(1) < BandAuto(2).
			if got > c.want {
				t.Errorf("computed=%v floor=%v produced %v — a declared floor LOWERED the bar; a floor that "+
					"can lower the bar is not a floor, it is a runtime-writable path to autonomy",
					c.want, f, got)
			}
			// And it must actually BITE when it is stricter, or the seam is decorative.
			if f < c.want && got != f {
				t.Errorf("computed=%v floor=%v produced %v — a stricter floor was ignored", c.want, f, got)
			}
		}
	}
}

// TestUnsetFloorIsInert guards the trap in this seam's own design: safety.Band's zero value is the MOST
// restrictive band, so a floor field without an applies-flag would poll the entire estate.
//
// RED CONTROL EXECUTED: removed the `!in.BandFloorApplies` early return from applyBandFloor ->
//
//	"an input that declared NO floor was clamped to poll_pause — the zero value of safety.Band is the
//	 strictest band, so a missing applies-flag floors the entire estate"
func TestUnsetFloorIsInert(t *testing.T) {
	in := autoEligibleInput() // BandFloor left at its zero value (= BandPollPause), BandFloorApplies false
	d := Classify(in)
	if d.Band != safety.BandAuto {
		t.Fatalf("an input that declared NO floor was clamped to %v — the zero value of safety.Band is the "+
			"strictest band, so a missing applies-flag floors the entire estate", d.Band)
	}
	if _, recorded := d.Signals["band_floor"]; recorded {
		t.Error("an inert floor must not record a band_floor signal — the audit row would claim a policy applied")
	}
}

// TestAutoNoticeFloorStillActsAndPages pins what the LADDER's floor means (REQ-2809). This is the rung's
// entire behavioural difference: the action happens, and someone finds out.
//
// RED CONTROL EXECUTED: made the BandAutoNotice arm call poll(d, ...) like the default arm ->
//
//	"band = POLL_PAUSE, want auto_notice"
//
// Note the control tripped the BAND assertion, not the auto_approved one below it — poll() moves both, and
// the band check is first. The auto_approved/auto_resolve assertions are recorded as an additional guard on
// a mutation that changes those fields WITHOUT changing the band; they are not independently proven by this
// control, and are documented as such rather than counted as covered.
func TestAutoNoticeFloorStillActsAndPages(t *testing.T) {
	in := autoEligibleInput()
	in.BandFloor = safety.BandAutoNotice
	in.BandFloorApplies = true
	in.BandFloorReason = "ladder-auto-notice"

	d := Classify(in)
	if d.Band != safety.BandAutoNotice {
		t.Fatalf("band = %v, want auto_notice", d.Band)
	}
	if !d.AutoApproved || !d.AutoResolve {
		t.Fatalf("an auto_notice floor STOPPED the action (auto_approved=%v auto_resolve=%v) — 'acts and "+
			"pages' was silently turned into 'does not act', which is a different and unrequested refusal",
			d.AutoApproved, d.AutoResolve)
	}
	if !d.NotifyRequired {
		t.Error("an auto_notice floor that does not PAGE is just auto with extra steps — the notice IS the rung")
	}
	if d.Signals["band_floor"] != "auto_notice" {
		t.Errorf("the floor must be recorded for the audit row, got %q", d.Signals["band_floor"])
	}
}

// TestPollPauseFloorRoutesThroughTheRealPollPath is spec/026 REQ-2611's arm: authored-action evidence floors
// to POLL_PAUSE, and the result must be indistinguishable from a poll the gate reached on its own.
//
// RED CONTROL EXECUTED: made the default arm set only d.Band = in.BandFloor instead of calling poll() ->
//
//	"a floored poll_pause left auto_approved/auto_resolve set — the band said ask a human while the rest
//	 of the decision still said proceed, and the executing path reads those fields, not the band name"
func TestPollPauseFloorRoutesThroughTheRealPollPath(t *testing.T) {
	in := autoEligibleInput()
	in.BandFloor = safety.BandPollPause
	in.BandFloorApplies = true
	in.BandFloorReason = "authored-action-evidence"

	d := Classify(in)
	if d.Band != safety.BandPollPause {
		t.Fatalf("band = %v, want poll_pause", d.Band)
	}
	if d.AutoApproved || d.AutoResolve || d.AutoProceedOnTimeout {
		t.Fatalf("a floored poll_pause left auto_approved=%v auto_resolve=%v proceed_on_timeout=%v — the band "+
			"said ask a human while the rest of the decision still said proceed, and the executing path reads "+
			"those fields, not the band name", d.AutoApproved, d.AutoResolve, d.AutoProceedOnTimeout)
	}
	if got := d.Signals["poll_reason"]; got != "band-floor-authored-action-evidence" {
		t.Errorf("poll_reason = %q — the audit row must name WHY the floor was declared, not merely that one was", got)
	}
}

// TestUnrecognisedFloorFailsClosed proves the `default` arm is deliberate: an unknown floor value must land
// on the most restrictive outcome, not fall through unclamped. This is the arm a future band would hit.
//
// RED CONTROL EXECUTED: changed the switch's `default` to an explicit `case safety.BandPollPause` ->
//
//	"an UNRECOGNISED floor value passed through unclamped as auto — a floor the code does not understand
//	 must fail closed, which is exactly what a `default` arm buys and an explicit case does not"
func TestUnrecognisedFloorFailsClosed(t *testing.T) {
	in := autoEligibleInput()
	in.BandFloor = safety.Band(-7) // below every real band, so the >= guard cannot short-circuit it
	in.BandFloorApplies = true

	d := Classify(in)
	if d.Band != safety.BandPollPause {
		t.Fatalf("an UNRECOGNISED floor value passed through unclamped as %v — a floor the code does not "+
			"understand must fail closed, which is exactly what a `default` arm buys and an explicit case "+
			"does not", d.Band)
	}
	if got := d.Signals["poll_reason"]; got != "band-floor-declared" {
		t.Errorf("a floor with no stated reason must still be recorded, got %q", got)
	}
}

// TestFloorNeverOutranksAMechanicalGate proves the floor composes ON TOP of the constitutional floors rather
// than replacing them: where a mechanical gate already polled, ITS reason survives. An operator reading the
// audit row at 3am must see the strongest true reason, not the one that ran last.
//
// RED CONTROL EXECUTED: applied the floor BEFORE classify() instead of after ->
//
//	"a mechanical poll_reason was overwritten by the band floor — the audit row would say a policy floor
//	 stopped an action that the never-auto floor stopped"
func TestFloorNeverOutranksAMechanicalGate(t *testing.T) {
	in := autoEligibleInput()
	in.OpClass = "reboot" // safety.IsNeverAuto — the inviolable mechanical floor
	in.BandFloor = safety.BandAutoNotice
	in.BandFloorApplies = true
	in.BandFloorReason = "ladder-auto-notice"

	d := Classify(in)
	if d.Band != safety.BandPollPause {
		t.Fatalf("the never-auto floor was composed away by a band floor: band=%v", d.Band)
	}
	if got := d.Signals["poll_reason"]; got != "irreversible-or-never-auto-floor" {
		t.Fatalf("a mechanical poll_reason was overwritten by the band floor (%q) — the audit row would say a "+
			"policy floor stopped an action that the never-auto floor stopped", got)
	}
}
