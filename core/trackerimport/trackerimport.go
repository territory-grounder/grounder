// Package trackerimport is the COMPOUNDING half of tracker history (TG-244).
//
// THE GAP IT CLOSES. get-tracker-history (MR !845) is a read-only agent tool over adapters/tracker.History:
// the agent can FETCH prior incidents on demand from the estate's own tracker (ServiceNow / YouTrack / Jira),
// but nothing persists them, so recall does not compound. Every session pays to re-fetch and re-read the same
// tickets, and the human resolutions the site's engineers already wrote for these exact faults never reach
// the retriever's ranking, the wiki, or the next session. This package is the WRITE half: it distils that
// history into ranked core/knowledge corpus rows, so the knowledge is RETRIEVED, not merely fetchable.
//
// THE CONSTRAINT THAT KEEPS IT HONEST. An imported ticket resolution is a CLAIM BY AN ENGINEER, not an
// outcome TG produced and verified. Every distilled row is stamped knowledge.ProvenanceTrackerImport — a
// distinct provenance that ranks between operator and inherited and NEVER renders as a TG-verified
// resolution (core/knowledge/retriever.go). TG's earned ladder is built on confirmed-clear outcomes from its
// own spine; an imported row can never launder into that, and by construction the corpus does not feed
// opclass_candidate accrual at all.
//
// UNTRUSTED DATA (INV-08, MECH-409). Tracker text is written by humans and, at some sites, by another
// autonomous system, and site tickets carry credentials far more often than TG's own corpus does. Every
// tracker-sourced field passes screen.Scrub before it can become a corpus row, and — deliberately stricter
// than tools/seed-knowledge, which redacts-and-keeps a curated predecessor DB — a row that trips the screen
// at all (a neutralized injection OR a redacted secret) is DROPPED rather than imported in scrubbed form. For
// compounding precedent drawn unattended from a credential-heavy source, drop-on-flag is the safe direction,
// and it guarantees no un-scrubbed text is ever written. The (host, rule) stamped on each row is TG's own
// trusted vocabulary (the search key), never tracker-controlled, so it is not screened.
//
// SAME-SLUG BY CONSTRUCTION. The query set is derived from the corpus's OWN (host, rule) shapes — the
// production vocabulary knowledge.Count and the retriever match on — and each returned incident is stamped
// with the shape it was searched under. This is the tools/seed-knowledge discipline: an imported row whose
// (host, rule) did not match live queries would populate the corpus and retrieve to nobody.
package trackerimport

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/screen"
)

// summaryMaxRunes / resolutionMaxRunes bound each distilled field, rune-safe (never mid-codepoint). Screening
// runs BEFORE the clip, so a truncation can never bisect a secret into a partial the redactor no longer
// matches — the same ordering tools/seed-knowledge documents.
const (
	summaryMaxRunes    = 500
	resolutionMaxRunes = 500
)

// History is the read side this lane consumes. adapters/tracker.MultiHistory satisfies it; a fake satisfies
// it in tests. It is deliberately the narrow capability (one method, read-only) rather than the full Tracker
// contract, so this write lane cannot mutate a ticket even by mistake.
type History interface {
	SearchIncidents(ctx context.Context, host, rule string, limit int) ([]tracker.HistoricalIncident, error)
}

// Query is one (host, rule) shape to enrich from tracker history — the search key, stamped onto every row the
// search returns so an imported precedent carries the production vocabulary the retriever matches on.
type Query struct {
	Host      string
	AlertRule string
	Site      string
}

// Result is one pass's accounting — the numbers the composition-root seam reports as its runtime yield.
type Result struct {
	QueriesRun int      // distinct (host, rule) shapes searched
	Offered    int      // historical incidents the searches returned, across all shapes (the seam's OFFERED)
	Kept       int      // distilled rows that survived screening
	Dropped    int      // distilled rows dropped by screen.Scrub (never written un-scrubbed)
	Produced   int      // precedents actually merged — new-or-updated refs (the seam's PRODUCED)
	Changed    bool     // did the corpus content change? The caller writes the corpus file iff this is true.
	Failures   []string // per-shape read errors; a partial outage records and continues, it does not abort
}

