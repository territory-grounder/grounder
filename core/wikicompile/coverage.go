package wikicompile

import (
	"fmt"
	"sort"
	"strings"
)

// The COVERAGE page: what this compile saw, what it rendered, and what it could not.
//
// WHY THE PREDECESSOR'S VERSION IS A CAUTIONARY TALE RATHER THAN A TEMPLATE. `wiki-compile.py:1092`
// builds a coverage matrix by ASSERTING a hardcoded Status string per source — and at least two of those
// strings are provably false in the same run that prints them (`openclaw_memory: "9282 | Compiled to
// services/openclaw.md"` while `compile_services` caps that category at 10). Its sibling health report
// (`:962`) is 408 KB of issues nobody reads. Both fail the same way: they describe the compile they were
// written for, not the compile that just ran.
//
// So this page COMPUTES every number from the same values that produced the articles, in the same pass.
// It cannot report a coverage the compiler did not achieve, because there is no second source of truth to
// drift from — the counts here and the articles beside them come from one call.
//
// It is deliberately SMALL. A coverage page an operator does not read is worth nothing, and TG already has
// `tools/specvalidate/opcover` for spec-coverage arithmetic; this answers one question that tool cannot:
// "is the wiki I am reading a complete view of the estate, and if not, which parts are missing and why?"

// CoverageInputs is the compile's own result, handed back to be described. Every field is a value the
// caller already holds — nothing is re-derived, which is what keeps the page honest.
type CoverageInputs struct {
	// RosterSize is how many hosts the spine offered.
	RosterSize int
	// Articles are the pages actually produced.
	Articles []Article
	// Skipped are the hosts refused, each with its reason.
	Skipped []Skip
	// Sources are the denominators the compile read (triage sessions, distinct hosts, ...).
	Sources map[string]int
	// EstateEdges is how many estate-graph edges were available; 0 with a non-empty roster means the
	// estate read failed or returned nothing, and every dependency section on every page says so.
	EstateEdges int
	// CorpusRows is how many knowledge-corpus entries were available to cite as precedent.
	CorpusRows int
}

// CoverageSlug is the fixed slug of the coverage page.
const CoverageSlug = "wiki-coverage"

