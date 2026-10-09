package authlog

import (
	"strings"
	"testing"
	"time"
)

// TestToEnvelopeAggregateSweep — the aggregate enumeration-sweep envelope (TG-421). Its summary carries the
// distinct-principal COUNT and NAMES the loudest principal (so a targeted attack inside a spray is not masked
// by the aggregation), and its ExternalRef carries the sweep marker and is STABLE across polls of the same
// window — a later poll seeing a different subset (and a different loudest) must dedup to the SAME incident,
// not re-mint it every poll.
func TestToEnvelopeAggregateSweep(t *testing.T) {
	m := New(WithClock(fixedClock())) // fixedClock == 2026-08-06 12:00Z; the sweep must be in its past
	seen := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	agg := Event{
		Host: "web01", Kind: KindFailure, DistinctPrincipals: 40, Count: 43, Principal: "root",
		FirstSeen: seen, LastSeen: seen.Add(90 * time.Second),
	}

	env, err := m.ToEnvelope(agg)
	if err != nil {
		t.Fatalf("ToEnvelope(aggregate): %v", err)
	}
	if !strings.Contains(env.Summary, "enumeration sweep") || !strings.Contains(env.Summary, "40 distinct") {
		t.Errorf("aggregate summary missing the sweep shape (distinct-principal count): %q", env.Summary)
	}
	if !strings.Contains(env.Summary, "root") {
		t.Errorf("aggregate summary must NAME the loudest principal so a targeted attack is not masked: %q", env.Summary)
	}
	if !strings.Contains(env.ExternalRef, "enumeration-sweep") {
		t.Errorf("aggregate ExternalRef must carry the sweep marker, not a principal: %q", env.ExternalRef)
	}

	// STABILITY: a second poll of the SAME window with a DIFFERENT loudest and DIFFERENT distinct count must
	// dedup to the SAME ExternalRef — the sweep is one ongoing incident, not re-minted each poll.
	other := agg
	other.Principal = "admin"
	other.DistinctPrincipals = 37
	env2, err := m.ToEnvelope(other)
	if err != nil {
		t.Fatalf("ToEnvelope(aggregate 2): %v", err)
	}
	if env.ExternalRef != env2.ExternalRef {
		t.Errorf("aggregate ExternalRef changed across polls of the same window (%q vs %q) — a sweep whose "+
			"membership shifts poll to poll must stay ONE incident", env.ExternalRef, env2.ExternalRef)
	}

	// And the aggregate ref must NOT collide with the per-principal ref for the same host/kind/window: the
	// sweep is a distinct incident from any one account's failures.
	individual := Event{Host: "web01", Kind: KindFailure, Principal: "root", Count: 3, FirstSeen: seen, LastSeen: seen}
	envI, err := m.ToEnvelope(individual)
	if err != nil {
		t.Fatalf("ToEnvelope(individual): %v", err)
	}
	if env.ExternalRef == envI.ExternalRef {
		t.Errorf("the aggregate sweep ref collides with the per-principal ref for root: %q", env.ExternalRef)
	}
}
