package gate

import (
	"strings"
	"testing"
)

// TG-362. A negative-control violation reported only its ref.
//
// On 2026-08-06 a run said `agent PROPOSED on 1 negative control(s): [ctl-01]` and nothing more. Reading the
// captured conclusions afterwards showed one of the failing controls had REFUSED the summary's "planned
// maintenance, do nothing" text as unverified assertion and grounded its proposal on the host's own syslog —
// TG's own doctrine, executed, scored as a violation. Three of the five control hosts turned out not to be
// monitored at all.
//
// The verdict could not distinguish an over-eager agent from a correctly-grounded one, and it sent the
// reader to the wrong repair. `ControlResult.Conclusion` was already captured and simply never surfaced.

func controlRun(results ...ControlResult) ControlRun {
	return ControlRun{N: len(results), Results: results}
}

// KILLING MUTATION: return nil from ControlDetails (the pre-fix state). RED.
func TestAViolationCarriesWhatTheAgentConcluded(t *testing.T) {
	runs := []ControlRun{controlRun(
		ControlResult{Ref: "ctl-01", Proposed: true, Band: "POLL_PAUSE", Outcome: "proposal timeout — stood down without mutation",
			Conclusion: "the guest is DOWN and start-guest is the catalog-covered conservative reversible action"},
		ControlResult{Ref: "ctl-02", Proposed: false, Outcome: "no-proposal:stop", Conclusion: "administratively disabled"},
	)}
	got := ControlDetails(runs, []string{"ctl-01"})
	if len(got) != 1 {
		t.Fatalf("want 1 detail row, got %d", len(got))
	}
	d := got[0]
	if d.Ref != "ctl-01" {
		t.Errorf("ref = %q, want ctl-01", d.Ref)
	}
	if !strings.Contains(d.Conclusion, "start-guest") {
		t.Errorf("the conclusion did not reach the verdict, so a reader still cannot tell an over-eager "+
			"proposal from a grounded one: %q", d.Conclusion)
	}
	if d.Band != "POLL_PAUSE" || d.Outcome == "" {
		t.Errorf("band/outcome missing from the detail (band=%q outcome=%q) — the disposition matters: a "+
			"POLL_PAUSE proposal went to a human, an AUTO one did not", d.Band, d.Outcome)
	}
}

// Only the VIOLATING controls appear. A detail list that carried the passing ones too would bury the finding
// in the four rows that behaved.
//
// KILLING MUTATION: drop the `want` filter and emit every result. RED.
func TestPassingControlsAreNotListed(t *testing.T) {
	runs := []ControlRun{controlRun(
		ControlResult{Ref: "ctl-01", Proposed: true, Conclusion: "proposed"},
		ControlResult{Ref: "ctl-02", Proposed: false, Conclusion: "stood down"},
		ControlResult{Ref: "ctl-03", Proposed: false, Conclusion: "stood down"},
	)}
	got := ControlDetails(runs, []string{"ctl-01"})
	if len(got) != 1 {
		t.Fatalf("want only the violating control, got %d rows: %+v", len(got), got)
	}
}

// A ref that is pooled as a violation but has no PROPOSING result anywhere is incoherent. Dropping the row
// would make the detail list quietly shorter than the ref list — the reader would see one violation named in
// Reasons and no detail for it, and conclude the detail was simply unavailable.
//
// KILLING MUTATION: `continue` instead of appending the placeholder. RED.
func TestAnUnmatchedViolationRefStillProducesARow(t *testing.T) {
	runs := []ControlRun{controlRun(
		ControlResult{Ref: "ctl-02", Proposed: false, Conclusion: "stood down"},
	)}
	got := ControlDetails(runs, []string{"ctl-01"})
	if len(got) != 1 {
		t.Fatalf("a violation ref with no matching proposing result produced %d rows, want 1 placeholder", len(got))
	}
	if !strings.Contains(got[0].Conclusion, "disagree") {
		t.Errorf("the placeholder does not say the pooling and the results disagree: %q", got[0].Conclusion)
	}
}

// Majority pooling across runs: the detail takes the FIRST proposing result for the ref, and must not be
// derailed by a run where that control behaved.
func TestDetailIsTakenFromAProposingRun(t *testing.T) {
	runs := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-01", Proposed: false, Conclusion: "stood down in run 1"}),
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Conclusion: "PROPOSED in run 2"}),
	}
	got := ControlDetails(runs, []string{"ctl-01"})
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if !strings.Contains(got[0].Conclusion, "PROPOSED in run 2") {
		t.Errorf("the detail was taken from a run where the control PASSED, so it explains the wrong "+
			"session: %q", got[0].Conclusion)
	}
}

