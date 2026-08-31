// Package acceptance is spec/023's acceptance runner.
//
// spec/023 shipped a 32-scenario feature file and NO runner. Nothing referenced actor-attribution.feature,
// so all 32 scenarios — for the spec that decides a SUSPICIOUS actor is never auto-healed — executed nowhere
// while reading, to anyone auditing, as coverage.
//
// WHAT THIS RUNNER ASSERTS, AND WHAT IT DELIBERATELY DOES NOT. The behaviour is already covered, thoroughly,
// by Go tests in core/attribution, modules/actorevidence/* and temporal/runner. Re-expressing those as 32
// godog steps would produce a second, shallower oracle for each property — and this session has spent its
// length removing exactly that: steps that grep for a word, controls that perturb a copy, assertions that
// restate their own setup. Writing 32 new shallow steps to close a coverage gap would BE the defect.
//
// So the runner verifies the EDGE that was missing, which is what _test_mapping.json exists to record:
//
//   - every scenario in the feature file has a mapping entry (and vice versa);
//   - every entry claiming `present` NAMES A TEST, and that test EXISTS in the tree;
//   - `retrospective_gap` is ZERO — INV-22 forbids an undeclared test gap for governed behaviour;
//   - `pending` entries name the requirement they are waiting on, so unbuilt work stays visible.
//
// That catches the two failures a missing runner allows: a scenario whose named oracle was renamed or deleted
// (imaginary coverage, and nothing would have noticed), and a scenario with no mapping at all.
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type mappingEntry struct {
	Name   string `json:"name"`
	Req    string `json:"req"`
	Status string `json:"status"`
	Test   string `json:"test"`
}

type mappingFile struct {
	Feature   string         `json:"feature"`
	Scenarios []mappingEntry `json:"scenarios"`
}

var scenarioRe = regexp.MustCompile(`(?m)^\s*Scenario(?: Outline)?:\s*(.+?)\s*$`)

// testNameRe pulls Go test identifiers out of a free-text mapping value such as
// "TestRunnerAttributionUnattributableWhenNoReader; TestAttribute (core/attribution)".
var testNameRe = regexp.MustCompile(`\bTest[A-Za-z0-9_]+`)

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func load(t *testing.T) (root string, m mappingFile, scenarios []string) {
	t.Helper()
	root = repoRoot(t)
	dir := filepath.Join(root, "spec", "023-actor-attribution", "acceptance")

	raw, err := os.ReadFile(filepath.Join(dir, "_test_mapping.json"))
	if err != nil {
		t.Fatalf("spec/023 has no _test_mapping.json — the coverage frontier is unrecorded: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("_test_mapping.json: %v", err)
	}
	feat, err := os.ReadFile(filepath.Join(dir, m.Feature))
	if err != nil {
		t.Fatalf("mapping names feature %q, which does not exist: %v", m.Feature, err)
	}
	for _, s := range scenarioRe.FindAllStringSubmatch(string(feat), -1) {
		scenarios = append(scenarios, strings.TrimSpace(s[1]))
	}
	return root, m, scenarios
}

// EVERY SCENARIO IS ACCOUNTED FOR, BOTH WAYS. A scenario with no mapping entry has an unrecorded coverage
// status; a mapping entry naming a scenario the feature file lost points at nothing.
func TestEveryScenarioIsMappedAndEveryMappingIsAScenario(t *testing.T) {
	_, m, scenarios := load(t)
	if len(scenarios) == 0 {
		t.Fatal("the feature file declares no scenarios")
	}
	mapped := map[string]bool{}
	for _, e := range m.Scenarios {
		mapped[e.Name] = true
	}
	for _, s := range scenarios {
		if !mapped[s] {
			t.Errorf("scenario %q has NO mapping entry — its coverage status is unrecorded, which is how a "+
				"feature file comes to read as coverage it does not have", s)
		}
	}
	inFeature := map[string]bool{}
	for _, s := range scenarios {
		inFeature[s] = true
	}
	for _, e := range m.Scenarios {
		if !inFeature[e.Name] {
			t.Errorf("mapping entry %q names a scenario the feature file does not contain — it was renamed or "+
				"deleted, and the mapping still claims it", e.Name)
		}
	}
}

// THE LOAD-BEARING CHECK. A `present` entry asserts a runnable test drives real code. If the test it names
// does not exist, the scenario reads as covered and is not — and with no runner, nothing could ever have said so.
func TestEveryPresentScenarioNamesATestThatExists(t *testing.T) {
	root, m, _ := load(t)
	all := goTestIndex(t, root)
	for _, e := range m.Scenarios {
		if e.Status != "present" {
			continue
		}
		names := testNameRe.FindAllString(e.Test, -1)
		if len(names) == 0 {
			t.Errorf("scenario %q is marked present but NAMES NO TEST — present means a runnable step drives "+
				"real code, so it must say which one", e.Name)
			continue
		}
		for _, n := range names {
			if !all[n] {
				t.Errorf("scenario %q (%s) names test %q, which does NOT EXIST in the tree — the scenario "+
					"reads as covered and is not", e.Name, e.Req, n)
			}
		}
	}
}

// INV-22: a governed behaviour shipped without a driving test is an UNDECLARED gap. The mapping's own note
// says this must be zero; nothing enforced it.
func TestNoRetrospectiveGapsRemain(t *testing.T) {
	_, m, _ := load(t)
	for _, e := range m.Scenarios {
		if e.Status == "retrospective_gap" {
			t.Errorf("scenario %q (%s) is a retrospective_gap — governed behaviour shipped with no driving "+
				"test (INV-22)", e.Name, e.Req)
		}
	}
}

// A pending entry is legitimate — unbuilt work — but must stay visible by naming its requirement, or it
// becomes a quiet parking space.
func TestEveryScenarioCarriesAKnownStatusAndItsRequirement(t *testing.T) {
	_, m, _ := load(t)
	known := map[string]bool{"present": true, "pending": true, "retrospective_gap": true}
	for _, e := range m.Scenarios {
		if !known[e.Status] {
			t.Errorf("scenario %q has status %q, outside the closed set [present pending retrospective_gap]",
				e.Name, e.Status)
		}
		if strings.TrimSpace(e.Req) == "" {
			t.Errorf("scenario %q names no requirement — a scenario that traces to no law cannot be audited", e.Name)
		}
	}
}

// goTestIndex collects every Go test function name in the repo, so a named oracle can be checked for existence
// rather than assumed.
func goTestIndex(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	funcRe := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			base := fi.Name()
			if base == ".git" || base == "node_modules" || base == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, m := range funcRe.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("index go tests: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("indexed zero Go tests — the walk is broken, and every existence check below would pass vacuously")
	}
	fmt.Printf("spec/023 acceptance: indexed %d Go test functions\n", len(out))
	return out
}
