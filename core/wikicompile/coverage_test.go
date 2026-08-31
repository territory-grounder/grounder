package wikicompile

import (
	"strings"
	"testing"
)

// TestCoverageCountsAreComputedNotAsserted — the defect this page exists NOT to repeat.
//
// The predecessor's coverage matrix asserts a hardcoded Status string per source, and at least two of
// those strings are provably false in the same run that prints them (wiki-compile.py:1092, e.g.
// "openclaw_memory: 9282 | Compiled to services/openclaw.md" while compile_services caps that category at
// 10). A coverage page that can disagree with the compile beside it is worse than no coverage page: it
// launders an assumption into a figure.
//
// So every number here must come from the SAME values that produced the articles. This asserts that
// relationship directly: change the inputs, and the page changes with them.
//
// RED MUTATION CONTROL (executed 2026-08-01): hardcoding `rendered := len(in.Articles)` to a constant
// fails with the mismatched count; restored green.
func TestCoverageCountsAreComputedNotAsserted(t *testing.T) {
	in := CoverageInputs{
		RosterSize: 5,
		Articles:   []Article{{Slug: "host-a"}, {Slug: "host-b"}, {Slug: "host-c"}},
		Skipped: []Skip{
			{Host: "bad/name", Reason: "hostname is not a safe page identifier"},
			{Host: "broken1", Reason: "the triage read for this host failed: connection reset"},
		},
		Sources:     map[string]int{"triage_sessions": 3202, "distinct_hosts": 5},
		EstateEdges: 42,
		CorpusRows:  670,
	}
	body := CompileCoverage(in).Body

	for _, want := range []string{"3 of 5 hosts have a page", "2 were refused"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page must state %q computed from its inputs; body:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "| triage_sessions | 3202 |") {
		t.Error("the denominators must be rendered from Sources, not restated from anywhere else")
	}
	if !strings.Contains(body, "| estate graph edges | 42 |") || !strings.Contains(body, "| knowledge corpus rows | 670 |") {
		t.Error("the enrichment counts must be rendered")
	}
	// Both refusal reasons must survive, grouped.
	if !strings.Contains(body, "not a safe page identifier") || !strings.Contains(body, "connection reset") {
		t.Error("every refusal reason must appear — a host missing without explanation is " +
			"indistinguishable from a host that does not exist")
	}
	// And the counts must MOVE with the inputs.
	in.Articles = append(in.Articles, Article{Slug: "host-d"})
	in.RosterSize = 6
	if !strings.Contains(CompileCoverage(in).Body, "4 of 6 hosts have a page") {
		t.Error("the counts are not derived from the inputs — that is the predecessor's defect exactly")
	}
}

// TestCoverageNamesItsBlindSpots — a coverage report that lists only what it HAS is a success report.
//
// Each blind spot is a real degradation an operator would otherwise have to infer by noticing that every
// page says the same empty thing.
func TestCoverageNamesItsBlindSpots(t *testing.T) {
	t.Run("no estate edges", func(t *testing.T) {
		b := CompileCoverage(CoverageInputs{RosterSize: 3, Articles: []Article{{Slug: "a"}}, CorpusRows: 1}).Body
		if !strings.Contains(b, "No estate-graph edges were available") {
			t.Error("zero edges over a non-empty roster must be named as a blind spot")
		}
		if !strings.Contains(b, "NOT evidence") {
			t.Error("it must say that empty dependency sections are not evidence of isolation")
		}
	})
	t.Run("empty corpus", func(t *testing.T) {
		b := CompileCoverage(CoverageInputs{RosterSize: 3, Articles: []Article{{Slug: "a"}}, EstateEdges: 5}).Body
		if !strings.Contains(b, "knowledge corpus was empty or unreadable") {
			t.Error("an empty corpus must be named")
		}
	})
	t.Run("roster narrower than the spine", func(t *testing.T) {
		b := CompileCoverage(CoverageInputs{
			RosterSize: 2, Articles: []Article{{Slug: "a"}}, EstateEdges: 1, CorpusRows: 1,
			Sources: map[string]int{"distinct_hosts": 78},
		}).Body
		if !strings.Contains(b, "78 distinct hosts but this compile's roster offered 2") {
			t.Error("a roster narrower than the spine must be named — that is silent under-coverage")
		}
	})
	t.Run("clean compile does not claim completeness", func(t *testing.T) {
		b := CompileCoverage(CoverageInputs{
			RosterSize: 2, Articles: []Article{{Slug: "a"}, {Slug: "b"}},
			EstateEdges: 5, CorpusRows: 10, Sources: map[string]int{"distinct_hosts": 2},
		}).Body
		if !strings.Contains(b, "not a guarantee of completeness") {
			t.Error("a clean run must NOT read as 'fully covered' — it can only report the blind spots " +
				"it knows how to look for, and saying otherwise is the predecessor's Status column again")
		}
	})
}

