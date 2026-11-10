package wikicompile

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// HostSession is one triage session as the compiler needs it — a narrowed copy of db.PriorTriage so this
// package imports no database.
type HostSession struct {
	ExternalRef    string
	AlertRule      string
	Outcome        string
	OpClass        string
	Proposed       bool
	Mutated        bool
	ConfirmedClear bool
	Conclusion     string
	CreatedAt      time.Time
}

// HostEdge is one estate-graph relation touching the host.
type HostEdge struct {
	From, To, Rel string
	Confidence    float64
}

// HostPrecedent is one corpus entry recorded against the host.
type HostPrecedent struct {
	ExternalRef string
	AlertRule   string
	Summary     string
	Resolution  string
	ResolvedAt  time.Time
}

// HostFacts is everything known about ONE host, already fetched. `SessionsErr` is how a failed read
// arrives: it is carried rather than swallowed, because the difference between "this host had no
// incidents" and "we could not find out" is the whole point of this package.
type HostFacts struct {
	Host        string
	Sessions    []HostSession
	SessionsErr error
	Edges       []HostEdge
	Precedents  []HostPrecedent
	// EntityType and Status come from the approved world model; empty means the host is not in it, which
	// is a real and reportable state rather than a gap to paper over.
	EntityType string
	Status     string
	// SessionsTruncated is set when the per-host read hit its limit, so the page can say the list is a
	// window rather than the whole record.
	SessionsTruncated bool
}

// HostInputs is the compiler's entire world. No clock, no callbacks, no error-returning accessors: every
// value is already resolved, so CompileHosts is a pure function of its argument.
type HostInputs struct {
	Facts []HostFacts
}

const (
	// hostSlugPrefix namespaces compiled host pages away from corpus lessons and embedded runbooks, whose
	// slugs are authored at build time and could otherwise be shadowed by a hostname.
	hostSlugPrefix = "host-"
	// maxBodySessions bounds what one page RENDERS. The count above it is always the full number the
	// compiler saw, so a bounded list never misrepresents how much exists.
	maxBodySessions = 40
	// maxBodyEdges likewise bounds the dependency lists.
	maxBodyEdges = 30
)

// CompileHosts renders one article per host and returns the hosts it refused to render, with reasons.
//
// THE RULE THIS FUNCTION EXISTS TO ENFORCE: a section with nothing in it says so, in words, every time.
// The predecessor omits empty sections (compile_host_pages:540), so its degenerate page is four lines that
// read exactly like a page for a host where nothing ever happened — and this codebase's central pathology
// is precisely a surface rendering absence as health. Every section below emits a line in all cases.
func CompileHosts(in HostInputs) ([]Article, []Skip) {
	articles := make([]Article, 0, len(in.Facts))
	var skips []Skip

	facts := make([]HostFacts, len(in.Facts))
	copy(facts, in.Facts)
	sort.Slice(facts, func(i, j int) bool { return facts[i].Host < facts[j].Host })

	for _, f := range facts {
		slug, ok := SafeSlug(hostSlugPrefix, f.Host)
		if !ok {
			skips = append(skips, Skip{Host: f.Host, Reason: "hostname is not a safe page identifier — refused rather than rewritten, because two hosts rewritten to one slug would merge their incidents onto a single misleading page"})
			continue
		}
		// A FAILED READ IS NOT AN EMPTY HOST. Emitting a page here would publish "no triage session
		// recorded" over a query that errored — a confident claim about a host nobody could see. The page
		// is withheld and the reason is carried to the envelope instead.
		if f.SessionsErr != nil {
			skips = append(skips, Skip{Host: f.Host, Reason: "the triage read for this host failed, so no page was compiled: " + f.SessionsErr.Error()})
			continue
		}
		articles = append(articles, Article{
			Slug:  slug,
			Title: f.Host,
			Kind:  "article",
			Body:  hostBody(f),
			Meta:  hostMeta(f),
		})
	}
	sort.Slice(skips, func(i, j int) bool { return skips[i].Host < skips[j].Host })
	return articles, skips
}

// hostMeta carries the countable facts OUT of the body, so the body stays byte-stable across compiles and
// the surface can still show scale and provenance.
func hostMeta(f HostFacts) map[string]string {
	m := map[string]string{
		"host":       f.Host,
		"sessions":   fmt.Sprint(len(f.Sessions)),
		"edges":      fmt.Sprint(len(f.Edges)),
		"precedents": fmt.Sprint(len(f.Precedents)),
	}
	if f.EntityType != "" {
		m["entity_type"] = f.EntityType
	}
	if f.Status != "" {
		m["status"] = f.Status
	}
	return m
}

