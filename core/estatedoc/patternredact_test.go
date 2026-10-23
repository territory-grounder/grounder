package estatedoc

// TG-86 producer-side identifier redaction. Every identifier below is SYNTHETIC (zzsite/qqsite,
// example-int.lan) — a real estate token in a committed test would itself be the leak this pass exists
// to stop. The load-bearing oracle is gate parity: after redaction, NONE of the vocabulary patterns may
// match the output, because that grep is exactly what scripts/estate-docs-corpus.sh's abort-on-survivor
// floor runs against the persisted corpus.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/screen"
)

// syntheticPatterns mirrors the denylist's SHAPE classes: site-code prefixes, a domain, a path prefix,
// and a dotted-quad IP shape (the estate-redact-extra.txt class).
var syntheticPatterns = []string{
	`zzsite[0-9]{2}`,
	`[Qq][Qq]site`,
	`example-int`,
	`/opt/buildhome`,
	`\b([0-9]{1,3}\.){3}[0-9]{1,3}\b`,
}

func TestLoadRedactPatterns_SkipsCommentsAndBlanks_FailsClosedOnEmpty(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "deny.txt")
	if err := os.WriteFile(good, []byte("# comment\n\nzzsite[0-9]{2}\n  \nexample-int\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRedactPatterns(good)
	if err != nil {
		t.Fatalf("LoadRedactPatterns: %v", err)
	}
	if len(got) != 2 || got[0] != "zzsite[0-9]{2}" || got[1] != "example-int" {
		t.Fatalf("patterns = %v, want the two non-comment lines", got)
	}

	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte("# only comments\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRedactPatterns(empty); err == nil {
		t.Fatal("zero patterns must be an error — half a floor is no floor")
	}
	if _, err := LoadRedactPatterns(filepath.Join(dir, "absent.txt")); err == nil {
		t.Fatal("an unreadable pattern file must be an error")
	}
}

func TestNewPatternRedactor_RedactsWholeContainingToken(t *testing.T) {
	redact, err := NewPatternRedactor(syntheticPatterns)
	if err != nil {
		t.Fatal(err)
	}
	in := "host zzsite01app02.example-int.lan (10.20.30.40) logs to /opt/buildhome/logs/app.log daily"
	out, n := redact(in)
	if n == 0 {
		t.Fatal("expected redactions")
	}
	for _, residue := range []string{"zzsite", "app02", "example-int", "10.20.30.40", "/opt/buildhome", "app.log"} {
		if strings.Contains(out, residue) {
			t.Errorf("residue %q survived in %q — the whole containing token must be redacted", residue, out)
		}
	}
	if !strings.Contains(out, EstateIDMarker) {
		t.Errorf("marker missing from %q", out)
	}
	// Words adjacent to the tokens survive: the expansion is per-token, not per-line.
	for _, keep := range []string{"host ", " logs to ", " daily"} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-identifier text %q was over-redacted in %q", keep, out)
		}
	}
}

func TestNewPatternRedactor_CaseInsensitive_ProbeFirst_EmptySet(t *testing.T) {
	redact, err := NewPatternRedactor([]string{`zzsite[0-9]{2}`})
	if err != nil {
		t.Fatal(err)
	}
	if out, n := redact("saw ZZSITE07CORE01 up"); n != 1 || strings.Contains(out, "ZZSITE") {
		t.Errorf("case-insensitive redaction failed: %q (n=%d)", out, n)
	}
	clean := "no identifiers here at all"
	if out, n := redact(clean); n != 0 || out != clean {
		t.Errorf("probe-first: clean text must pass byte-identical, got %q (n=%d)", out, n)
	}
	passthrough, err := NewPatternRedactor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, n := passthrough("zzsite01"); n != 0 || out != "zzsite01" {
		t.Error("empty pattern set must be a passthrough")
	}
}

func TestNewPatternRedactor_InvalidPatternIsError(t *testing.T) {
	if _, err := NewPatternRedactor([]string{`zzsite[0-9]{2}`, `broken(`}); err == nil {
		t.Fatal("a non-compiling pattern must fail construction — a dropped pattern is an un-floored identifier")
	}
}

