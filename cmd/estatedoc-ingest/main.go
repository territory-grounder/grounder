// Command estatedoc-ingest runs the TG-86 slice-1 estate-doc ingest: walk a documentation root, chunk and
// SCRUB every document through core/screen, and write the secret-free corpus to an on-box file — reporting
// coverage (files, chunks, redactions). It is the runnable caller of core/estatedoc, the deterministic
// grounding half of TG-86 ("the estate's own docs have never been read"). The agent-retrieval channel that
// would surface this corpus in the agent's reasoning context is the eval-gated slice 2, deliberately separate.
//
// With -redact-patterns (comma-separated pattern files, denylist format), estate IDENTIFIERS are redacted
// too — the whole token containing any pattern match becomes estatedoc.EstateIDMarker — so the persisted
// corpus passes the producer's abort-on-survivor denylist floor by construction (TG-86 follow-up: the
// site-code/domain/IP vocabulary the live-hostname redactor does not cover). The vocabulary is runtime
// input, never compiled in.
//
// The corpus is on-box data derived from the operator's private docs and is NEVER committed (the public-mirror
// rule), which is why this is an ingest TOOL writing a local file, not a repo artifact.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/territory-grounder/grounder/core/estatedoc"
	"github.com/territory-grounder/grounder/core/screen"
)

func main() {
	root := flag.String("root", os.Getenv("TG_ESTATE_DOCS_DIR"), "root directory of estate documentation (recursed; *.md)")
	out := flag.String("out", os.Getenv("TG_ESTATE_DOC_CORPUS"), "path to write the scrubbed corpus JSON")
	maxBytes := flag.Int("max-chunk-bytes", estatedoc.DefaultMaxChunkBytes, "maximum bytes per chunk")
	redactPatterns := flag.String("redact-patterns", os.Getenv("TG_ESTATE_DOC_REDACT_PATTERNS"),
		"comma-separated pattern files (denylist format); matching tokens are redacted from every chunk")
	flag.Parse()

	if *root == "" || *out == "" {
		log.Fatal("estatedoc-ingest: -root and -out (or TG_ESTATE_DOCS_DIR / TG_ESTATE_DOC_CORPUS) are required")
	}
	scrub := estatedoc.ScrubFunc(screen.Scrub)
	var pathMatch func(string) bool
	if *redactPatterns != "" {
		files := strings.Split(*redactPatterns, ",")
		patterns, err := estatedoc.LoadRedactPatterns(files...)
		if err != nil {
			log.Fatalf("estatedoc-ingest: %v", err)
		}
		redact, err := estatedoc.NewPatternRedactor(patterns)
		if err != nil {
			log.Fatalf("estatedoc-ingest: %v", err)
		}
		if pathMatch, err = estatedoc.NewPatternMatcher(patterns); err != nil {
			log.Fatalf("estatedoc-ingest: %v", err)
		}
		scrub = estatedoc.ComposeScrub(scrub, redact)
		fmt.Printf("estatedoc-ingest: identifier redaction armed — %d pattern(s) from %d file(s)\n",
			len(patterns), len(files))
	}
	corpus, err := estatedoc.Load(*root, *maxBytes, scrub)
	if err != nil {
		log.Fatalf("estatedoc-ingest: load %q: %v", *root, err)
	}
	if pathMatch != nil {
		// Content scrubbing cannot reach the refs: a corpus ingested from "<site>/…" carries the site code
		// in every Path/ExternalRef. Alias matching segments (stable, distinct) so the denylist floor holds.
		if n := estatedoc.RedactCorpusPaths(&corpus, pathMatch); n > 0 {
			corpus.Redactions += n
			fmt.Printf("estatedoc-ingest: %d path segment(s) aliased\n", n)
		}
	}
	wrote, err := estatedoc.WriteCorpus(*out, corpus)
	if err != nil {
		log.Fatalf("estatedoc-ingest: write %q: %v", *out, err)
	}
	status := "unchanged (hash-guard skipped the rewrite)"
	if wrote {
		status = "written"
	}
	hash := estatedoc.CorpusHash(corpus)
	fmt.Printf("estatedoc-ingest: %s — %d files, %d chunks, %d redactions -> %s (corpus %s)\n",
		status, corpus.Files, len(corpus.Chunks), corpus.Redactions, *out, hash[:12])
}
