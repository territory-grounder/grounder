package wikicompile

import (
	"strings"
	"testing"
)

func opSess() []RuleSession {
	return []RuleSession{
		rs("a", "h1", "device-down", "Device-Down", "proposed", "start-guest", true, true, 30),
		rs("b", "h2", "device-down", "Device-Down", "proposed", "start-guest", true, false, 29),
		rs("c", "h1", "device-down", "Device-Down", "proposed", "start-guest", false, false, 28),
		rs("d", "h3", "disk-full", "DiskFull-90", "proposed", "disk-grow", false, false, 27),
	}
}

func opOnly(t *testing.T, in OpClassInputs, slug string) Article {
	t.Helper()
	arts, _ := CompileOpClasses(in)
	for _, a := range arts {
		if a.Slug == slug {
			return a
		}
	}
	t.Fatalf("no page %q; got %d pages", slug, len(arts))
	return Article{}
}

// TestOpClassPagesAreKeyedOnUseNotRatification — the inversion this family rests on.
//
// Measured in production 2026-08-01: opclass_ratified holds ZERO rows while session_triage carries SEVEN
// distinct op-classes over 3,202 sessions and action_execution holds 460 real executions. A page set
// keyed on the ratified catalogue would render NOTHING — the same failure that made #manifest and
// #proposals empty, and blank reads as "nothing to see" rather than "the ladder is not built yet".
//
// RED MUTATION CONTROL (executed 2026-08-01): skipping classes absent from in.Ratified produces zero
// pages against an empty catalogue; restored green.
func TestOpClassPagesAreKeyedOnUseNotRatification(t *testing.T) {
	arts, skips := CompileOpClasses(OpClassInputs{
		Sessions: opSess(), Ratified: map[string]bool{}, RatifiedKnown: true,
	})
	if len(skips) != 0 {
		t.Fatalf("unexpected refusals: %+v", skips)
	}
	if len(arts) != 2 {
		t.Fatalf("two op-classes were USED, so two pages must exist even with an EMPTY ratified "+
			"catalogue; got %d", len(arts))
	}
	body := opOnly(t, OpClassInputs{Sessions: opSess(), Ratified: map[string]bool{}, RatifiedKnown: true},
		"opclass-start-guest").Body
	if !strings.Contains(body, "NOT in the earned catalogue") {
		t.Error("an unratified class must say so — that IS the finding, not a gap in the page")
	}
	if !strings.Contains(body, "2 execution(s) have already happened under a class no operator has ratified") {
		t.Errorf("the page must state plainly that executions preceded ratification; body:\n%s", body)
	}
	// ...and it must NOT read as an escape, because it is not one.
	if !strings.Contains(body, "this is not an escape") {
		t.Error("it must say the authored registry permitted those executions and the mode chokepoint " +
			"gated them — otherwise the page reads as an uncontrolled-actuation report, which is false")
	}
}

// TestUnreadableCatalogueIsNotReportedAsUnratified — an unreadable catalogue and an empty one are
// different facts, and the difference is the whole day's theme.
//
// RED MUTATION CONTROL (executed 2026-08-01): dropping RatifiedKnown and treating a nil map as "none
// ratified" makes every page assert NOT RATIFIED over a read that never happened; restored green.
func TestUnreadableCatalogueIsNotReportedAsUnratified(t *testing.T) {
	body := opOnly(t, OpClassInputs{Sessions: opSess(), RatifiedKnown: false}, "opclass-start-guest").Body
	if !strings.Contains(body, "could not be read") {
		t.Error("an unreadable earned catalogue must be reported as UNKNOWN standing")
	}
	if strings.Contains(body, "NOT in the earned catalogue") {
		t.Error("a failed read must never render as a confident 'not ratified' — that is a claim about " +
			"the catalogue derived from not having seen it")
	}
	if !strings.Contains(body, "not the same as unratified") {
		t.Error("the page must spell out that unknown != unratified")
	}
}

// TestExecutionsWithoutConfirmationAreCountedSeparately — "TG changed something and could not verify it
// held" is a different fact from a heal that worked, and on a capability page it is the one that decides
// whether to grant more autonomy.
func TestExecutionsWithoutConfirmationAreCountedSeparately(t *testing.T) {
	body := opOnly(t, OpClassInputs{Sessions: opSess(), Ratified: map[string]bool{}, RatifiedKnown: true},
		"opclass-start-guest").Body
	if !strings.Contains(body, "1 of 2** confirmed clear") {
		t.Errorf("one of two executions was confirmed; body:\n%s", body)
	}
	if !strings.Contains(body, "could not verify it held") {
		t.Error("the unconfirmed execution must be named in those words")
	}
}

// TestNeverExecutedClassSaysSoRatherThanShowingZeroPercent — production has disk-grow at 31 proposals and
// ZERO executions. A "0% confirmed" would read as a failing capability; the truth is it has never run.
func TestNeverExecutedClassSaysSoRatherThanShowingZeroPercent(t *testing.T) {
	body := opOnly(t, OpClassInputs{Sessions: opSess(), Ratified: map[string]bool{}, RatifiedKnown: true},
		"opclass-disk-grow").Body
	if !strings.Contains(body, "no confirmation rate") || !strings.Contains(body, "nothing has run") {
		t.Errorf("a never-executed class must say nothing has run, not report a zero rate; body:\n%s", body)
	}
}

// TestOpClassCompileIsDeterministic — host maps and class maps are unordered in Go.
func TestOpClassCompileIsDeterministic(t *testing.T) {
	in := OpClassInputs{Sessions: append(opSess(),
		rs("e", "h9", "device-down", "Device-Down", "proposed", "start-guest", true, true, 30),
		rs("f", "h4", "device-down", "Device-Down", "proposed", "start-guest", false, false, 26),
	), Ratified: map[string]bool{"start-guest": true}, RatifiedKnown: true,
		Candidates: map[string]string{"disk-grow": "observing"}}
	first := opOnly(t, in, "opclass-start-guest").Body
	for i := 0; i < 25; i++ {
		if opOnly(t, in, "opclass-start-guest").Body != first {
			t.Fatal("op-class page is not deterministic")
		}
	}
}
