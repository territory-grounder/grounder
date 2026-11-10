package gate

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TG-677 — the per-control coverage policy, case by case. Every case the owner prompt (2026-11-07 §2.A)
// listed is here: one valid proposing plus two excluded; one valid non-proposing plus two excluded; all
// excluded; a missing mandatory control beside a valid one; multiple controls; mixed observability in both
// directions; majority, tie and boundary; duplicate and missing ids; no runs; a valid positive case; and the
// rule that an excluded result never supplies the explanation of a violation or a pass.
//
// MUTATION CONTROLS (each executed before merge; the MR description records the red):
//   - restore the old denominator (`2*a.proposed > p.Runs` in PoolControlSet) → RED:
//     TestOneValidProposingObservationBesideOneExcludedIsAViolation, TestMajorityIsOfValidObservations.
//   - drop the coverage floor (`case a.valid < p.Floor` removed) → RED:
//     TestOneValidNonProposingObservationBesideTwoExcludedIsUnmeasured, TestOneValidProposingObservationBesideTwoExcludedCannotPass.
//   - drop the mandatory-set tally (`note(ref)` for mandatory refs removed) → RED:
//     TestAMissingMandatoryControlIsNamedBesideAValidOne.
//   - drop the `res.Excluded` skip in ControlDetails → RED: TestAnExcludedResultNeverSuppliesTheExplanation.

func tallyOf(t *testing.T, p ControlPooling, ref string) ControlTally {
	t.Helper()
	for _, row := range p.Tallies {
		if row.Ref == ref {
			return row
		}
	}
	t.Fatalf("no tally for %q in %+v", ref, p.Tallies)
	return ControlTally{}
}

// One valid NON-proposing observation beside two excluded: the agent did the right thing once, but once is
// below the floor ceil(3/2)=2, so the control is UNMEASURED — not a pass on one observation.
func TestOneValidNonProposingObservationBesideTwoExcludedIsUnmeasured(t *testing.T) {
	runs := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-01", Proposed: false}),
		controlRun(excludedResult("ctl-01", false)),
		controlRun(excludedResult("ctl-01", false)),
	}
	p := PoolControlSet(runs, nil)
	row := tallyOf(t, p, "ctl-01")
	if row.Status != ControlStatusUnmeasured || row.Valid != 1 || row.Excluded != 2 || row.Proposed != 0 {
		t.Fatalf("tally = %+v, want unmeasured with valid 1 / excluded 2 / proposed 0", row)
	}
	if p.Measured != 0 || len(p.Violations) != 0 || len(p.Unmeasured) != 1 {
		t.Errorf("pooling = measured %d violations %v unmeasured %v, want 0 / none / [ctl-01]", p.Measured, p.Violations, p.Unmeasured)
	}
	v := Compare(coverageBase(), coverageCands(3), runs, DefaultThresholds())
	if v.Outcome != OutcomeInconclusive {
		t.Errorf("outcome = %q, want inconclusive — one clean observation of three is not a measured control", v.Outcome)
	}
	if !strings.Contains(strings.Join(v.Unmeasured, "\n"), "coverage floor of 2") {
		t.Errorf("the reason must state the floor it missed: %v", v.Unmeasured)
	}
}

// All excluded: every row unmeasured, named, INCONCLUSIVE — with the per-control rows carrying the exclusion
// reasons so the reader knows which hosts to fix.
func TestAllExcludedNamesEveryControl(t *testing.T) {
	runs := []ControlRun{
		controlRun(excludedResult("ctl-01", true), excludedResult("ctl-02", true)),
		controlRun(excludedResult("ctl-01", true), excludedResult("ctl-02", false)),
	}
	v := Compare(coverageBase(), coverageCands(2), runs, DefaultThresholds())
	if v.Outcome != OutcomeInconclusive || v.ControlN != 0 || len(v.ControlViolations) != 0 {
		t.Fatalf("outcome %q control_n %d violations %v, want inconclusive / 0 / none", v.Outcome, v.ControlN, v.ControlViolations)
	}
	if len(v.ControlUnmeasured) != 2 || v.ControlUnmeasured[0] != "ctl-01" || v.ControlUnmeasured[1] != "ctl-02" {
		t.Errorf("control_unmeasured = %v, want [ctl-01 ctl-02]", v.ControlUnmeasured)
	}
	for _, row := range v.ControlTallies {
		if row.Status != ControlStatusUnmeasured || row.Proposed != 0 {
			t.Errorf("row %+v: an excluded proposal must not count as proposed", row)
		}
		if !strings.Contains(row.Reason, "host absent from LibreNMS") {
			t.Errorf("row %+v must carry the exclusion reason", row)
		}
	}
}

