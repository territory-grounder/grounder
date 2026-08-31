package db

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A PACKAGE THAT PRINTS "ok" WHILE TWO THIRDS OF IT NEVER RAN IS NOT A PASSING PACKAGE — IT IS AN
// UNMEASURED ONE (TG-258).
//
// Measured on this repository on 2026-08-03, immediately before this file existed:
//
//	$ go test ./core/db/
//	ok  	github.com/territory-grounder/grounder/core/db	0.412s
//
//	$ go test ./core/db/ -v | grep -c '^--- SKIP'
//	83
//
// One hundred and twenty-two tests are declared in this package. Thirty-nine of them ran. The other
// eighty-three called t.Skip because no database DSN was in the environment, and t.Skip is INVISIBLE
// without -v: `go test` folds a skip into the same "ok" it prints for a pass, and `make all` runs
// `go test ./... -count=1` without -v. So the exact line a reader, a reviewer and CI all treat as
// "the database layer is guarded" was, in the default local environment, a statement about 32% of
// the package with nothing anywhere saying so.
//
// This is not hypothetical damage. Among those 83 silently-skipped tests:
//
//   - TestVerdictSingleWriterSkipsExecuted — the single-writer oracle that is the ONLY executed proof
//     of the [P1][integrity] fix TG-184. Every "done" on TG-184 rested on a run in which it did not run.
//   - TestTraceSpineRoundTrip — which stood RED on main for EIGHT DAYS behind a CI -run filter
//     (see ci_coverage_test.go), and which a DSN-less local run cannot distinguish from green.
//   - the alert-history / open-incident oracles behind the corroboration switch (REQ-1126) and the
//     actuation baseline gate (REQ-1228).
//
// ci_coverage_test.go already closed the neighbouring hole: it reads .gitlab-ci.yml and fails if the
// core/db invocation carries a -run filter, because a written oracle no pipeline executes is an
// undeclared test gap (INV-22). This file closes the other half of the same hole. A -run filter is not
// the only way to not run a test; an absent DSN is the other, and it is quieter, because it leaves the
// test *listed* and *green*.
//
// WHAT THIS FILE ADDS
//
//  1. TestMain FAILS the package, non-zero, whenever any test in it could not run, and prints a counted
//     banner naming the missing variable, the number of tests it silences, and the load-bearing oracles
//     among them. Failing is not a stylistic choice; see the next section, it is the only channel Go
//     leaves open.
//
//  2. Two environment variables, both of which only ever make the gate STRICTER or move the
//     acknowledgement somewhere a reader can see it — neither can make it quieter:
//     TG_DB_TESTS_MAY_SKIP=1 says "I know this run proves nothing about the database layer", and is set
//     in exactly two places, each of which prints the banner itself where `go test` cannot eat it;
//     TG_DB_TESTS_REQUIRE_DSN=1 overrides that acknowledgement and is set by the harness job, so the one
//     run whose green is cited as proof of this package cannot be talked out of proving it.
//
//  3. TestDSNGateRosterIsIntact is a SOURCE-property check, not DSN-gated, so it runs in `make all`
//     and in the DSN-less `build-test` CI job. It fails if a test stops being gated on a DSN variable
//     this repository actually supplies — which is what a typo'd or renamed env var looks like from the
//     outside. That failure mode is worse than the one above, because a test gated on a variable NO job
//     sets skips in EVERY environment including the harness job, and reports "ok" in all of them.
//
// WHY A DSN-LESS RUN FAILS INSTEAD OF WARNING — MEASURED, NOT ASSUMED
//
// The first version of this file printed the banner and exited 0, on the theory that a loud line is
// enough and that failing every developer box is how gates get deleted. That version was WRONG, and the
// probe that proved it is worth keeping written down, because the conclusion is not obvious:
//
//	go test discards a passing package's output entirely unless -v is given.
//
// Not the skip lines — ALL of it. stdout and stderr, from inside a test function and from TestMain
// before and after m.Run(). Measured on go1.25.12: a package whose TestMain writes a 25-line banner to
// os.Stdout and exits 0 produces exactly `ok <pkg> 0.070s` under `go test ./...`, byte for byte
// identical to a package that proved everything. Add one failing test to the same package and the whole
// banner appears. `make all` runs `go test ./... -count=1`, without -v.
//
// So for the default invocation there is no such thing as "report loudly but do not fail" — the loud
// report is thrown away, and what reaches the reader is a bare "ok". A warning that the tooling deletes
// before anyone sees it is not a louder hat on a silent skip; it IS the silent skip, with extra code to
// maintain and a comment claiming otherwise. Of the two channels ("visible in the exit status or in a
// line no one can miss"), Go leaves exactly one open here, so the gate uses it.
//
// The cost of that choice is real: `go test ./...` on a machine with no Postgres now reds. It is paid in
// the two places that legitimately cannot supply a database, and paid differently in each:
//
//   - `make test` sets TG_DB_TESTS_MAY_SKIP=1 and, immediately before the suite, prints this banner
//     itself out of `go test`'s reach — so `make all`, the line a developer or an agent actually cites
//     as "green", states the count in its own output on every run.
//   - CI's build-test job (`go test ./... -race`, no Postgres service) sets it WITHOUT printing the
//     banner, and that gap is deliberate rather than overlooked. Printing it would require a second
//     `go test ./core/db/ -run ...` step, and ci_coverage_test.go fails the build on any -run filter
//     against this package — correctly, because that filter is how 21 oracles once ran in no pipeline.
//     Buying a log line by loosening that gate is a bad trade, and build-test's green was never the
//     claim at issue: nothing in this repository cites it as proof of the database layer.
//
// That leaves the asymmetry that keeps this honest. The acknowledgement is not a mute button — it cannot
// remove the banner from a run that fails, it cannot be set by accident, it is greppable in the two
// files that set it, and it is overridden by TG_DB_TESTS_REQUIRE_DSN. The one job whose green IS cited
// as proof of core/db is therefore the one job that cannot acknowledge its way out of producing it.
//
// WHY THE COUNT IS DERIVED FROM THE SOURCE AND NOT FROM RUNTIME SKIPS
//
// Go's testing package gives TestMain no access to per-test results: m.Run() returns one int. A test
// also cannot report its own non-execution — that is the entire failure mode. So this file counts the
// same way ci_coverage_test.go does, by reading what is actually written: it parses every _test.go in
// this directory, builds the call graph among the functions declared there, and propagates "reads
// os.Getenv(<a DSN variable>)" transitively, so a test reaching a DSN only through skipWithoutDB ->
// testDSN is counted. That derivation is exact here rather than approximate, because every one of the
// 83 skips is literally `if dsn == "" { t.Skip(...) }` on one of these variables — verified by
// grouping the -v skip messages: 83 of 83 name a DSN. The scanner is itself guarded by floors below,
// so a scanner that silently stopped finding tests would fail rather than report a comfortable zero.
//
// WHY THERE ARE TWO VARIABLE NAMES, WHICH LOOKS LIKE A BUG AND IS A TRAP EITHER WAY
//
// TG_TEST_DSN and TG_TEST_POSTGRES_DSN are NOT two names for one thing. .gitlab-ci.yml points them at
// two DIFFERENT databases on the same server: TG_TEST_DSN at `goldtest`, which the harness job migrates
// with every file in core/db/migrations before the suite runs, and TG_TEST_POSTGRES_DSN at `translog`,
// which is created EMPTY because those tests call Migrate() themselves and would fail against an
// already-migrated database with "type band already exists". Both are load-bearing and neither is
// redundant.
//
// The trap is that nothing said so. Setting the one name you happen to know gets you a green "ok" over
// 39 or 77 of 122 tests, indistinguishable from the 122 case. That is why a partially-satisfied
// environment is a HARD FAILURE here and not a warning: the operator who set a DSN is precisely the
// operator who now believes the database layer was exercised.
//
//	silent skip:  green, no output, no exit signal, indistinguishable from proof
//	this gate:    non-zero exit on every run that could not prove the package, and the banner
//	              printed by whoever chose to accept that — never by nobody
//
// THREE WAYS THIS GATE WAS ITSELF BLIND, FOUND BY MUTATION AND CLOSED HERE
//
// The first version of this file recognised exactly ONE symptom: an os.Getenv whose argument looked like a
// DSN name and was not on the roster. An oracle that names a symptom passes every defect wearing a
// different one, so the gate was mutation-tested against the defect CLASS — "this test executes in no
// environment while the package prints ok" — rather than against that symptom. Three mutations were
// applied to TG-184's single-writer oracle, each run in the HARNESS configuration (both DSNs set,
// TG_DB_TESTS_REQUIRE_DSN=1 — the one run whose green is cited as proof of this package). All three
// produced `ok ... EXIT=0` with an empty banner:
//
//  1. `t.Skip("flaky in CI, re-enable after TG-999")` as the first statement of the test — the ordinary
//     quarantine that outlives the ticket. The DSN read below it was still lexically present, so the
//     scanner still counted the test as gated and the floors were still satisfied.
//  2. `//go:build integration` at the top of verdict_write_test.go. The compiler dropped the whole file
//     (125 tests in the binary, then 124), but the scanner parsed it off disk with go/parser, which
//     ignores build constraints — so the roster kept counting a test that no longer existed in the binary
//     and every floor stayed green.
//  3. `if dsn == "" || os.Getenv("TG_RUN_SLOW_ORACLES") == ""` — the same disconnection as the typo'd
//     variable, wearing a name with no "DSN", "POSTGRES" or "_PG" in it. dsnShaped could not see it, the
//     DSN read was still there so the per-variable floor never moved, and nothing sets that variable.
//
// What they have in common is that NONE of them is an unrecognised DSN name, and ALL of them leave the
// test listed, green, and unexecuted everywhere. The two properties added below are stated over the
// class instead of the symptom:
//
//   - the roster is a statement about the TEST BINARY, not about the directory listing — build
//     constraints are evaluated exactly as the compiler evaluates them, so a file the compiler drops
//     stops counting and the floors fall (closes 2), and the load-bearing oracles are additionally
//     asserted BY NAME, so losing one is reported as itself rather than as an arithmetic dip that a
//     newly-added test could mask;
//   - every t.Skip in this package must be reachable ONLY because a database fixture is absent
//     (TestEverySkipIsAnAbsentDatabaseFixture) — an unconditional skip has no guard (closes 1) and a
//     guard reading any environment variable that is not one of the two rostered DSNs is a gate nothing
//     satisfies (closes 3). The single legitimate exception is enumerated, with its reason, in
//     nonDSNSkips, and an entry that stops matching a real skip is itself an error, so the exemption
//     list cannot quietly grow into a permission slip.

