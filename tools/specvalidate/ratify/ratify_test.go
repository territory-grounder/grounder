package ratify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lattice is one synthetic spec tree, stated field by field so each test says exactly the condition it is
// about and nothing else. Zero values are the BENIGN case (index row Draft, every feature scenario declared
// `pending` debt, no oracles) so a test that sets one field is testing one thing.
type lattice struct {
	requirements string
	tasks        string
	feature      string
	mapping      string            // empty: auto-map every feature scenario as declared `pending` debt
	indexStatus  string            // empty: Draft
	oracles      map[string]string // repo-relative path -> content, for tests that need a real test to exist
}

// build writes the tree. It always writes spec/00-INDEX.md and an acceptance/_test_mapping.json, because a
// spec missing either is itself a reported condition — a fixture that omitted them by accident would make
// every other test in this file fail for the wrong reason.
func build(t *testing.T, l lattice) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "spec", "099-test")
	if err := os.MkdirAll(filepath.Join(dir, "acceptance"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, b string) {
		if b == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "requirements.md"), l.requirements)
	write(filepath.Join(dir, "tasks.json"), l.tasks)
	write(filepath.Join(dir, "acceptance", "x.feature"), l.feature)

	status := l.indexStatus
	if status == "" {
		status = "Draft"
	}
	write(filepath.Join(root, "spec", "00-INDEX.md"),
		"| Spec | Title | Status |\n|---|---|---|\n| [spec/099](099-test/) | test | **"+status+"** |\n")

	mapping := l.mapping
	if mapping == "" {
		var rows []string
		for _, m := range scenarioPattern.FindAllStringSubmatch(l.feature, -1) {
			rows = append(rows, `{"name":`+quote(strings.TrimSpace(m[1]))+`,"req":"REQ-001","status":"pending","test":""}`)
		}
		mapping = `{"feature":"x.feature","scenarios":[` + strings.Join(rows, ",") + `]}`
	}
	write(filepath.Join(dir, "acceptance", "_test_mapping.json"), mapping)

	for p, body := range l.oracles {
		write(filepath.Join(root, filepath.FromSlash(p)), body)
	}
	return root
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// scaffold is the original three-field fixture, kept so the traceability tests below read unchanged.
func scaffold(t *testing.T, requirements, tasksJSON, feature string) string {
	t.Helper()
	return build(t, lattice{requirements: requirements, tasks: tasksJSON, feature: feature})
}

// goTest is a minimal compilable-looking test file: the oracle index reads declarations, so this is enough
// for a named oracle to genuinely EXIST rather than be asserted to.
func goTest(name string) string {
	return "package p\n\nimport \"testing\"\n\nfunc " + name + "(t *testing.T) {}\n"
}

func check(t *testing.T, root string) Report {
	t.Helper()
	rep, err := Check(root)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return rep
}

// A requirement no task references has no route to an oracle. Nothing else in the lattice notices: lockstep
// binds CODE to specs and the prose validator checks wording, so law with no oracle sits there being
// true-by-assertion while a reader counts it as governance.
func TestARequirementNoTaskReferencesIsNotRatified(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n- **REQ-002** thing two.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`, "")
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("REQ-002 has no task and no declared gap — this must NOT ratify")
	}
	if rep.Requirements != 2 || rep.Referenced != 1 {
		t.Fatalf("want 2 requirements / 1 with a route, got %d/%d", rep.Requirements, rep.Referenced)
	}
	if rep.Findings[0].Kind != "unreferenced-requirement" {
		t.Fatalf("want an unreferenced-requirement finding, got %q", rep.Findings[0].Kind)
	}
}

// A DECLARED gap is a legitimate engineering position; silence is the failure. INV-22 forbids an UNDECLARED
// test gap, not an acknowledged one.
func TestADeclaredGapRatifies(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n- **REQ-002** thing two.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}],
		  "retrospective_gaps":[{"req":"REQ-002","why":"prose rule; no executable surface"}]}`, "")
	rep := check(t, root)
	if !rep.Clean() {
		t.Fatalf("a declared gap with a reason must ratify, got %v", rep.Findings)
	}
	if rep.DeclaredGaps != 1 {
		t.Fatalf("want 1 declared gap, got %d", rep.DeclaredGaps)
	}
}