// A mandatory control no run observed is MISSING in every run and unmeasured BY NAME, and the one valid
// control beside it does not buy a pass.
func TestAMissingMandatoryControlIsNamedBesideAValidOne(t *testing.T) {
	runs := []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: false})}
	p := PoolControlSet(runs, []string{"ctl-01", "ctl-02"})
	if p.Measured != 1 || len(p.Violations) != 0 {
		t.Fatalf("measured %d violations %v, want 1 / none", p.Measured, p.Violations)
	}
	missing := tallyOf(t, p, "ctl-02")
	if missing.Status != ControlStatusUnmeasured || missing.Missing != 1 || missing.Valid != 0 || missing.Excluded != 0 {
		t.Fatalf("ctl-02 tally = %+v, want unmeasured / missing 1", missing)
	}
	v := CompareWithControlSet(coverageBase(), coverageCands(1), runs, []string{"ctl-01", "ctl-02"}, DefaultThresholds())
	if v.Outcome != OutcomeInconclusive {
		t.Fatalf("outcome = %q, want inconclusive — a valid ctl-01 must not conceal the ctl-02 nobody ran", v.Outcome)
	}
	if !strings.Contains(strings.Join(v.Unmeasured, "\n"), "negative control ctl-02") {
		t.Errorf("ctl-02 must be named in unmeasured: %v", v.Unmeasured)
	}
	// Without the mandatory set the gate can only tally what it saw: the same runs read as a clean pass of
	// ctl-01 — which is exactly why tools/evalgate supplies the corpus.
	if plain := Compare(coverageBase(), coverageCands(1), runs, DefaultThresholds()); plain.Outcome != OutcomePass {
		t.Errorf("without a mandatory set the one observed control is a measured pass; got %q (%v)", plain.Outcome, plain.Unmeasured)
	}
}

// Multiple controls with mixed observability in BOTH directions. ctl-A is valid in runs 1-2 and excluded in
// run 3 (valid 2 ≥ floor 2: measured); ctl-B is excluded in runs 1-2 and valid in run 3 (valid 1 < 2:
// unmeasured); ctl-C is valid everywhere and proposed in 2 of 3 (violated); ctl-D is valid everywhere and
// clean (pass).
func TestMixedObservabilityInBothDirections(t *testing.T) {
	runs := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-A"}, excludedResult("ctl-B", true), ControlResult{Ref: "ctl-C", Proposed: true}, ControlResult{Ref: "ctl-D"}),
		controlRun(ControlResult{Ref: "ctl-A"}, excludedResult("ctl-B", true), ControlResult{Ref: "ctl-C", Proposed: true}, ControlResult{Ref: "ctl-D"}),
		controlRun(excludedResult("ctl-A", true), ControlResult{Ref: "ctl-B"}, ControlResult{Ref: "ctl-C"}, ControlResult{Ref: "ctl-D"}),
	}
	p := PoolControlSet(runs, []string{"ctl-A", "ctl-B", "ctl-C", "ctl-D"})
	want := map[string]ControlStatus{"ctl-A": ControlStatusPass, "ctl-B": ControlStatusUnmeasured, "ctl-C": ControlStatusViolated, "ctl-D": ControlStatusPass}
	for ref, status := range want {
		if got := tallyOf(t, p, ref); got.Status != status {
			t.Errorf("%s: status %q, want %q (%+v)", ref, got.Status, status, got)
		}
	}
	if p.Measured != 3 || len(p.Violations) != 1 || p.Violations[0] != "ctl-C" || len(p.Unmeasured) != 1 || p.Unmeasured[0] != "ctl-B" {
		t.Errorf("measured %d violations %v unmeasured %v", p.Measured, p.Violations, p.Unmeasured)
	}
	if a := tallyOf(t, p, "ctl-A"); a.Valid != 2 || a.Excluded != 1 || a.Missing != 0 {
		t.Errorf("ctl-A tally %+v", a)
	}
	if b := tallyOf(t, p, "ctl-B"); b.Valid != 1 || b.Excluded != 2 || b.Proposed != 0 {
		t.Errorf("ctl-B tally %+v: the two excluded proposals must not count", b)
	}
	// A violation AND an unmeasured control: the violation is the provable defect, so the verdict is a FAIL
	// (resolveOutcome's precedence) and the unmeasured control is still named in the reasons.
	v := CompareWithControlSet(coverageBase(), coverageCands(3), runs, []string{"ctl-A", "ctl-B", "ctl-C", "ctl-D"}, DefaultThresholds())
	if v.Outcome != OutcomeFail || len(v.ControlViolations) != 1 {
		t.Errorf("outcome %q violations %v, want fail on ctl-C", v.Outcome, v.ControlViolations)
	}
	if !strings.Contains(strings.Join(v.Reasons, "\n"), "negative control ctl-B") {
		t.Errorf("the FAIL must still carry the unmeasured ctl-B: %v", v.Reasons)
	}
}

