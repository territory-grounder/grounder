package attribution

import (
	"testing"
	"time"
)

// A GUEST THE CARVE-OUTS DO NOT NAME LOSES AUTONOMY SILENTLY.
//
// matchCarveOut matches hosts with containsFold — exact, case-folded, no glob or CIDR. That is the right
// choice for an authorization rule, but it makes the carve-out host list and the estate's guest pool two
// lists that must agree, edited in different places, with nothing comparing them. A guest added to the pool
// is absent from every carve-out, so the harness cycle on it (the injector's sanctioned change + TG's own
// heal) stops resolving to authorized-test and becomes the {AttributedAuthorized, AttributedSelf}
// contradiction, which escalates. Nothing logs it; the symptom looks like estate noise on one host.
//
// This is the same defect class as the actuation allowlist (17 guests) vs the ssh-actuatable set (7 hosts),
// which also differed with nothing checking.
func TestCarveOutHostCoverageNamesTheGuestsThatWillEscalate(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	cfg := Config{CarveOuts: []CarveOut{{
		ID: "harness", Domain: "pve", Actors: []string{"root@pam!injector"},
		Hosts: []string{"dc1mealie01", "NLLEI01CLOUDBEAVER01"},
		// Both bounds are REQUIRED for a carve-out to match at all: an unbounded carve-out is invalid, not
		// eternal (REQ-2309). Before that was enforced, this literal omitted them and still counted as
		// covering its hosts — the fixture quietly asserted that a permanent carve-out was a normal one.
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}}}

	pool := []string{"dc1mealie01", "dc1cloudbeaver01", "dc1linkwarden02", "dc1excalidraw01"}
	covered, uncovered := CarveOutHostCoverage(cfg, pool, now)

	// Case-folding is load-bearing: the config says NLLEI01CLOUDBEAVER01 and the estate says lowercase.
	// If this regressed to an exact match, a correctly-configured host would be reported as a gap and an
	// operator would "fix" a file that was already right.
	if len(covered) != 2 {
		t.Errorf("covered = %v, want the 2 hosts the carve-out names (case-insensitively)", covered)
	}
	want := map[string]bool{"dc1linkwarden02": true, "dc1excalidraw01": true}
	if len(uncovered) != 2 {
		t.Fatalf("uncovered = %v, want exactly the 2 pool guests no carve-out names", uncovered)
	}
	for _, h := range uncovered {
		if !want[h] {
			t.Errorf("uncovered names %q, which the carve-out DOES cover", h)
		}
	}
	// Order follows the pool so a boot log line is stable across restarts rather than reshuffling and
	// looking like the estate changed.
	if uncovered[0] != "dc1linkwarden02" {
		t.Errorf("uncovered = %v, want pool order preserved", uncovered)
	}
}

// AN EXPIRED CARVE-OUT COVERS NOTHING — and this is the case most worth reporting, because the config file
// still LOOKS like it covers the host. carveOutValid is consulted per row, so coverage lapses exactly when
// the rule does.
func TestExpiredAndFutureCarveOutsCoverNothing(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	pool := []string{"dc1mealie01"}

	for _, c := range []struct {
		name string
		co   CarveOut
	}{
		{"expired an hour ago", CarveOut{ID: "lapsed", Hosts: []string{"dc1mealie01"},
			ValidUntil: now.Add(-time.Hour)}},
		{"not valid until tomorrow", CarveOut{ID: "future", Hosts: []string{"dc1mealie01"},
			ValidFrom: now.Add(24 * time.Hour)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, uncovered := CarveOutHostCoverage(Config{CarveOuts: []CarveOut{c.co}}, pool, now)
			if len(uncovered) != 1 {
				t.Errorf("a carve-out that is %s reported the host as COVERED — the config still names it, "+
					"so this is exactly the case an operator cannot see by reading the file", c.name)
			}
		})
	}

	// The converse, so the test cannot pass by reporting everything uncovered: a VALID window covers.
	valid := CarveOut{ID: "current", Hosts: []string{"dc1mealie01"},
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}
	covered, uncovered := CarveOutHostCoverage(Config{CarveOuts: []CarveOut{valid}}, pool, now)
	if len(covered) != 1 || len(uncovered) != 0 {
		t.Errorf("a carve-out valid RIGHT NOW did not cover its host (covered=%v uncovered=%v) — the check "+
			"must report real gaps, not all hosts", covered, uncovered)
	}
}

// With no carve-outs at all, EVERY guest is uncovered. This is the shipped default state
// (default_config.json carries "carve_outs": []), so the check must say so plainly rather than reporting a
// comfortable empty list.
func TestNoCarveOutsMeansEveryGuestIsUncovered(t *testing.T) {
	pool := []string{"a", "b", "c"}
	covered, uncovered := CarveOutHostCoverage(Config{}, pool, time.Now())
	if len(covered) != 0 {
		t.Errorf("covered = %v with no carve-outs configured", covered)
	}
	if len(uncovered) != 3 {
		t.Errorf("uncovered = %v, want all 3 — the shipped default has no carve-outs, so a check that "+
			"reported nothing here would be silent in precisely the default configuration", uncovered)
	}
	// An empty pool is not a gap — nothing to cover is not the same as nothing covered.
	if _, u := CarveOutHostCoverage(Config{}, nil, time.Now()); len(u) != 0 {
		t.Errorf("an EMPTY pool reported %v uncovered — with no guests there is no coverage question, and a "+
			"spurious warning at boot trains operators to ignore this line", u)
	}
	// Blank entries in the pool are skipped rather than reported as a nameless uncovered host.
	if _, u := CarveOutHostCoverage(Config{}, []string{"", "  "}, time.Now()); len(u) != 0 {
		t.Errorf("blank pool entries were reported as uncovered hosts: %v", u)
	}
}