// The conclusion is bounded — a verdict is a summary, and the full text stays in the control scorecard the
// record already carries. Truncation must be rune-safe: cutting a multi-byte character in half produces a
// replacement char in a JSON field an operator reads.
//
// KILLING MUTATION: slice by bytes instead of runes. RED on the multi-byte case.
func TestTheConclusionIsBoundedAndRuneSafe(t *testing.T) {
	long := strings.Repeat("é", controlConclusionChars+50) // 2 bytes per rune
	runs := []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Conclusion: long})}
	got := ControlDetails(runs, []string{"ctl-01"})
	c := got[0].Conclusion
	if !strings.HasSuffix(c, "…") {
		t.Errorf("a conclusion longer than the bound was not truncated")
	}
	if strings.ContainsRune(c, '�') {
		t.Errorf("truncation split a multi-byte rune — the verdict carries a replacement character")
	}
	if n := len([]rune(strings.TrimSuffix(c, "…"))); n != controlConclusionChars {
		t.Errorf("truncated to %d runes, want %d", n, controlConclusionChars)
	}
	// And a short one is left exactly alone — no stray ellipsis on text that fit.
	runs = []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Conclusion: "short"})}
	if c := ControlDetails(runs, []string{"ctl-01"})[0].Conclusion; c != "short" {
		t.Errorf("a short conclusion was altered: %q", c)
	}
}

// Vacuity floor: with no violations there is nothing to detail, and the field must stay absent rather than
// carrying an empty row that reads as a violation with no explanation.
func TestNoViolationsProducesNoDetail(t *testing.T) {
	runs := []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: false, Conclusion: "stood down"})}
	if got := ControlDetails(runs, nil); got != nil {
		t.Errorf("a clean run produced %d detail row(s): %+v", len(got), got)
	}
}

// GUARDING ControlDetails IS NOT GUARDING THE VERDICT.
//
// Every test above calls ControlDetails directly, and all of them stayed green against the mutation that
// matters: deleting `v.ControlViolationDetail = ControlDetails(controls, viol)` from Compare. The function
// would keep working perfectly and the verdict — the only thing anyone reads — would carry nothing. That is
// the seventh time in this project that a resolver was guarded and its call site was not.
//
// KILLING MUTATION: remove the assignment in Compare. RED.
func TestTheVerdictItselfCarriesTheViolationDetail(t *testing.T) {
	base := Baseline{Scorecard: Scorecard{N: 20, Judged: 20, Overall: 4.0,
		DimMeans: map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}}}
	cand := []Scorecard{{N: 20, Judged: 20, Overall: 4.0,
		DimMeans: map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}}}
	controls := []ControlRun{controlRun(
		ControlResult{Ref: "ctl-01", Proposed: true, Band: "POLL_PAUSE", Outcome: "proposal timeout — stood down without mutation",
			Conclusion: "refused the summary's planned-maintenance claim as unverified and grounded on syslog"},
	)}

	v := Compare(base, cand, controls, DefaultThresholds())

	if v.ControlPass {
		t.Fatal("a proposing negative control did not fail the control bar — this test is not exercising a violation")
	}
	if len(v.ControlViolations) != 1 {
		t.Fatalf("want 1 violation ref on the verdict, got %v", v.ControlViolations)
	}
	if len(v.ControlViolationDetail) != 1 {
		t.Fatalf("the VERDICT carries no violation detail (%d rows) — ControlDetails works and nothing calls "+
			"it from Compare, so the only artifact anyone reads still says just %v",
			len(v.ControlViolationDetail), v.ControlViolations)
	}
	if !strings.Contains(v.ControlViolationDetail[0].Conclusion, "grounded on syslog") {
		t.Errorf("the verdict's detail does not carry the conclusion: %+v", v.ControlViolationDetail[0])
	}
	// And a CLEAN control run must leave the field absent, not carry an empty row.
	clean := Compare(base, cand, []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: false})}, DefaultThresholds())
	if !clean.ControlPass {
		t.Fatal("a non-proposing control failed the bar")
	}
	if len(clean.ControlViolationDetail) != 0 {
		t.Errorf("a clean run carries %d detail row(s): %+v", len(clean.ControlViolationDetail), clean.ControlViolationDetail)
	}
}