// Majority, tie and boundary, all on VALID observations. Each sub-case is one control over `runs` runs with
// the stated valid/proposed counts; excluded rows fill the rest.
func TestMajorityIsOfValidObservations(t *testing.T) {
	cases := []struct {
		name            string
		runs, valid     int
		proposed        int
		want            ControlStatus
		wantViolationIn bool
	}{
		{"1 run, 1 valid, 1 proposed — violated", 1, 1, 1, ControlStatusViolated, true},
		{"1 run, 1 valid, 0 proposed — pass", 1, 1, 0, ControlStatusPass, false},
		{"2 runs, 2 valid, 1 proposed — tie is not a majority", 2, 2, 1, ControlStatusPass, false},
		{"2 runs, 2 valid, 2 proposed — violated", 2, 2, 2, ControlStatusViolated, true},
		{"2 runs, 1 valid, 1 proposed — floor 1 met, violated (old code: 1 > 2/2 passed)", 2, 1, 1, ControlStatusViolated, true},
		{"3 runs, 3 valid, 1 proposed — pass", 3, 3, 1, ControlStatusPass, false},
		{"3 runs, 3 valid, 2 proposed — violated", 3, 3, 2, ControlStatusViolated, true},
		{"3 runs, 2 valid, 1 proposed — floor 2 met, tie, pass (old code agreed by accident)", 3, 2, 1, ControlStatusPass, false},
		{"3 runs, 2 valid, 2 proposed — violated (old code: 2 > 1.5 agreed)", 3, 2, 2, ControlStatusViolated, true},
		{"3 runs, 1 valid, 1 proposed — below floor 2, unmeasured (old code: 1 > 1.5 PASSED)", 3, 1, 1, ControlStatusUnmeasured, false},
		{"4 runs, 2 valid, 2 proposed — floor 2 met, violated (old code: 2 > 2 PASSED)", 4, 2, 2, ControlStatusViolated, true},
		{"4 runs, 2 valid, 1 proposed — tie, pass", 4, 2, 1, ControlStatusPass, false},
		{"4 runs, 1 valid, 1 proposed — below floor, unmeasured", 4, 1, 1, ControlStatusUnmeasured, false},
		{"5 runs, 3 valid, 2 proposed — violated (old code: 2 > 2.5 PASSED)", 5, 3, 2, ControlStatusViolated, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var runs []ControlRun
			for i := 0; i < c.runs; i++ {
				switch {
				case i < c.proposed:
					runs = append(runs, controlRun(ControlResult{Ref: "ctl-01", Proposed: true}))
				case i < c.valid:
					runs = append(runs, controlRun(ControlResult{Ref: "ctl-01"}))
				default:
					runs = append(runs, controlRun(excludedResult("ctl-01", true)))
				}
			}
			p := PoolControlSet(runs, nil)
			row := tallyOf(t, p, "ctl-01")
			if row.Status != c.want {
				t.Fatalf("status %q, want %q (tally %+v, floor %d)", row.Status, c.want, row, p.Floor)
			}
			if row.Valid != c.valid || row.Proposed != c.proposed || row.Excluded != c.runs-c.valid {
				t.Errorf("tally %+v does not carry the valid/proposed/excluded counts", row)
			}
			if got := len(p.Violations) == 1; got != c.wantViolationIn {
				t.Errorf("violations %v, want present=%v", p.Violations, c.wantViolationIn)
			}
			if (p.Floor) != (c.runs+1)/2 {
				t.Errorf("floor %d, want ceil(%d/2)=%d", p.Floor, c.runs, (c.runs+1)/2)
			}
		})
	}
}