// dsnVar is one database fixture this package's tests gate on.
//
// minGatedTests is a FLOOR, not an exact count: new DSN-gated tests are welcome and must not red the
// build, but a DROP means a test that used to require a real Postgres no longer does. There are only
// two ways that happens — the test was deleted, or its gate was disconnected (a typo'd env var name, a
// renamed helper, a getenv that lost its call site). The second is the dangerous one and is invisible
// by construction: a test gated on a variable that no pipeline sets skips in EVERY environment, the
// harness job included, and prints "ok" in all of them. Raise a floor whenever the real count rises;
// the failure message prints the number to raise it to.
type dsnVar struct {
	name          string
	fixture       string
	minGatedTests int
}

var knownDSNVars = []dsnVar{
	{
		name: "TG_TEST_DSN",
		fixture: "a Postgres with EVERY file in core/db/migrations ALREADY APPLIED — the harness job's " +
			"`goldtest` database, seeded by the psql loop before the suite runs",
		minGatedTests: 38,
	},
	{
		name: "TG_TEST_POSTGRES_DSN",
		fixture: "an EMPTY, UNMIGRATED Postgres — the harness job's `translog` database. These tests call " +
			"Migrate() themselves, so this CANNOT be the same database as TG_TEST_DSN (it would fail on " +
			"\"type band already exists\"). Two names, two fixtures, both required",
		minGatedTests: 45,
	},
}

