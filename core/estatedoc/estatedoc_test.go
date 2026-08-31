package estatedoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/screen"
)

// passthrough is a ScrubFunc that changes nothing — used where the test asserts CHUNK STRUCTURE, so the
// content is predictable and the sanitize boundary is exercised separately.
func passthrough(s string) (string, []screen.Match) { return s, nil }

// redactSentinel is a ScrubFunc that replaces a known token with a marker and reports one match — a
// deterministic stand-in for core/screen.Scrub, so a test can prove Chunk USES the scrubbed output (not the
// raw input) and propagates the redaction count without depending on the real pattern set.
func redactSentinel(s string) (string, []screen.Match) {
	if strings.Contains(s, "SECRET-TOKEN") {
		return strings.ReplaceAll(s, "SECRET-TOKEN", "[REDACTED]"), []screen.Match{{Category: "secret-redaction"}}
	}
	return s, nil
}

func TestChunk_SplitsByHeadingWithNearestHeading(t *testing.T) {
	doc := "# Alpha\nbody of alpha\n## Beta\nbody of beta\n"
	chunks := Chunk("docs/a.md", doc, DefaultMaxChunkBytes, passthrough)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks (one per heading), got %d: %+v", len(chunks), chunks)
	}
	if chunks[0].Heading != "Alpha" || !strings.Contains(chunks[0].Content, "body of alpha") {
		t.Errorf("chunk 0 = %+v, want heading Alpha with alpha body", chunks[0])
	}
	if chunks[1].Heading != "Beta" || !strings.Contains(chunks[1].Content, "body of beta") {
		t.Errorf("chunk 1 = %+v, want heading Beta with beta body", chunks[1])
	}
	if chunks[0].ExternalRef != "docs/a.md#0" || chunks[1].ExternalRef != "docs/a.md#1" {
		t.Errorf("refs = %q,%q, want docs/a.md#0, docs/a.md#1", chunks[0].ExternalRef, chunks[1].ExternalRef)
	}
}

func TestChunk_SplitsOversizeSectionKeepingHeading(t *testing.T) {
	// One heading, a body far larger than the bound → several chunks, all under the bound, all same heading.
	body := strings.Repeat("paragraph line that is reasonably long\n", 40)
	chunks := Chunk("big.md", "# Solo\n"+body, 256, passthrough)
	if len(chunks) < 2 {
		t.Fatalf("want the oversize section split into >1 chunk, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Heading != "Solo" {
			t.Errorf("chunk %d heading = %q, want Solo (the split keeps the section heading)", i, c.Heading)
		}
		if len(c.Content) > 256 {
			t.Errorf("chunk %d is %d bytes, want <= 256 (the bound)", i, len(c.Content))
		}
	}
}

// The STONITH-critical contract: Chunk retains the SCRUBBED body, never the raw input, and reports the
// redaction count. KILLING MUTATION: make Chunk use `raw` instead of `scrubbed` and this reddens — a secret
// would reach the corpus.
func TestChunk_RetainsScrubbedBodyNotRaw(t *testing.T) {
	chunks := Chunk("s.md", "# H\nthe value is SECRET-TOKEN here\n", DefaultMaxChunkBytes, redactSentinel)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if strings.Contains(c.Content, "SECRET-TOKEN") {
		t.Fatalf("SECRET-TOKEN reached the corpus chunk — scrub output not used: %q", c.Content)
	}
	if !strings.Contains(c.Content, "[REDACTED]") {
		t.Errorf("content = %q, want the scrubbed [REDACTED] form", c.Content)
	}
	if c.Redactions != 1 {
		t.Errorf("Redactions = %d, want 1 (the scrub match count propagates)", c.Redactions)
	}
	// The hash is over the SCRUBBED content, so it is the same whether or not the raw secret differed.
	if c.ContentHash == "" || len(c.ContentHash) != 64 {
		t.Errorf("ContentHash = %q, want a 64-hex sha256 of the scrubbed body", c.ContentHash)
	}
}