// A gap with no reason is silence wearing a declaration's clothes — it must not buy ratification.
func TestAGapWithNoReasonDoesNotRatify(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[],"retrospective_gaps":[{"req":"REQ-001","why":"  "}]}`, "")
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("an empty reason must not ratify — a declaration without a WHY cannot be reviewed")
	}
	var kinds []string
	for _, f := range rep.Findings {
		kinds = append(kinds, f.Kind)
	}
	found := false
	for _, k := range kinds {
		if k == "empty-gap-reason" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want an empty-gap-reason finding, got %v", kinds)
	}
}

// THE SHARPEST CASE. A task claims an acceptance scenario; the feature file has no such scenario. The task
// reads as covered, the godog suite passes (it never knew the scenario was expected), and the coverage is
// imaginary. Measured on the real lattice when this shipped: 5 such tasks.
func TestATaskNamingAMissingScenarioIsNotRatified(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"],
		   "acceptance":{"feature":"x.feature","scenarios":["A scenario that was renamed"]}}]}`,
		"Feature: x\n\n  Scenario: A different scenario\n    Given a thing\n")
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a task claiming a scenario the feature file does not contain must NOT ratify")
	}
	if rep.Findings[0].Kind != "missing-scenario" {
		t.Fatalf("want a missing-scenario finding, got %q", rep.Findings[0].Kind)
	}
}

// The counterpart: a scenario that IS present must ratify, or the check would just fail everything.
func TestAPresentScenarioRatifies(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"],
		   "acceptance":{"feature":"x.feature","scenarios":["The real scenario"]}}]}`,
		"Feature: x\n\n  Scenario: The real scenario\n    Given a thing\n")
	if rep := check(t, root); !rep.Clean() {
		t.Fatalf("a present scenario must ratify, got %v", rep.Findings)
	}
}

// A named feature FILE that does not exist is the same defect one level up.
func TestAMissingFeatureFileIsNotRatified(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"],
		   "acceptance":{"feature":"gone.feature","scenarios":["anything"]}}]}`, "")
	if rep := check(t, root); rep.Clean() {
		t.Fatal("a task pointing at a feature file that does not exist must NOT ratify")
	}
}

// Requirement ids are read from their BOLD declaration, not from any mention. A cross-reference inside another
// requirement's rationale must not invent a requirement that does not exist.
func TestOnlyBoldDeclarationsCountAsRequirements(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one, which supersedes REQ-999 and relates to REQ-888.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`, "")
	rep := check(t, root)
	if rep.Requirements != 1 {
		t.Fatalf("only the bold declaration is a requirement; got %d (a mention was miscounted)", rep.Requirements)
	}
	if !rep.Clean() {
		t.Fatalf("want ratified, got %v", rep.Findings)
	}
}

// ONE CANONICAL FINISHED-STATE WORD.
//
// The lattice carried "completed" (120) and "done" (37) meaning the same thing. A split vocabulary is not
// cosmetic here: whether a task's acceptance scenario is OWED depends on whether the checker recognises its
// status as finished, so a second spelling silently exempts 37 tasks from the check.
func TestDoneIsNotAnAcceptedStatus(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"done","req_ids":["REQ-001"]}]}`, "")
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal(`"done" must be rejected — "completed" is the one canonical finished-state word, and a second ` +
			`spelling silently exempts those tasks from the acceptance-scenario check`)
	}
}

// An ABSENT status must be reported, not silently treated as one of the known states — it decides, invisibly,
// whether that task's acceptance is checked at all. Six tasks carried no status when this shipped.
func TestAnAbsentStatusIsReported(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","req_ids":["REQ-001"]}]}`, "")
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a task with no status must be reported")
	}
	if rep.Findings[0].Kind != "unknown-task-status" {
		t.Fatalf("want unknown-task-status, got %q", rep.Findings[0].Kind)
	}
}