// minTestsInPackage floors the total the banner divides by. Without it a scanner that found nothing
// would report a serene "0 of 0 blocked" — the precise shape of failure this whole file exists to
// prevent, reintroduced inside the thing preventing it. 122 tests existed before this file; it adds 3.
//
// This is a floor over the COMPILED roster, not over the files on disk (see scanDSNGates): a test removed
// from the build by a `//go:build` constraint is a test that runs nowhere, so it stops counting here and
// this floor is what notices.
const minTestsInPackage = 125

// The two controls over this gate. Both are CONTROLS, not fixture gates, so both are exempt from the
// dsnShaped roster check below — without the exemption the scanner reads TestMain's own os.Getenv of
// them and reports the gate itself as a test that runs nowhere.
//
// maySkipVar is an ACKNOWLEDGEMENT, not a mute button: it accepts that this run proves nothing about the
// database layer. It is honoured ONLY for a wholly unprovisioned run — never for a half-provisioned one,
// and never for a test gated on a variable nothing supplies, because in both of those the operator is
// making a claim the acknowledgement does not cover. requireDSNVar OVERRIDES it outright, so an
// acknowledgement cannot reach the harness job by being inherited from a wider `variables:` block or an
// exported shell variable.
const (
	maySkipVar    = "TG_DB_TESTS_MAY_SKIP"
	requireDSNVar = "TG_DB_TESTS_REQUIRE_DSN"
)

// loadBearingOracles names blocked tests in the banner with what their absence costs. A bare count is
// easy to read past; "the only executed proof of a [P1][integrity] fix did not run" is not.
var loadBearingOracles = map[string]string{
	"TestVerdictSingleWriterSkipsExecuted": "the single-writer oracle — the ONLY executed proof of the " +
		"[P1][integrity] fix TG-184",
	"TestTraceSpineRoundTrip": "the trace-spine round-trip — stood RED on main for EIGHT DAYS behind a " +
		"CI -run filter and nothing noticed",
	"TestAxisRead_MutationControl_HostPredicateIsLoadBearing": "the mutation control for the published " +
		"benchmark axes (spec/025 REQ-2501) — the test that proves the other axis tests can fail",
}

// ---------------------------------------------------------------------------------------------------
// The scanner.
// ---------------------------------------------------------------------------------------------------

// dsnScan is what this package's own source says about which tests need which database.
type dsnScan struct {
	tests   []string            // every Test* function COMPILED INTO THE TEST BINARY from a _test.go file here
	needs   map[string][]string // test name -> the DSN variables it transitively requires, sorted
	byVar   map[string][]string // DSN variable -> the tests requiring it, sorted
	unknown map[string][]string // unrecognised DSN-shaped variable -> the functions reading it

	// Retained for the skip audit, which has to reason about statements rather than counts.
	fset    *token.FileSet
	bodies  map[string]*ast.FuncDecl   // every function declared in this package's included test files
	env     map[string]map[string]bool // function -> environment variables it transitively reads
	callers map[string][]string        // function -> the Test* functions that transitively reach it
}

// dsnShaped reports whether an environment variable name read by a test looks like a database DSN gate.
//
// Deliberately broader than the two known names: the point is to catch a THIRD name appearing —
// TG_TEST_PG_DSN, TG_TEST_POSTGRES_DSN_2, a typo — because a new gate variable that no CI job sets is a
// set of tests that run nowhere while still being listed and still printing "ok". A rename that evades
// this pattern entirely is caught instead by the per-variable floors, which see the gated count drop.
func dsnShaped(name string) bool {
	if !strings.HasPrefix(name, "TG_") || name == requireDSNVar || name == maySkipVar {
		return false
	}
	return strings.Contains(name, "DSN") || strings.Contains(name, "POSTGRES") || strings.Contains(name, "_PG")
}

func isKnownDSNVar(name string) bool {
	for _, v := range knownDSNVars {
		if v.name == name {
			return true
		}
	}
	return false
}