// TestNewPatternRedactor_GateParity is the load-bearing oracle: the producer's denylist floor greps the
// persisted corpus with these same patterns, so redacted output containing ANY match — including inside
// the marker itself — would fail the real gate.
func TestNewPatternRedactor_GateParity(t *testing.T) {
	redact, err := NewPatternRedactor(syntheticPatterns)
	if err != nil {
		t.Fatal(err)
	}
	dirty := strings.Join([]string{
		"zzsite01core01 uplinks to qqsiteedge02 via 192.168.7.9",
		"clone from https://git.example-int.lan/infra and build under /opt/buildhome/src",
		"mixed-case QQSite mention and a bare zzsite99",
	}, "\n")
	out, _ := redact(dirty)
	for _, p := range syntheticPatterns {
		if regexp.MustCompile(p).MatchString(out) {
			t.Errorf("pattern %q still matches redacted output %q — the corpus gate would refuse this", p, out)
		}
		if regexp.MustCompile(p).MatchString(EstateIDMarker) {
			t.Errorf("pattern %q matches the marker itself", p)
		}
	}
}

func TestRedactCorpusPaths_AliasesSegmentsStablyAndDistinctly(t *testing.T) {
	match, err := NewPatternMatcher([]string{`zzsite[0-9]{2}`, `[Qq][Qq]site`})
	if err != nil {
		t.Fatal(err)
	}
	corpus := &Corpus{Chunks: []DocChunk{
		{ExternalRef: "zzsite01/prod/CLAUDE.md#0", Path: "zzsite01/prod/CLAUDE.md"},
		{ExternalRef: "zzsite01/prod/CLAUDE.md#1", Path: "zzsite01/prod/CLAUDE.md"},
		{ExternalRef: "qqsite/prod/CLAUDE.md#0", Path: "qqsite/prod/CLAUDE.md"},
		{ExternalRef: "common/README.md#0", Path: "common/README.md"},
	}}
	// One aliased segment per DISTINCT path (the per-file cache stops double-counting a file's chunks):
	// zzsite01 in one path + qqsite in one path = 2.
	if n := RedactCorpusPaths(corpus, match); n != 2 {
		t.Errorf("aliased segments = %d, want 2", n)
	}
	byRef := map[string]DocChunk{}
	var refs []string
	for _, c := range corpus.Chunks {
		byRef[c.ExternalRef] = c
		refs = append(refs, c.ExternalRef)
	}
	for ref, c := range byRef {
		if strings.Contains(ref, "zzsite") || strings.Contains(ref, "qqsite") ||
			strings.Contains(c.Path, "zzsite") || strings.Contains(c.Path, "qqsite") {
			t.Errorf("identifier survived in ref %q / path %q", ref, c.Path)
		}
	}
	// The two files must alias to DISTINCT paths (a flat marker would collide them), the clean path is
	// untouched, and a file's own chunks agree on one rewritten path with ordinals preserved.
	if _, ok := byRef["common/README.md#0"]; !ok {
		t.Error("clean path must be untouched")
	}
	var site1Paths, site2Paths []string
	for _, c := range corpus.Chunks {
		if strings.HasSuffix(c.ExternalRef, "#1") {
			site1Paths = append(site1Paths, c.Path) // only the zzsite file had a #1 chunk
		}
	}
	if len(site1Paths) != 1 {
		t.Fatalf("ordinal #1 chunk lost: refs=%v", refs)
	}
	for _, c := range corpus.Chunks {
		if c.Path != site1Paths[0] && c.Path != "common/README.md" {
			site2Paths = append(site2Paths, c.Path)
		}
	}
	if len(site2Paths) != 1 || site2Paths[0] == site1Paths[0] {
		t.Errorf("aliased paths must be distinct per source file: %v vs %v", site1Paths, site2Paths)
	}
	// Stability: a second corpus from the same source paths aliases identically.
	again := &Corpus{Chunks: []DocChunk{{ExternalRef: "zzsite01/prod/CLAUDE.md#0", Path: "zzsite01/prod/CLAUDE.md"}}}
	RedactCorpusPaths(again, match)
	if again.Chunks[0].Path != site1Paths[0] {
		t.Errorf("aliasing not stable across re-ingest: %q vs %q", again.Chunks[0].Path, site1Paths[0])
	}
	// Gate parity on the alias itself.
	for _, p := range []string{`zzsite[0-9]{2}`, `[Qq][Qq]site`} {
		if regexp.MustCompile(p).MatchString(site1Paths[0] + " " + site2Paths[0]) {
			t.Errorf("pattern %q matches an aliased path", p)
		}
	}
}