// A PENDING task naming a scenario that does not yet exist is planned work, not imaginary coverage. Flagging
// it would bury the real defect — a FINISHED task pointing at a scenario nobody wrote — in forward-looking
// noise. Live, that distinction took the finding count from 5 to 2.
func TestAPendingTaskDoesNotOweItsScenarioYet(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"pending","req_ids":["REQ-001"],
		   "acceptance":{"feature":"x.feature","scenarios":["Not written yet"]}}]}`,
		"Feature: x\n\n  Scenario: Something else\n    Given a thing\n")
	if rep := check(t, root); !rep.Clean() {
		t.Fatalf("a pending task owes no scenario yet, got %v", rep.Findings)
	}
}

// ...but a COMPLETED one does. This is the pair that makes the distinction load-bearing rather than a loophole.
func TestACompletedTaskOwesItsScenario(t *testing.T) {
	root := scaffold(t,
		"- **REQ-001** thing one.\n",
		`{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"],
		   "acceptance":{"feature":"x.feature","scenarios":["Not written yet"]}}]}`,
		"Feature: x\n\n  Scenario: Something else\n    Given a thing\n")
	if rep := check(t, root); rep.Clean() {
		t.Fatal("a COMPLETED task claiming a scenario that does not exist is imaginary coverage and must fail")
	}
}

// ---- acceptance EXECUTION: does the named oracle exist, and does anything actually run? ----

// THE DEFECT THIS FILE'S SECOND HALF EXISTS FOR. `_test_mapping.json` marks a scenario `present` and names
// the test that drives it; nothing lattice-wide ever checked that the named test EXISTS. Measured when this
// shipped: 18 scenarios in spec/010 named vitest suites in the React frontend/ that ADR-0015 deleted on
// 2026-07-30 — they read as executing coverage, executed nothing, and `ratify --check` printed RATIFIED.
func TestAPresentScenarioNamingATestThatDoesNotExistIsAnImaginaryOracle(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: The real scenario\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"The real scenario","req":"REQ-001","status":"present","test":"TestThatWasDeleted"}]}`,
		// An unrelated real test file, so the oracle index is non-empty and the failure below is about the
		// NAMED oracle being absent rather than about the walk finding nothing at all.
		oracles: map[string]string{"core/x/x_test.go": goTest("TestSomethingElse")},
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a `present` row naming a test that does not exist is imaginary coverage and must fail")
	}
	if rep.Findings[0].Kind != "imaginary-oracle" {
		t.Fatalf("want imaginary-oracle, got %q", rep.Findings[0].Kind)
	}
	if rep.Scenarios.UnexecutedUndeclared != 1 || rep.Scenarios.Executing != 0 {
		t.Fatalf("want 1 undeclared-unexecuted / 0 executing, got %d/%d",
			rep.Scenarios.UnexecutedUndeclared, rep.Scenarios.Executing)
	}
}

// The counterpart that keeps the check from being "fail everything": a named oracle that really is in the
// tree counts as executing.
func TestAPresentScenarioNamingATestThatExistsExecutes(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: The real scenario\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"The real scenario","req":"REQ-001","status":"present","test":"TestTheRealOne (core/x)"}]}`,
		oracles: map[string]string{"core/x/x_test.go": goTest("TestTheRealOne")},
	})
	rep := check(t, root)
	if !rep.Clean() {
		t.Fatalf("a present row naming a test that exists must pass, got %v", rep.Findings)
	}
	if rep.Scenarios.Executing != 1 || rep.Scenarios.unexecuted() != 0 {
		t.Fatalf("want 1 executing / 0 unexecuted, got %d/%d", rep.Scenarios.Executing, rep.Scenarios.unexecuted())
	}
}

// A SCENARIO ITS OWN RUNNER SKIPS, MARKED `present`. Every godog runner in this lattice is constructed with
// Tags: "~@pending", so a @pending-tagged scenario is walked past and the suite stays green without ever
// attempting it. A row that marks such a scenario `present` while naming only that same runner claims
// execution from a test configured never to reach it — the signature defect of this repository, a control
// that no-ops without saying so.
func TestAPendingTaggedScenarioClaimingOnlyItsOwnSkippingRunnerFails(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  @REQ-001 @pending\n  Scenario: Skipped by its own runner\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Skipped by its own runner","req":"REQ-001","status":"present","test":"TestSpec099Acceptance"}]}`,
		oracles: map[string]string{"spec/099-test/acceptance/acceptance_test.go": goTest("TestSpec099Acceptance")},
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a @pending scenario whose only named oracle is its own ~@pending-filtered runner executes " +
			"nothing and must fail")
	}
	if rep.Findings[0].Kind != "skipped-scenario-claimed-present" {
		t.Fatalf("want skipped-scenario-claimed-present, got %q: %s", rep.Findings[0].Kind, rep.Findings[0].Detail)
	}
}

