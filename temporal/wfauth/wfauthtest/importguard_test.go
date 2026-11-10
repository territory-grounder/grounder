package wfauthtest_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The permissive test doubles — wfauthtest.AllowAll (an envelope verifier that admits everything) and the
// runnertest records (an approval for every action, the loosest band) — exist so oracles whose subject is
// elsewhere keep running. They must never be reachable from a binary: one import into a composition root would
// switch the TG-660 authority checks off silently, and the deadcode gate (rooted at the binaries) would then see
// them as live code, not as a defect. This test walks every non-test Go file in the module and refuses the import.
func TestPermissiveTestDoublesAreImportedOnlyByTests(t *testing.T) {
	root := moduleRoot(t)
	forbidden := []string{"/temporal/wfauth/wfauthtest", "/temporal/runner/runnertest"}
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".claude" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			for _, fb := range forbidden {
				if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), fb) {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, rel+" imports "+imp.Path.Value)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("a permissive test double is imported by NON-test code — the authority checks would be off:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func moduleRoot(t *testing.T) string {
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
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
