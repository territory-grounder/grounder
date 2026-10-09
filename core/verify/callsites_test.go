package verify

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// O5 — THE SEAM GUARD: every production call of the BASELINE-LESS verdict authors is a declared, justified
// exception, enumerated here in the code that defines them.
//
// Behaviour tests structurally cannot catch a NEW no-baseline caller being added — each one is green in its
// own fixture, exactly as core/regime/asyncverify.go:510 was green for weeks while adjudicating deferred
// verdicts against an estate-wide observation with no baseline at all (the async twin of the 2026-07-28
// false deviation, governance ledger 5153-5155). So this oracle asserts over the CLOSED ENUMERATION of call
// sites in the shipped source tree: a new caller of ComputeVerdict / ComputeVerdictDetail fails this test by
// NAME until it is either given a baseline (ComputeVerdictDetailWithBaselines) or added below with a written
// reason why an unbaselined verdict cannot reach a destructive branch from there.
//
// This is the "IMPLEMENTED IS NOT REACHABLE" discipline inverted: not "prove the wired path is reachable"
// but "prove the dangerous path has no unreviewed callers".

// baselineless matches a CALL of the baseline-less authors — the optional `verify.` qualifier, then the bare
// name, then `(`. The `With...` variants do not match (longer identifiers fail the word boundary before `(`).
var baselineless = regexp.MustCompile(`(?:verify\.)?ComputeVerdict(?:Detail)?\(`)

// allowedBaselinelessCallers is the reviewed exception list, path → justification. Paths are repo-relative.
// (Phase C4 removed the falsify scorer from this list: it now adjudicates through
// ComputeVerdictDetailScoped with the commit-time baseline read back from the durable ingest ledger — the
// exception's own "baseline it when..." condition came due when prediction_verdict went 19/19 deviation.)
var allowedBaselinelessCallers = map[string]string{
	// The author file itself: definitions and the enum projection delegating to the full-context author.
	"core/verify/verdict.go": "the definitions",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test working directory — cannot locate the repo root")
		}
		dir = parent
	}
}

func TestEveryBaselinelessVerdictCallSiteIsADeclaredException(t *testing.T) {
	root := repoRoot(t)
	found := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Strip line comments so prose mentioning the name does not count as a call.
		var code []string
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			code = append(code, line)
		}
		if baselineless.MatchString(strings.Join(code, "\n")) {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			found[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var violations, stale []string
	for f := range found {
		if _, ok := allowedBaselinelessCallers[f]; !ok {
			violations = append(violations, f)
		}
	}
	for f := range allowedBaselinelessCallers {
		if !found[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)

	if len(violations) > 0 {
		t.Errorf("baseline-less verdict call site(s) outside the reviewed exception list: %v\n"+
			"A verdict computed without a pre-action baseline cannot separate this action's cascade from "+
			"pre-existing faults; on any path that feeds the breaker, graduation, or a mode transition it "+
			"manufactures deviations (the 2026-07-28 estate-wide halt). Use "+
			"ComputeVerdictDetailWithBaselines, or add the file here WITH its justification.", violations)
	}
	if len(stale) > 0 {
		t.Errorf("stale exception(s) — allowlisted files no longer call the baseline-less authors: %v\n"+
			"Remove them so the list stays a truthful enumeration.", stale)
	}
	// The oracle must be proven able to SEE: the author file always matches (the definitions call each other).
	if !found["core/verify/verdict.go"] {
		t.Fatal("scanner self-check failed: core/verify/verdict.go must always match — the walker or the regex is broken, and a green run proves nothing")
	}
}