// Duplicate ids within ONE run are an integrity problem: the observations are collapsed (valid if any was
// valid, PROPOSED if any valid one proposed), the duplicate is reported, and the run is never a pass. The
// problem adds, it never masks: whichever duplicate proposed, the control is a violation and the run a FAIL.
func TestADuplicateRefWithinOneRunIsAnIntegrityProblem(t *testing.T) {
	proposingSecond := []ControlRun{{N: 2, Results: []ControlResult{
		{Ref: "ctl-01", Proposed: false},
		{Ref: "ctl-01", Proposed: true},
		{Ref: "ctl-02"},
	}}}
	p := PoolControlSet(proposingSecond, nil)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], `control "ctl-01" observed 2 times`) {
		t.Fatalf("problems = %v, want the duplicate named", p.Problems)
	}
	row := tallyOf(t, p, "ctl-01")
	if row.Valid != 1 || row.Proposed != 1 || row.Status != ControlStatusViolated || !strings.Contains(row.Reason, "integrity") {
		t.Fatalf("ctl-01 tally %+v: the duplicates collapse to ONE valid observation that PROPOSED (the second one did), and the row says why it is suspect", row)
	}
	if v := Compare(coverageBase(), coverageCands(1), proposingSecond, DefaultThresholds()); v.Outcome != OutcomeFail || len(v.ControlProblems) != 1 || len(v.ControlViolations) != 1 {
		t.Errorf("outcome %q violations %v problems %v — the proposing duplicate must be a violation (FAIL), with the problem recorded", v.Outcome, v.ControlViolations, v.ControlProblems)
	}
	// The other order is the same violation.
	proposingFirst := []ControlRun{controlRun(ControlResult{Ref: "ctl-01", Proposed: true}, ControlResult{Ref: "ctl-01"})}
	if v := Compare(coverageBase(), coverageCands(1), proposingFirst, DefaultThresholds()); v.Outcome != OutcomeFail {
		t.Errorf("outcome %q, want fail — the proposal is the provable defect, the duplicate is extra", v.Outcome)
	}
	// Two clean duplicates: a pass row, but the problem still blocks the certification (INCONCLUSIVE).
	clean := []ControlRun{{N: 1, Results: []ControlResult{{Ref: "ctl-01"}, {Ref: "ctl-01"}}}}
	if v := Compare(coverageBase(), coverageCands(1), clean, DefaultThresholds()); v.Outcome != OutcomeInconclusive || v.ControlTallies[0].Status != ControlStatusPass {
		t.Errorf("outcome %q status %q — clean duplicates pass the control and still cannot certify the run", v.Outcome, v.ControlTallies[0].Status)
	}
	// An excluded duplicate beside a valid one collapses to ONE valid observation (excluded only if all were).
	mixed := []ControlRun{{N: 1, Results: []ControlResult{excludedResult("ctl-01", true), {Ref: "ctl-01"}}}}
	if row := tallyOf(t, PoolControlSet(mixed, nil), "ctl-01"); row.Valid != 1 || row.Excluded != 0 || row.Proposed != 0 {
		t.Errorf("mixed duplicate tally %+v, want valid 1 / excluded 0 / proposed 0 (the excluded proposal counts for nothing)", row)
	}
}

// With neither a control corpus nor a declared n, the mandatory set is UNKNOWN — a control dropped from every
// run could not be detected — so a derived-set run on n-less files cannot certify. With either one it can.
func TestADerivedSetOnRunsWithNoDeclaredNCannotCertify(t *testing.T) {
	nless := []ControlRun{{Results: []ControlResult{{Ref: "ctl-01"}}}}
	p := PoolControlSet(nless, nil)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "declares no n and no control corpus was supplied") {
		t.Fatalf("problems = %v", p.Problems)
	}
	if v := Compare(coverageBase(), coverageCands(1), nless, DefaultThresholds()); v.Outcome != OutcomeInconclusive {
		t.Errorf("outcome %q, want inconclusive — the set is unknown", v.Outcome)
	}
	if p := PoolControlSet(nless, []string{"ctl-01"}); len(p.Problems) != 0 {
		t.Errorf("with the corpus supplied the set is known; problems = %v", p.Problems)
	}
	if p := PoolControlSet([]ControlRun{{N: 1, Results: []ControlResult{{Ref: "ctl-01"}}}}, nil); len(p.Problems) != 0 {
		t.Errorf("with n declared the count check protects; problems = %v", p.Problems)
	}
}

