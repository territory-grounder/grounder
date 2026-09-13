package deploy

// TG-112 — the mutation_enabled terminology is RETIRED. The 4-mode chokepoint (Shadow / HITL / Semi-auto /
// Full-auto) is the single source of truth; `may_actuate` is its derived "can this process act right now"
// signal (tg_may_actuate, runtime_posture.may_actuate, the may_actuate JSON fields), and tg_policy_mode /
// runtime_posture.mode carry the owner-set mode itself. A parallel mutation ON/OFF binary implies a switch
// that no longer exists (owner ruling on TG-112: removed completely from logic, codebase and UX).
//
// This guard is the closed-enumeration oracle for that retirement. It walks every LIVE surface — Go with
// comments stripped (retirement comments are deliberately KEPT as history), sql/sh/py/yml with full-line
// comments stripped, and json/html/js verbatim — and fails on any remaining `mutation_enabled` /
// `MutationEnabled` / "MUTATION ON|OFF" token. Out of scope BY DESIGN, each for a stated reason:
// docs/ (prose + ADRs + history may document the retiree), core/db/migrations/ (immutable applied history;
// 0078's down file legitimately restores the old column name), frontend/ (unreachable-by-construction and
// scheduled for removal — BOARD Phase E), .claude/ (session scratch), and this file itself.
// `MutationBreaker` / "mutation breaker" survive on purpose: that is a live safety control, not the retired
// switch — the banned family is the *_enabled binary only, which "breaker" never matches.
//
// KILLING MUTATION: reintroduce a live reference — a `mutation_enabled` metric sample, JSON field, SQL
// column or console label — and this goes RED naming file:line. The TG-365 emptiness arm is
// TestMutationTerminologyScannerCannotGoQuiet: the same matcher must still hit a seeded fixture and the
// walk must cover a healthy minimum of files, so an empty walk or a broken matcher fails loudly instead of
// certifying an unscanned tree.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	mutationEnabledRe = regexp.MustCompile(`(?i)mutation_?enabled`)
	mutationLabelRe   = regexp.MustCompile(`MUTATION\s+(ON|OFF)\b`)
)

// scannedExts are the live-surface file types. Markdown is deliberately absent (docs are allowed to name
// the retiree); everything executable or rendered is present.
var scannedExts = map[string]bool{
	".go": true, ".sql": true, ".sh": true, ".py": true,
	".yml": true, ".yaml": true, ".json": true, ".html": true, ".js": true, ".mjs": true,
}

// skippedDirs are subtrees that are not live surfaces. Every entry carries its justification in the file
// header above — an addition here without one is a review finding, not a convenience.
var skippedDirs = map[string]bool{
	".git": true, ".claude": true, "docs": true, "frontend": true, "node_modules": true,
}

func isSkippedDir(rel string) bool {
	if skippedDirs[rel] {
		return true
	}
	// Immutable applied migration history (and 0078's down file restores the old column name by design).
	if rel == filepath.Join("core", "db", "migrations") {
		return true
	}
	// spec/015 is the spec that PERFORMED the retirement (T-015-13, REQ-1520/1521): its lattice records —
	// scenario names, task titles, the coverage narrative — must name the retiree to describe absorbing it,
	// and rewriting that history would falsify the record this guard exists to protect.
	return rel == filepath.Join("spec", "015-policy-engine")
}

// skippedFiles are individual artifacts outside the scan, each with its reason.
var skippedFiles = map[string]bool{
	// BUILD ARTIFACT: byte-reproduced from console.html + modules/* (which ARE scanned) — proven by
	// `assemble.py --check` in make console-verify and TestAssemblePyByteReproducesTheServedIndex. Scanning
	// the bundle would double-count the sources through a heuristic JS lexer instead of trusting the
	// byte-equality gate that already ties artifact to sources.
	filepath.Join("deploy", "console", "v2", "index.html"): true,
}

// stripComments removes the comment forms of the given extension so KEPT retirement commentary does not
// trip the live-surface scan: C-style line+block comments (string-literal aware) for Go and for the
// JS-heavy html/js/mjs surfaces (the console sources embed their incident history as comments), and
// full-line #/-- comments for sh/py/yml/sql. json passes through verbatim — data has no commentary.
func stripComments(ext, content string) string {
	switch ext {
	case ".go", ".html", ".js", ".mjs":
		return stripGoComments(content)
	case ".sh", ".py", ".yml", ".yaml":
		return stripFullLine(content, "#")
	case ".sql":
		return stripFullLine(content, "--")
	default:
		return content
	}
}