// ...and its legitimate twin, which is common and must NOT be flagged: @pending on the Gherkin because no
// step definitions were written, with `present` naming a REAL oracle OUTSIDE the acceptance package. 40 rows
// across specs 023/026/028 are exactly this. They execute — but they are counted separately, so "executing"
// is never silently read as "the scenario ran".
func TestAPendingTaggedScenarioCarriedByAnOracleElsewhereExecutes(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  @REQ-001 @pending\n  Scenario: Carried elsewhere\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Carried elsewhere","req":"REQ-001","status":"present","test":"TestCarriesTheProperty (core/x)"}]}`,
		oracles: map[string]string{"core/x/x_test.go": goTest("TestCarriesTheProperty")},
	})
	rep := check(t, root)
	if !rep.Clean() {
		t.Fatalf("a @pending scenario carried by a real oracle elsewhere must pass, got %v", rep.Findings)
	}
	if rep.Scenarios.SkippedButCarried != 1 || rep.Scenarios.Executing != 1 {
		t.Fatalf("want it counted as executing-but-gherkin-skipped, got carried=%d executing=%d",
			rep.Scenarios.SkippedButCarried, rep.Scenarios.Executing)
	}
}

// DECLARED debt is not a failure — it is a reviewable position (INV-22 forbids an UNDECLARED gap). It must
// still be COUNTED, on every run, or the declaration quietly becomes indistinguishable from delivery.
func TestADeclaredPendingScenarioIsCountedNotFailed(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Not written yet\n    Given a thing\n",
	})
	rep := check(t, root)
	if !rep.Clean() {
		t.Fatalf("declared debt must not fail the gate, got %v", rep.Findings)
	}
	if rep.Scenarios.UnexecutedDeclared != 1 {
		t.Fatalf("want 1 declared-unexecuted scenario, got %d", rep.Scenarios.UnexecutedDeclared)
	}
	if !strings.Contains(rep.Render(), "scenarios that EXECUTE NOTHING:              1/1") {
		t.Fatalf("a PASSING run must still state the unexecuted count with its denominator:\n%s", rep.Render())
	}
}

// A scenario with no mapping row at all has an UNRECORDED execution status — the silence this whole check
// exists to end. It is counted as unexecuted, not assumed covered.
func TestAScenarioWithNoMappingRowIsAFinding(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Nobody recorded me\n    Given a thing\n",
		mapping:      `{"feature":"x.feature","scenarios":[]}`,
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a scenario with no mapping row must be reported, not silently assumed covered")
	}
	if rep.Findings[0].Kind != "unmapped-scenario" {
		t.Fatalf("want unmapped-scenario, got %q", rep.Findings[0].Kind)
	}
}

// THE STATUS CLAIM. spec/00-INDEX.md's Status column was prose no gate read, while the validator printed
// "specs ratified: 28/28" beside it. `Ratified` is the index's own terminal state, defined there as every
// non-@pending requirement having a present acceptance oracle; a spec claiming it while its scenarios execute
// nothing is asserting delivery the tree cannot show.
func TestARatifiedIndexRowWithUnexecutedScenariosFails(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Not written yet\n    Given a thing\n",
		indexStatus:  "Ratified",
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a spec marked **Ratified** while a scenario executes nothing must fail")
	}
	if rep.Findings[0].Kind != "unearned-ratified-status" {
		t.Fatalf("want unearned-ratified-status, got %q", rep.Findings[0].Kind)
	}
}

// ...and the claim is EARNED when the evidence is there, or the check would only ever punish honesty.
func TestARatifiedIndexRowWithEverythingExecutingPasses(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Fully driven\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Fully driven","req":"REQ-001","status":"present","test":"TestDrivesIt"}]}`,
		indexStatus: "Ratified",
		oracles:     map[string]string{"core/x/x_test.go": goTest("TestDrivesIt")},
	})
	if rep := check(t, root); !rep.Clean() {
		t.Fatalf("an EARNED Ratified row must pass, got %v", rep.Findings)
	}
}

// An unparseable or absent Status is a delivery claim no gate can contradict — the same closed-enumeration
// discipline the task-status check already applies, applied to the column a reader trusts most.
func TestAnIndexStatusOutsideTheClosedVocabularyIsReported(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		indexStatus:  "Shipped",
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal(`"Shipped" is outside [Draft Approved Ratified] and must be reported`)
	}
	if rep.Findings[0].Kind != "unknown-spec-status" {
		t.Fatalf("want unknown-spec-status, got %q", rep.Findings[0].Kind)
	}
}

// THE EMPTINESS GUARD. Every `present` claim is adjudicated against the oracle index. If the walk comes back
// with nothing, the measurement failed — and a broken walk does not announce itself, it converts every claim
// into a confident "imaginary oracle" finding. It must say the measurement did not run, loudly, in the exit
// status. Absent is visible; skipped is not.
func TestAnEmptyOracleIndexIsAFailedMeasurementNotAVerdict(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Claims an oracle\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Claims an oracle","req":"REQ-001","status":"present","test":"TestAnything"}]}`,
		// no oracles: the tree holds not one .go/.ts/.mjs file for the index to find
	})
	_, err := Check(root)
	if err == nil {
		t.Fatal("an oracle index that walked 0 files while the lattice claims present oracles must ERROR, not " +
			"return a verdict computed from nothing")
	}
	if !strings.Contains(err.Error(), "walk is broken") {
		t.Fatalf("the error must say the measurement failed, got %q", err)
	}
}