// Run executes one import pass over an in-memory corpus and returns the merged corpus plus its accounting. It
// is pure — no filesystem, no clock, no network beyond the injected History — so the composition root owns
// the read-file / write-file / reload / mutex, and this stays fully unit-testable with a fake History.
//
// FAILURE SEMANTICS mirror MultiHistory's own asymmetry. A per-shape read error is recorded and skipped; the
// pass still imports the shapes that answered. But the corpus is only ever GROWN: MergeCorpus adds by union
// under downhill protection, so an imported claim can neither remove a row nor displace a more-verified one.
// If every read fails (or nothing new survives screening/merge), the merged corpus is byte-identical to the
// input and Result.Changed is false — the caller then writes nothing, so a failed tracker read leaves the
// corpus exactly as it was.
func Run(ctx context.Context, existing []knowledge.Incident, h History, limit int) ([]knowledge.Incident, Result) {
	var res Result
	queries := corpusQueries(existing)
	res.QueriesRun = len(queries)

	var distilled []knowledge.Incident
	for _, q := range queries {
		incidents, err := h.SearchIncidents(ctx, q.Host, q.AlertRule, limit)
		if err != nil {
			// An unreadable tracker is an outage, never "this estate has no history": record it and move on,
			// so one dead source cannot erase the shapes that answered.
			res.Failures = append(res.Failures, q.Host+"/"+q.AlertRule+": "+err.Error())
			continue
		}
		res.Offered += len(incidents)
		kept, dropped := distil(q, incidents)
		res.Kept += len(kept)
		res.Dropped += dropped
		distilled = append(distilled, kept...)
	}

	merged := knowledge.MergeCorpus(existing, distilled)
	res.Produced = countChanged(existing, merged)
	res.Changed = res.Produced > 0
	return merged, res
}

// corpusQueries derives the distinct (host, rule) shapes to search from the corpus itself — the same-slug
// discipline. A blank host or rule has no shape to search; the fleet-wide "*" host is an operator-authored
// advisory row, not a real device, so it is skipped rather than searched as a literal hostname. The result is
// sorted so a pass is deterministic (reproducible imports, stable logs).
func corpusQueries(corpus []knowledge.Incident) []Query {
	seen := make(map[string]Query)
	for _, inc := range corpus {
		host := strings.TrimSpace(inc.Host)
		rule := strings.TrimSpace(inc.AlertRule)
		if host == "" || rule == "" || host == "*" {
			continue
		}
		key := strings.ToLower(host) + "\x00" + strings.ToLower(rule)
		if _, ok := seen[key]; !ok {
			seen[key] = Query{Host: host, AlertRule: rule, Site: strings.TrimSpace(inc.Site)}
		}
	}
	out := make([]Query, 0, len(seen))
	for _, q := range seen {
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].AlertRule < out[j].AlertRule
	})
	return out
}

// distil maps the incidents one search returned into screened, provenance-tagged corpus rows, counting the
// rows the screen dropped.
func distil(q Query, incidents []tracker.HistoricalIncident) (kept []knowledge.Incident, dropped int) {
	for _, hi := range incidents {
		row, ok := distilOne(q, hi)
		if !ok {
			dropped++
			continue
		}
		kept = append(kept, row)
	}
	return kept, dropped
}