// scanDSNGates parses every _test.go in dir and works out, for each test, which DSN variables it needs.
//
// It resolves through helpers: a test calling skipWithoutDB, which calls testDSN, which calls
// os.Getenv("TG_TEST_DSN"), is reported as needing TG_TEST_DSN. Without that transitive step the 38
// tests in the golden-fixture family would look ungated and the banner would under-report by a third —
// an under-reporting counter being the one bug here that reproduces the original defect exactly.
func scanDSNGates(dir string) (*dsnScan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	bodies := map[string]*ast.FuncDecl{}
	var order []string
	// THE ROSTER MUST DESCRIBE THE TEST BINARY, NOT THE DIRECTORY LISTING.
	//
	// go/parser reads a file regardless of its build constraints, so without this the scanner happily
	// counts tests the compiler dropped. Measured: adding `//go:build integration` to verdict_write_test.go
	// took the binary from 125 tests to 124 while every count here stayed at 125, so both floors and the
	// banner reported a fully-provisioned package and `go test` printed "ok" with TG-184's single-writer
	// oracle compiled out entirely. Build-tagging an integration test out of the default build is the
	// single most ordinary way an oracle stops running, and it leaves the file on disk looking untouched.
	// build.Default matches how the harness job invokes `go test` (no -tags, the runner's GOOS/GOARCH), so
	// a file it excludes is a file that job does not run — and excluding one now makes the count FALL,
	// which is what the floors below are watching for.
	bctx := build.Default
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		included, err := bctx.MatchFile(dir, e.Name())
		if err != nil {
			return nil, fmt.Errorf("evaluate build constraints on %s: %w", e.Name(), err)
		}
		if !included {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Body == nil {
				continue
			}
			if _, dup := bodies[fd.Name.Name]; !dup {
				order = append(order, fd.Name.Name)
			}
			bodies[fd.Name.Name] = fd
		}
	}
	if len(bodies) == 0 {
		return nil, fmt.Errorf("no test functions found in %s — the scanner cannot see this package's "+
			"source, so any count it produced would be a fiction", dir)
	}

	scan := &dsnScan{
		needs:   map[string][]string{},
		byVar:   map[string][]string{},
		unknown: map[string][]string{},
		fset:    fset,
		bodies:  bodies,
		callers: map[string][]string{},
	}

	// Direct reads and intra-package call edges, one walk per function body.
	direct := map[string]map[string]bool{}
	edges := map[string]map[string]bool{}
	for name, fd := range bodies {
		direct[name] = map[string]bool{}
		edges[name] = map[string]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if env, ok := envLookupArg(v); ok {
					if dsnShaped(env) {
						direct[name][env] = true
						if !isKnownDSNVar(env) {
							scan.unknown[env] = append(scan.unknown[env], name)
						}
					}
				}
			case *ast.Ident:
				// Any identifier naming a function declared in this package's tests is an edge. Using
				// identifiers rather than only call expressions also catches a helper passed as a value
				// (t.Cleanup(closeFixture), a table of funcs) — under-linking here would under-report.
				if v.Name != name && bodies[v.Name] != nil {
					edges[name][v.Name] = true
				}
			}
			return true
		})
	}

	// Transitive closure to a fixpoint. Cycles are fine: the set only grows and is bounded by the
	// number of DSN variables, so this terminates.
	for changed := true; changed; {
		changed = false
		for name := range bodies {
			for callee := range edges[name] {
				for env := range direct[callee] {
					if !direct[name][env] {
						direct[name][env] = true
						changed = true
					}
				}
			}
		}
	}

	scan.env = direct

	// Reverse reachability: which tests lose their oracle if a given function misbehaves. A finding inside
	// a shared helper is easy to read past — "skipWithoutDB skips unconditionally" sounds like one line of
	// test plumbing, while "these 38 tests then execute nowhere" is the actual cost, and one t.Skip in
	// skipWithoutDB is exactly how it would be paid.
	for name := range bodies {
		if !strings.HasPrefix(name, "Test") || name == "TestMain" {
			continue
		}
		seen := map[string]bool{name: true}
		var walk func(string)
		walk = func(f string) {
			for callee := range edges[f] {
				if seen[callee] {
					continue
				}
				seen[callee] = true
				walk(callee)
			}
		}
		walk(name)
		for f := range seen {
			scan.callers[f] = append(scan.callers[f], name)
		}
	}
	for f := range scan.callers {
		sort.Strings(scan.callers[f])
	}

	for _, name := range order {
		// TestMain is the harness, not a test: it is never reported by `go test` and can never be
		// "blocked". Counting it would inflate both sides of the banner's fraction and, worse, make the
		// gate report ITSELF as an unexecuted oracle.
		if !strings.HasPrefix(name, "Test") || name == "TestMain" {
			continue
		}
		scan.tests = append(scan.tests, name)
		var needs []string
		for env := range direct[name] {
			needs = append(needs, env)
		}
		sort.Strings(needs)
		if len(needs) > 0 {
			scan.needs[name] = needs
			for _, env := range needs {
				scan.byVar[env] = append(scan.byVar[env], name)
			}
		}
	}
	sort.Strings(scan.tests)
	for env := range scan.byVar {
		sort.Strings(scan.byVar[env])
	}
	return scan, nil
}

// envLookupArg returns the literal variable name of an os.Getenv / os.LookupEnv call.
func envLookupArg(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 1 {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" {
		return "", false
	}
	if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return name, true
}

// ---------------------------------------------------------------------------------------------------
// The skip audit.
//
// Counting which tests are GATED on a database says nothing about the other ways a test stops running.
// The only legitimate reason for a test in this package to skip is "the database fixture this test needs
// is not in this environment" — a condition that ENDS the moment someone supplies a DSN. Every other
// skip is permanent: it fires in the harness job too, so the test executes nowhere, forever, while
// `go test` prints "ok". The checks below are stated over that property rather than over any particular
// way of violating it.
// ---------------------------------------------------------------------------------------------------

// nonDSNSkips enumerates the skips in this package that fire for a reason OTHER than an absent database,
// each with the reason it is defensible. It is an exact roster, not a floor: an entry that no longer
// matches a real skip is an error too (see TestEverySkipIsAnAbsentDatabaseFixture), so it cannot rot into
// a blanket permission slip for whatever gets added next.
//
// Keep this list at zero if you can. Every entry is a test that can report "ok" in a fully provisioned
// harness run without having executed, which is precisely the condition TG-258 exists to end — the
// exemption makes it VISIBLE and reviewed, it does not make it harmless.
var nonDSNSkips = map[string]string{
	"TestASecondVerdictRowStillYieldsOneRibbon": "the skip fires only when the INSERT of a second " +
		"action_verdict row for one action_id is rejected by a unique constraint — i.e. when the database " +
		"makes the multiplication hazard this test guards structurally impossible, which is a strictly " +
		"stronger guarantee than the test itself. Reviewed and kept because the skip means the property " +
		"holds by construction, not because the test could not be run",
}

// skipFinding is one t.Skip that can silence a test for a reason an operator cannot fix by providing a
// database — which means it silences it in the harness job as well.
type skipFinding struct {
	fn     string   // the function containing the skip (a test, or a helper several tests reach)
	pos    string   // file:line, so the message points at the statement and not just the test
	reason string   // why this skip is not "the fixture is absent"
	tests  []string // the tests that stop executing because of it
}