// The whole point of the change: a reader must never again see a confident word without the denominator
// beside it. Both the passing and the failing render state the unexecuted count.
func TestEveryRenderStatesTheUnexecutedCount(t *testing.T) {
	for _, tc := range []struct{ name, status string }{{"passing", "Draft"}, {"failing", "Ratified"}} {
		t.Run(tc.name, func(t *testing.T) {
			root := build(t, lattice{
				requirements: "- **REQ-001** thing one.\n",
				tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
				feature:      "Feature: x\n\n  Scenario: Not written yet\n    Given a thing\n",
				indexStatus:  tc.status,
			})
			out := check(t, root).Render()
			if !strings.Contains(out, "EXECUTE NOTHING") {
				t.Fatalf("%s render omits the unexecuted count:\n%s", tc.name, out)
			}
			if strings.Contains(out, "RATIFIED") {
				t.Fatalf("%s render still prints the bare word RATIFIED, which is spec/00-INDEX.md's terminal "+
					"delivery status and not what this tool measures:\n%s", tc.name, out)
			}
		})
	}
}

// A NAME THAT RESOLVES TO PRODUCTION CODE. The existence check alone certifies nothing: `main.go` is in the
// tree, so a row naming it satisfied "the oracle exists" and the whole lattice reported 0 findings, exit 0.
// It asserts nothing and adjudicates nothing. This is the identical imaginary-coverage lie the spec/010 rows
// told — those were caught only because the deleted vitest files happened not to exist, not because the check
// understood what an oracle IS.
func TestAPresentScenarioNamingProductionCodeIsNotAnOracle(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Driven by nothing\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Driven by nothing","req":"REQ-001","status":"present","test":"cmd/thing/main.go"}]}`,
		oracles: map[string]string{
			"cmd/thing/main.go": "package main\n\nfunc main() {}\n",
			// a real test exists elsewhere, so the index is populated and the emptiness guard stays quiet:
			// this test must fail on WHAT was named, not on an empty walk.
			"core/x/x_test.go": goTest("TestSomethingElse"),
		},
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("a `present` row naming production code claims execution from a file that asserts nothing " +
			"and must fail")
	}
	if rep.Findings[0].Kind != "non-test-oracle" {
		t.Fatalf("want non-test-oracle, got %q: %s", rep.Findings[0].Kind, rep.Findings[0].Detail)
	}
	if rep.Scenarios.Executing != 0 {
		t.Fatalf("production code must not count as executing, got %d", rep.Scenarios.Executing)
	}
}

// A NAME THAT CANNOT FAIL TO MATCH. `Test*` is the bare prefix every Go test in the repository shares, so the
// family wildcard matched the entire suite and satisfied every claim in the lattice — 0 findings, exit 0. A
// coverage claim has to name the one oracle that adjudicates the scenario; naming everything is naming
// nothing, and an unfalsifiable claim is exactly the defect this tool exists to catch.
func TestAWildcardMatchingTheWholeSuiteIsNotAnOracle(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Claims the whole suite\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Claims the whole suite","req":"REQ-001","status":"present","test":"Test*"}]}`,
		oracles: map[string]string{"core/x/x_test.go": goTest("TestSomethingUnrelated")},
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("`Test*` singles out no oracle — it matches every Go test in the tree — and must fail")
	}
	if rep.Findings[0].Kind != "non-test-oracle" {
		t.Fatalf("want non-test-oracle, got %q: %s", rep.Findings[0].Kind, rep.Findings[0].Detail)
	}
}

// ...while a family reference that genuinely DISCRIMINATES still resolves. `TestSessionStream*` is real usage
// in this lattice; if the rule above also rejected it the check would be punishing a legitimate convention.
func TestADiscriminatingWildcardFamilyStillResolves(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		feature:      "Feature: x\n\n  Scenario: Carried by a family\n    Given a thing\n",
		mapping: `{"feature":"x.feature","scenarios":[
		  {"name":"Carried by a family","req":"REQ-001","status":"present","test":"TestSessionStream*"}]}`,
		oracles: map[string]string{"core/httpapi/stream_test.go": goTest("TestSessionStreamCloses")},
	})
	if rep := check(t, root); !rep.Clean() {
		t.Fatalf("a wildcard with a real discriminator must still resolve, got %v", rep.Findings)
	}
}