func hostBody(f HostFacts) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	w("# " + f.Host)
	w("")
	w("Compiled from TG's own audit spine. Every line below traces to a recorded row; nothing here is authored.")
	w("")

	// ── Identity ───────────────────────────────────────────────────────────────
	w("## Identity")
	w("")
	switch {
	case f.EntityType != "" && f.Status != "":
		w(fmt.Sprintf("In the approved world model as **%s**, status **%s**.", f.EntityType, f.Status))
	case f.EntityType != "":
		w(fmt.Sprintf("In the approved world model as **%s**; no status recorded.", f.EntityType))
	default:
		w("Not in the approved world model. TG has seen this host in incidents but has never had it ratified as an entity, so nothing here is governed by the manifest.")
	}
	w("")

	// ── Incidents ──────────────────────────────────────────────────────────────
	w("## Incidents recorded here")
	w("")
	if len(f.Sessions) == 0 {
		w("No triage session has been recorded against this host. That is a statement about TG's spine, not about the host: it means nothing reached triage, not that nothing happened.")
	} else {
		healed, proposed, stood := 0, 0, 0
		for _, s := range f.Sessions {
			switch {
			case s.Mutated:
				healed++
			case s.Proposed:
				proposed++
			default:
				stood++
			}
		}
		w(fmt.Sprintf("**%d** recorded — %d acted on, %d proposed but not carried out, %d stood down without a proposal.",
			len(f.Sessions), healed, proposed, stood))
		if f.SessionsTruncated {
			w("")
			w(fmt.Sprintf("This page lists the newest %d. The count above is the number this compile read, which is itself bounded — treat both as a floor.", maxBodySessions))
		}
		w("")
		w("| when | alert rule | outcome | op-class | result |")
		w("|---|---|---|---|---|")
		shown := f.Sessions
		if len(shown) > maxBodySessions {
			shown = shown[:maxBodySessions]
		}
		for _, s := range shown {
			result := "recorded"
			switch {
			case s.Mutated && s.ConfirmedClear:
				result = "acted · confirmed clear"
			case s.Mutated:
				result = "acted · clear not confirmed"
			case s.Proposed:
				result = "proposed, not carried out"
			}
			w(fmt.Sprintf("| %s | %s | %s | %s | %s |",
				s.CreatedAt.UTC().Format("2006-01-02 15:04"),
				mdCell(s.AlertRule), mdCell(s.Outcome), mdCell(s.OpClass), result))
		}
	}
	w("")

	// ── Dependencies ───────────────────────────────────────────────────────────
	w("## Dependencies")
	w("")
	out, in := splitEdges(f.Host, f.Edges)
	writeEdgeList(w, out, "This host depends on", "No outbound dependency has been discovered for this host. The estate graph records none — which is not proof it has none.")
	w("")
	w("## Dependents")
	w("")
	writeEdgeList(w, in, "These depend on this host", "Nothing is recorded as depending on this host. Blast-radius reasoning about it therefore rests on no evidence, rather than on evidence of isolation.")
	w("")

	// ── Precedent ──────────────────────────────────────────────────────────────
	w("## Precedent in the knowledge corpus")
	w("")
	if len(f.Precedents) == 0 {
		w("No corpus entry is recorded against this host, so retrieval has no host-specific precedent to cite for it.")
	} else {
		w(fmt.Sprintf("**%d** entry/entries the retriever can cite for this host:", len(f.Precedents)))
		w("")
		for _, p := range f.Precedents {
			age := "date unknown"
			if !p.ResolvedAt.IsZero() {
				age = p.ResolvedAt.UTC().Format("2006-01-02")
			}
			w(fmt.Sprintf("- **%s** (%s) — %s", mdCell(p.ExternalRef), age, mdCell(firstNonEmpty(p.Summary, p.AlertRule, "no summary recorded"))))
			if r := strings.TrimSpace(p.Resolution); r != "" {
				w("  - resolution: " + mdCell(r))
			}
		}
	}
	return b.String()
}

func writeEdgeList(w func(string), edges []HostEdge, heading, empty string) {
	if len(edges) == 0 {
		w(empty)
		return
	}
	w(fmt.Sprintf("%s (**%d**):", heading, len(edges)))
	w("")
	shown := edges
	if len(shown) > maxBodyEdges {
		shown = shown[:maxBodyEdges]
	}
	for _, e := range shown {
		other := e.To
		if other == "" {
			other = e.From
		}
		w(fmt.Sprintf("- `%s` — %s (confidence %.2f)", mdCell(other), mdCell(e.Rel), e.Confidence))
	}
	if len(edges) > maxBodyEdges {
		w("")
		w(fmt.Sprintf("Showing %d of %d.", maxBodyEdges, len(edges)))
	}
}

// splitEdges partitions the host's edges into outbound and inbound, each deterministically ordered.
func splitEdges(host string, edges []HostEdge) (out, in []HostEdge) {
	for _, e := range edges {
		switch {
		case e.From == host:
			out = append(out, HostEdge{From: e.From, To: e.To, Rel: e.Rel, Confidence: e.Confidence})
		case e.To == host:
			in = append(in, HostEdge{From: e.From, To: e.To, Rel: e.Rel, Confidence: e.Confidence})
		}
	}
	byKey := func(s []HostEdge) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].To != s[j].To {
				return s[i].To < s[j].To
			}
			if s[i].From != s[j].From {
				return s[i].From < s[j].From
			}
			return s[i].Rel < s[j].Rel
		})
	}
	byKey(out)
	byKey(in)
	return out, in
}

// mdCell neutralizes text that arrived from an alert payload or a model conclusion before it lands in a
// markdown table. A pipe would silently restructure the row; a newline would break the table entirely.
// Both are plausible in a free-text `conclusion`, so neither is trusted.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
