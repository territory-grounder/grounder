package fuzzcorpus_test

// The BOUNDARY-COVERAGE MAP (TG-5 Phase 4, docs/TESTING-AND-BENCHMARK.md §3.1/§3.2), as a maintained
// registry enforced in `make test`. §3.2's contract — "adding an adapter without wiring it to the corpus
// fails the boundary-coverage gate" — needs a place that KNOWS the full set of trust boundaries and asserts
// each one drives the shared battery. This is that place, in non-protected test code.
//
// THE MAP is boundaries below: every ingress/actuation point where attacker-controlled bytes cross into TG,
// each paired with the fuzz file that must seed from core/fuzzcorpus. The test reads each file and fails if
// the import (hence the shared battery) is absent. When you add a new trust boundary with a fuzz suite, add
// it here AND wire fuzzcorpus into it — a boundary in the map but unwired reds this test; a boundary with no
// map entry is caught by TestNoFuzzTargetExistsOutsideTheBoundaryMap below (the §3.1 scanner: every Fuzz*
// target in the tree must be declared here). Both drift directions are now enforced on every pipeline.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// boundaries is the declared trust-boundary → fuzz-file map. Paths are repo-relative (the test resolves the
// repo root from its own location). Keep it sorted by path for reviewability.
var boundaries = map[string]string{
	"untrusted-text screen (INV-02/03)":        "core/screen/screen_fuzz_test.go",
	"model-output proposal parser (INV-06)":    "core/proposal/parse_fuzz_test.go",
	"ingest normalizer front door (INV-04)":    "core/ingest/normalize_fuzz_test.go",
	"crowdsec security-telemetry ingress":      "modules/ingest/crowdsec/fuzz_test.go",
	"manifest action-id content hash (INV-07)": "core/manifest/action_id_fuzz_test.go",
	"governance ledger tamper-replay (INV-19)": "core/audit/ledger_tamper_fuzz_test.go",
}

const corpusImport = "core/fuzzcorpus"

func TestEveryBoundarySuiteWiresTheSharedCorpus(t *testing.T) {
	root := repoRoot(t)
	for name, rel := range boundaries {
		path := filepath.Join(root, filepath.FromSlash(rel))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("boundary %q: cannot read its fuzz file %s: %v (did the file move? update the map)", name, rel, err)
			continue
		}
		src := string(b)
		if !strings.Contains(src, corpusImport) {
			t.Errorf("boundary %q (%s) does NOT import %s — it is not driving the shared §3.2 battery, so a hostile class added to the corpus never reaches it", name, rel, corpusImport)
		}
		// A boundary must actually SEED from the battery, not merely import it: assert one of the wiring
		// entry points appears.
		if !strings.Contains(src, "fuzzcorpus.SeedStrings") && !strings.Contains(src, "fuzzcorpus.Strings()") {
			t.Errorf("boundary %q (%s) imports the corpus but calls neither SeedStrings nor Strings() — the battery is imported but unused", name, rel)
		}
	}
}

// repoRoot walks up from the test's working directory (the package dir, core/fuzzcorpus) to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate the module root (go.mod) above the test working directory")
	return ""
}

// The §3.1 SCANNER — the inverse direction of the registry above, and the half its prose used to defer as
// "the full CI gate": the registry proves every DECLARED boundary is wired; this proves NO fuzz target exists
// OUTSIDE the declaration. Together they close both drift directions — an unwired declared boundary reds the
// registry, and a new fuzz suite added without a map entry reds this scan. It runs in `go test ./...`
// (make test + the build-test CI job on every pipeline), so no separate protected-path CI job is needed.
//
// KILLING MUTATION: add a `func FuzzX` file anywhere without a boundaries entry → RED here.
func TestNoFuzzTargetExistsOutsideTheBoundaryMap(t *testing.T) {
	root := repoRoot(t)
	declared := map[string]bool{}
	for _, rel := range boundaries {
		declared[filepath.ToSlash(rel)] = true
	}
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// Skip non-source trees: VCS, worktrees/claims, vendored deps, node_modules, the bin dir.
			if name == ".git" || name == ".claude" || name == "vendor" || name == "node_modules" || name == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		// A fuzz TARGET is a top-level `func Fuzz...(f *testing.F)` — the string form is deliberate and
		// cheap; a helper named fuzzSomething or a mention in a comment does not match.
		if !strings.Contains(src, "\nfunc Fuzz") && !strings.HasPrefix(src, "func Fuzz") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		found = append(found, rel)
		if !declared[rel] {
			t.Errorf("fuzz target file %s is NOT in the boundary map — a trust boundary is being fuzzed off-registry "+
				"(or a fuzz suite was added without declaring its boundary). Add it to `boundaries` above AND wire "+
				"core/fuzzcorpus into it, so the shared hostile battery reaches it (§3.1/§3.2)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// VACUITY FLOOR: the walk must at least rediscover every declared boundary file; finding fewer means the
	// scanner is broken (wrong root, over-aggressive skip) and would pass while checking nothing.
	if len(found) < len(boundaries) {
		t.Fatalf("the scan found only %d fuzz file(s) but the map declares %d — the scanner is not seeing the tree it must police (found: %v)", len(found), len(boundaries), found)
	}
}