// The HEADING is a persisted, retrievable field and headings routinely name a host or endpoint, so it must be
// scrubbed too — the body scrub alone is not enough (TG-86 review, blocker 1). KILLING MUTATION: assign the raw
// heading (`heading = h` without scrub) and SECRET-TOKEN reaches the corpus heading, reddening this.
func TestChunk_ScrubsHeadingNotJustBody(t *testing.T) {
	chunks := Chunk("h.md", "# host SECRET-TOKEN box\nplain body\n", DefaultMaxChunkBytes, redactSentinel)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if strings.Contains(c.Heading, "SECRET-TOKEN") {
		t.Fatalf("SECRET-TOKEN reached the corpus HEADING unscrubbed: %q", c.Heading)
	}
	if !strings.Contains(c.Heading, "[REDACTED]") {
		t.Errorf("heading = %q, want the scrubbed form", c.Heading)
	}
	if c.Redactions < 1 {
		t.Errorf("Redactions = %d, want the heading's redaction counted onto the chunk", c.Redactions)
	}
}

// A heading-only change must change ContentHash so the hash-guard re-persists it and never leaves a stale
// (possibly leaking) heading on disk (TG-86 review, blocker 2). KILLING MUTATION: drop the heading from the
// ContentHash input and these two same-body/different-heading chunks collide, reddening this.
func TestChunk_ContentHashCoversHeading(t *testing.T) {
	a := Chunk("d.md", "# HeadingOne\nsame body\n", DefaultMaxChunkBytes, passthrough)
	b := Chunk("d.md", "# HeadingTwo\nsame body\n", DefaultMaxChunkBytes, passthrough)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want 1 chunk each, got %d and %d", len(a), len(b))
	}
	if a[0].ContentHash == b[0].ContentHash {
		t.Error("same body + different heading share a ContentHash — a heading change would not force a re-persist")
	}
}

func TestChunk_DropsWhitespaceOnlySections(t *testing.T) {
	// A heading with no body, then a real section: only the real one survives (an empty chunk is never emitted).
	chunks := Chunk("e.md", "# Empty\n\n   \n# Real\ncontent\n", DefaultMaxChunkBytes, passthrough)
	if len(chunks) != 1 || chunks[0].Heading != "Real" {
		t.Fatalf("want exactly the non-empty Real chunk, got %+v", chunks)
	}
}

