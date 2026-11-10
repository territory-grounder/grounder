package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A WRITTEN ORACLE THAT NO PIPELINE EXECUTES IS AN UNDECLARED TEST GAP (INV-22), AND IT LOOKS EXACTLY LIKE
// COVERAGE.
//
// Until 2026-07-29 the harness job ran `go test ./core/db/ -run 'Axis|TransitionLog|Attribution'`. That
// matched 6 of 27 DSN-gated test functions in this package. The other 21 — including the alert-history and
// open-incident oracles behind the corroboration switch (REQ-1126), the actuation baseline gate that ended a
// 1h49m production halt (REQ-1228), and the SQL shadow-suppression check that a decision to DROP live alerts
// would rest on — executed in NO pipeline, while this job stood as the reason to believe the database layer
// was guarded. It also hid TestTraceSpineRoundTrip RED on main for EIGHT DAYS: the `screen` stage was added
// on 2026-07-21, its expected step count was never updated, and every pipeline reported green throughout.
//
// The filter is gone. This oracle exists so it cannot come back quietly — and so a NEW DSN-gated test cannot
// be added into an exclusion nobody notices. It reads the shipped CI file, so it is checking what actually
// runs rather than what someone believes runs.
//
// WHY IT ASSERTS ON THE PIPELINE FILE AND NOT ON BEHAVIOUR: no behavioural test can detect its own
// non-execution. That is the whole failure mode — the excluded tests were all perfectly correct and would
// have passed; they simply never ran. Only a check over the CI definition itself can see the gap, which is
// the same "assert over a closed enumeration" discipline the deep-link and fixture-hostname oracles use.
//
// Deliberately NOT DSN-gated: it must run in every ordinary `go test ./...` and in `make all`, precisely
// because the thing it guards is a job that might not run.

// dsnGatedHelpers are the helpers that make a test require a real Postgres. A test calling one of these
// cannot run without TG_TEST_DSN, so it must be inside the CI job that provisions one.
var dsnGatedHelpers = []string{"skipWithoutDB", "openFixture"}

func repoRootFrom(t *testing.T, start string) string {
	t.Helper()
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory — cannot locate the repo root")
		}
		dir = parent
	}
}

// TestEveryDSNGatedOracleRunsInCI reads .gitlab-ci.yml and fails if the core/db invocation carries a -run
// filter, naming the tests that filter would exclude.
func TestEveryDSNGatedOracleRunsInCI(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := repoRootFrom(t, wd)

	ci, err := os.ReadFile(filepath.Join(root, ".gitlab-ci.yml"))
	if err != nil {
		t.Fatalf("read .gitlab-ci.yml: %v", err)
	}

	// Every line that runs this package's tests, comments stripped so prose about the old filter is not read
	// as configuration.
	var invocations []string
	for _, line := range strings.Split(string(ci), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, "go test") && strings.Contains(line, "./core/db/") {
			invocations = append(invocations, strings.TrimSpace(line))
		}
	}
	if len(invocations) == 0 {
		t.Fatal("no CI invocation runs ./core/db/ at all — every oracle in this package is unexecuted, which " +
			"is the maximal form of the gap this test exists to prevent")
	}

	// Collect this package's DSN-gated test names, so a violation can be reported by NAME rather than count.
	gated := dsnGatedTestNames(t, filepath.Join(root, "core", "db"))
	if len(gated) < 10 {
		t.Fatalf("found only %d DSN-gated tests — the scanner is broken, and a green run here would prove "+
			"nothing about CI coverage", len(gated))
	}

	runFlag := regexp.MustCompile(`-run[= ]`)
	for _, inv := range invocations {
		if !runFlag.MatchString(inv) {
			continue // unfiltered — this invocation runs everything, which is the requirement
		}
		excluded := excludedBy(inv, gated)
		t.Errorf("the CI invocation %q carries a -run filter, so %d of %d DSN-gated oracle(s) in core/db never "+
			"execute in any pipeline: %v\n"+
			"A written oracle no pipeline runs is an undeclared test gap (INV-22) that reads as coverage — this "+
			"exact filter hid TestTraceSpineRoundTrip RED on main for eight days. If a subset genuinely must be "+
			"skipped, skip it in the TEST with a stated reason (t.Skip), where it is visible, not in a CI glob.",
			inv, len(excluded), len(gated), excluded)
	}
}

// dsnGatedTestNames returns the names of every test function in dir whose body reaches a DSN-gated helper.
func dsnGatedTestNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	funcRE := regexp.MustCompile(`(?s)func (Test\w+)\(t \*testing\.T\) \{(.*?)\n\}`)
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range funcRE.FindAllStringSubmatch(string(b), -1) {
			name, body := m[1], m[2]
			for _, h := range dsnGatedHelpers {
				if strings.Contains(body, h) {
					out = append(out, name)
					break
				}
			}
		}
	}
	return out
}

// excludedBy returns the gated test names a `-run '<pattern>'` invocation would NOT select.
func excludedBy(invocation string, gated []string) []string {
	pat := regexp.MustCompile(`-run[= ]'([^']*)'|-run[= ]"([^"]*)"|-run[= ](\S+)`).FindStringSubmatch(invocation)
	expr := ""
	for _, g := range pat[1:] {
		if g != "" {
			expr = g
			break
		}
	}
	if expr == "" {
		return gated // an unparseable filter is treated as excluding everything — fail loud, never silently pass
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return gated
	}
	var out []string
	for _, n := range gated {
		if !re.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}