func TestLoad_SkipsDotDirectories(t *testing.T) {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"docs/runbook.md":               "# keep\nreal documentation\n",
		".claude/worktrees/copy/dup.md": "# skip\nduplicate checkout content\n",
		".git/notes.md":                 "# skip\ntooling internals\n",
		"docs/.hidden/inner.md":         "# skip\nnested hidden dir\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	passthrough := func(s string) (string, []screen.Match) { return s, nil }
	corpus, err := Load(dir, 0, passthrough)
	if err != nil {
		t.Fatal(err)
	}
	if corpus.Files != 1 || len(corpus.Chunks) != 1 || corpus.Chunks[0].Path != "docs/runbook.md" {
		t.Fatalf("dot-directories must be skipped: files=%d chunks=%+v", corpus.Files, corpus.Chunks)
	}
	// A hidden ROOT still ingests — only hidden dirs BELOW the root are tooling internals.
	hiddenRoot := filepath.Join(dir, ".claude", "worktrees", "copy")
	c2, err := Load(hiddenRoot, 0, passthrough)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Files != 1 {
		t.Fatalf("a hidden root itself must still ingest, got files=%d", c2.Files)
	}
}

func TestComposeScrub_CountsBothPasses_AndChunkIntegration(t *testing.T) {
	redact, err := NewPatternRedactor(syntheticPatterns)
	if err != nil {
		t.Fatal(err)
	}
	stub := func(s string) (string, []screen.Match) {
		return strings.ReplaceAll(s, "hunter2", "[REDACTED:secret]"),
			[]screen.Match{{Category: screen.CategorySecretRedaction, Pattern: "stub"}}
	}
	composed := ComposeScrub(stub, redact)
	out, matches := composed("password hunter2 on zzsite01db01")
	if strings.Contains(out, "hunter2") || strings.Contains(out, "zzsite01db01") {
		t.Fatalf("composed scrub left residue: %q", out)
	}
	var idMatches int
	for _, m := range matches {
		if m.Category == CategoryEstateIdentifier {
			idMatches++
		}
	}
	if idMatches != 1 || len(matches) != 2 {
		t.Fatalf("matches = %+v, want 1 secret + 1 estate-identifier", matches)
	}

	// Chunk-level: the Redactions tally a coverage gauge reads must include identifier redactions.
	chunks := Chunk("runbook.md", "# zzsite01 recovery\nrestart the qqsiteedge02 node\n", 0, composed)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	c := chunks[0]
	if strings.Contains(c.Heading, "zzsite") || strings.Contains(c.Content, "qqsite") {
		t.Fatalf("chunk retained identifiers: heading=%q content=%q", c.Heading, c.Content)
	}
	if c.Redactions < 2 {
		t.Errorf("chunk Redactions = %d, want >= 2 (heading + body identifier redactions)", c.Redactions)
	}
	// ComposeScrub with a nil redactor is the unchanged production default.
	if got := ComposeScrub(stub, nil); got == nil {
		t.Fatal("nil redactor must return the base scrub")
	}
}

// TestComposeScrub_ScreenRunsBeforePatterns kills the order mutation: "hunter2-zzsite01" is ONE token, so a
// pattern-first composition would swallow the whole token (secret included) before the screen ever saw it —
// zero secret matches. Screen-first sees and counts the secret, and the pattern pass then redacts the
// identifier remainder. Both orders leak nothing, but only the correct order preserves the secret TALLY the
// coverage gauge reports — and a screen that never sees the text is a screen that cannot neutralize
// injection payloads riding in identifier-adjacent tokens.
func TestComposeScrub_ScreenRunsBeforePatterns(t *testing.T) {
	redact, err := NewPatternRedactor([]string{`zzsite[0-9]{2}`})
	if err != nil {
		t.Fatal(err)
	}
	stub := func(s string) (string, []screen.Match) {
		if !strings.Contains(s, "hunter2") {
			return s, nil // the order mutation starves this branch
		}
		return strings.ReplaceAll(s, "hunter2", "[REDACTED:secret]"),
			[]screen.Match{{Category: screen.CategorySecretRedaction, Pattern: "stub"}}
	}
	out, matches := ComposeScrub(stub, redact)("token hunter2-zzsite01 here")
	var secrets int
	for _, m := range matches {
		if m.Category == screen.CategorySecretRedaction {
			secrets++
		}
	}
	if secrets != 1 {
		t.Errorf("screen must run BEFORE pattern redaction (secret matches = %d, want 1); matches=%+v", secrets, matches)
	}
	if strings.Contains(out, "hunter2") || strings.Contains(out, "zzsite") {
		t.Errorf("residue in %q", out)
	}
}
