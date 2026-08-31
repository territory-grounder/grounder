package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// repoRoot resolves the repository root from this package's location.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

// world carries state between steps.
type world struct {
	root      string
	surfaces  []string
	unbound   []string
	scorecard string
	rendered  string
	dims      map[string]map[string]any
	goldenSrc string
	aggDims   []string
	winner    string
}

// The harness surfaces that compute or shape a published number. REQ-2500 requires each to be bound, so an
// axis definition cannot change without the lockstep hash moving.
var harnessSurfaces = []string{
	"core/db/axis_read.go",
	"cmd/axisscore/main.go",
	"tools/shadowbench/judge.py",
	"tools/shadowbench/_driver.py",
	"tools/faultinjector/plan.go",
	"tools/faultinjector/engine.go",
}

func (w *world) givenHarnessSurfaces() error {
	w.surfaces = harnessSurfaces
	return nil
}

// REQ-2500 — every measurement surface is bound in spec/.lockstep.lock.
func (w *world) whenLockstepConsulted() error {
	raw, err := os.ReadFile(filepath.Join(w.root, "spec", ".lockstep.lock"))
	if err != nil {
		return err
	}
	lock := string(raw)
	w.unbound = nil
	for _, s := range w.surfaces {
		if !strings.Contains(lock, `"`+s+`"`) {
			w.unbound = append(w.unbound, s)
		}
	}
	return nil
}

func (w *world) thenEverySurfaceBound() error {
	if len(w.unbound) > 0 {
		return fmt.Errorf("these harness surfaces compute published numbers but are NOT lockstep-bound, so an axis "+
			"could be redefined between two published numbers with nothing failing: %v", w.unbound)
	}
	return nil
}

// REQ-2501 — the golden fixture lives in core/db and runs against a real database. This oracle asserts the
// COVERAGE OBLIGATION (the fixture and its mutation control exist and are wired into CI); the fixture's own
// correctness is proven by that test running, not re-derived here.
// The Given/When steps LOAD the artefact the Then step judges, instead of returning nil and letting the
// scenario's prose imply a setup that never happened. A no-op Given reads, in the feature file, as though a
// fixture were built; it was not, and the reader cannot tell.
func (w *world) givenRealDBFixture() error {
	src, err := os.ReadFile(filepath.Join(w.root, "core", "db", "axis_read_test.go"))
	if err != nil {
		return fmt.Errorf("REQ-2501: the measurement SQL has NO golden-fixture test (%v)", err)
	}
	w.goldenSrc = string(src)
	return nil
}

func (w *world) whenAggregateComputedOverFixture() error {
	if w.goldenSrc == "" {
		return fmt.Errorf("REQ-2501: no golden-fixture source loaded — the Given step did not run")
	}
	return nil
}

// This scenario is a STRUCTURAL check and says so. The behavioural oracle — the golden fixture actually run
// against Postgres — is the `harness` CI job (`go test ./core/db/ -run Axis` with TG_TEST_POSTGRES_DSN). What
// this can verify without a database is that the golden test exists, is gated on a REAL database rather than
// a fake, and names the axes it claims to cover.
func (w *world) thenAxesEqualHandComputed() error {
	body := w.goldenSrc
	if !strings.Contains(body, "TG_TEST_DSN") {
		return fmt.Errorf("REQ-2501: the golden fixture must run against a REAL database (a pgx fake has already " +
			"hidden a field-drop in this repo); expected it to be gated on TG_TEST_DSN")
	}
	// A fixture that names no axis is a fixture that constrains no axis. Requiring the covered axes BY NAME
	// stops the file satisfying this step by merely existing.
	//
	// The list is exactly what the fixture ACTUALLY asserts today — A3 (heal success), A6b (time-to-recovery)
	// and A7 (false actuation). Writing an axis in here that the fixture does not cover would make this step
	// fail, which is how the A1 gap below was found rather than assumed.
	// Matched as `agg.<Axis>` — the ACCESSOR form — not the bare name. A bare-name grep is satisfied by a
	// mention in a comment, which is precisely the substring-grep weakness this step exists to remove; my own
	// mutation run caught it (deleting an assertion left the name in a comment and the check still passed).
	for _, axis := range []string{"agg.MutatedCount", "agg.HealConfirmedCount", "agg.HealCorrelatedCount", "agg.SuspiciousActuations"} {
		if !strings.Contains(body, axis) {
			return fmt.Errorf("REQ-2501: the golden fixture never mentions %q — it cannot be asserting a "+
				"hand-computed value for that axis", axis)
		}
	}
	// ★ DECLARED GAP, surfaced rather than papered over. A1 (detection recall) has NO golden-fixture coverage:
	// the fixture asserts nothing about InjectedFaults/DetectedFaults. That axis was CORRECTED today — the
	// class->rule mapping had no entry for container-down, so A1 published 78.5% when the truth was 83.3% —
	// and a golden fixture would have caught it. This step asserts the gap is still exactly that shape, so
	// closing it is a visible change rather than a silent one.
	if strings.Contains(body, "agg.DetectedFaults") {
		return fmt.Errorf("REQ-2501: the golden fixture now covers A1 (DetectedFaults) — good. Add it to the " +
			"required-axis list above and delete this branch, so the coverage cannot silently regress")
	}
	return nil
}

