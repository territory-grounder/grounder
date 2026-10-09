package estatedoc

import (
	"regexp"
	"sort"
	"strings"
)

// EstateIDMarker is the stable, model-safe substitution for a redacted estate identifier. It mirrors the
// shape of core/screen.RedactMarker and NEVER carries the redacted value.
const EstateIDMarker = "[REDACTED:estate-id]"

// NewIdentifierRedactor compiles a redactor over the given estate identifiers — the LIVE host names from the
// estate graph (estate.Graph.FreshHostNames). Grounding the pass in the runtime graph, not committed regex
// literals, is deliberate and load-bearing: an estate hostname compiled into this source would itself be a
// STONITH-class leak that scripts/lint-forbidden.sh fails the build on. TG-486 (owner ruling: estatedoc
// redacts identifiers) — the doc corpus that a future armed <estate> grounding fold would surface must be
// identifier-free by construction.
//
// The returned func replaces each identifier occurrence — matched CASE-INSENSITIVELY on WORD BOUNDARIES so a
// name is never redacted as a substring of a larger token — with EstateIDMarker, and returns (redacted,
// count). It is PROBE-FIRST: text containing no identifier is returned byte-identical (no allocation). An
// empty identifier set yields a no-op passthrough.
//
// KNOWN LIMIT: a very short/generic host name (e.g. "web") can still word-boundary-match an unrelated
// standalone word and over-redact; this estate's names are long and distinctive, so the risk is low, but a
// future refinement could impose a min-length floor or an allowlist. IPs/MACs and the estate DOMAIN/site-code
// vocabulary are NOT covered here (FreshHostNames is host names only) — tracked as follow-ups on TG-86.
func NewIdentifierRedactor(identifiers []string) func(string) (string, int) {
	ids := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		if strings.TrimSpace(id) != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return func(s string) (string, int) { return s, 0 }
	}
	// Longest-first so a longer name is preferred over a shorter one it contains (regex alternation is
	// leftmost-first, not longest-match).
	sort.Slice(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) })
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = regexp.QuoteMeta(id)
	}
	re := regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
	return func(text string) (string, int) {
		if text == "" || !re.MatchString(text) {
			return text, 0
		}
		n := 0
		out := re.ReplaceAllStringFunc(text, func(string) string {
			n++
			return EstateIDMarker
		})
		return out, n
	}
}