// CompileCoverage renders the coverage page. Pure: no clock, no io — the compile instant rides the
// envelope, as it does for every other article.
func CompileCoverage(in CoverageInputs) Article {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	// COUNT THE POPULATION THE SENTENCE IS ABOUT. This was len(in.Articles) — EVERY page produced by the
	// compile — printed against RosterSize, which counts HOSTS. Live on 2026-08-07 that rendered
	// "235 of 131 hosts have a page": 131 host pages + 94 rule pages + 8 op-class pages + decisions +
	// lane-health, divided by the host roster. A numerator larger than its denominator, on the one page
	// whose entire job is to be accurate about coverage — the precise failure this page was written to
	// avoid in the predecessor ("asserts a hardcoded Status per source and gets at least two wrong in the
	// run that prints them").
	renderedHosts := 0
	for _, a := range in.Articles {
		if strings.HasPrefix(a.Slug, hostSlugPrefix) {
			renderedHosts++
		}
	}
	// AND REFUSALS ARE NOT ALL HOSTS. cmd/worker/wiki_compile.go appends ONLY ruleSkips and opSkips — no
	// host skip site exists — so every entry here was rendered as "N host(s) were refused" while naming
	// alert rules. Counted per kind now.
	skippedHosts := 0
	for _, sk := range in.Skipped {
		if sk.SkipKind() == "host" {
			skippedHosts++
		}
	}
	rendered := renderedHosts
	skipped := skippedHosts

	w("# What this wiki covers, and what it does not")
	w("")
	w("Every number below is computed from the compile that produced the pages beside it — not asserted, " +
		"and not carried over from a previous run. If a figure here is wrong, the pages are wrong the same way.")
	w("")

	// ── The one number an operator wants ───────────────────────────────────────
	w("## Host pages")
	w("")
	switch {
	case in.RosterSize == 0:
		w("**No host has a page, because the spine offered none.** No triage session carries a host, so " +
			"there is nothing to compile a page from. This is a statement about the spine, not the estate.")
	case skipped == 0:
		w(fmt.Sprintf("**%d of %d hosts have a page.** Every host the spine offered was rendered.",
			rendered, in.RosterSize))
	default:
		w(fmt.Sprintf("**%d of %d hosts have a page. %d were refused** — listed below with the reason, "+
			"because a host missing without explanation is indistinguishable from a host that does not exist.",
			rendered, in.RosterSize, skipped))
	}
	w("")

	// The refused SECTION lists every refusal, not only host ones — gating it on the host count would
	// hide the rule and op-class refusals entirely, which is how three refused alert rules came to be
	// described as hosts in the first place.
	if len(in.Skipped) > 0 {
		// Grouped by reason so a systemic refusal reads as one line rather than N.
		byReason := map[string][]string{}
		byReasonKind := map[string]string{}
		for _, s := range in.Skipped {
			byReason[s.Reason] = append(byReason[s.Reason], s.Host)
			byReasonKind[s.Reason] = s.SkipKind()
		}
		reasons := make([]string, 0, len(byReason))
		for r := range byReason {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		w("### Refused")
		w("")
		for _, r := range reasons {
			hosts := byReason[r]
			sort.Strings(hosts)
			w(fmt.Sprintf("- **%d %s(s)** — %s", len(hosts), byReasonKind[r], mdCell(r)))
			shown := hosts
			if len(shown) > 10 {
				shown = shown[:10]
			}
			w("  - " + strings.Join(shown, ", ") + func() string {
				if len(hosts) > len(shown) {
					return fmt.Sprintf(" … and %d more", len(hosts)-len(shown))
				}
				return ""
			}())
		}
		w("")
	}

	// ── What fed the pages ─────────────────────────────────────────────────────
	w("## What the pages were built from")
	w("")
	w("| source | rows this compile read |")
	w("|---|---|")
	keys := make([]string, 0, len(in.Sources))
	for k := range in.Sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w(fmt.Sprintf("| %s | %d |", mdCell(k), in.Sources[k]))
	}
	w(fmt.Sprintf("| estate graph edges | %d |", in.EstateEdges))
	w(fmt.Sprintf("| knowledge corpus rows | %d |", in.CorpusRows))
	w("")

	// ── What is MISSING, stated rather than left to inference ──────────────────
	//
	// This section is the reason the page exists. A coverage report that lists only what it has is a
	// success report; the predecessor's prints a Status column asserting completeness it never checked.
	w("## Known blind spots in this compile")
	w("")
	blind := 0
	if in.EstateEdges == 0 && in.RosterSize > 0 {
		blind++
		w("- **No estate-graph edges were available.** Every host page's Dependencies and Dependents " +
			"sections say they have nothing recorded — which is true of this compile, and is NOT evidence " +
			"those hosts are isolated.")
	}
	if in.CorpusRows == 0 && in.RosterSize > 0 {
		blind++
		w("- **The knowledge corpus was empty or unreadable.** No page cites precedent, so retrieval has " +
			"nothing host-specific to offer for any of them.")
	}
	if skipped > 0 {
		blind++
		w(fmt.Sprintf("- **%d host(s) were refused** (see above). Their incidents exist on the spine and "+
			"are not represented anywhere in this wiki.", skipped))
	}
	if s, ok := in.Sources["distinct_hosts"]; ok && s > in.RosterSize {
		blind++
		w(fmt.Sprintf("- **The spine holds %d distinct hosts but this compile's roster offered %d.** "+
			"Some hosts are recorded but not reachable by the roster read.", s, in.RosterSize))
	}
	if blind == 0 {
		w("None found by this compile. That is a statement about the checks listed here, not a guarantee " +
			"of completeness — this page can only report blind spots it knows how to look for.")
	}
	w("")
	w("## What this page is not")
	w("")
	w("It describes the WIKI's coverage of the spine. It says nothing about whether the spine covers the " +
		"estate: a host TG has never triaged has no session, so it never reaches the roster and cannot " +
		"appear as missing here. Absence from this wiki means TG has no recorded experience of a machine — " +
		"never that the machine is fine.")

	return Article{
		Slug:  CoverageSlug,
		Title: "Wiki coverage — what is here and what is missing",
		Kind:  "article",
		Body:  b.String(),
		Meta: map[string]string{
			"rendered": fmt.Sprint(rendered),
			"roster":   fmt.Sprint(in.RosterSize),
			"skipped":  fmt.Sprint(skipped),
		},
	}
}
