package deploy_test

// THE GUARD THAT GUARDS THE GUARD (TG-264).
//
// TestEveryDescriptorEnvKeyIsActuallyReadByTheBinary is only as honest as its collector. The previous
// collector was `regexp.MustCompile(`+"`TG_[A-Z0-9_]+`"+`)` over raw file bytes, and under it
// modules/bootstrap/bootstrap.go contributed four keys to the "read by the binary" set — every one of them
// a COMMENT. These tests pin the collector to the structural reading so a rewrite back toward text-matching
// goes red immediately.
//
// KILLING MUTATION for each: swap collectEnvKeyCallArgs back to a byte-regex scan. Every "not counted"
// case below flips to counted, RED.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func collect(t *testing.T, src string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	out := map[string]bool{}
	collectEnvKeyCallArgs(f, regexp.MustCompile(`^TG_[A-Z0-9_]+$`), out)
	return out
}

func TestAKeyMentionedOnlyInACommentIsNotARead(t *testing.T) {
	got := collect(t, `package main
// The operator should set TG_ONLY_IN_A_COMMENT before booting.
func main() { _ = getenv("TG_REALLY_READ", "") }
func getenv(k, d string) string { return d }`)
	if got["TG_ONLY_IN_A_COMMENT"] {
		t.Fatal("a key that exists only in a COMMENT was counted as read — this is the exact defect " +
			"(TG-264): a dialog whose EnvKey is only ever written about passes the wired-to-nothing guard")
	}
	if !got["TG_REALLY_READ"] {
		t.Fatal("vacuity: the collector missed a genuine read, so the comment case above proves nothing")
	}
}

func TestAKeyInsideAProseStringIsNotARead(t *testing.T) {
	got := collect(t, `package main
import "log"
func main() { log.Printf("set TG_MENTIONED_IN_PROSE and restart the worker") }`)
	if got["TG_MENTIONED_IN_PROSE"] {
		t.Fatal("a key embedded in a longer log string was counted as read — an error message about a " +
			"setting is not the setting taking effect")
	}
}

func TestAConcatenatedKeyIsNotARead(t *testing.T) {
	got := collect(t, `package main
import "os"
func main() { _ = os.Getenv("TG_" + "ASSEMBLED") }`)
	if got["TG_ASSEMBLED"] {
		t.Fatal("a dynamically-assembled key was counted — assembled reads are exactly what the resolver " +
			"chokepoint bans, and the guard must not vouch for them")
	}
}

func TestAnyCallArgumentPositionCounts(t *testing.T) {
	got := collect(t, `package main
import "os"
func main() {
	_ = os.Getenv("TG_FIRST_ARG")
	compare("expected", "TG_SECOND_ARG")
}
func compare(a, b string) bool { return a == b }`)
	if !got["TG_FIRST_ARG"] || !got["TG_SECOND_ARG"] {
		t.Fatalf("collector is position-sensitive (%v) — accessor signatures differ across the roots, and "+
			"a matcher bound to argument 0 silently drops keys the moment one adds a prefix parameter", got)
	}
}

// TestTheWholeCollectionPathIgnoresComments drives envKeysReadAsCallArguments end to end over a synthetic
// package directory, so a regression at EITHER layer — the collector or the caller that feeds it — goes
// red. The pins above call the collector directly and would miss a caller that quietly went back to
// reading raw bytes; this one cannot.
//
// KILLING MUTATION (executed): re-implement envKeysReadAsCallArguments as the old byte-regex over file
// contents. RED here on the comment key, green everywhere else — which is exactly the TG-264 defect shape.
func TestTheWholeCollectionPathIgnoresComments(t *testing.T) {
	dir := t.TempDir()
	src := `package synth
// Operators must export TG_COMMENT_ONLY_KEY before boot.
func boot() { _ = getenv("TG_GENUINELY_READ", "") }
func getenv(k, d string) string { return d }
`
	if err := os.WriteFile(filepath.Join(dir, "synth.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := envKeysReadAsCallArguments(t, dir)
	if got["TG_COMMENT_ONLY_KEY"] {
		t.Fatal("the end-to-end collection counted a comment-only key — the caller is reading bytes again (TG-264)")
	}
	if !got["TG_GENUINELY_READ"] {
		t.Fatal("vacuity: the genuine read was missed, so the comment assertion proves nothing")
	}
}