// TestCoverageDistinguishesWikiCoverageFromEstateCoverage — the misreading that would make this page
// dangerous: treating absence from the wiki as evidence a machine is healthy.
func TestCoverageDistinguishesWikiCoverageFromEstateCoverage(t *testing.T) {
	b := CompileCoverage(CoverageInputs{RosterSize: 1, Articles: []Article{{Slug: "a"}}}).Body
	if !strings.Contains(b, "never that the machine is fine") {
		t.Error("the page must state that absence means TG has no recorded experience of a host, not that " +
			"the host is healthy — otherwise a complete-looking wiki reads as a clean estate")
	}
}

// TestCoverageIsDeterministic — same inputs, byte-identical page. Map iteration over Sources and the
// refusal grouping are both unordered in Go; either would churn the page on every compile.
func TestCoverageIsDeterministic(t *testing.T) {
	in := CoverageInputs{
		RosterSize: 4,
		Articles:   []Article{{Slug: "host-a"}},
		Skipped:    []Skip{{Host: "z", Reason: "r2"}, {Host: "a", Reason: "r1"}, {Host: "m", Reason: "r1"}},
		Sources:    map[string]int{"z_last": 1, "a_first": 2, "m_mid": 3, "triage_sessions": 4},
	}
	first := CompileCoverage(in).Body
	for i := 0; i < 25; i++ {
		if CompileCoverage(in).Body != first {
			t.Fatal("coverage page is not deterministic — map iteration or refusal grouping is unordered, " +
				"so the page would differ on every compile even when nothing changed")
		}
	}
}

// TG-242 follow-through. The coverage page is the one artifact whose entire job is to be accurate about
// coverage, and it printed a numerator larger than its denominator.
//
// LIVE ON 2026-08-07, read through the authenticated console API a human uses:
//
//	"235 of 131 hosts have a page. 3 were refused"
//
// 235 is EVERY page the compile produced — 131 host + 94 rule + 8 op-class + decisions + lane-health —
// divided by the HOST roster. And the "3 host(s)" refused were three alert RULES: cmd/worker/
// wiki_compile.go appends only ruleSkips and opSkips, and no host skip site exists in the tree at all.
//
// This is the failure the page was written to avoid in the predecessor, whose equivalent "asserts a
// hardcoded Status per source and gets at least two of them wrong in the run that prints them".

func TestCoverageCountsHostPagesNotEveryPage(t *testing.T) {
	art := CompileCoverage(CoverageInputs{
		RosterSize: 3,
		Articles: []Article{
			{Slug: "host-a"}, {Slug: "host-b"}, {Slug: "host-c"},
			{Slug: "rule-x"}, {Slug: "rule-y"}, {Slug: "opclass-z"}, {Slug: "governance-decisions"},
		},
	})
	if strings.Contains(art.Body, "7 of 3 hosts") {
		t.Fatal("the coverage page counted EVERY article against the HOST roster — a numerator larger " +
			"than its denominator, on the page whose whole purpose is to be accurate about coverage")
	}
	if !strings.Contains(art.Body, "3 of 3 hosts have a page") {
		t.Errorf("want \"3 of 3 hosts have a page\"; the host numerator must count host pages only.\n%s",
			firstLines(art.Body, 12))
	}
}

func TestCoverageNamesWhatWasActuallyRefused(t *testing.T) {
	art := CompileCoverage(CoverageInputs{
		RosterSize: 2,
		Articles:   []Article{{Slug: "host-a"}, {Slug: "host-b"}},
		Skipped: []Skip{
			{Kind: "rule", Host: "port-status-up/down", Reason: "rule name is not a safe page identifier"},
			{Kind: "rule", Host: "service-up/down", Reason: "rule name is not a safe page identifier"},
		},
	})
	if strings.Contains(art.Body, "host(s)") {
		t.Errorf("two refused RULES were reported as hosts. An operator reading \"2 host(s) were refused\" "+
			"goes looking for two missing machines that were never missing.\n%s", firstLines(art.Body, 16))
	}
	if !strings.Contains(art.Body, "2 rule(s)") {
		t.Errorf("the refusal is not labelled as a rule refusal.\n%s", firstLines(art.Body, 16))
	}
	// The host line must report a CLEAN host sweep, because no host was refused.
	if !strings.Contains(art.Body, "2 of 2 hosts have a page") {
		t.Errorf("rule refusals were subtracted from the HOST coverage line.\n%s", firstLines(art.Body, 12))
	}
	// And the refused section must still appear even though zero HOSTS were refused — gating it on the
	// host count would hide rule and op-class refusals entirely.
	if !strings.Contains(art.Body, "Refused") {
		t.Error("the Refused section vanished when no HOST was refused, hiding the rule refusals that did " +
			"happen — the same silence that let three rules be described as hosts")
	}
}

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