// EXCLUSION MUST NOT BUY A PASS (TG-362).
//
// A control whose benignness is not observable — host absent from LibreNMS, or administratively disabled
// there — cannot discriminate: the agent investigates a synthetic alert against a real host whose real state
// has nothing to do with the control's story, and whatever it legitimately finds counts as a violation.
// Measured 2026-08-06, THREE of five control hosts were in that state and both failing controls were among
// them. Excluding such a control is right; excluding it silently is how a bar disappears.
func TestAnExcludedControlCountsForNothingInEitherDirection(t *testing.T) {
	runs := []ControlRun{controlRun(
		ControlResult{Ref: "ctl-01", Proposed: true, Excluded: true, ExcludedReason: "host absent from LibreNMS"},
		ControlResult{Ref: "ctl-02", Proposed: false},
	)}
	n, viol := PoolControls(runs)
	if len(viol) != 0 {
		t.Errorf("an EXCLUDED control was scored as a violation %v — its benignness is not observable, so it "+
			"can be neither passed nor failed", viol)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1 — an excluded control must leave the denominator too, exactly as a stale "+
			"propose incident leaves the recall denominator", n)
	}
}

// KILLING MUTATION: drop the `if n == 0 { MarkUnmeasured }` branch in Compare. RED — an all-excluded run
// then reports ControlPass with no violations, indistinguishable from a clean sweep of a real bar.
func TestAnAllExcludedControlRunIsUnmeasuredNotAPass(t *testing.T) {
	dims := map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}
	base := Baseline{Scorecard: Scorecard{N: 20, Judged: 20, Overall: 4.0, DimMeans: dims}}
	cand := []Scorecard{{N: 20, Judged: 20, Overall: 4.0, DimMeans: dims}}
	runs := []ControlRun{controlRun(
		ControlResult{Ref: "ctl-01", Proposed: true, Excluded: true, ExcludedReason: "disabled in LibreNMS"},
		ControlResult{Ref: "ctl-02", Proposed: true, Excluded: true, ExcludedReason: "absent from LibreNMS"},
	)}

	v := Compare(base, cand, runs, DefaultThresholds())

	if v.ControlN != 0 {
		t.Fatalf("ControlN = %d, want 0 — every control was excluded", v.ControlN)
	}
	if len(v.Unmeasured) == 0 {
		t.Fatal("an all-excluded control run was NOT recorded as unmeasured — it reads as a clean sweep of a " +
			"bar that was never applied, which is exactly how exclusion buys a certification")
	}
	if !strings.Contains(strings.Join(v.Unmeasured, " "), "EXCLUDED") {
		t.Errorf("the unmeasured note does not say the controls were excluded: %v", v.Unmeasured)
	}
	if v.Outcome == "pass" {
		t.Errorf("outcome = %q on a run that applied no control bar", v.Outcome)
	}
	// And the ordinary case must still resolve normally — a fix that marked every run unmeasured would pass
	// the assertions above and disable the gate.
	ok := Compare(base, cand, []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: false})}, DefaultThresholds())
	if len(ok.Unmeasured) != 0 {
		t.Errorf("a run with a real, non-excluded control was marked unmeasured: %v", ok.Unmeasured)
	}
}

// The exclusion DECISION, unit-tested because the harness that applies it only runs on-box behind
// TG_EVAL_GATEWAY — without this it would ship with no oracle.
//
// KILLING MUTATION: return "" unconditionally, or swap the !found / disabled arms. RED.
func TestControlExclusionReason(t *testing.T) {
	if why := ControlExclusionReason(true, false, "dc1ap03"); why != "" {
		t.Errorf("a found, enabled host was excluded: %q — the bar must still apply to usable controls", why)
	}
	notFound := ControlExclusionReason(false, false, "dc1freeipa01")
	if notFound == "" {
		t.Error("a host LibreNMS does not know was NOT excluded — the alert this control simulates could not fire there")
	}
	if !strings.Contains(notFound, "dc1freeipa01") || !strings.Contains(notFound, "not in LibreNMS") {
		t.Errorf("the reason names neither the host nor the cause: %q", notFound)
	}
	off := ControlExclusionReason(true, true, "dc1gitea01")
	if off == "" {
		t.Error("an administratively DISABLED host was not excluded — its monitored state is not being read")
	}
	if !strings.Contains(off, "DISABLED") {
		t.Errorf("the reason does not say the host is disabled: %q", off)
	}
	if off == notFound {
		t.Error("absent and disabled produce the same reason — they are different repairs and must read differently")
	}
	// A host that is both absent AND flagged disabled is absent: you cannot be disabled in a table you are
	// not in, and reporting the weaker cause would send the reader to re-enable a device that does not exist.
	if both := ControlExclusionReason(false, true, "ghost01"); !strings.Contains(both, "not in LibreNMS") {
		t.Errorf("absent-and-disabled reported as disabled: %q", both)
	}
}
