// Package wikicompile turns what TG has RECORDED into what an operator can READ.
//
// THE MECHANISM, AND WHERE IT COMES FROM. The predecessor compiles a browsable markdown wiki from its
// structured knowledge sources (`scripts/wiki-compile.py`, 1,386 lines, 12 article families over 7
// sources). Its docstring claims the articles are "compiled by an LLM"; they are not — every article is
// string concatenation over filtered source lists, and no model is invoked anywhere in the file. That is
// the property worth porting. A deterministic compiler can be tested by an oracle, produces byte-identical
// output for identical input, cannot hallucinate a dependency that does not exist, and costs nothing to
// re-run. An LLM in this path would forfeit all four.
//
// WHAT IS DELIBERATELY NOT PORTED. Reading the predecessor's implementation closely turned up defects that
// a faithful port would have inherited:
//
//   - Its incremental model is dead code. `compute_source_checksums` builds a per-article dependency map
//     and `save_source_map` persists it, but `load_source_map` HAS NO CALLER — so a single changed source
//     key rewrites all 86 articles, and a source that is DELETED is invisible forever. This package writes
//     one envelope atomically instead; a full rewrite of a single small file needs no dirty-tracking, and
//     an article whose source disappeared simply stops being emitted.
//   - Every article carries a compile timestamp in its BODY (`NOW` at wiki-compile.py:50), so all 86 files
//     churn on every run even when nothing changed. Here the timestamp lives on the ENVELOPE, never in an
//     article body — which is what makes byte-identical output testable at all.
//   - Raw hostnames are joined straight into file paths (`wiki-compile.py:547`). A literal `*` hostname in
//     the source data produced a real 203,900-byte file at `wiki/hosts/*.md`. See SafeSlug.
//   - Several compilers assert facts they did not compute: the topology articles build a source set, print
//     its count and never render it (`:619,:642,:665`); the Grafana table reports six of thirteen
//     dashboards with `title == "*.json"` and `panel_count == 0`, i.e. six silent parse failures printed
//     as measurements. This package's rule is the inverse and is enforced by oracle: a section that has
//     nothing to say SAYS SO, and nothing is printed that was not derived from a row actually read.
//
// PURITY. This file and hosts.go contain no clock, no filesystem, no database and no network. Everything
// they need arrives as an argument. That is not tidiness for its own sake: it is what lets the oracle
// assert determinism directly, and it keeps the compile path structurally incapable of reaching an
// actuator (this lane reads the spine and writes a file — it must never traverse the mode chokepoint).
package wikicompile

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the envelope's format. A reader that does not recognise it must refuse the file rather
// than guess at its shape — the same rule the corpus follows.
const SchemaVersion = 1

// Article is one compiled page. It is DERIVED, never authored: every line traces to a row that was read,
// and the compiler that produced it is the only writer.
type Article struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Kind distinguishes a compiled article from a corpus lesson or an embedded runbook on the serving
	// surface. Compiled articles are the only one of the three derived from live data.
	Kind string `json:"kind"`
	Body string `json:"body"`
	// Meta carries facts ABOUT the article that must not live in its body — most importantly staleness.
	// A timestamp in the body would make every article differ on every compile (the predecessor's bug);
	// a timestamp in Meta lets the surface show age while the body stays byte-stable.
	Meta map[string]string `json:"meta,omitempty"`
}

// Skip records a page that was NOT produced and why. It is a first-class result, not a log line: a host
// whose read failed must be visibly absent-with-a-reason, never silently missing and never — the dangerous
// case — rendered as a page with an empty "no incidents" section, which reads as a quiet host.
type Skip struct {
	// Host is the identifier that was refused. Despite the name it is not always a HOST — rule and
	// op-class compilers refuse by their own identifier — which is exactly how the coverage page came to
	// report "3 host(s) were refused" for three refused ALERT RULES. Kind says which.
	Host string `json:"host"`
	// Kind is what the refused identifier names: "host", "rule" or "opclass". Empty is read as "host" so
	// a previously-written envelope still deserialises, but every producer in the tree sets it.
	Kind   string `json:"kind,omitempty"`
	Reason string `json:"reason"`
}

