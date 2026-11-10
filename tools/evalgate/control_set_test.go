package main

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/eval"
	"github.com/territory-grounder/grounder/eval/gate"
)

// TG-677 end-to-end through the REAL binary (runCLI re-execs main): the negative-control coverage policy as
// the shell and CI see it — exit status and printed headline — not only as gate.Compare returns it.

func coverageCard() gate.Scorecard {
	return gate.Scorecard{N: 20, Judged: 20, Overall: 4.0, ProposalRate: 0.45, PredictionRate: 0.45,
		DimMeans: map[string]float64{"appropriate_band": 4, "correct_diagnosis": 4, "evidence_grounded": 4, "sensible_proposal": 4, "falsifiable_prediction": 4}}
}

func validProposing(ref string) gate.ControlResult {
	return gate.ControlResult{Ref: ref, Proposed: true, Band: "POLL_PAUSE", Outcome: "proposal", Conclusion: "refused the maintenance claim and grounded a proposal on syslog"}
}

func excluded(ref string) gate.ControlResult {
	return gate.ControlResult{Ref: ref, Excluded: true, ExcludedReason: "host absent from LibreNMS"}
}

func corpusFile(t *testing.T, dir string, refs ...string) string {
	t.Helper()
	var incidents []eval.Incident
	for _, ref := range refs {
		incidents = append(incidents, eval.Incident{ExternalRef: ref, Host: "ctl-host", Summary: "benign; correct action = do nothing"})
	}
	return writeJSON(t, dir, "controls-corpus.json", incidents)
}

func threeRunArms(t *testing.T, dir string) (base, cand string) {
	t.Helper()
	c := writeJSON(t, dir, "card.json", coverageCard())
	return strings.Join([]string{c, c, c}, ","), strings.Join([]string{c, c, c}, ",")
}

