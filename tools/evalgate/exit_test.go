package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/eval/gate"
)

// TestExitFor pins the CLI's half of TG-258: the exit status is what CI, eval/eval-gate.sh and `make`
// actually read, so an INCONCLUSIVE verdict — a run that declares it measured nothing about a gated
// capability — must leave the process NON-ZERO, exactly like a regression. The committed record
// eval/history/2026-07-30-change-74f599c65f39/verdict.json is what an exit-0 on that shape looks like
// afterwards: `"pass": true` beside "this run proves nothing about propose behavior in either direction".
//
// The distinct code 3 (vs 1 for a regression) is so a caller can tell "your change made it worse" from
// "this run certified nothing" without parsing text. NB: `go run` collapses any non-zero to 1, so 3 is only
// observable when the built binary is invoked; the guarantee that matters — non-zero — holds either way.
func TestExitFor(t *testing.T) {
	for _, c := range []struct {
		name string
		v    gate.Verdict
		want int
	}{
		{"pass exits 0", gate.Verdict{Outcome: gate.OutcomePass, Pass: true}, 0},
		{"regression exits 1", gate.Verdict{Outcome: gate.OutcomeFail, Reasons: []string{"overall Δ -0.20 < -0.15"}}, 1},
		{"unmeasured exits 3", gate.Verdict{Outcome: gate.OutcomeInconclusive, Unmeasured: []string{"proposal capability: all 3 expected-propose incident(s) were stale"}}, 3},
		// A Verdict that reached the CLI without an outcome is a bug upstream, not a green run: it must fail
		// closed. This is the arm that stops "the field was never set" from reading as success.
		{"zero-value verdict fails closed", gate.Verdict{}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := exitFor(c.v); got != c.want {
				t.Fatalf("exitFor(%q) = %d, want %d", c.v.Outcome, got, c.want)
			}
		})
	}
	// The property that must survive every future edit: only OutcomePass is allowed to unblock a merge.
	for _, o := range []gate.Outcome{gate.OutcomeFail, gate.OutcomeInconclusive, gate.Outcome("something-new")} {
		if exitFor(gate.Verdict{Outcome: o, Pass: true}) == 0 {
			t.Fatalf("outcome %q exited 0 — a non-PASS outcome must never unblock a merge, even if Pass is set", o)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------
// The END-TO-END exit oracle: these tests execute the REAL main().
//
// TestExitFor above proves the exit-code TABLE is right. It does not prove the table is WIRED, and that gap
// is not hypothetical: with `if code := exitFor(v); code != 0 { os.Exit(code) }` deleted from the change-mode
// arm — leaving the identical "GATE: INCONCLUSIVE — NOT a pass … do NOT merge on this verdict" report on
// stdout — `go test ./eval/gate/ ./tools/evalgate/` stayed fully green while the CLI exited 0 on the real
// archived unmeasured record (measured 2026-08-03). The exit status is the ONLY thing eval/eval-gate.sh
// (`exit $?`), .gitlab-ci.yml (`RC -ne 0`, which files the YouTrack issue) and `make eval-gate` read; a gate
// whose verdict never reaches os.Exit is precisely the control this ticket exists to abolish — one that
// prints a scary number and returns success. The same hole covered `loadControls` (silently dropping every
// control run), the candidate-arm integrity abort, and the printed headline itself: all four survived the
// unit oracle because nothing executed main().
//
// Mechanism: the test binary re-execs ITSELF with TG_EVALGATE_E2E_ARGS set; TestMain sees the variable,
// replaces os.Args and hands control to main(), so the process really is evalgate — flag parsing, os.Exit
// and all. No `go build`, no toolchain assumptions, no network; it runs anywhere `go test` runs.
const (
	e2eArgsEnv = "TG_EVALGATE_E2E_ARGS"
	e2eArgSep  = "\x1f" // ASCII unit separator: cannot occur in a path or a flag value
)

func TestMain(m *testing.M) {
	if raw, ok := os.LookupEnv(e2eArgsEnv); ok {
		os.Args = append([]string{"evalgate"}, strings.Split(raw, e2eArgSep)...)
		main()
		os.Exit(0) // main() returning without an os.Exit IS the "success" case; make that explicit.
	}
	os.Exit(m.Run())
}

// runCLI executes the CLI in a child process and returns its exit status and combined output. A child that
// could not be started at all is a hard FAILURE, never a skip: a gate oracle that quietly does not run is
// the defect this file is about (absent is visible; skipped is not).
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), e2eArgsEnv+"="+strings.Join(args, e2eArgSep))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("could not execute the gate CLI (%v) — this oracle must RUN, not silently pass: %s", err, out)
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		t.Fatalf("gate CLI was killed by a signal instead of exiting (%v); output:\n%s", cmd.ProcessState, out)
	}
	t.Logf("evalgate %v => exit %d\n%s", args, code, out)
	return code, string(out)
}