// SkipKind normalises Kind, defaulting an empty value to "host" (the pre-Kind envelope shape).
func (s Skip) SkipKind() string {
	if strings.TrimSpace(s.Kind) == "" {
		return "host"
	}
	return s.Kind
}

// Envelope is the compiled artifact as it is written and read. Sources carries the denominators the
// compile actually saw, so the surface can state what it is a view OF rather than implying completeness.
type Envelope struct {
	SchemaVersion int               `json:"schema_version"`
	CompiledAt    time.Time         `json:"compiled_at"`
	Sources       map[string]int    `json:"sources,omitempty"`
	Skipped       []Skip            `json:"skipped,omitempty"`
	Articles      []Article         `json:"articles"`
	Meta          map[string]string `json:"meta,omitempty"`
}

// slugRe is the shape every emitted slug must have. It is an ALLOW-list, not an escape: the slug becomes a
// URL path element on /v1/wiki/{slug}, and the only safe way to handle a hostname that arrived from a
// database is to refuse the ones that are not obviously inert.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// SafeSlug derives a slug from an untrusted identifier, reporting whether it is usable.
//
// The predecessor has no equivalent and joins raw database hostnames into filesystem paths
// (wiki-compile.py:547); a literal `*` hostname in its source data produced a 203,900-byte file at
// `wiki/hosts/*.md`. TG's identifiers arrive from session_triage.host, which is populated from inbound
// alert payloads — external input by any reading — so this refuses rather than sanitizes. Silently
// rewriting `a/b` into `a-b` would let two different hosts collide onto one page, which is worse than
// dropping one: a page that merges two machines' incidents is actively misleading.
func SafeSlug(prefix, raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if id == "" || !slugRe.MatchString(id) {
		return "", false
	}
	// Defence in depth: the regex already excludes these, but a future edit to slugRe must not silently
	// re-open a traversal. This check is cheap and states the invariant at the point it matters.
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return "", false
	}
	return prefix + id, true
}

// ParseArticles reads a compiled envelope. Discipline mirrors knowledge.ParseCorpus deliberately: unknown
// fields are an error (a writer and reader that disagree about the schema must fail loudly, not silently
// drop data), and an article with no slug is rejected — a page with no identity cannot be linked to, so it
// cannot be served.
func ParseArticles(r io.Reader) (Envelope, error) {
	var env Envelope
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return Envelope{}, fmt.Errorf("wikicompile: malformed articles JSON: %w", err)
	}
	if env.SchemaVersion != SchemaVersion {
		return Envelope{}, fmt.Errorf("wikicompile: articles schema_version %d, this build reads %d",
			env.SchemaVersion, SchemaVersion)
	}
	seen := make(map[string]struct{}, len(env.Articles))
	for i, a := range env.Articles {
		if strings.TrimSpace(a.Slug) == "" {
			return Envelope{}, fmt.Errorf("wikicompile: article %d has no slug (a page with no identity cannot be served)", i)
		}
		if _, dup := seen[a.Slug]; dup {
			return Envelope{}, fmt.Errorf("wikicompile: duplicate article slug %q — one slug must resolve to one page", a.Slug)
		}
		seen[a.Slug] = struct{}{}
	}
	if env.Articles == nil {
		env.Articles = []Article{}
	}
	return env, nil
}

// WriteArticles serializes an envelope deterministically: articles sorted by slug, stable indentation, and
// no value anywhere that varies between two compiles of the same input except CompiledAt on the envelope
// itself. Two runs over identical inputs must differ in exactly one field, which is what makes "did
// anything actually change?" answerable.
func WriteArticles(w io.Writer, env Envelope) error {
	if env.SchemaVersion == 0 {
		env.SchemaVersion = SchemaVersion
	}
	if env.Articles == nil {
		env.Articles = []Article{}
	}
	sorted := make([]Article, len(env.Articles))
	copy(sorted, env.Articles)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })
	env.Articles = sorted
	if len(env.Skipped) > 1 {
		sk := make([]Skip, len(env.Skipped))
		copy(sk, env.Skipped)
		sort.Slice(sk, func(i, j int) bool { return sk[i].Host < sk[j].Host })
		env.Skipped = sk
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("wikicompile: encode articles: %w", err)
	}
	return nil
}