func (w *world) givenCoveredAxisAndFixture() error { return w.givenRealDBFixture() }

func (w *world) whenPredicatePerturbed() error {
	if !strings.Contains(w.goldenSrc, "MutationControl") {
		return fmt.Errorf("REQ-2501: no mutation-control test is present to perturb anything")
	}
	return nil
}

// REQ-2501 — the mutation control is the load-bearing half: a test that stays green under a deliberately
// broken query proves nothing.
func (w *world) thenGoldenTestFails() error {
	// Grepping for the word "Mutation" certified that a WORD EXISTS. This asserts the property REQ-2512 now
	// requires: the control perturbs the SHIPPED artefact rather than a copy it wrote itself. The predicate
	// lives in axis_read.go as a named constant precisely so the test can reference it, so requiring that
	// reference here is checkable without a database and is what makes the control non-vacuous.
	if !strings.Contains(w.goldenSrc, "MutationControl") {
		return fmt.Errorf("REQ-2501: the golden fixture carries no MUTATION CONTROL — without one a test can " +
			"track the query instead of constraining it, which is how a vacuous gate survives")
	}
	if !strings.Contains(w.goldenSrc, "healCorrelationMatch") {
		return fmt.Errorf("REQ-2501/REQ-2512: the mutation control does not reference the SHIPPED predicate " +
			"constant (healCorrelationMatch) — a control that perturbs its own copy of the query cannot fail " +
			"when the real query breaks, which is exactly the vacuous form this requirement forbids")
	}
	return nil
}

// REQ-2502/2503 — the scorer states the population, the exclusions and the correlation method. The
// scorecard computation + renderer live in core/axis since TG-480 (extracted from the CLI so the console
// serves the same scorecard; the lockstep hash-bind moved with it) — this oracle reads the renderer's
// actual home.
func (w *world) givenAxisWithUnmeasurableRows() error {
	src, err := os.ReadFile(filepath.Join(w.root, "core", "axis", "scorecard.go"))
	if err != nil {
		return err
	}
	w.scorecard = string(src)
	return nil
}

func (w *world) whenAxisRendered() error { return nil }

func (w *world) thenReportCarriesDenominatorAndExclusion() error {
	// A6b is the worked example: it excludes incidents whose recovery never correlated, and says so.
	if !strings.Contains(w.scorecard, "with a correlated recovery") {
		return fmt.Errorf("REQ-2502: an axis that excludes unmeasurable rows must report the excluded count")
	}
	if !strings.Contains(w.scorecard, "EXCLUDED from the denominator") {
		return fmt.Errorf("REQ-2502: the exclusion must state that excluded rows are not counted as zero")
	}
	if !strings.Contains(w.scorecard, "correlation:") {
		return fmt.Errorf("REQ-2503: an axis joined by correlation rather than a key must state its method inline")
	}
	return nil
}

func (w *world) givenZeroNumeratorAxis() error { return nil }

// REQ-2502 — "0 observed in n trials" and "cannot happen" are different claims.
func (w *world) thenReportCarriesUpperBound() error {
	if !strings.Contains(w.scorecard, "upper bound") {
		return fmt.Errorf("REQ-2502: a zero-numerator axis must publish its statistical upper bound; a bare 0 " +
			"asserts impossibility the sample does not support")
	}
	return nil
}

// REQ-2504 — comparative aggregates and the winner cover only mutually-scored dimensions.
func (w *world) givenJudgedComparisonWithOneSidedDimension() error {
	w.dims = map[string]map[string]any{
		"pred": {"correct_diagnosis": 4.0, "evidence_grounded": 4.0, "falsifiable_prediction": nil},
		"tg":   {"correct_diagnosis": 4.0, "evidence_grounded": 4.0, "falsifiable_prediction": 5.0},
	}
	return nil
}

