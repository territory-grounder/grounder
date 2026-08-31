package trackerimport

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ANTI-ACCRUAL PIN (TG-244 item 3). The earned graduation ladder accrues on CONFIRMED-CLEAN OUTCOMES from
// TG's own action_execution / session_triage spine — policy.Ladder.Record(ctx, opClass, RunOutcome), where
// RunOutcome is a bare verdict enum. It does not, and must not, read the precedent corpus: an imported tracker
// resolution (knowledge.ProvenanceTrackerImport, this package) is an engineer's CLAIM, not an outcome TG
// produced and verified, so letting it reach opclass_candidate accrual would graduate an op-class toward
// `auto` on unearned evidence.
//
// The corpus is retrieval-only — core/knowledge feeds the seed context the model reasons WITH, never the
// ladder that decides what may actuate. This holds TODAY BY CONSTRUCTION: neither the graduation ladder
// (core/policy) nor the interceptor that drives its accrual (core/actuate) takes any dependency on
// core/knowledge. This pin keeps it true. It goes RED the moment either package imports the corpus — the
// shape any "N imported precedents ⇒ graduate this op-class" wiring would have to introduce — naming the file.
//
// It lives HERE, in the import feature's own package, rather than under core/policy: it is a safety property
// OF this feature, and core/policy is a protected path whose every touch (tests included) demands a spec
// restamp and owner sign-off. Reading the two package directories from outside asserts exactly the same fact
// without touching either.
//
// KILLING MUTATION: add an import of core/knowledge to any production file in core/policy or core/actuate.
func TestAccrualDoesNotReadTheCorpus(t *testing.T) {
	const corpusPkg = "core/knowledge"
	for _, dir := range []string{filepath.Join("..", "policy"), filepath.Join("..", "actuate")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read package dir %s: %v", dir, err)
		}
		scanned := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue // production files only — a test may legitimately import the corpus to assert about it
			}
			scanned++
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
			if perr != nil {
				t.Fatalf("parse %s: %v", filepath.Join(dir, name), perr)
			}
			for _, imp := range f.Imports {
				p, uerr := strconv.Unquote(imp.Path.Value)
				if uerr != nil {
					continue
				}
				if strings.Contains(p, corpusPkg) {
					t.Errorf("%s imports %q: the earned graduation ladder accrues on the confirmed-clean RUN "+
						"spine (RunOutcome), never on corpus precedent. An imported tracker row is an engineer's "+
						"claim, not a TG-verified outcome — it must never reach opclass_candidate accrual.",
						filepath.Join(dir, name), p)
				}
			}
		}
		if scanned == 0 {
			t.Fatalf("vacuity floor: scanned 0 production .go files in %s — the pin asserted nothing", dir)
		}
	}
}
