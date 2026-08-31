package wikicompile

import (
	"fmt"
	"sort"
	"strings"
)

// THE LANE HEALTH PAGE — which of TG's lanes are running, which are dark, and what each darkness costs.
//
// This is the health report, and it is deliberately NOT the predecessor's. `wiki-compile.py:962` emits a
// 408 KB dump of every issue it can enumerate, which nobody reads; and its coverage matrix asserts a
// hardcoded Status per source, at least two of them provably false in the same run. Copying either would
// produce a page whose only function is to exist.
//
// What TG has instead is a CLOSED SET of declared wiring seams, each carrying prose — written when the
// seam was declared — saying exactly what its darkness costs an operator. That prose is already the best
// text in the codebase for this purpose and it currently reaches a boot log and a ledger row, neither of
// which anyone reads at 3am. Rendering it is the whole page.
//
// WHY THIS MATTERS MORE THAN A GENERIC HEALTH DUMP. Every serious defect found on 2026-08-01 was a lane
// that existed and did not run: temporal/worlddiscovery imported by nothing (manifest_entry empty in
// production for as long as it existed), the tier-1 suppression chain guarded with no else and no seam,
// the dark-seam gauge itself computed every boot and dropped. The manifest was built to make that class
// visible; until now it had no operator-readable surface at all.
//
// A LIVE LANE IS REPORTED AS LIVE, not omitted. A page that lists only problems cannot distinguish "this
// lane is fine" from "this lane is not covered by the manifest", and that distinction is the entire
// value of a CLOSED set.

// SeamStatus is one declared seam's standing, resolved by the caller from the wiring manifest.
type SeamStatus struct {
	// Name is the seam id (e.g. "wiki.compile").
	Name string
	// Dark is true when the manifest reports the seam as not live.
	Dark bool
	// Critical marks a seam whose darkness leaves someone un-reached rather than merely un-served.
	Critical bool
	// Consequence is the prose written when the seam was declared — what this darkness costs.
	Consequence string
	// Detail is the manifest's per-finding detail (the declared reason, or the unrecorded-branch note).
	Detail string
}

// LaneInputs is the page's whole world.
type LaneInputs struct {
	Seams []SeamStatus
}

// LanesSlug is the fixed slug of the lane health page.
const LanesSlug = "lane-health"

// CompileLanes renders the lane health page.
func CompileLanes(in LaneInputs) Article {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	seams := make([]SeamStatus, len(in.Seams))
	copy(seams, in.Seams)
	// Dark first, critical-dark before normal-dark, then by name: the page is read to find what is wrong.
	sort.Slice(seams, func(i, j int) bool {
		if seams[i].Dark != seams[j].Dark {
			return seams[i].Dark
		}
		if seams[i].Dark && seams[i].Critical != seams[j].Critical {
			return seams[i].Critical
		}
		return seams[i].Name < seams[j].Name
	})

	dark, criticalDark := 0, 0
	for _, s := range seams {
		if s.Dark {
			dark++
			if s.Critical {
				criticalDark++
			}
		}
	}

	w("# Lane health")
	w("")
	w("Every wiring seam TG declares, and whether it is actually running. Compiled from the wiring " +
		"manifest — the same source as the boot report and the governance-ledger row, rendered where " +
		"someone might read it.")
	w("")

	if len(seams) == 0 {
		w("## No seams declared")
		w("")
		w("The wiring manifest's closed set is empty, so this page can report nothing. That is a statement " +
			"about the manifest, not about the lanes: code can be dark without a seam to declare it, which " +
			"is exactly how a whole lane once ran unwired in production with nothing saying so.")
		return Article{Slug: LanesSlug, Title: "Lane health", Kind: "article", Body: b.String(),
			Meta: map[string]string{"seams": "0", "dark": "0"}}
	}

	// ── The headline ───────────────────────────────────────────────────────────
	w("## Standing")
	w("")
	switch {
	case dark == 0:
		w(fmt.Sprintf("**All %d declared lane(s) are live.**", len(seams)))
	case criticalDark > 0:
		w(fmt.Sprintf("**%d of %d declared lane(s) are DARK, and %d of those are CRITICAL** — a critical "+
			"seam going dark leaves someone un-reached, not merely un-served.", dark, len(seams), criticalDark))
	default:
		w(fmt.Sprintf("**%d of %d declared lane(s) are dark.** None is critical: nobody is un-reached, "+
			"but each costs something an operator should know about.", dark, len(seams)))
	}
	w("")
	w("This page covers the seams TG DECLARES. A lane with no seam cannot appear here, live or dark — " +
		"and that gap is not hypothetical: the world-model discovery pass ran unwired in production for " +
		"as long as it existed, with nothing anywhere saying so, because no seam covered it.")
	w("")

	// ── Dark lanes, with what each costs ───────────────────────────────────────
	if dark > 0 {
		w("## Dark")
		w("")
		for _, s := range seams {
			if !s.Dark {
				continue
			}
			marker := ""
			if s.Critical {
				marker = " — **CRITICAL**"
			}
			w(fmt.Sprintf("### `%s`%s", mdCell(s.Name), marker))
			w("")
			if c := strings.TrimSpace(s.Consequence); c != "" {
				w("**What this costs:** " + mdCell(c))
			} else {
				w("**No consequence prose was declared for this seam.** That is a defect in the declaration: " +
					"a finding that says only \"dark\" tells an operator nothing they can act on.")
			}
			if d := strings.TrimSpace(s.Detail); d != "" {
				w("")
				w("Reported as: " + mdCell(d))
			}
			w("")
		}
	}

	// ── Live lanes, named rather than omitted ──────────────────────────────────
	w("## Live")
	w("")
	live := make([]string, 0, len(seams))
	for _, s := range seams {
		if !s.Dark {
			live = append(live, s.Name)
		}
	}
	if len(live) == 0 {
		w("None. Every declared lane is dark.")
	} else {
		w("Named rather than omitted, because a page listing only problems cannot distinguish a healthy " +
			"lane from one the manifest does not cover:")
		w("")
		for _, n := range live {
			w("- `" + mdCell(n) + "`")
		}
	}

	return Article{
		Slug:  LanesSlug,
		Title: "Lane health",
		Kind:  "article",
		Body:  b.String(),
		Meta: map[string]string{
			"seams":         fmt.Sprint(len(seams)),
			"dark":          fmt.Sprint(dark),
			"critical_dark": fmt.Sprint(criticalDark),
		},
	}
}