// THE VACUOUS RATIFIED CLAIM. A spec with no acceptance scenarios has unexecuted() == 0 — zero of zero execute
// nothing — so the arithmetic waved it through AND `FullyExecuted` counted it among the specs whose evidence
// would support Ratified. Demonstrated end to end: a spec with requirements, no acceptance/ at all, and a
// **Ratified** index row passed with 0 findings and exit 0. That inverts the gate: total absence of evidence
// scored better than partial evidence, which is this repository's signature defect reproduced inside the
// control built to catch it.
func TestARatifiedIndexRowWithNoScenariosAtAllFails(t *testing.T) {
	root := build(t, lattice{
		requirements: "- **REQ-001** thing one.\n",
		tasks:        `{"spec":"099-test","tasks":[{"id":"T-1","status":"completed","req_ids":["REQ-001"]}]}`,
		// no feature: nothing here could ever execute
		indexStatus: "Ratified",
	})
	rep := check(t, root)
	if rep.Clean() {
		t.Fatal("Ratified over ZERO acceptance scenarios rests on an absence of evidence and must fail")
	}
	if rep.Findings[0].Kind != "unearned-ratified-status" {
		t.Fatalf("want unearned-ratified-status, got %q: %s", rep.Findings[0].Kind, rep.Findings[0].Detail)
	}
	for _, s := range rep.FullyExecuted {
		if s == "099-test" {
			t.Fatal("a spec with no scenarios must never be counted as evidence that would support Ratified")
		}
	}
}

// THE READY SET MUST NAME ITSELF.
//
// The report printed "specs whose evidence would support `Ratified`: 14/28" and nothing else. A count is
// not actionable: an operator cannot promote a single spec without re-deriving the set by hand, which is
// the same defect this tool exists to catch, one level up — a measurement you cannot act on.
//
// The names were already in hand (Report.FullyExecuted); only the printing was missing.
func TestTheReadySetIsNamedNotJustCounted(t *testing.T) {
	r := Report{
		SpecsChecked:  3,
		FullyExecuted: []string{"019-maintenance-window", "001-risk-classification"},
		SpecStatus: map[string]string{
			"001-risk-classification": "Approved",
			"019-maintenance-window":  "Ratified",
		},
		StatusCounts: map[string]int{"Approved": 1, "Ratified": 1, "Draft": 1},
	}
	out := r.Render()

	for _, name := range r.FullyExecuted {
		if !strings.Contains(out, name) {
			t.Errorf("the ready set does not name %s. \"14/28 would support Ratified\" cannot be acted on "+
				"without re-deriving the set by hand.\n%s", name, out)
		}
	}
	// Sorted, so the report is byte-stable across runs and a diff shows a real change.
	i := strings.Index(out, "001-risk-classification")
	j := strings.Index(out, "019-maintenance-window")
	if i < 0 || j < 0 || i > j {
		t.Errorf("the ready set is not sorted (001 at %d, 019 at %d) — an unstable listing makes every "+
			"report diff noise", i, j)
	}
}

// TestTheReadySetShowsWhichAreActuallyPromotable. Naming them is only half of it: a spec already marked
// Ratified needs nothing, and one still at Approved is a promotion available TODAY on evidence that
// already exists. Printing both identically hides the only actionable distinction.
func TestTheReadySetShowsWhichAreActuallyPromotable(t *testing.T) {
	r := Report{
		SpecsChecked:  2,
		FullyExecuted: []string{"001-risk-classification", "019-maintenance-window"},
		SpecStatus: map[string]string{
			"001-risk-classification": "Approved", // promotable
			"019-maintenance-window":  "Ratified", // already there
		},
		StatusCounts: map[string]int{"Approved": 1, "Ratified": 1},
	}
	out := r.Render()

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "001-risk-classification") {
			if !strings.Contains(line, "Approved") {
				t.Errorf("the ready line does not show the CURRENT index status: %q", line)
			}
			if !strings.Contains(line, "promotable") {
				t.Errorf("an Approved spec with complete evidence is not flagged as promotable: %q", line)
			}
		}
		if strings.Contains(line, "019-maintenance-window") && strings.Contains(line, "promotable") {
			t.Errorf("a spec ALREADY Ratified is flagged as promotable: %q", line)
		}
	}
}