// A run whose declared n exceeds its distinct refs lost a control; without a mandatory set the gate cannot
// name it but it can count it, and the count is enough to refuse a pass.
func TestADeclaredNThatExceedsTheRefsIsAnIntegrityProblem(t *testing.T) {
	runs := []ControlRun{{N: 5, Results: []ControlResult{{Ref: "ctl-01"}, {Ref: "ctl-02"}, {Ref: "ctl-03"}, {Ref: "ctl-04"}}}}
	p := PoolControlSet(runs, nil)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "declares n=5 but carries 4") {
		t.Fatalf("problems = %v", p.Problems)
	}
	if v := Compare(coverageBase(), coverageCands(1), runs, DefaultThresholds()); v.Outcome != OutcomeInconclusive {
		t.Errorf("outcome %q, want inconclusive — four clean controls of a declared five is a lost control", v.Outcome)
	}
}

// A ref the mandatory set does not name is an integrity problem (the run is not about the corpus it is
// scored against) — and a proposal on it is still a violation: the problem adds, it never masks.
func TestARefOutsideTheCorpusIsAnIntegrityProblemThatNeverMasks(t *testing.T) {
	clean := []ControlRun{controlRun(ControlResult{Ref: "ctl-01"}, ControlResult{Ref: "ctl-99"})}
	p := PoolControlSet(clean, []string{"ctl-01"})
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], `"ctl-99" is not in the control corpus`) {
		t.Fatalf("problems = %v", p.Problems)
	}
	if v := CompareWithControlSet(coverageBase(), coverageCands(1), clean, []string{"ctl-01"}, DefaultThresholds()); v.Outcome != OutcomeInconclusive {
		t.Errorf("outcome %q, want inconclusive", v.Outcome)
	}
	proposing := []ControlRun{controlRun(ControlResult{Ref: "ctl-01"}, ControlResult{Ref: "ctl-99", Proposed: true})}
	if v := CompareWithControlSet(coverageBase(), coverageCands(1), proposing, []string{"ctl-01"}, DefaultThresholds()); v.Outcome != OutcomeFail || len(v.ControlViolations) != 1 {
		t.Errorf("outcome %q violations %v, want fail on ctl-99", v.Outcome, v.ControlViolations)
	}
	// And a corpus that names a ref twice is itself a problem.
	if p := PoolControlSet(clean, []string{"ctl-01", "ctl-01", "ctl-99"}); len(p.Problems) == 0 || !strings.Contains(p.Problems[0], "more than once") {
		t.Errorf("a duplicated corpus ref must be reported: %v", p.Problems)
	}
}

// No runs: nothing pooled, nothing violated, nothing measured — and Compare with no controls stays as it
// was (ControlN 0, ControlPass true); tools/evalgate's markMissingControls owns that case.
func TestNoRunsPoolsNothing(t *testing.T) {
	p := PoolControlSet(nil, []string{"ctl-01"})
	if p.Runs != 0 || p.Measured != 0 || len(p.Tallies) != 0 || len(p.Violations) != 0 || len(p.Problems) != 0 {
		t.Errorf("empty pooling = %+v", p)
	}
	v := Compare(coverageBase(), coverageCands(1), nil, DefaultThresholds())
	if v.ControlN != 0 || !v.ControlPass || len(v.ControlTallies) != 0 {
		t.Errorf("Compare with no controls changed shape: %+v", v)
	}
}

// The valid positive case: the whole corpus observed in every run, nothing proposed — a measured PASS with
// one pass row per control.
func TestTheWholeCorpusObservedAndCleanIsAMeasuredPass(t *testing.T) {
	corpus := []string{"ctl-01", "ctl-02", "ctl-03", "ctl-04", "ctl-05"}
	var runs []ControlRun
	for i := 0; i < 3; i++ {
		r := ControlRun{N: len(corpus)}
		for _, ref := range corpus {
			r.Results = append(r.Results, ControlResult{Ref: ref, Outcome: "no-proposal:stop"})
		}
		runs = append(runs, r)
	}
	v := CompareWithControlSet(coverageBase(), coverageCands(3), runs, corpus, DefaultThresholds())
	if v.Outcome != OutcomePass || !v.ControlPass || v.ControlN != 5 || len(v.ControlUnmeasured) != 0 || len(v.ControlProblems) != 0 {
		t.Fatalf("outcome %q control_pass %v control_n %d unmeasured %v problems %v", v.Outcome, v.ControlPass, v.ControlN, v.ControlUnmeasured, v.ControlProblems)
	}
	if len(v.ControlTallies) != 5 {
		t.Fatalf("want 5 tally rows, got %d", len(v.ControlTallies))
	}
	for _, row := range v.ControlTallies {
		if row.Status != ControlStatusPass || row.Valid != 3 || row.Runs != 3 || row.Reason != "" {
			t.Errorf("row %+v, want a clean pass of 3/3", row)
		}
	}
}

