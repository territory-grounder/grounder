package wikicompile

import (
	"strings"
	"testing"
)

func seamSet() LaneInputs {
	return LaneInputs{Seams: []SeamStatus{
		{Name: "wiki.compile"},
		{Name: "world.discovery"},
		{Name: "suppression.tier1", Dark: true,
			Consequence: "TG spends a full triage session on alerts an operator has already declared expected",
			Detail:      "declared-dark — no TG_SUPPRESSION_* source is configured"},
		{Name: "escalation.page", Dark: true, Critical: true,
			Consequence: "FireDue marks the queue row fired, Page() returns success, and the escalation is permanently lost",
			Detail:      "dark-unrecorded"},
		{Name: "lessons.feed", Dark: true,
			Consequence: "the corpus can grow ONLY from TG's own confirmed-clean heals"},
	}}
}

// TestLiveLanesAreNamedNotOmitted — the property that makes a CLOSED set worth having.
//
// A page listing only problems cannot distinguish "this lane is fine" from "this lane is not covered by
// the manifest" — and that second case is not hypothetical: world discovery ran unwired in production for
// as long as it existed because no seam covered it.
//
// RED MUTATION CONTROL (executed 2026-08-01): rendering only the dark seams drops the Live section and
// fails; restored green.
func TestLiveLanesAreNamedNotOmitted(t *testing.T) {
	body := CompileLanes(seamSet()).Body
	for _, live := range []string{"wiki.compile", "world.discovery"} {
		if !strings.Contains(body, "`"+live+"`") {
			t.Errorf("live lane %q must be NAMED — omitting it makes a healthy lane indistinguishable "+
				"from one the manifest does not cover", live)
		}
	}
	if !strings.Contains(body, "## Live") {
		t.Error("the Live section must exist even when there are dark lanes")
	}
	if !strings.Contains(body, "A lane with no seam cannot appear here") {
		t.Error("the page must state its own coverage limit — the closed set is what it can see, and the " +
			"gap outside it is exactly how a lane once shipped dark")
	}
}

// TestCriticalDarknessIsDistinguished — a critical seam going dark leaves someone UN-REACHED; a normal
// one leaves them un-served. Flattening them would put a lost escalation beside a quieter corpus.
//
// RED MUTATION CONTROL (executed 2026-08-01): dropping the Critical distinction from the headline makes
// the critical case indistinguishable; restored green.
func TestCriticalDarknessIsDistinguished(t *testing.T) {
	body := CompileLanes(seamSet()).Body
	if !strings.Contains(body, "3 of 5 declared lane(s) are DARK, and 1 of those are CRITICAL") {
		t.Errorf("the headline must separate critical darkness; body:\n%s", body)
	}
	if !strings.Contains(body, "un-reached, not merely un-served") {
		t.Error("it must say what CRITICAL means, or the word is decoration")
	}
	// The critical one must sort FIRST among the dark, because the page is read to find what is wrong.
	darkSection := body[strings.Index(body, "## Dark"):]
	esc := strings.Index(darkSection, "escalation.page")
	sup := strings.Index(darkSection, "suppression.tier1")
	if esc < 0 || sup < 0 || esc > sup {
		t.Error("a CRITICAL dark seam must be listed before a normal one")
	}
}

// TestEveryDarkLaneCarriesWhatItCosts — the reason this page is worth compiling at all. The consequence
// prose was written when each seam was declared and currently reaches only a boot log and a ledger row.
func TestEveryDarkLaneCarriesWhatItCosts(t *testing.T) {
	body := CompileLanes(seamSet()).Body
	for _, phrase := range []string{
		"permanently lost",                         // escalation.page
		"already declared expected",                // suppression.tier1
		"ONLY from TG's own confirmed-clean heals", // lessons.feed
	} {
		if !strings.Contains(body, phrase) {
			t.Errorf("every dark lane must render its declared consequence; %q missing", phrase)
		}
	}
}

// TestSeamWithNoConsequenceIsCalledOutAsADefect — a finding that says only "dark" tells an operator
// nothing they can act on, which is the exact complaint the wiring package's own comments make.
func TestSeamWithNoConsequenceIsCalledOutAsADefect(t *testing.T) {
	body := CompileLanes(LaneInputs{Seams: []SeamStatus{{Name: "mystery.lane", Dark: true}}}).Body
	if !strings.Contains(body, "No consequence prose was declared") {
		t.Error("a dark seam with no consequence must be reported as a DECLARATION defect, not rendered " +
			"as a bare name — otherwise the page reproduces the uninformative finding it exists to replace")
	}
}

// TestAllLiveSaysSo — the honest-clean state. It must not render as an empty page.
func TestAllLiveSaysSo(t *testing.T) {
	body := CompileLanes(LaneInputs{Seams: []SeamStatus{{Name: "a"}, {Name: "b"}}}).Body
	if !strings.Contains(body, "All 2 declared lane(s) are live") {
		t.Error("a fully-live manifest must say so explicitly")
	}
	if strings.Contains(body, "## Dark") {
		t.Error("no Dark section when nothing is dark — an empty heading reads as a missing list")
	}
}

// TestEmptyManifestBlamesTheManifestNotTheLanes — the honest-empty rule, applied to the page whose whole
// subject is things that are missing.
func TestEmptyManifestBlamesTheManifestNotTheLanes(t *testing.T) {
	body := CompileLanes(LaneInputs{}).Body
	if !strings.Contains(body, "statement about the manifest, not about the lanes") {
		t.Error("an empty closed set means nothing is DECLARED — not that everything is fine")
	}
}

// TestLanePageIsDeterministic — the sort must be total, or the page churns on every compile.
func TestLanePageIsDeterministic(t *testing.T) {
	in := LaneInputs{Seams: []SeamStatus{
		{Name: "z", Dark: true}, {Name: "a", Dark: true}, {Name: "m", Dark: true, Critical: true},
		{Name: "y"}, {Name: "b"},
	}}
	first := CompileLanes(in).Body
	for i := 0; i < 25; i++ {
		if CompileLanes(in).Body != first {
			t.Fatal("lane page is not deterministic")
		}
	}
}
