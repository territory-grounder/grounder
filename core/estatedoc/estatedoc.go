// Package estatedoc ingests the operator's estate DOCUMENTATION — CLAUDE.md, IaC READMEs, runbooks — into a
// reduced, secret-scrubbed chunk corpus. It is the deterministic, retrieval-ready half of TG-86 (estate
// GROUNDING): the knowledge plane is 100% incident precedents, and the estate's own hundreds of KiB of prose
// (36 CLAUDE.md files alone) has never been read beside a system that reasons about that estate.
//
// This package is SLICE 1 — the loader only. It walks a configured root, chunks each document on heading and
// size boundaries, and scrubs EVERY chunk through the ScrubFunc it is handed before the chunk is retained.
// The BASE scrub is core/screen, which strips SECRETS — credentials, tokens, keys, injection payloads — but
// NOT estate IDENTIFIERS: hostnames, IPs, and site-codes PASS THROUGH it. Identifier redaction is the
// producer's obligation via composition (patternredact.go: ComposeScrub over the mirror-denylist vocabulary
// + RedactCorpusPaths for the refs) — BOTH producer transports carry it: cmd/estatedoc-ingest arms it with
// -redact-patterns (scripts/estate-docs-corpus.sh always passes it, then verifies with an abort-on-survivor
// denylist scan), and the worker's own docs-dir walk (cmd/worker/estate_doc_coverage.go) arms it with
// TG_ESTATE_DOC_REDACT_PATTERNS and REFUSES to persist a corpus without it. A corpus loaded with the bare
// screen scrub is secret-free but identifier-LADEN — in-memory coverage/grounding use only, owner-ruled on
// TG-486 (the model-facing NewIdentifierRedactor is the belt there). (An earlier version of this comment
// claimed "no hostname, IP, or secret ever enters" — that guarantee did NOT hold; a vacuous slice-1 oracle
// certified it against an already-mirror-clean docs/ tree. TestLoad_RealScreenScrubStripsSecretsButNotEstateIdentifiers
// now pins the REAL boundary on a deliberately dirty input.) Each chunk is keyed on a stable, path-relative
// ExternalRef and a content hash, so re-ingesting an unchanged tree yields byte-identical chunks (idempotent)
// and a changed file re-chunks under the same refs.
//
// It changes NO agent behaviour: the retrieval channel that would wire this corpus into the agent's reasoning
// context is the eval-gated slice 2, deliberately kept separate so slice 1 stays off the eval-behaviour
// surface. Producing the corpus and reporting its coverage is the whole of slice 1.
package estatedoc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/screen"
)

// DefaultMaxChunkBytes bounds one chunk so a single large document cannot dominate a later top-k retrieval.
// ~2 KiB is a few paragraphs — enough of a doc span to carry context without swamping the corpus.
const DefaultMaxChunkBytes = 2048

// ScrubFunc is core/screen.Scrub, taken as a parameter so a test can assert the sanitize boundary is actually
// crossed (with a trivial stub) while production always passes screen.Scrub. Every chunk's body goes through
// it before the chunk is retained.
type ScrubFunc func(string) (string, []screen.Match)