// auditSkips finds every t.Skip / t.Skipf / t.SkipNow in this package's test files and classifies its
// guard. Three shapes are reported:
//
//   - UNGUARDED — the skip is not inside any conditional, so it fires unconditionally in every
//     environment. This is the ordinary "quarantine a flaky test and move on" edit, and it is invisible
//     to every count-based check because the DSN read below it is still there.
//   - a guard reading an environment variable that is not one of the rostered DSNs — the disconnected
//     gate, wearing a name dsnShaped cannot recognise. Nothing supplies it, so the test runs nowhere.
//   - a guard that has nothing to do with a database at all — a runtime condition that may be true in
//     the harness job as well, in which case the test is decorative there too.
func (s *dsnScan) auditSkips() []skipFinding {
	var out []skipFinding
	names := make([]string, 0, len(s.bodies))
	for n := range s.bodies {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, fn := range names {
		fd := s.bodies[fn]
		derived := s.dsnDerivedIdents(fd)
		s.walkGuarded(fd.Body, nil, func(call *ast.CallExpr, guards []ast.Expr) {
			f := skipFinding{fn: fn, pos: s.fset.Position(call.Pos()).String(), tests: s.callers[fn]}
			if len(guards) == 0 {
				f.reason = "the skip is UNCONDITIONAL. It fires in every environment — locally, in " +
					"build-test, and in the harness job whose whole purpose is to execute this — so the " +
					"test is listed, green, and executed NOWHERE. No DSN can undo it. This is what " +
					"quarantining a test looks like after the ticket that justified it is closed"
				out = append(out, f)
				return
			}
			foreign, isDSN := s.guardEnv(guards, derived)
			switch {
			case len(foreign) > 0:
				f.reason = "the skip is guarded on os.Getenv(" + strconv.Quote(foreign[0]) + ")" +
					plural(len(foreign)-1) + ", which this repository supplies NOWHERE (.gitlab-ci.yml " +
					"sets TG_TEST_DSN and TG_TEST_POSTGRES_DSN, and nothing else). A skip condition that " +
					"no job can falsify is a test that runs in no environment, and because the DSN read " +
					"is still present the gated-test floors never move"
			case !isDSN:
				f.reason = "the skip is guarded on a condition that does not test for an absent database " +
					"fixture, so it can fire in the harness job too — where a skip is invisible without " +
					"-v and reports as \"ok\". Either make the skip conditional on a missing DSN, turn the " +
					"condition into an assertion, or add this test to nonDSNSkips with the reason it is " +
					"defensible"
			default:
				return
			}
			out = append(out, f)
		})
	}
	return out
}

func plural(more int) string {
	if more <= 0 {
		return ""
	}
	return fmt.Sprintf(" (and %d more)", more)
}

// walkGuarded calls visit for every skip call in n, handing it the conditions the call is lexically
// nested inside. Else-branches deliberately do NOT inherit their if's condition, and switch cases carry
// both the tag and the case expressions, so an unguarded skip cannot hide inside one.
func (s *dsnScan) walkGuarded(n ast.Node, guards []ast.Expr, visit func(*ast.CallExpr, []ast.Expr)) {
	if n == nil {
		return
	}
	push := func(extra ...ast.Expr) []ast.Expr {
		next := append([]ast.Expr(nil), guards...)
		for _, e := range extra {
			if e != nil {
				next = append(next, e)
			}
		}
		return next
	}
	if ifs, ok := n.(*ast.IfStmt); ok {
		s.walkGuarded(ifs.Init, guards, visit)
		s.walkGuarded(ifs.Cond, guards, visit)
		s.walkGuarded(ifs.Body, push(ifs.Cond), visit)
		s.walkGuarded(ifs.Else, guards, visit)
		return
	}
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil {
			return false
		}
		if c == n {
			return true
		}
		switch v := c.(type) {
		case *ast.IfStmt:
			s.walkGuarded(v, guards, visit)
			return false
		case *ast.SwitchStmt:
			s.walkGuarded(v.Init, guards, visit)
			for _, stmt := range v.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				inner := guards
				if len(cc.List) > 0 { // `default:` guards nothing, so it keeps the outer guards only
					inner = push(append([]ast.Expr{v.Tag}, cc.List...)...)
				}
				for _, st := range cc.Body {
					s.walkGuarded(st, inner, visit)
				}
			}
			return false
		case *ast.CallExpr:
			if isSkipCall(v) {
				visit(v, guards)
			}
		}
		return true
	})
}

// isSkipCall is deliberately permissive about the receiver: t, tb, b, a captured sub-test variable — all
// of them stop the test. A false positive here is one loud message that a human resolves in a minute; a
// false negative is an oracle that runs nowhere and says "ok" about it for a year.
func isSkipCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Skip", "Skipf", "SkipNow":
	default:
		return false
	}
	_, ok = sel.X.(*ast.Ident)
	return ok
}

// guardEnv reports the environment variables a skip's guard consults that are NOT rostered DSNs, and
// whether the guard consults a rostered DSN at all (directly, through a helper such as testDSN, or
// through a local holding the result — `dsn := os.Getenv(...)` then `if dsn == ""` is the shape every
// legitimate skip in this package uses).
func (s *dsnScan) guardEnv(guards []ast.Expr, derived map[string]bool) (foreign []string, isDSN bool) {
	seen := map[string]bool{}
	for _, g := range guards {
		ast.Inspect(g, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if env, ok := envLookupArg(v); ok {
					seen[env] = true
				} else if id, ok := v.Fun.(*ast.Ident); ok {
					for env := range s.env[id.Name] {
						seen[env] = true
					}
				}
			case *ast.Ident:
				if derived[v.Name] {
					isDSN = true
				}
			}
			return true
		})
	}
	for env := range seen {
		if isKnownDSNVar(env) {
			isDSN = true
		} else {
			foreign = append(foreign, env)
		}
	}
	sort.Strings(foreign)
	return foreign, isDSN
}