// archivedChangeRun is the real crime scene: the committed quality record of a change-gate run that returned
// `"pass": true` beside its own "PROPOSAL CAPABILITY UNMEASURED … proves nothing" warning, and unblocked a
// merge. Driving the CLI with the archived record (not a hand-rolled lookalike) is what makes these tests
// evidence about what actually shipped.
const archivedChangeRun = "../../eval/history/2026-07-30-change-74f599c65f39"

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	var out T
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture %s must exist: %v", path, err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return out
}

func writeJSON(t *testing.T, dir, name string, val any) string {
	t.Helper()
	p := filepath.Join(dir, name)
	b, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// cleanControls is a negative-control pass the agent handled CORRECTLY (it stood down on every benign
// incident), so a verdict built with it is attributable to the other bars alone.
func cleanControls(t *testing.T, dir string) string {
	t.Helper()
	return writeJSON(t, dir, "controls.json", gate.ControlRun{N: 2, Results: []gate.ControlResult{
		{Ref: "ctl-1", Proposed: false, Band: "none", Outcome: "stood down", Conclusion: "benign"},
		{Ref: "ctl-2", Proposed: false, Band: "none", Outcome: "stood down", Conclusion: "expected"},
	}})
}

// TestCLI_ArchivedUnmeasuredRunExitsNonZero is the executed end-to-end oracle for TG-258: the archived
// unmeasured run, fed back through the real binary, must leave the process NON-ZERO and must not print the
// PASS headline. Controls are supplied and clean, so the only thing blocking this run is the bar the run
// itself made inapplicable.
func TestCLI_ArchivedUnmeasuredRunExitsNonZero(t *testing.T) {
	dir := t.TempDir()
	comparator := readJSON[gate.Baseline](t, filepath.Join(archivedChangeRun, "comparator.json"))
	basePath := writeJSON(t, dir, "base.json", comparator.Scorecard)
	candPath := filepath.Join(archivedChangeRun, "scorecard.json")

	code, out := runCLI(t, "--mode", "change", "--runs", "1",
		"--base", basePath, "--candidate", candPath, "--controls", cleanControls(t, dir))

	if code == 0 {
		t.Fatalf("TG-258 REGRESSION: the gate exited 0 on the archived run that certifies nothing — the merge is unblocked. Output:\n%s", out)
	}
	if code != 3 {
		t.Fatalf("an unmeasured run must exit 3 (INCONCLUSIVE), got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "GATE: INCONCLUSIVE") {
		t.Fatalf("the headline a human reads must say INCONCLUSIVE:\n%s", out)
	}
	if strings.Contains(out, "GATE: PASS") {
		t.Fatalf("a run that measured nothing must never print the PASS headline:\n%s", out)
	}
	if !strings.Contains(out, "UNMEASURED: proposal capability") {
		t.Fatalf("the report must name the capability that went unproven:\n%s", out)
	}
}

// TestCLI_MeasuredCleanRunStillExitsZero is the anti-vacuity half: a gate that can never exit 0 is as
// useless as one that always does. The same shapes, with propose capability actually measured (4 live
// expected-propose incidents at recall 1.00) and a clean control arm, must still certify.
func TestCLI_MeasuredCleanRunStillExitsZero(t *testing.T) {
	dir := t.TempDir()
	base, cand := measurableArms()
	code, out := runCLI(t, "--mode", "change", "--runs", "1",
		"--base", writeJSON(t, dir, "base.json", base),
		"--candidate", writeJSON(t, dir, "cand.json", cand),
		"--controls", cleanControls(t, dir))
	if code != 0 || !strings.Contains(out, "GATE: PASS") {
		t.Fatalf("a run that measured every bar and broke none must PASS and exit 0; got %d:\n%s", code, out)
	}
}

// TestCLI_MissingControlArmIsInconclusive covers the SECOND symptom of the same defect. The negative-control
// bar exists only if a control arm was supplied, and eval/eval-gate.sh supplies it conditionally
// (`[ -f "$cand_ctrl" ]`): a candidate arm that never wrote a controls file used to be certified having never
// been asked whether it proposes on benign incidents — no flag, no warning, GATE: PASS, exit 0. Deleting the
// controls (`ctrlRuns = nil` in main, or an omitted --controls in the shell) must now be visible in the exit
// status, exactly like the unmeasured propose capability.
func TestCLI_MissingControlArmIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	base, cand := measurableArms()
	code, out := runCLI(t, "--mode", "change", "--runs", "1",
		"--base", writeJSON(t, dir, "base.json", base),
		"--candidate", writeJSON(t, dir, "cand.json", cand))
	if code == 0 {
		t.Fatalf("a run with NO negative-control arm skipped a documented bar and must not certify; exit 0:\n%s", out)
	}
	if code != 3 || !strings.Contains(out, "UNMEASURED: negative controls") {
		t.Fatalf("want exit 3 naming the skipped control bar, got %d:\n%s", code, out)
	}
}

// TestCLI_ControlViolationExitsOne pins the control bar's teeth end-to-end: when the arm IS supplied and the
// agent proposed on a benign incident, the gate must FAIL (exit 1, not 3 — this run measured the capability
// and it came back bad). It is also what makes a silent `ctrlRuns = nil` in main detectable: dropping the
// controls would turn this red into a green.
func TestCLI_ControlViolationExitsOne(t *testing.T) {
	dir := t.TempDir()
	base, cand := measurableArms()
	bad := writeJSON(t, dir, "controls.json", gate.ControlRun{N: 2, Results: []gate.ControlResult{
		{Ref: "ctl-1", Proposed: true, Band: "propose", Outcome: "PROPOSED on a benign incident", Conclusion: "violation"},
		{Ref: "ctl-2", Proposed: false, Band: "none", Outcome: "stood down", Conclusion: "benign"},
	}})
	code, out := runCLI(t, "--mode", "change", "--runs", "1",
		"--base", writeJSON(t, dir, "base.json", base),
		"--candidate", writeJSON(t, dir, "cand.json", cand), "--controls", bad)
	if code != 1 || !strings.Contains(out, "GATE: FAIL") {
		t.Fatalf("proposing on a negative control must FAIL with exit 1, got %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ctl-1") {
		t.Fatalf("the failing control must be named:\n%s", out)
	}
}

// TestCLI_DegradedArmIsAnIntegrityError pins the third computed-but-unwired check: a short candidate arm
// (a 429/contended run that judged 3 of 8 sessions) must abort as an INTEGRITY error — exit 2, never a
// quietly-compared verdict over half a corpus.
func TestCLI_DegradedArmIsAnIntegrityError(t *testing.T) {
	dir := t.TempDir()
	base, cand := measurableArms()
	cand.Judged = 3 // n=8, judged=3: the arm lost 5 sessions to contention
	code, out := runCLI(t, "--mode", "change", "--runs", "1",
		"--base", writeJSON(t, dir, "base.json", base),
		"--candidate", writeJSON(t, dir, "cand.json", cand), "--controls", cleanControls(t, dir))
	if code != 2 || !strings.Contains(out, "integrity") {
		t.Fatalf("a degraded arm must abort with the integrity error (exit 2), got %d:\n%s", code, out)
	}
}

// TestCLI_TrendUnmeasuredBlocksAndDoesNotRatchetTheBaseline covers the trend arm, which has its own exit
// site AND the one path where an uncertified run does LASTING damage: --refresh-baseline would write the
// meaningless measurement into eval/baseline-scorecard.json, the anchor every future run is compared
// against. The night must exit non-zero and the committed file must come out byte-identical.
func TestCLI_TrendUnmeasuredBlocksAndDoesNotRatchetTheBaseline(t *testing.T) {
	dir := t.TempDir()
	original, err := os.ReadFile(filepath.Join(archivedChangeRun, "comparator.json"))
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := filepath.Join(dir, "baseline.json")
	if err := os.WriteFile(baselinePath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runCLI(t, "--mode", "trend", "--runs", "1", "--baseline", baselinePath,
		"--candidate", filepath.Join(archivedChangeRun, "scorecard.json"),
		"--controls", cleanControls(t, dir), "--refresh-baseline", baselinePath)
	if code != 3 {
		t.Fatalf("an unmeasured trend night must exit 3, got %d:\n%s", code, out)
	}
	after, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("the trend baseline was RATCHETED from a run that measured nothing — every future gate would\nbe judged against it. Before:\n%s\nAfter:\n%s", original, after)
	}
	if !strings.Contains(out, "baseline NOT refreshed") {
		t.Fatalf("the log must say WHY the anchor did not move, or a reader assumes it did:\n%s", out)
	}
}

// measurableArms returns a base/candidate pair modelled on the archived record but with propose capability
// actually MEASURED (live expected-propose incidents, none stale) and no regression: the shape that is
// legitimately certifiable. Both arms carry the same n so VerifyComparable is satisfied.
func measurableArms() (base, cand gate.Scorecard) {
	dims := map[string]float64{
		"appropriate_band": 5, "correct_diagnosis": 5, "evidence_grounded": 5,
		"falsifiable_prediction": 1, "sensible_proposal": 5,
	}
	base = gate.Scorecard{N: 8, DimMeans: dims, Overall: 4.2, ProposalRate: 0.25, PredictionRate: 0.25,
		ExpectedProposeN: 4, ProposalRecall: 1.0, OverallFormula: gate.OverallFormulaV2}
	cand = base
	cand.MeanDecisionSteps = 3.6
	return base, cand
}

// TestCLI_DisarmingABarByFlagCannotCertify is the reachable-without-editing-code half of the same defect:
// no mutation is needed, just `--recall-floor 0`. Before TG-258 that invocation printed "GATE: PASS" and
// exited 0 on a candidate that stood down on all four live action-warranted incidents. The gate may still be
// disarmed — it may not be disarmed and then quoted as proof.
func TestCLI_DisarmingABarByFlagCannotCertify(t *testing.T) {
	dir := t.TempDir()
	base, cand := measurableArms()
	cand.ProposalRecall = 0.0 // total collapse of the capability the recall floor exists to catch
	args := []string{"--mode", "change", "--runs", "1",
		"--base", writeJSON(t, dir, "base.json", base),
		"--candidate", writeJSON(t, dir, "cand.json", cand),
		"--controls", cleanControls(t, dir)}

	if code, out := runCLI(t, args...); code != 1 || !strings.Contains(out, "proposal_recall 0.00") {
		t.Fatalf("with the bar ARMED this collapse must FAIL with exit 1, got %d:\n%s", code, out)
	}
	code, out := runCLI(t, append(args, "--recall-floor", "0")...)
	if code == 0 {
		t.Fatalf("disarming the only propose bar produced a CERTIFIED run (exit 0):\n%s", out)
	}
	if code != 3 || !strings.Contains(out, "DISABLED") {
		t.Fatalf("want exit 3 naming the disabled bar, got %d:\n%s", code, out)
	}
}
