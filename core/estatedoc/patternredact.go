package estatedoc

// Producer-side estate-identifier redaction (TG-86 follow-up: site-codes, domains, IPs/MACs — the
// vocabulary NewIdentifierRedactor's live-hostname set does not cover). The corpus producer
// (cmd/estatedoc-ingest via scripts/estate-docs-corpus.sh) composes this over core/screen.Scrub so the
// PERSISTED corpus is identifier-free by construction and passes the script's abort-on-survivor denylist
// scan HONESTLY — the STONITH floor itself is untouched. The redaction vocabulary is supplied at RUNTIME
// (the same github-sync/denylist.txt the floor greps, plus IP/MAC shape files): compiling an estate
// identifier into this source would itself be the STONITH-class leak scripts/lint-forbidden.sh exists to
// stop, which is why this file holds MECHANISM only, never vocabulary.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/core/screen"
)

// CategoryEstateIdentifier labels the synthetic screen.Match ComposeScrub appends per pattern-redaction,
// so a corpus chunk's Redactions tally counts identifier redactions alongside secret redactions.
const CategoryEstateIdentifier screen.Category = "estate-identifier"

// redactTokenChars is the character class a matched pattern is EXPANDED across: the whole containing
// token is redacted, not just the matched substring. "site01app02.example.lan" must not degrade to
// "[REDACTED:estate-id]app02.example.lan" — the residue would still identify the host. '/' is included
// so a denied path prefix swallows the full path it anchors.
const redactTokenChars = `[A-Za-z0-9._/-]`

// LoadRedactPatterns reads one or more pattern files (the denylist format: one extended regex per line,
// '#' comments and blank lines skipped) and returns the combined pattern list. Fail-closed like the
// consumers of the same format: an unreadable file is an error, and zero total patterns is an error —
// half a floor is no floor.
func LoadRedactPatterns(paths ...string) ([]string, error) {
	var patterns []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("estatedoc: read redact-patterns %q: %w", p, err)
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, line)
		}
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("estatedoc: redact-pattern files %v yielded zero patterns — refusing an empty floor", paths)
	}
	return patterns, nil
}

// NewPatternRedactor compiles a redactor over pattern (extended-regex) vocabulary. Each match is expanded
// to its whole containing token (redactTokenChars on both sides) and replaced with EstateIDMarker —
// matching case-insensitively, so the redaction is a SUPERSET of a case-sensitive grep of the same
// patterns (the property the corpus producer's denylist floor then verifies). A pattern that does not
// compile is an error, not a skip: silently dropping one pattern would un-floor exactly that identifier.
func NewPatternRedactor(patterns []string) (func(string) (string, int), error) {
	if len(patterns) == 0 {
		return func(s string) (string, int) { return s, 0 }, nil
	}
	alts := make([]string, len(patterns))
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("estatedoc: redact pattern %q does not compile: %w", p, err)
		}
		alts[i] = redactTokenChars + `*(?:` + p + `)` + redactTokenChars + `*`
	}
	re, err := regexp.Compile(`(?i)` + `(?:` + strings.Join(alts, "|") + `)`)
	if err != nil {
		return nil, fmt.Errorf("estatedoc: combined redact pattern does not compile: %w", err)
	}
	return func(text string) (string, int) {
		if text == "" || !re.MatchString(text) {
			return text, 0 // probe-first: identifier-free text passes through byte-identical
		}
		n := 0
		out := re.ReplaceAllStringFunc(text, func(string) string {
			n++
			return EstateIDMarker
		})
		return out, n
	}, nil
}

// NewPatternMatcher compiles the same case-insensitive combined vocabulary as NewPatternRedactor but as a
// bare matcher (no token expansion, no replacement) — the probe RedactCorpusPaths uses per path segment.
func NewPatternMatcher(patterns []string) (func(string) bool, error) {
	if len(patterns) == 0 {
		return func(string) bool { return false }, nil
	}
	for _, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("estatedoc: redact pattern %q does not compile: %w", p, err)
		}
	}
	re, err := regexp.Compile(`(?i)(?:` + strings.Join(patterns, "|") + `)`)
	if err != nil {
		return nil, fmt.Errorf("estatedoc: combined match pattern does not compile: %w", err)
	}
	return re.MatchString, nil
}

// aliasSegment is the stable, denylist-clean pseudonym for one redacted path segment: "seg-" + the first
// 10 hex of the segment's SHA-256. Deterministic (the same segment aliases identically on every re-ingest,
// keeping ExternalRefs stable) and DISTINCT per segment (unlike the flat EstateIDMarker, which would
// collide "siteA/runbook.md" with "siteB/runbook.md" into one ref). Hex output cannot re-match the
// identifier vocabulary the denylist floor greps.
func aliasSegment(seg string) string {
	sum := sha256.Sum256([]byte(seg))
	return "seg-" + hex.EncodeToString(sum[:])[:10]
}

// RedactCorpusPaths rewrites every chunk Path (and its ExternalRef, preserving the "#ordinal" suffix)
// whose path SEGMENTS match the identifier vocabulary — the leak surface content scrubbing cannot reach:
// a corpus ingested from "<site>/production/CLAUDE.md" carries the site code in every ref. Segment-wise,
// so non-identifier segments stay readable and refs stay distinct. Returns the number of segments
// aliased, which callers add to the corpus Redactions tally. Chunks are re-sorted afterwards so corpus
// order (and CorpusHash determinism) keys on the REWRITTEN refs.
func RedactCorpusPaths(c *Corpus, match func(string) bool) int {
	if c == nil || match == nil {
		return 0
	}
	aliased := 0
	rewritten := make(map[string]string, 8) // original path -> rewritten, so a file's chunks agree
	for i := range c.Chunks {
		ch := &c.Chunks[i]
		newPath, ok := rewritten[ch.Path]
		if !ok {
			segs := strings.Split(ch.Path, "/")
			changed := false
			for j, seg := range segs {
				if seg != "" && match(seg) {
					segs[j] = aliasSegment(seg)
					aliased++
					changed = true
				}
			}
			newPath = ch.Path
			if changed {
				newPath = strings.Join(segs, "/")
			}
			rewritten[ch.Path] = newPath
		}
		if newPath == ch.Path {
			continue
		}
		if idx := strings.LastIndex(ch.ExternalRef, "#"); idx >= 0 {
			ch.ExternalRef = newPath + ch.ExternalRef[idx:]
		} else {
			ch.ExternalRef = newPath
		}
		ch.Path = newPath
	}
	if aliased > 0 {
		sort.Slice(c.Chunks, func(i, j int) bool { return c.Chunks[i].ExternalRef < c.Chunks[j].ExternalRef })
	}
	return aliased
}

// ComposeScrub returns a ScrubFunc that applies scrub (the secret/injection screen) FIRST, then the
// pattern redactor, reporting the redactor's count as synthetic CategoryEstateIdentifier matches so
// downstream Redactions tallies (chunk, corpus, coverage gauges) count both passes. Order matters:
// screen's own markers must not be considered identifier tokens, and redacting first could split a
// secret across a marker boundary and hide it from screen.
func ComposeScrub(scrub ScrubFunc, redact func(string) (string, int)) ScrubFunc {
	if redact == nil {
		return scrub
	}
	return func(text string) (string, []screen.Match) {
		scrubbed, matches := scrub(text)
		redacted, n := redact(scrubbed)
		for range n {
			matches = append(matches, screen.Match{Category: CategoryEstateIdentifier, Pattern: "estate-identifier"})
		}
		return redacted, matches
	}
}