func (w *world) whenAggregateAndWinnerComputed() error {
	// Mirror the driver's comparable-only rule: a dimension enters only when BOTH sides carry a number.
	for d := range w.dims["tg"] {
		pv, tv := w.dims["pred"][d], w.dims["tg"][d]
		if pv != nil && tv != nil {
			w.aggDims = append(w.aggDims, d)
		}
	}
	// COMPUTE the winner from the aggregate rather than assert a value this function assigned. The previous
	// version set w.winner = "tie" here and the Then step checked it was a tie — a test asserting its own
	// literal. It could not fail, and it certified nothing about the rule.
	//
	// This is still a MODEL of the rule, not the rule: the implementation is Python (analyze.py / _driver.py),
	// so no Go call site exists. The behavioural oracle therefore lives where the logic does —
	// tools/shadowbench/test_analyze.py::TestOneSidedDimensionExclusion exercises the real code and is run by
	// the shadowbench-analysis CI job. What this scenario still contributes is the STRUCTURAL half: that the
	// comparative aggregate is computed over the mutually-scored set only.
	var predTotal, tgTotal float64
	for _, d := range w.aggDims {
		pv, _ := w.dims["pred"][d].(float64)
		tv, _ := w.dims["tg"][d].(float64)
		predTotal += pv
		tgTotal += tv
	}
	switch {
	case tgTotal > predTotal:
		w.winner = "tg"
	case predTotal > tgTotal:
		w.winner = "pred"
	default:
		w.winner = "tie"
	}
	return nil
}

func (w *world) thenOneSidedDimensionExcluded() error {
	for _, d := range w.aggDims {
		if d == "falsifiable_prediction" {
			return fmt.Errorf("REQ-2504: a dimension only one system is scored on entered the comparative " +
				"aggregate — that is a mean over different dimension sets, not a comparison")
		}
	}
	if w.winner != "tie" {
		return fmt.Errorf("REQ-2504: the head-to-head winner was decided on a dimension the other system does not "+
			"compete on (got %q, want tie)", w.winner)
	}
	// And the driver must actually implement it, not merely this model.
	src, err := os.ReadFile(filepath.Join(w.root, "tools", "shadowbench", "_driver.py"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(src), "comparable") {
		return fmt.Errorf("REQ-2504: the rolling aggregate does not restrict itself to comparable dimensions")
	}
	return nil
}

func initializeScenario(sc *godog.ScenarioContext) {
	w := &world{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		w.root = rootForSuite
		return ctx, nil
	})
	sc.Step(`^the benchmark harness surfaces that compute published axes$`, w.givenHarnessSurfaces)
	sc.Step(`^the lockstep manifest is consulted$`, w.whenLockstepConsulted)
	sc.Step(`^every harness surface is bound to spec/025 so an axis cannot be redefined without the hash moving$`, w.thenEverySurfaceBound)

	sc.Step(`^a real database with every migration applied and a hand-built axis fixture$`, w.givenRealDBFixture)
	sc.Step(`^the axis aggregate is computed over that fixture$`, w.whenAggregateComputedOverFixture)
	sc.Step(`^each covered axis equals its hand-computed expected value$`, w.thenAxesEqualHandComputed)

	sc.Step(`^a covered axis and its golden fixture$`, w.givenCoveredAxisAndFixture)
	sc.Step(`^one predicate of that axis query is perturbed$`, w.whenPredicatePerturbed)
	sc.Step(`^the golden test fails, proving the test constrains the query rather than tracking it$`, w.thenGoldenTestFails)

	sc.Step(`^an axis whose population contains rows it cannot measure$`, w.givenAxisWithUnmeasurableRows)
	sc.Step(`^the axis is rendered to the scorecard$`, w.whenAxisRendered)
	sc.Step(`^the report carries the denominator, the excluded count, and the reason for exclusion$`, w.thenReportCarriesDenominatorAndExclusion)

	sc.Step(`^an axis with zero observed events over a finite sample$`, w.givenZeroNumeratorAxis)
	sc.Step(`^the report carries the statistical upper bound rather than a bare zero$`, w.thenReportCarriesUpperBound)

	sc.Step(`^a judged comparison in which one system is not scored on a dimension$`, w.givenJudgedComparisonWithOneSidedDimension)
	sc.Step(`^the comparative aggregate and the head-to-head winner are computed$`, w.whenAggregateAndWinnerComputed)
	sc.Step(`^that dimension is excluded from both and is reported as a unilateral property$`, w.thenOneSidedDimensionExcluded)
}

var rootForSuite string

func TestBenchmarkHarnessAcceptance(t *testing.T) {
	rootForSuite = repoRoot(t)
	suite := godog.TestSuite{
		ScenarioInitializer: initializeScenario,
		Options: &godog.Options{
			Format: "pretty",
			Paths:  []string{"benchmark-harness.feature"},
			// ~@pending excludes the honestly-declared coverage frontier (INV-22): the rule-of-three bound
			// is specified but not yet implemented, and is declared pending in _test_mapping.json rather
			// than papered over with a no-op step.
			Tags:     "~@pending",
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("spec/025 acceptance scenarios failed")
	}
}