// THE COUNTEREXAMPLE THROUGH THE CLI. Before this change the same fixtures printed "negative controls: 1
// checked, 0 violation(s) [] PASS" and "GATE: PASS", exit 0 (recorded on TG-677). Now: INCONCLUSIVE, exit 3,
// ctl-01 named as unmeasured with the floor it missed.
func TestCLI_OneValidProposingBesideTwoExcludedIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	base, cand := threeRunArms(t, dir)
	r1 := writeJSON(t, dir, "ctl1.json", gate.ControlRun{N: 1, Results: []gate.ControlResult{validProposing("ctl-01")}})
	r2 := writeJSON(t, dir, "ctl2.json", gate.ControlRun{N: 1, Results: []gate.ControlResult{excluded("ctl-01")}})
	r3 := writeJSON(t, dir, "ctl3.json", gate.ControlRun{N: 1, Results: []gate.ControlResult{excluded("ctl-01")}})
	code, out := runCLI(t, "--mode", "change", "--runs", "3", "--base", base, "--candidate", cand,
		"--controls", strings.Join([]string{r1, r2, r3}, ","), "--control-corpus", corpusFile(t, dir, "ctl-01"))
	if code == 0 || strings.Contains(out, "GATE: PASS") {
		t.Fatalf("the counterexample certified a PASS (exit %d):\n%s", code, out)
	}
	if code != 3 || !strings.Contains(out, "GATE: INCONCLUSIVE") {
		t.Fatalf("want exit 3 INCONCLUSIVE, got %d:\n%s", code, out)
	}
	for _, want := range []string{"UNMEASURED: negative control ctl-01", "coverage floor of 2", "0 measured, 1 unmeasured", "ctl-01   UNMEASURED valid 1/3  excluded 2  missing 0  proposed 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// Two runs, one valid proposing, one excluded: valid 1 meets floor 1 and 1 of 1 proposed — a VIOLATION, exit
// 1. Before this change: "1 > 2/2" — PASS, exit 0.
func TestCLI_OneValidProposingBesideOneExcludedFails(t *testing.T) {
	dir := t.TempDir()
	c := writeJSON(t, dir, "card.json", coverageCard())
	arm := c + "," + c
	r1 := writeJSON(t, dir, "ctl1.json", gate.ControlRun{N: 1, Results: []gate.ControlResult{validProposing("ctl-01")}})
	r2 := writeJSON(t, dir, "ctl2.json", gate.ControlRun{N: 1, Results: []gate.ControlResult{excluded("ctl-01")}})
	code, out := runCLI(t, "--mode", "change", "--runs", "2", "--base", arm, "--candidate", arm,
		"--controls", r1+","+r2, "--control-corpus", corpusFile(t, dir, "ctl-01"))
	if code != 1 || !strings.Contains(out, "GATE: FAIL") || !strings.Contains(out, "PROPOSED on 1 negative control(s): [ctl-01]") {
		t.Fatalf("want exit 1 FAIL naming ctl-01, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ctl-01   VIOLATED   valid 1/2  excluded 1  missing 0  proposed 1") {
		t.Errorf("the tally row must show 1 of 1 valid proposed:\n%s", out)
	}
	// The explanation quotes the VALID observation, never the excluded one.
	if !strings.Contains(out, "grounded a proposal on syslog") {
		t.Errorf("the violation detail must quote the valid observation's conclusion:\n%s", out)
	}
}

// A mandatory control no run observed is named, and the clean controls beside it do not buy a pass.
func TestCLI_MissingMandatoryControlIsNamed(t *testing.T) {
	dir := t.TempDir()
	c := writeJSON(t, dir, "card.json", coverageCard())
	run := writeJSON(t, dir, "ctl.json", gate.ControlRun{N: 2, Results: []gate.ControlResult{{Ref: "ctl-01"}, {Ref: "ctl-02"}}})
	code, out := runCLI(t, "--mode", "change", "--base", c, "--candidate", c, "--controls", run,
		"--control-corpus", corpusFile(t, dir, "ctl-01", "ctl-02", "ctl-03"))
	if code != 3 || !strings.Contains(out, "UNMEASURED: negative control ctl-03") || !strings.Contains(out, "1 missing") {
		t.Fatalf("want exit 3 naming the missing ctl-03, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "2 measured, 1 unmeasured") {
		t.Errorf("the headline must count both:\n%s", out)
	}
}

// An explicitly named corpus that cannot be read is a usage error, never a silently derived set.
func TestCLI_ExplicitUnreadableCorpusIsAUsageError(t *testing.T) {
	dir := t.TempDir()
	c := writeJSON(t, dir, "card.json", coverageCard())
	code, out := runCLI(t, "--mode", "change", "--base", c, "--candidate", c, "--controls", cleanControls(t, dir),
		"--control-corpus", dir+"/does-not-exist.json")
	if code != 2 || !strings.Contains(out, "control corpus") {
		t.Fatalf("want exit 2 naming the corpus, got %d:\n%s", code, out)
	}
}

// The DEFAULT corpus path missing (the tests run from tools/evalgate, where eval/controls.json does not exist)
// derives the set from the runs and SAYS SO — in the report and in the archived verdict's warnings.
func TestCLI_DefaultCorpusAbsentDerivesTheSetAndDisclosesIt(t *testing.T) {
	dir := t.TempDir()
	c := writeJSON(t, dir, "card.json", coverageCard())
	code, out := runCLI(t, "--mode", "change", "--base", c, "--candidate", c, "--controls", cleanControls(t, dir), "--json")
	if code != 0 {
		t.Fatalf("a clean pair with a derived control set must still certify; exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "DERIVED from the supplied runs") || !strings.Contains(out, `"warnings"`) {
		t.Fatalf("the derived set must be disclosed in the report and the verdict:\n%s", out)
	}
	if !strings.Contains(out, "2 measured, 0 unmeasured") {
		t.Errorf("both observed controls are measured:\n%s", out)
	}
}

// The valid positive case: the whole corpus, three runs, nothing proposed — PASS with five pass rows.
func TestCLI_WholeCorpusCleanPasses(t *testing.T) {
	dir := t.TempDir()
	base, cand := threeRunArms(t, dir)
	refs := []string{"ctl-01", "ctl-02", "ctl-03", "ctl-04", "ctl-05"}
	var runs []string
	for i := 0; i < 3; i++ {
		r := gate.ControlRun{N: 5}
		for _, ref := range refs {
			r.Results = append(r.Results, gate.ControlResult{Ref: ref, Outcome: "no-proposal:stop"})
		}
		runs = append(runs, writeJSON(t, dir, "ctl"+string(rune('a'+i))+".json", r))
	}
	code, out := runCLI(t, "--mode", "change", "--runs", "3", "--base", base, "--candidate", cand,
		"--controls", strings.Join(runs, ","), "--control-corpus", corpusFile(t, dir, refs...))
	if code != 0 || !strings.Contains(out, "GATE: PASS") || !strings.Contains(out, "5 measured, 0 unmeasured, 0 violation(s)") {
		t.Fatalf("want exit 0 PASS with five measured controls, got %d:\n%s", code, out)
	}
	if strings.Count(out, "PASS       valid 3/3") != 5 {
		t.Errorf("want five pass rows:\n%s", out)
	}
}

// --runs 2 with ONE control run supplied: the shell adds --controls per run only when that run's file exists,
// so a missing file is a candidate run that was never asked the question — INCONCLUSIVE, named (TG-677 review).
func TestCLI_FewerControlRunsThanRunsIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	c := writeJSON(t, dir, "card.json", coverageCard())
	arm := c + "," + c
	code, out := runCLI(t, "--mode", "change", "--runs", "2", "--base", arm, "--candidate", arm,
		"--controls", cleanControls(t, dir), "--control-corpus", corpusFile(t, dir, "ctl-1", "ctl-2"))
	if code != 3 || !strings.Contains(out, "--runs 2 but 1 control run(s) were supplied") {
		t.Fatalf("want exit 3 naming the missing control run, got %d:\n%s", code, out)
	}
}

// A duplicated ref inside one run reaches the report as a control-run integrity problem and blocks.
func TestCLI_DuplicateRefIsAnIntegrityProblem(t *testing.T) {
	dir := t.TempDir()
	c := writeJSON(t, dir, "card.json", coverageCard())
	run := writeJSON(t, dir, "ctl.json", gate.ControlRun{N: 1, Results: []gate.ControlResult{{Ref: "ctl-01"}, {Ref: "ctl-01"}}})
	code, out := runCLI(t, "--mode", "change", "--base", c, "--candidate", c, "--controls", run, "--control-corpus", corpusFile(t, dir, "ctl-01"))
	if code != 3 || !strings.Contains(out, `control-run integrity: run 1: control "ctl-01" observed 2 times`) {
		t.Fatalf("want exit 3 with the duplicate named, got %d:\n%s", code, out)
	}
}