// DocChunk is one retrievable span of estate documentation. ExternalRef is stable across re-ingest — the
// source path (repo-relative, never absolute, so no host filesystem layout leaks into the corpus) plus the
// chunk's ordinal in that file — so the corpus stays addressable as documents change.
type DocChunk struct {
	ExternalRef string `json:"external_ref"`
	Path        string `json:"path"`
	Heading     string `json:"heading,omitempty"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	Redactions  int    `json:"redactions,omitempty"`
}

// Corpus is the ingested doc set plus the tallies the coverage metric reads. A non-zero Redactions count is
// EXPECTED and healthy — estate docs are full of hostnames and the occasional secret, and the point of the
// scrub is that they never reach a retrievable chunk.
type Corpus struct {
	Chunks     []DocChunk
	Files      int
	Redactions int
}

// headingLine reports the trimmed heading text if line is a markdown ATX heading (`#`..`######` + space),
// else "".
func headingLine(line string) string {
	t := strings.TrimLeft(line, " ")
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(t) || t[n] != ' ' {
		return ""
	}
	return strings.TrimSpace(t[n+1:])
}

// Chunk splits one document's raw content into scrubbed, addressable chunks. A markdown heading starts a new
// chunk (so a chunk is a coherent section, not an arbitrary byte window); a section longer than maxBytes is
// further split at line boundaries to keep chunks near the bound (a single source line longer than maxBytes is
// kept whole rather than split mid-content, so only such a line can exceed it). BOTH the heading and the body
// of every emitted chunk are scrubbed through scrub(); chunks whose scrubbed body is empty (whitespace-only
// sections) are dropped. relPath is used verbatim in each ExternalRef, so callers must pass a repo-relative
// path, never an absolute one.
func Chunk(relPath, content string, maxBytes int, scrub ScrubFunc) []DocChunk {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxChunkBytes
	}
	var chunks []DocChunk
	heading := ""   // ALREADY SCRUBBED — set below through scrub(), never raw
	headingRed := 0 // redactions found in the current heading, counted once on its first emitted chunk
	var buf []string
	bufLen := 0

	flush := func() {
		if len(buf) == 0 {
			return
		}
		raw := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		bufLen = 0
		if raw == "" {
			return
		}
		scrubbed, matches := scrub(raw)
		scrubbed = strings.TrimSpace(scrubbed)
		if scrubbed == "" {
			return
		}
		// The heading is scrubbed at assignment, so it is safe to store and to hash. Its redaction count is
		// added ONCE — on the first chunk of the section — so a size-split section does not re-count it.
		red := len(matches) + headingRed
		headingRed = 0
		ord := len(chunks)
		// Hash BOTH the heading and the body: the heading is a persisted, retrievable attribute, so a change
		// to it must change ContentHash (and thus CorpusHash) or the hash-guard would skip re-persisting a
		// corrected/altered heading and leave a stale one on disk forever (TG-86 review, blocker 2).
		sum := sha256.Sum256([]byte(heading + "\x00" + scrubbed))
		chunks = append(chunks, DocChunk{
			ExternalRef: fmt.Sprintf("%s#%d", relPath, ord),
			Path:        relPath,
			Heading:     heading,
			Content:     scrubbed,
			ContentHash: hex.EncodeToString(sum[:]),
			Redactions:  red,
		})
	}

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r") // normalize CRLF so a stray \r never rides into a chunk
		if h := headingLine(line); h != "" {
			flush() // a heading closes the previous section
			// SCRUB THE HEADING TOO. A markdown heading routinely names a host or an endpoint
			// ("## dc1x (10.0.0.5) recovery"); an unscrubbed heading is the same STONITH-class leak the
			// body scrub exists to stop (TG-86 review, blocker 1). Redactions are carried onto the section's
			// first chunk.
			hs, hm := scrub(h)
			heading = strings.TrimSpace(hs)
			headingRed = len(hm)
			continue
		}
		if bufLen+len(line)+1 > maxBytes && len(buf) > 0 {
			flush() // the section overflows the bound — split it, keeping the same heading
		}
		buf = append(buf, line)
		bufLen += len(line) + 1
	}
	flush()
	return chunks
}

// isDocFile reports whether name (a base filename) is estate documentation this loader ingests. Markdown only
// in slice 1 — the CLAUDE.md/README prose that carries the estate's design intent; broadening to raw IaC
// (*.tf, *.yaml) is a later slice with its own chunking rules.
func isDocFile(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".md")
}

// Load walks root for documentation files, chunks each through Chunk, and returns the assembled corpus with
// path-relative ExternalRefs. A read error on one file is fatal (a partial corpus that silently omits a doc is
// worse than a loud failure — a caller must know its grounding is incomplete). An absent or empty root yields
// an empty corpus and no error, so an unconfigured deployment ingests nothing rather than failing to boot.
func Load(root string, maxBytes int, scrub ScrubFunc) (Corpus, error) {
	var corpus Corpus
	if strings.TrimSpace(root) == "" {
		return corpus, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return corpus, nil
		}
		return corpus, fmt.Errorf("estatedoc: stat root %q: %w", root, err)
	}
	var chunks []DocChunk
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Dot-directories (.git, .claude — agent worktrees hold whole duplicate checkouts) are tooling
		// internals, not estate documentation: ingesting them bloats the corpus with near-duplicate chunks
		// that then dominate retrieval. The root itself is exempt so a hidden STAGING dir still ingests.
		if d.IsDir() && path != root && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if d.IsDir() || !isDocFile(d.Name()) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("estatedoc: read %q: %w", path, rerr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = filepath.Base(path) // never leak an absolute path into a ref
		}
		rel = filepath.ToSlash(rel)
		fileChunks := Chunk(rel, string(b), maxBytes, scrub)
		if len(fileChunks) == 0 {
			return nil
		}
		corpus.Files++
		for _, c := range fileChunks {
			corpus.Redactions += c.Redactions
		}
		chunks = append(chunks, fileChunks...)
		return nil
	})
	if err != nil {
		return Corpus{}, err
	}
	// Deterministic order regardless of the filesystem's walk order, so the corpus (and its hash) is stable
	// across machines and re-ingests.
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].ExternalRef < chunks[j].ExternalRef })
	corpus.Chunks = chunks
	return corpus, nil
}
