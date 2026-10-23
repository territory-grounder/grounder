package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TG-469 — G6's guard on its own blind spot: RollbackWorkflow must remain the SOLE producer of
// InvertsActionID.
//
// The G6 loop-bypass axis (core/db/axis_read.go, REQ-2530) excludes rows whose request carries a
// sealed inverse — `inverts_action_id IS NOT NULL AS sealed_inverse` — because a manual rollback
// (TG-462) is an operator-ordered inversion, not an un-predicted mutation (TG-448). That exclusion
// is exactly as trustworthy as the CLAIM that only the sealed rollback lane ever sets the field: a
// second producer — some future convenience path stamping InvertsActionID onto an ordinary
// mutation — would ride the same exclusion and vanish from the loop-bypass axis without tripping
// any behaviour test (each caller is green in its own fixture; the axis query cannot know which
// writer was legitimate).
//
// So, the callsites_test.go discipline again: assert over the CLOSED ENUMERATION of setter sites
// in the shipped source tree. A new file that sets the field fails this test BY NAME until it is
// either routed through RollbackWorkflow's sealed lane or added below with a written reason why
// its inverses deserve the G6 exclusion.
//
// The pattern matches the composite-literal/keyed setter `InvertsActionID: <value>` only:
//   - the struct field DECLARATION (`InvertsActionID string`, core/actuate) has no colon;
//   - reads (`r.InvertsActionID`) and positional pass-throughs (interceptor Record) have no colon;
//   - `:=` is excluded so a local variable of the same name cannot false-positive;
//   - line comments are stripped before matching, so prose does not count.
var invertsSetter = regexp.MustCompile(`InvertsActionID\s*:\s*[^=\s]`)

// allowedInverseProducers is the reviewed exception list, path → justification.
var allowedInverseProducers = map[string]string{
	// TG-462: the sealed manual-rollback lane — seal at POLL_PAUSE, operator approval, gated
	// execute. The ONE producer G6's sealed_inverse exclusion was written for.
	"temporal/runner/rollback_workflow.go": "the sealed rollback lane (TG-462/TG-404)",
}

func inverseRepoRoot(t *testing.T) string {
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

func TestRollbackWorkflowIsTheSoleInvertsActionIDProducer(t *testing.T) {
	root := inverseRepoRoot(t)
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
		// Strip line comments so prose mentioning the setter shape does not count.
		var code []string
		for _, line := range strings.Split(string(b), "\n") {
			if i := strings.Index(line, "//"); i >= 0 {
				line = line[:i]
			}
			code = append(code, line)
		}
		if invertsSetter.MatchString(strings.Join(code, "\n")) {
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
		if _, ok := allowedInverseProducers[f]; !ok {
			violations = append(violations, f)
		}
	}
	for f := range allowedInverseProducers {
		if !found[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(violations)
	sort.Strings(stale)
	for _, f := range violations {
		t.Errorf("NEW InvertsActionID producer outside the sealed rollback lane: %s — route it "+
			"through RollbackWorkflow or add a written justification here; an unreviewed producer "+
			"rides G6's sealed_inverse exclusion and its mutations vanish from the loop-bypass axis", f)
	}
	for _, f := range stale {
		t.Errorf("stale exception: %s no longer sets InvertsActionID — remove it so the "+
			"enumeration stays closed", f)
	}
	if len(found) == 0 {
		t.Fatal("walk found NO InvertsActionID setter at all — the pattern or the tree moved; " +
			"an oracle that cannot see the one legitimate producer proves nothing")
	}
}
