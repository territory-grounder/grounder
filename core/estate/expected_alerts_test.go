package estate

import (
	"context"
	"testing"
	"time"
)

// TG-188 — chaos-measured ExpectedAlerts: the drill's observed downstream alert rules ride the chaos edge as
// its MEASURED expected-alert set, and the set follows the WINNING provenance in Upsert exactly like
// DelaySeconds/RecoverySeconds. These mirror the slice-2c recovery tests: the chaos carrier and the ratchet.

// THE DoD's NAMED TEST: a cascade carrying observed rules produces a chaos edge whose ExpectedAlerts is that
// measured set. RED before chaos.go carries ObservedRules onto the edge; GREEN after. Killing mutation: remove
// `ExpectedAlerts: c.ObservedRules` from ChaosSource.Edges and this reddens.
func TestChaosSourceCarriesMeasuredExpectedAlerts(t *testing.T) {
	injectedAt := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	src := NewChaosSource(chaosLoaderFrom(ChaosCascade{
		Root: "root", Downstream: "down", Injections: 2, LatestInjectedAt: injectedAt,
		ObservedRules: []string{"Service-up/down", "SNMP-unreachable"},
	}))
	edges, err := src.Edges(context.Background())
	if err != nil || len(edges) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(edges))
	}
	got := edges[0].ExpectedAlerts
	if len(got) != 2 || got[0] != "Service-up/down" || got[1] != "SNMP-unreachable" {
		t.Errorf("chaos edge ExpectedAlerts = %v, want the measured [Service-up/down SNMP-unreachable]", got)
	}
	// A cascade with NO recorded rules leaves ExpectedAlerts empty (unmeasured), never a fabricated expectation.
	src0 := NewChaosSource(chaosLoaderFrom(ChaosCascade{Root: "r", Downstream: "d", Injections: 1, LatestInjectedAt: injectedAt}))
	e0, err := src0.Edges(context.Background())
	if err != nil || len(e0) != 1 {
		t.Fatalf("Edges: %v, %d edges (want 1)", err, len(e0))
	}
	if len(e0[0].ExpectedAlerts) != 0 {
		t.Errorf("unmeasured cascade ExpectedAlerts = %v, want none (no recorded rule is 'no measurement', not an expectation)", e0[0].ExpectedAlerts)
	}
}

// The expected-alert set follows the WINNING provenance on the same rule as delay/recovery: a lower-confidence
// LEARNED re-seed must never overwrite a chaos-MEASURED set on a shared edge key — which the previous
// unconditional replacement allowed. Killing mutation: restore the unconditional
// `if len(e.ExpectedAlerts) > 0 { cur.ExpectedAlerts = ... }` in Upsert and the first sub-case reddens.
func TestUpsertExpectedAlertsFollowWinningProvenance(t *testing.T) {
	from, to, rel := Entity{Type: TypeHost, Name: "dep"}, Entity{Type: TypeHost, Name: "root"}, RelDependsOn
	measured := []string{"Service-up/down"}
	learned := []string{"guessed-rule"}

	// chaos first (0.90, measured set), then a learned re-seed (0.75, its own set) — the measured set SURVIVES.
	g1 := NewGraph()
	g1.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, ExpectedAlerts: measured})
	g1.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, ExpectedAlerts: learned})
	if got := g1.edges[edgeKey(from, to, rel)].ExpectedAlerts; len(got) != 1 || got[0] != "Service-up/down" {
		t.Errorf("after a learned re-seed over a chaos edge, ExpectedAlerts = %v, want the measured %v", got, measured)
	}

	// learned first, then chaos — chaos wins the confidence AND the expected-alert set.
	g2 := NewGraph()
	g2.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, ExpectedAlerts: learned})
	g2.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, ExpectedAlerts: measured})
	if got := g2.edges[edgeKey(from, to, rel)].ExpectedAlerts; len(got) != 1 || got[0] != "Service-up/down" {
		t.Errorf("after a chaos edge over a learned edge, ExpectedAlerts = %v, want the measured %v", got, measured)
	}

	// a same-source re-seed still refreshes the set (the winner-or-tie path) — a fresh drill's measurement wins.
	g3 := NewGraph()
	g3.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, ExpectedAlerts: []string{"old-rule"}})
	g3.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, ExpectedAlerts: measured})
	if got := g3.edges[edgeKey(from, to, rel)].ExpectedAlerts; len(got) != 1 || got[0] != "Service-up/down" {
		t.Errorf("a same-source re-seed did not refresh the set: got %v, want %v", got, measured)
	}

	// an EMPTY set never clobbers a measured one — absent is "no measurement", not "expect nothing".
	g4 := NewGraph()
	g4.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90, ExpectedAlerts: measured})
	g4.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceChaos, Confidence: 0.90})
	if got := g4.edges[edgeKey(from, to, rel)].ExpectedAlerts; len(got) != 1 || got[0] != "Service-up/down" {
		t.Errorf("an empty set clobbered a measured one: got %v, want %v", got, measured)
	}

	// the DECLARED set on a higher-confidence edge also survives a lower-confidence learned writer — the ratchet
	// protects any incumbent measured/declared set, not only chaos.
	g5 := NewGraph()
	g5.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceDeclared, Confidence: 0.85, ExpectedAlerts: []string{"declared-rule"}})
	g5.Upsert(Edge{From: from, To: to, Rel: rel, Source: SourceIncident, Confidence: 0.75, ExpectedAlerts: learned})
	if got := g5.edges[edgeKey(from, to, rel)].ExpectedAlerts; len(got) != 1 || got[0] != "declared-rule" {
		t.Errorf("a lower-confidence learned writer stomped a declared set: got %v, want [declared-rule]", got)
	}
}
