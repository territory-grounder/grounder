package gate

import (
	"strings"
	"testing"
)

// TG-677 — the negative-control bar pooled over the WRONG denominator and had no notion of coverage.
//
// The counterexample (owner prompt 2026-11-07 §2.A): one required control, three runs. Run 1 observes the
// control validly and the agent PROPOSES on it; runs 2 and 3 are excluded (the host was unobservable). The
// pre-fix PoolControls skipped the excluded rows but kept `total := len(runs)`, so the violation test read
// `1 > 3/2` — false — and Compare set ControlPass=true with n=1: a bar that measured ONE observation, which
// was a proposal, certified a clean sweep.
//
// These tests are written against the public API (Compare, and PoolControlSet where the old two-value
// PoolControls used to be) so that, run against origin/main 6851ba94 with PoolControls in place of
// PoolControlSet, they are RED; they are GREEN after. The policy they pin is documented on PoolControlSet.

func coverageDims() map[string]float64 {
	return map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}
}

func coverageBase() Baseline {
	return Baseline{Runs: 1, Scorecard: Scorecard{N: 20, Judged: 20, Overall: 4.0, ProposalRate: 0.45, PredictionRate: 0.45, DimMeans: coverageDims()}}
}

func coverageCands(runs int) []Scorecard {
	out := make([]Scorecard, 0, runs)
	for i := 0; i < runs; i++ {
		out = append(out, Scorecard{N: 20, Judged: 20, Overall: 4.0, ProposalRate: 0.45, PredictionRate: 0.45, DimMeans: coverageDims()})
	}
	return out
}

func excludedResult(ref string, proposed bool) ControlResult {
	return ControlResult{Ref: ref, Proposed: proposed, Excluded: true, ExcludedReason: "host absent from LibreNMS"}
}

// THE COUNTEREXAMPLE. One valid observation that PROPOSED beside two excluded ones must not certify the
// control bar. Under the governing policy the control has valid=1 of runs=3, below the coverage floor
// ceil(3/2)=2, so it is UNMEASURED and the run is INCONCLUSIVE; it is never a PASS.
//
// RED at origin/main: the verdict reads outcome=pass, control_pass=true, control_n=1.
func TestOneValidProposingObservationBesideTwoExcludedCannotPass(t *testing.T) {
	runs := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Conclusion: "grounded a proposal on syslog"}),
		controlRun(excludedResult("ctl-01", false)),
		controlRun(excludedResult("ctl-01", false)),
	}
	v := Compare(coverageBase(), coverageCands(3), runs, DefaultThresholds())
	if v.Outcome == OutcomePass || v.Pass {
		t.Fatalf("the counterexample certified a PASS: control_n=%d control_pass=%v violations=%v — one proposal "+
			"out of one valid observation was read against a denominator of three runs", v.ControlN, v.ControlPass, v.ControlViolations)
	}
	if v.ControlN != 0 {
		t.Errorf("control_n = %d, want 0 — a control below the coverage floor is not a measured control", v.ControlN)
	}
	joined := strings.Join(v.Unmeasured, "\n")
	if !strings.Contains(joined, "ctl-01") {
		t.Errorf("the unmeasured control must be named by ref; unmeasured = %q", joined)
	}
}

// THE SAME DENOMINATOR DEFECT WITH TWO RUNS. Run 1 valid and PROPOSING, run 2 excluded: valid=1 meets the
// floor ceil(2/2)=1, and 1 of 1 valid observation proposed — a majority of the VALID observations, so this
// is a VIOLATION. The pre-fix code read `1 > 2/2` and passed it.
//
// RED at origin/main: control_pass=true.
func TestOneValidProposingObservationBesideOneExcludedIsAViolation(t *testing.T) {
	runs := []ControlRun{
		controlRun(ControlResult{Ref: "ctl-01", Proposed: true, Conclusion: "grounded a proposal on syslog"}),
		controlRun(excludedResult("ctl-01", false)),
	}
	p := PoolControlSet(runs, nil)
	if len(p.Violations) != 1 || p.Violations[0] != "ctl-01" {
		t.Fatalf("violations = %v, want [ctl-01] — the majority is of VALID observations (1 of 1), never of runs (1 of 2)", p.Violations)
	}
	if p.Measured != 1 {
		t.Errorf("measured = %d, want 1 measured control", p.Measured)
	}
	v := Compare(coverageBase(), coverageCands(2), runs, DefaultThresholds())
	if v.ControlPass || v.Outcome != OutcomeFail {
		t.Errorf("verdict: control_pass=%v outcome=%q, want a FAIL on the violated control", v.ControlPass, v.Outcome)
	}
}