// dsnDerivedIdents returns the locals in fd that hold a value obtained from a rostered DSN, so that the
// standard `dsn := testDSN(); if dsn == "" { t.Skip(...) }` reads as a database guard and not as an
// arbitrary condition.
func (s *dsnScan) dsnDerivedIdents(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	readsDSN := func(e ast.Expr) bool {
		found := false
		ast.Inspect(e, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if env, ok := envLookupArg(call); ok && isKnownDSNVar(env) {
				found = true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				for env := range s.env[id.Name] {
					if isKnownDSNVar(env) {
						found = true
					}
				}
			}
			return true
		})
		return found
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			var rhs ast.Expr
			switch {
			case len(as.Rhs) == len(as.Lhs):
				rhs = as.Rhs[i]
			case len(as.Rhs) == 1:
				rhs = as.Rhs[0]
			default:
				continue
			}
			if readsDSN(rhs) {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

// ---------------------------------------------------------------------------------------------------
// The verdict.
// ---------------------------------------------------------------------------------------------------

type dsnVerdict struct {
	scanErr      error
	total        int
	blocked      []string       // tests that cannot run in this environment, sorted
	blockedByVar map[string]int // absent DSN variable -> how many tests it silences
	absent       []string       // known DSN variables with no value, in roster order
	present      []string       // known DSN variables with a value, in roster order
	fatal        bool           // this run must exit non-zero
	why          []string       // why it is fatal, printed in the banner
}

func auditDSNGates(dir string) *dsnVerdict {
	v := &dsnVerdict{blockedByVar: map[string]int{}}

	scan, err := scanDSNGates(dir)
	if err != nil {
		// A scanner that cannot read the source must never fall through to "nothing is blocked". That
		// would be this package's signature defect — a control that no-ops without saying so — rebuilt
		// inside the control meant to end it.
		v.scanErr = err
		v.fatal = true
		v.why = append(v.why, "the DSN-gate scanner could not read this package's source: "+err.Error())
		return v
	}
	v.total = len(scan.tests)

	for _, kv := range knownDSNVars {
		if os.Getenv(kv.name) == "" {
			v.absent = append(v.absent, kv.name)
		} else {
			v.present = append(v.present, kv.name)
		}
	}
	absent := map[string]bool{}
	for _, n := range v.absent {
		absent[n] = true
	}

	for _, name := range scan.tests {
		missing := false
		for _, need := range scan.needs[name] {
			// An unknown DSN variable is treated as never satisfiable: nothing sets it, so a test behind
			// it is blocked everywhere. TestDSNGateRosterIsIntact reports that case by name.
			if absent[need] || !isKnownDSNVar(need) {
				missing = true
				v.blockedByVar[need]++
			}
		}
		if missing {
			v.blocked = append(v.blocked, name)
		}
	}

	if len(v.blocked) == 0 {
		return v // fully provisioned: stay completely silent, exactly as before this file existed
	}

	// THE DEFAULT IS FAILURE. `go test` throws away a passing package's output, so a blocked run that
	// exits 0 reaches the reader as a bare "ok" and nothing else — see the header. Non-zero is the only
	// channel left, so it is the one used unless a caller has taken explicit responsibility.
	v.fatal = true
	v.why = append(v.why, fmt.Sprintf("%d of this package's %d tests could not run, so this run is not "+
		"evidence about core/db. `go test` prints a passing package's output NOWHERE without -v, so a "+
		"zero exit here would be indistinguishable from a run that proved all %d", len(v.blocked),
		v.total, v.total))

	// Two conditions the acknowledgement must NOT be able to excuse, because in both the operator is
	// making a claim that "I know this run proves nothing about the database layer" does not cover.
	inexcusable := false

	// (a) A PARTIALLY provisioned environment — some fixtures held, others missing. Someone holding one
	// DSN is someone who believes they ran the database suite; a green "ok" over 80 of 125 tests is the
	// exact reading error this gate exists to make impossible. The fix is cheap and named in the banner:
	// provision the other database. Note both sides of the test — with EVERY DSN present this must not
	// fire, or the message blames the environment for a defect that is in the source.
	if len(v.present) > 0 && len(v.absent) > 0 {
		inexcusable = true
		v.why = append(v.why, "a DSN IS set ("+strings.Join(v.present, ", ")+") but this package also needs "+
			strings.Join(v.absent, ", ")+": a run holding some of the fixtures proves neither the tests it "+
			"ran nor the tests it silently did not, and "+maySkipVar+" does not excuse it")
	}

	// (b) A test gated on a variable NOTHING supplies. This is not an under-provisioned environment at
	// all — it is a broken test, and no environment can fix it. Excusing it would let a disconnected
	// oracle ride along in every acknowledged run forever, which is strictly worse than the skip this
	// file was written to end: at least a skip ends when someone supplies a DSN.
	for name := range v.blockedByVar {
		if !isKnownDSNVar(name) {
			inexcusable = true
			v.why = append(v.why, "os.Getenv("+strconv.Quote(name)+") gates a test but is supplied by "+
				"nothing, so that test runs in NO environment — see TestDSNGateRosterIsIntact. No DSN and "+
				"no "+maySkipVar+" can make this run prove it")
		}
	}

	// The acknowledgement. Only honoured for a wholly unprovisioned run, and only when the demand for
	// proof has not been made.
	if truthy(os.Getenv(requireDSNVar)) {
		v.why = append(v.why, requireDSNVar+" is set: this run was REQUIRED to prove the package and did "+
			"not. That demand overrides "+maySkipVar+", so no acknowledgement inherited from a wider "+
			"environment can make this run green")
		return v
	}
	if !inexcusable && truthy(os.Getenv(maySkipVar)) {
		v.fatal = false
		v.why = nil
	}
	return v
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// render builds the banner. Empty when every test can run — with a full set of DSNs this file adds
// nothing to the output, which is the requirement: the machinery is for the unproven case only.
//
// Every line is prefixed so the block survives being grepped out of a long `go test ./...` log, and so
// no one can mistake it for one package's ordinary chatter.
func (v *dsnVerdict) render() string {
	if v.scanErr == nil && len(v.blocked) == 0 {
		return ""
	}
	const rule = "!! ============================================================================="
	var b strings.Builder
	p := func(format string, args ...any) {
		fmt.Fprintf(&b, "!! "+format+"\n", args...)
	}
	b.WriteString(rule + "\n")

	if v.scanErr != nil {
		p("core/db DSN-GATE ACCOUNTING FAILED — this run's result means nothing.")
		p("%v", v.scanErr)
		p("Refusing to report \"0 tests blocked\" from a scanner that cannot see the source.")
		b.WriteString(rule + "\n")
		return b.String()
	}

	// "(0% of this package)" next to "DID NOT PROVE ITSELF" reads as a rounding artefact and invites the
	// reader to dismiss the block. A single disconnected oracle can be the whole proof of a [P1] fix.
	share := "<1%"
	if v.total > 0 && len(v.blocked)*100/v.total > 0 {
		share = fmt.Sprintf("%d%%", len(v.blocked)*100/v.total)
	}
	p("core/db DID NOT PROVE ITSELF: %d of %d TESTS COULD NOT RUN (%s of this package).",
		len(v.blocked), v.total, share)
	p("The pass/ok line below covers the %d tests that DID run. It is not a statement about",
		v.total-len(v.blocked))
	p("this package, and `go test` without -v shows a skip and a pass identically.")
	p("")
	for _, kv := range knownDSNVars {
		n := v.blockedByVar[kv.name]
		if n == 0 {
			continue
		}
		p("%-22s UNSET -> %3d tests could not run.", kv.name, n)
		p("%-22s needs %s.", "", kv.fixture)
	}
	for name, n := range v.blockedByVar {
		if isKnownDSNVar(name) {
			continue
		}
		p("%-22s UNKNOWN VARIABLE -> %d tests gated on it, and NO CI job sets it, so those", name, n)
		p("%-22s tests run in NO environment. See TestDSNGateRosterIsIntact.", "")
	}

	var named []string
	for _, name := range v.blocked {
		if why, ok := loadBearingOracles[name]; ok {
			named = append(named, name+" — "+why)
		}
	}
	if len(named) > 0 {
		p("")
		p("Among the tests that did not run:")
		for _, n := range named {
			p("  * %s", n)
		}
	}

	p("")
	if v.fatal {
		p("THIS RUN IS A FAILURE, because:")
		for _, w := range v.why {
			p("  * %s", w)
		}
		p("")
		p("If this caller genuinely cannot supply a database and is NOT claiming to test the database")
		p("layer, set %s=1 — and print this block yourself the way `make test` does,", maySkipVar)
		p("because `go test` will not print it for you once the package passes.")
	} else {
		p("%s is set, so this run is green. It is NOT proof of this package: %d of", maySkipVar, len(v.blocked))
		p("%d tests did not execute. Whoever set that variable is responsible for surfacing this block.", v.total)
	}
	p("")
	p("To actually run them (mirrors the .gitlab-ci.yml `harness` job — two DATABASES, not two names")
	p("for one): migrate one database and apply every core/db/migrations/*.up.sql to it, create a")
	p("second EMPTY one, then export TG_TEST_DSN at the migrated one and TG_TEST_POSTGRES_DSN at the")
	p("empty one. The image must be pgvector/pgvector (migration 0013 creates the `vector` extension).")
	b.WriteString(rule + "\n")
	return b.String()
}

// TestMain makes the accounting unavoidable. It is the only place in a Go test binary that can speak
// after every test has reported and still influence the exit status, which is exactly the two channels
// the banner needs: a line no reader can miss, and a signal CI reads.
func TestMain(m *testing.M) {
	v := auditDSNGates(".")
	code := m.Run()
	if out := v.render(); out != "" {
		fmt.Fprint(os.Stdout, out)
	}
	if code == 0 && v.fatal {
		code = 1
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------------------------------------
// Source-property oracles. Deliberately NOT DSN-gated: they must run in `make all` and in the DSN-less
// build-test job, because what they guard is the possibility that a test runs nowhere at all.
// ---------------------------------------------------------------------------------------------------

// TestDSNGateRosterIsIntact fails if a test stopped being gated on a DSN this repository supplies.
//
// The failure it prevents is the quietest one in this package. A test whose gate reads a variable that
// no job sets — a typo, a rename, a copy-paste of a variable that only ever existed in one developer's
// shell — skips in EVERY environment: locally, in build-test, and in the harness job that exists to run
// it. It stays listed in `go test -v`, it prints "ok", and it is indistinguishable from a test that
// passed. TG-184's single-writer oracle is one `os.Getenv` string literal away from that state right
// now, and nothing else in this repository would notice.
func TestDSNGateRosterIsIntact(t *testing.T) {
	scan, err := scanDSNGates(".")
	if err != nil {
		t.Fatalf("DSN-gate scanner failed: %v\nA scanner that cannot read the source cannot report a "+
			"trustworthy zero, so this is a failure and not a skip.", err)
	}

	if len(scan.tests) < minTestsInPackage {
		t.Errorf("the scanner found %d tests in core/db but at least %d are expected: either tests were "+
			"deleted or the scanner is broken. Both make the DSN-skip banner under-report, which is the "+
			"defect TG-258 exists to end. If tests were legitimately removed, lower minTestsInPackage to %d "+
			"in the same change that removes them.", len(scan.tests), minTestsInPackage, len(scan.tests))
	}

	for env, funcs := range scan.unknown {
		sort.Strings(funcs)
		t.Errorf("test code reads os.Getenv(%q), which is not a DSN variable this repository supplies "+
			"(.gitlab-ci.yml sets TG_TEST_DSN and TG_TEST_POSTGRES_DSN, and nothing else). Every test "+
			"behind it therefore skips in EVERY environment — locally, in build-test, and in the harness "+
			"job whose whole purpose is to run it — while still printing \"ok\". Read by: %v.\n"+
			"Fix the variable name, or add it to knownDSNVars AND to the harness job that provisions it.",
			env, funcs)
	}

	// The floors below are arithmetic, and arithmetic can be balanced. Adding one cheap DSN-gated test in
	// the same change that build-tags out, renames or disconnects an expensive one keeps every count at or
	// above its floor while the oracle that actually mattered stops existing. These three are named
	// individually because their absence is not interchangeable with anything: each is cited somewhere in
	// this repository as the executed proof of a specific fix, so losing one must be reported as ITSELF
	// and not as a number that happens to still be large enough.
	for _, name := range sortedGateKeys(loadBearingOracles) {
		if len(scan.needs[name]) > 0 {
			continue
		}
		if !containsString(scan.tests, name) {
			t.Errorf("%s is named in loadBearingOracles as %s, but it is not in this package's compiled "+
				"test roster at all. It was renamed, deleted, or removed from the build by a //go:build "+
				"constraint — in every case the proof it carried is gone, and the per-variable floors "+
				"cannot see it if another DSN-gated test was added alongside. Restore it, or delete the "+
				"loadBearingOracles entry in the same change that retires the claim it backs.",
				name, loadBearingOracles[name])
			continue
		}
		t.Errorf("%s (%s) is no longer gated on ANY database DSN. Either it stopped needing a real "+
			"Postgres — in which case say so and remove it from loadBearingOracles — or its gate was "+
			"DISCONNECTED and it now skips in every environment while reporting \"ok\".",
			name, loadBearingOracles[name])
	}

	for _, kv := range knownDSNVars {
		got := len(scan.byVar[kv.name])
		if got < kv.minGatedTests {
			t.Errorf("%s now gates only %d tests, down from a floor of %d. A test that used to require a "+
				"real Postgres no longer does. Either it was deleted, or its gate was DISCONNECTED — a "+
				"renamed helper or a typo'd env var, which makes the test skip everywhere and report \"ok\" "+
				"everywhere. Missing tests are the ones this package can no longer prove anything about. "+
				"If the removal is intended, lower the floor to %d in the same change.",
				kv.name, got, kv.minGatedTests, got)
		}
	}
}

// TestHarnessJobRequiresTheDSN reads the shipped pipeline and fails unless the job that runs this
// package declares TG_DB_TESTS_REQUIRE_DSN.
//
// Without that declaration the harness job still has one way to be green while proving nothing: if its
// Postgres service comes up but the DSN variables arrive empty, every DSN-gated test skips, the banner
// prints, and `go test` exits 0 — a green pipeline over 83 unexecuted tests, which is precisely
// today's behaviour. The declaration is what converts that run from "loud but green" to red.
//
// This asserts over the pipeline file for the same reason ci_coverage_test.go does: no behavioural
// test can detect its own non-execution, so only a check over the CI definition can see the gap.
func TestHarnessJobRequiresTheDSN(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := repoRootFrom(t, wd)
	raw, err := os.ReadFile(filepath.Join(root, ".gitlab-ci.yml"))
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}
	// The real number of oracles at stake, read from the source rather than hardcoded, so the message
	// cannot drift into understating the gap it is describing.
	atStake := 0
	if scan, err := scanDSNGates("."); err == nil {
		for _, kv := range knownDSNVars {
			atStake += len(scan.byVar[kv.name])
		}
	}
	var pipeline map[string]any
	if err := yaml.Unmarshal(raw, &pipeline); err != nil {
		t.Fatalf("parse .gitlab-ci.yml: %v", err)
	}

	found := 0
	for jobName, raw := range pipeline {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Only an UNFILTERED invocation counts as "this job proves the package". A `-run`-filtered line
		// is a report or a subset, not the proof run, and demanding a DSN of it would fire on the wrong
		// job — the same distinction ci_coverage_test.go draws when it refuses to accept a filter as
		// coverage.
		script, _ := job["script"].([]any)
		runsThisPackage := false
		for _, line := range script {
			s, _ := line.(string)
			if strings.Contains(s, "go test") && strings.Contains(s, "./core/db/") &&
				!strings.Contains(s, "-run") {
				runsThisPackage = true
			}
		}
		if !runsThisPackage {
			continue
		}
		found++
		vars, _ := job["variables"].(map[string]any)
		for _, need := range append([]string{requireDSNVar}, dsnVarNames()...) {
			if v, ok := vars[need]; !ok || fmt.Sprint(v) == "" {
				t.Errorf("CI job %q runs ./core/db/ but does not set %s. Without it this job can report a "+
					"green pipeline over a run in which %d of the tests it exists to execute did not "+
					"execute — the DSN variables can arrive empty even when the service comes up, and a "+
					"skip is invisible without -v. A job that is cited as proof of this package must be "+
					"unable to pass without producing it.", jobName, need, atStake)
			}
		}
	}
	if found == 0 {
		t.Fatal("no CI job runs `go test ./core/db/` at all, so nothing in any pipeline ever supplies a " +
			"DSN and every DSN-gated oracle in this package is decorative. This is the maximal form of " +
			"the gap TG-258 and ci_coverage_test.go both exist to prevent.")
	}
}

// TestEverySkipIsAnAbsentDatabaseFixture fails if any test in this package can be silenced by something
// other than a missing database.
//
// This is the half of TG-258 that counting cannot reach. Every count-based check here — the per-variable
// floors, the roster, the banner — answers "which tests NEED a database", and all of them stay perfectly
// green while a test is quarantined with an unconditional t.Skip or re-gated on an environment variable
// no job sets, because the os.Getenv that made it count is still sitting right there in the source.
// Measured: both edits, applied to TestVerdictSingleWriterSkipsExecuted with BOTH DSNs present and
// TG_DB_TESTS_REQUIRE_DSN=1, produced `ok` and exit 0 with an empty banner before this test existed.
//
// A missing DSN is the ONE skip reason that ends when someone provides a database. Any other skip fires
// in the harness job too, so it takes the test out of every environment there is — and a skip is
// invisible without -v, so the pipeline stays green and nothing anywhere says the oracle stopped running.
// Deliberately not DSN-gated: a check on how tests avoid running must not itself be avoidable that way.
func TestEverySkipIsAnAbsentDatabaseFixture(t *testing.T) {
	scan, err := scanDSNGates(".")
	if err != nil {
		t.Fatalf("DSN-gate scanner failed: %v\nA scanner that cannot read the source cannot report a "+
			"trustworthy \"no bad skips\", so this is a failure and not a skip.", err)
	}

	exempted := map[string]bool{}
	for _, f := range scan.auditSkips() {
		if reason, ok := nonDSNSkips[f.fn]; ok {
			exempted[f.fn] = true
			t.Logf("known non-DSN skip in %s (%s), exempted: %s", f.fn, f.pos, reason)
			continue
		}
		t.Errorf("%s skips for a reason that is not \"the database fixture is absent\": %s.\n"+
			"    Tests that stop executing because of it: %v\n"+
			"    A skip like this is permanent — supplying TG_TEST_DSN and TG_TEST_POSTGRES_DSN does not "+
			"lift it, so the harness job runs these tests in no environment and still prints \"ok\", which "+
			"is indistinguishable from proving them. Fix the condition, or add %q to nonDSNSkips with the "+
			"reason the skip is defensible.",
			f.pos, f.reason, f.tests, f.fn)
	}

	// An exemption for a skip that no longer exists is not harmless housekeeping: it is a standing licence
	// for the next skip that happens to land in a function with that name. The roster is exact in both
	// directions so that granting permission always costs a deliberate edit.
	for _, fn := range sortedGateKeys(nonDSNSkips) {
		if !exempted[fn] {
			t.Errorf("nonDSNSkips exempts %q, but the scanner found no non-DSN skip there. The skip was "+
				"fixed or the test was removed — either way the exemption is now a pre-approval for "+
				"whatever appears in that function next. Delete the entry.", fn)
		}
	}
}

func sortedGateKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func dsnVarNames() []string {
	out := make([]string, 0, len(knownDSNVars))
	for _, v := range knownDSNVars {
		out = append(out, v.name)
	}
	return out
}