func stripFullLine(content, marker string) string {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), marker) {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// stripGoComments blanks // and /* */ comments while respecting "..." / '...' / `...` literals, preserving
// line structure so reported line numbers stay true. A tokenizer would be sturdier; this stays dependency-
// free and is itself proven by the fixture arm of TestMutationTerminologyScannerCannotGoQuiet.
func stripGoComments(src string) string {
	var out []byte
	inLine, inBlock, inStr, inChar, inRaw := false, false, false, false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				out = append(out, c)
			} else {
				out = append(out, ' ')
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock = false
				out = append(out, ' ', ' ')
				i++
			} else if c == '\n' {
				out = append(out, c)
			} else {
				out = append(out, ' ')
			}
		case inStr:
			if c == '\\' && i+1 < len(src) {
				out = append(out, c, src[i+1])
				i++
			} else {
				if c == '"' {
					inStr = false
				}
				out = append(out, c)
			}
		case inChar:
			if c == '\\' && i+1 < len(src) {
				out = append(out, c, src[i+1])
				i++
			} else {
				if c == '\'' {
					inChar = false
				}
				out = append(out, c)
			}
		case inRaw:
			if c == '`' {
				inRaw = false
			}
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine = true
			out = append(out, ' ', ' ')
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock = true
			out = append(out, ' ', ' ')
			i++
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '\'':
			inChar = true
			out = append(out, c)
		case c == '`':
			inRaw = true
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// scanContent returns the 1-based lines of `content` (comments already stripped) that still carry the
// retired terminology.
func scanContent(content string) []int {
	var hits []int
	for i, line := range strings.Split(content, "\n") {
		if mutationEnabledRe.MatchString(line) || mutationLabelRe.MatchString(line) {
			hits = append(hits, i+1)
		}
	}
	return hits
}

// terminologyRepoRoot mirrors envparity_test.go's repoRoot; separately named so the guard stays drop-in.
func terminologyRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root (go.mod) not found above the deploy package")
		}
		dir = parent
	}
}

// TestMutationTerminologyIsRetired is the TG-112 closed-enumeration guard described in the file header.
func TestMutationTerminologyIsRetired(t *testing.T) {
	root := terminologyRepoRoot(t)
	scanned := 0
	var failures []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if info.IsDir() {
			if isSkippedDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if !scannedExts[ext] || filepath.Base(path) == "mutation_terminology_retired_test.go" || skippedFiles[rel] {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		for _, line := range scanContent(stripComments(ext, string(raw))) {
			failures = append(failures, fmt.Sprintf("%s:%d", rel, line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 300 {
		t.Fatalf("scanned only %d live-surface files — the walk is not covering the tree (TG-365: an empty scan certifies nothing)", scanned)
	}
	if len(failures) > 0 {
		t.Errorf("retired mutation_enabled terminology still LIVE at %d site(s) of %d scanned files (TG-112 — the 4-mode chokepoint + may_actuate is the vocabulary):\n  %s",
			len(failures), scanned, strings.Join(failures, "\n  "))
	} else {
		t.Logf("mutation_enabled terminology retired: 0 live sites across %d scanned files", scanned)
	}
}

// TestMutationTerminologyScannerCannotGoQuiet is the TG-365 arm: prove the matcher and the comment
// stripper can each still fail before trusting the green above.
func TestMutationTerminologyScannerCannotGoQuiet(t *testing.T) {
	liveGo := "package x\nvar name = \"mutation_enabled\"\n"
	if got := scanContent(stripComments(".go", liveGo)); len(got) != 1 || got[0] != 2 {
		t.Fatalf("matcher missed a LIVE Go string literal (got hits %v) — the guard above is not able to fail", got)
	}
	commentGo := "package x\n// the retired mutation_enabled switch is documented here\n"
	if got := scanContent(stripComments(".go", commentGo)); len(got) != 0 {
		t.Fatalf("stripper flagged a KEEP-class Go comment (hits %v) — retirement history must stay writable", got)
	}
	label := `<span class="chip">MUTATION OFF</span>`
	if got := scanContent(stripComments(".html", label)); len(got) != 1 {
		t.Fatalf("matcher missed a MUTATION OFF console label (hits %v)", got)
	}
	breaker := "package x\nvar b *safety.MutationBreaker // mutation breaker stays: a live control, not the switch\n"
	if got := scanContent(stripComments(".go", breaker)); len(got) != 0 {
		t.Fatalf("matcher wrongly bans MutationBreaker (hits %v) — the keep-list is part of the ruling", got)
	}
}
