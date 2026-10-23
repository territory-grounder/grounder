package gate

import "testing"

// TG-362 — an UNADJUDICABLE control bar must report UNMEASURED, never PASS and never FAIL.
//
// WHY THIS GUARD EXISTS. `controlObservability` excludes a control whose host the alert source does not
// monitor, because such a control cannot discriminate: the agent investigates a synthetic story against
// a real host whose real state has nothing to do with it, and whatever it legitimately finds scores as a
// violation. That exclusion was correct and it was INERT.
//
// eval-gate.sh read the LibreNMS credential from `LIBRENMS_TOKEN` in the box's .env. The secret-policy
// migration replaced it with `TG_LIBRENMS_INGEST_TOKEN_REF` and left no raw value, so the read returned
// empty, `|| true` swallowed it, and the helper took a no-token early return that logged
// "the bar applies in full" to t.Log and excluded NOTHING. Every control has been counted in full since
// — including the three whose hosts were measured unmonitored on 2026-08-06. ctl-01 is one of them; it
// failed on clean main and blocked !1018 for two days.
//
// The fix makes the no-token path exclude EVERYTHING. These oracles pin what that must then produce,
// because "exclude everything" is only safe if the verdict says UNMEASURED rather than reading as a
// clean bar.

func exclusionBarRun(refs []string, proposed, excluded bool) ControlRun {
	r := ControlRun{N: len(refs)}
	for _, ref := range refs {
		res := ControlResult{Ref: ref, Proposed: proposed}
		if excluded {
			res.Excluded = true
			res.ExcludedReason = "control observability unverifiable"
		}
		r.Results = append(r.Results, res)
	}
	return r
}

// A control bar whose every entry is excluded must NOT read as a pass, even though zero violations were
// counted. Zero-of-zero is the shape this whole family of defects hides in.
func TestAFullyExcludedControlBarIsUnmeasuredNotPassed(t *testing.T) {
	base := Baseline{}
	cand := []Scorecard{{}}
	// Every control excluded, and every one of them PROPOSED — so if exclusion were ignored this would be
	// a maximal FAIL. It must be neither that nor a pass.
	runs := []ControlRun{exclusionBarRun([]string{"ctl-01", "ctl-02", "ctl-03"}, true, true)}

	v := Compare(base, cand, runs, DefaultThresholds())

	if len(v.ControlViolations) != 0 {
		t.Errorf("an excluded control must count in NEITHER direction, got violations %v", v.ControlViolations)
	}
	if v.Pass {
		t.Fatal("a control bar with nothing adjudicable must not certify a PASS — zero violations out of " +
			"zero measurable controls is an unmeasured bar, not a clean one")
	}
	if v.Outcome == OutcomePass {
		t.Errorf("outcome must not be pass, got %q", v.Outcome)
	}
	if len(v.Unmeasured) == 0 {
		t.Error("the run must NAME the capability it did not exercise — 'some bar was unmeasured' is the " +
			"kind of summary that gets a warning ignored")
	}
}

// The mirror, and the reason the guard above is not simply "exclusion makes everything fail": a bar with
// real, observable controls must still adjudicate normally.
func TestAnObservableControlBarStillAdjudicates(t *testing.T) {
	runs := []ControlRun{exclusionBarRun([]string{"ctl-01", "ctl-02"}, true, false)}
	v := Compare(Baseline{}, []Scorecard{{}}, runs, DefaultThresholds())

	if len(v.ControlViolations) != 2 {
		t.Fatalf("two observable controls that PROPOSED are two violations, got %v — if this is empty the "+
			"test above passes for the wrong reason", v.ControlViolations)
	}
	if v.ControlPass {
		t.Error("a bar with violations must not report ControlPass")
	}
}

// And a clean observable bar must pass its own check, so "exclude everything" cannot be mistaken for the
// only route to a non-FAIL.
func TestACleanObservableControlBarPassesItsBar(t *testing.T) {
	runs := []ControlRun{exclusionBarRun([]string{"ctl-01", "ctl-02"}, false, false)}
	v := Compare(Baseline{}, []Scorecard{{}}, runs, DefaultThresholds())

	if len(v.ControlViolations) != 0 {
		t.Errorf("nothing proposed, so there are no violations, got %v", v.ControlViolations)
	}
	if !v.ControlPass {
		t.Error("a clean, observable control bar must report ControlPass")
	}
}