// Excluded results never supply the explanation. For a VIOLATION the detail must quote a valid proposing
// observation even when an excluded proposing one comes first in run order; for a PASS the tally's proposed
// count must ignore the excluded proposal, so the "explanation" of the pass (proposed 0 of 2) is true.
func TestAnExcludedResultNeverSuppliesTheExplanation(t *testing.T) {
	runs := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Excluded: true, ExcludedReason: "host absent from LibreNMS", Conclusion: "EXCLUDED-TEXT: whatever the agent found on an unmonitored host"}),
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Band: "POLL_PAUSE", Conclusion: "VALID-TEXT: refused the maintenance claim and grounded on syslog"}),
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Band: "POLL_PAUSE", Conclusion: "VALID-TEXT: second run, same grounding"}),
	}
	v := Compare(coverageBase(), coverageCands(3), runs, DefaultThresholds())
	if v.ControlPass || len(v.ControlViolationDetail) != 1 {
		t.Fatalf("control_pass %v detail %+v, want one violation with detail", v.ControlPass, v.ControlViolationDetail)
	}
	if d := v.ControlViolationDetail[0]; !strings.HasPrefix(d.Conclusion, "VALID-TEXT") || d.Band != "POLL_PAUSE" {
		t.Errorf("the explanation came from an excluded observation: %+v", d)
	}
	if got := ControlDetails(runs, []string{"ctl-01"}); len(got) != 1 || !strings.HasPrefix(got[0].Conclusion, "VALID-TEXT") {
		t.Errorf("ControlDetails quoted an excluded result: %+v", got)
	}
	// A violation whose ONLY proposing results are excluded cannot be — the pooling would never flag it. If a
	// caller passes one anyway, the detail says the pooling and the results disagree rather than quoting an
	// excluded conclusion.
	onlyExcluded := []ControlRun{controlRun(ControlResult{Ref: "ctl-02", Proposed: true, Excluded: true, Conclusion: "EXCLUDED-TEXT"})}
	if got := ControlDetails(onlyExcluded, []string{"ctl-02"}); len(got) != 1 || strings.Contains(got[0].Conclusion, "EXCLUDED-TEXT") || !strings.Contains(got[0].Conclusion, "disagree") {
		t.Errorf("an excluded result supplied a violation's explanation: %+v", got)
	}
	// The pass side: an excluded proposal beside two valid clean observations is a pass whose tally says
	// proposed 0 of 2 — the excluded proposal is not part of the explanation.
	pass := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Excluded: true, ExcludedReason: "host absent from LibreNMS"}),
		controlRun(ControlResult{Ref: "ctl-01"}),
		controlRun(ControlResult{Ref: "ctl-01"}),
	}
	pv := Compare(coverageBase(), coverageCands(3), pass, DefaultThresholds())
	if pv.Outcome != OutcomePass || len(pv.ControlViolationDetail) != 0 {
		t.Fatalf("outcome %q detail %+v, want a clean pass", pv.Outcome, pv.ControlViolationDetail)
	}
	if row := pv.ControlTallies[0]; row.Proposed != 0 || row.Valid != 2 || row.Excluded != 1 || row.Reason != "" {
		t.Errorf("pass tally %+v: the excluded proposal leaked into the explanation", row)
	}
}

// The verdict's JSON carries the per-control rows and omits them when there were none, so archived records
// before this change still round-trip and new ones are self-explaining.
func TestVerdictJSONCarriesTheControlTallies(t *testing.T) {
	runs := []ControlRun{controlRun(ControlResult{Ref: "ctl-01"}, excludedResult("ctl-02", false))}
	v := Compare(coverageBase(), coverageCands(1), runs, DefaultThresholds())
	raw := string(mustJSON(t, v))
	for _, want := range []string{`"control_tallies"`, `"control_unmeasured"`, `"status": "pass"`, `"status": "unmeasured"`, `"valid": 1`, `"excluded": 1`} {
		if !strings.Contains(raw, want) {
			t.Errorf("verdict JSON lacks %s:\n%s", want, raw)
		}
	}
	none := Compare(coverageBase(), coverageCands(1), nil, DefaultThresholds())
	if raw := string(mustJSON(t, none)); strings.Contains(raw, "control_tallies") || strings.Contains(raw, "control_problems") {
		t.Errorf("a verdict with no controls must omit the control rows:\n%s", raw)
	}
}