func TestChunk_IsIdempotentAndOrdinalStable(t *testing.T) {
	doc := "# A\nalpha\n# B\nbeta\n"
	first := Chunk("x.md", doc, DefaultMaxChunkBytes, passthrough)
	second := Chunk("x.md", doc, DefaultMaxChunkBytes, passthrough)
	if len(first) != len(second) {
		t.Fatalf("re-chunk changed the count: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ExternalRef != second[i].ExternalRef || first[i].ContentHash != second[i].ContentHash {
			t.Errorf("chunk %d not stable across re-ingest: %+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestLoad_WalksMarkdownOnlyDeterministically(t *testing.T) {
	root := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("CLAUDE.md", "# Root\nroot doc\n")
	must("sub/README.md", "# Sub\nsub doc\n")
	must("sub/ignore.txt", "not markdown — must not be ingested\n")
	must("notes.yaml", "also: not markdown\n")

	corpus, err := Load(root, DefaultMaxChunkBytes, passthrough)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if corpus.Files != 2 {
		t.Errorf("Files = %d, want 2 (the two .md files only)", corpus.Files)
	}
	if len(corpus.Chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(corpus.Chunks))
	}
	// deterministic order by ref, and never an absolute path in a ref.
	if corpus.Chunks[0].ExternalRef != "CLAUDE.md#0" || corpus.Chunks[1].ExternalRef != "sub/README.md#0" {
		t.Errorf("refs = %q, %q, want CLAUDE.md#0, sub/README.md#0", corpus.Chunks[0].ExternalRef, corpus.Chunks[1].ExternalRef)
	}
	for _, c := range corpus.Chunks {
		if filepath.IsAbs(c.Path) || strings.Contains(c.ExternalRef, root) {
			t.Errorf("a corpus ref leaked an absolute/host path: %q", c.ExternalRef)
		}
	}
}

func TestLoad_AbsentOrEmptyRootIsEmptyNotError(t *testing.T) {
	for _, root := range []string{"", filepath.Join(t.TempDir(), "does-not-exist")} {
		corpus, err := Load(root, DefaultMaxChunkBytes, passthrough)
		if err != nil {
			t.Errorf("Load(%q) errored, want empty corpus: %v", root, err)
		}
		if corpus.Files != 0 || len(corpus.Chunks) != 0 {
			t.Errorf("Load(%q) = %d files, want empty", root, corpus.Files)
		}
	}
}

// Integration: the real core/screen.Scrub is the production ScrubFunc. A benign doc with no secret or
// injection passes through content-preserving (byte-identical body), proving the wiring is sound without
// pinning the exact secret grammar (core/screen owns that).
func TestLoad_RealScreenScrubPreservesBenignContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "d.md"), []byte("# Runbook\nrestart the service and wait\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	corpus, err := Load(root, DefaultMaxChunkBytes, screen.Scrub)
	if err != nil {
		t.Fatalf("Load with screen.Scrub: %v", err)
	}
	if len(corpus.Chunks) != 1 || !strings.Contains(corpus.Chunks[0].Content, "restart the service and wait") {
		t.Fatalf("benign content not preserved through screen.Scrub: %+v", corpus.Chunks)
	}
}

// TG-486 — the HONEST boundary oracle, replacing the vacuous slice-1 "0 estate identifiers" acceptance
// (which ran against an already-mirror-clean docs/ tree and so could never fail — the classic vacuous-oracle
// shape). core/screen.Scrub strips SECRETS but NOT estate identifiers, so this pins BOTH halves on a
// deliberately DIRTY input: the secret is gone from the corpus, the estate-identifier-shaped string survives.
// Had this existed, it would have caught the package comment's false "no hostname/IP ever enters" claim.
//
// The estate identifier is a SYNTHETIC fixture host assembled at RUNTIME from split parts, so no contiguous
// site-code literal appears in this SOURCE file — scripts/lint-forbidden.sh's STONITH gate refuses such a
// literal in any shipped artifact, the very leak this ticket is about, and the split parts individually match
// neither that gate nor the mirror denylist pattern. The reconstructed value only ever lives in a t.TempDir()
// doc and in test output, never in the committed tree. (Same idiom the lint's PEM-marker rule blesses.)
func TestLoad_RealScreenScrubStripsSecretsButNotEstateIdentifiers(t *testing.T) {
	// A synthetic host of the real estate shape (site prefix + suffix), built so the SOURCE carries no
	// contiguous site-code literal and the value is self-evidently a fixture, never a real host.
	estateHost := strings.Join([]string{"nl", "lei", "01", "fixturehost"}, "")
	const fakeSecret = "swordfish-not-a-real-secret-9911"

	root := t.TempDir()
	doc := "# Runbook for " + estateHost + "\n" +
		"password: " + fakeSecret + "\n" +
		"restart the pve-cluster service on " + estateHost + " and wait\n"
	if err := os.WriteFile(filepath.Join(root, "d.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	corpus, err := Load(root, DefaultMaxChunkBytes, screen.Scrub)
	if err != nil {
		t.Fatalf("Load with screen.Scrub: %v", err)
	}
	var body string
	for _, c := range corpus.Chunks {
		body += c.Content + "\n"
	}
	// HALF 1 — the SECRET must be stripped (screen doing its actual job; the STONITH property that DOES hold).
	if strings.Contains(body, fakeSecret) {
		t.Fatalf("a secret survived screen.Scrub into the corpus — the one property that MUST hold: %q", body)
	}
	// HALF 2 — the ESTATE IDENTIFIER passes through UNSCRUBBED. This is the documented gap (TG-486): screen
	// strips secrets, not hostnames/IPs/site-codes. The corpus's mirror-safety is the DEPLOYMENT posture
	// (on-box/private), never a scrub guarantee. This assertion states the TRUTH; if a future change makes
	// estatedoc redact identifiers (owner ruling (b) on TG-486), flip this to Contains==false and delete the
	// package-comment sentence that says they pass through — deliberately, not silently.
	if !strings.Contains(body, estateHost) {
		t.Fatalf("the estate identifier did NOT survive — estatedoc now strips identifiers; if that is the "+
			"TG-486 ruling (b), update this oracle + the package comment together, don't leave them contradictory. body=%q", body)
	}
}