// distilOne turns one historical incident, searched under query q, into a corpus row — or reports that it was
// dropped. A row is dropped when it has no identity, nothing to teach, or any tracker-sourced field trips the
// screen.
func distilOne(q Query, hi tracker.HistoricalIncident) (knowledge.Incident, bool) {
	// external_ref = the tracker id, source-qualified when several vendors are merged (ids live in per-vendor
	// namespaces; an unqualified "INC0010023" beside a "IFRNLLEI01PRD-2198" names nothing a reader can look
	// up). A row with no id is not importable — ParseCorpus rejects a corpus entry with no external_ref, and
	// a precedent with no identity cannot be cited.
	ref := strings.TrimSpace(hi.ID)
	if src := strings.TrimSpace(hi.Source); src != "" && ref != "" {
		ref = src + ":" + ref
	}
	if ref == "" {
		return knowledge.Incident{}, false
	}

	summary := clip(strings.TrimSpace(hi.Summary), summaryMaxRunes)
	// The resolution at most sites is written in a comment, not a field (adapters/tracker.HistoricalIncident
	// documents this) — the LAST substantive comment is the likeliest statement of what fixed it.
	resolution := clip(resolutionFrom(hi.Comments), resolutionMaxRunes)
	if summary == "" && resolution == "" {
		return knowledge.Incident{}, false // nothing to teach
	}

	// SCREEN, then DROP on any trip. Scrub each tracker-sourced field; a non-empty match set means the text
	// carried a neutralized injection or a redacted secret, and such a row is dropped whole rather than
	// imported in scrubbed form. This guarantees an un-scrubbed row is never written and keeps the
	// credential-heavy failure mode (site tickets) out of compounding precedent entirely.
	if screenTrips(ref) || screenTrips(summary) || screenTrips(resolution) {
		return knowledge.Incident{}, false
	}

	return knowledge.Incident{
		ExternalRef: ref,
		Host:        q.Host,      // TG's own trusted vocabulary — the search key, not tracker text
		AlertRule:   q.AlertRule, // ditto
		Site:        q.Site,
		Summary:     summary,
		Resolution:  resolution,
		// ResolvedAt is the incident's FILING date — the only timestamp the History capability exposes. It is
		// the recency anchor; a zero Filed stays zero (unknown, earns no recency credit, and is never
		// invented). A separate resolution timestamp would be more precise, but the capability does not carry
		// one, and the filing date is honest and monotonically related.
		ResolvedAt: hi.Filed,
		Source:     knowledge.ProvenanceTrackerImport,
	}, true
}

// screenTrips reports whether screen.Scrub found anything to neutralize or redact in s. Empty text trips
// nothing.
func screenTrips(s string) bool {
	if s == "" {
		return false
	}
	_, ms := screen.Scrub(s)
	return len(ms) > 0
}

// resolutionFrom returns the last non-empty comment — the likeliest resolution statement in a tracker thread.
func resolutionFrom(comments []string) string {
	for i := len(comments) - 1; i >= 0; i-- {
		if c := strings.TrimSpace(comments[i]); c != "" {
			return c
		}
	}
	return ""
}

// clip truncates to at most n runes (rune-safe), appending an ellipsis when it cut. Screening runs before
// clip, so truncation can never bisect a secret.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// countChanged reports how many rows in merged are new-or-different versus existing, keyed by external_ref. It
// is the seam's PRODUCED number and the write guard: zero means the pass added no new precedent (every
// candidate a duplicate, dropped, or downhill), so the corpus file must not be rewritten. Comparison is by
// json.Marshal, so it rides Incident's own MarshalJSON (which omits an unknown resolved_at) rather than a
// hand-rolled field list that could drift from the serialized form.
func countChanged(existing, merged []knowledge.Incident) int {
	prev := make(map[string]string, len(existing))
	for _, inc := range existing {
		if b, err := json.Marshal(inc); err == nil {
			prev[strings.TrimSpace(inc.ExternalRef)] = string(b)
		}
	}
	n := 0
	for _, inc := range merged {
		b, err := json.Marshal(inc)
		if err != nil {
			continue
		}
		if prev[strings.TrimSpace(inc.ExternalRef)] != string(b) {
			n++
		}
	}
	return n
}
