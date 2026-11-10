package estate

// TG-681 — the estate snapshot is a SHARED input of the agent seed, the predictor and the verifier. These
// oracles pin two facts about it: (1) a rejected refresh keeps serving the prior snapshot and every consumer
// keeps getting the same stale answers (the shared failure mode, stated rather than hidden), and (2) the
// holder now stamps the install time so that staleness is observable (Age grows through the outage).
//
// KILLING MUTATIONS (executed 2026-11-07, each RED, then reverted):
//  1. stamp refreshedAt in Refresh even when kept=true → TestARejectedRefreshKeepsServingTheStaleSnapshot
//     fails on "age must grow through a rejected refresh".
//  2. drop the stamp from NewHolderWithClock for a non-nil graph → TestABootGraphCountsAsInstalled fails.
//  3. stamp refreshedAt for a nil initial graph → TestANilPlaceholderReadsAsNeverInstalled fails.

import (
	"context"
	"testing"
	"time"
)

type tickingClock struct{ t time.Time }

func (c *tickingClock) now() time.Time { return c.t }
func (c *tickingClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func sharedInputGraph() *Graph {
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "web01"}, To: Entity{Type: TypePVENode, Name: "pve01"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "app01"}, To: Entity{Type: TypeLXC, Name: "web01"}, Rel: RelDependsOn, Confidence: 0.85, Source: SourceDeclared})
	return g
}

func TestABootGraphCountsAsInstalled(t *testing.T) {
	clk := &tickingClock{t: time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)}
	h := NewHolderWithClock(sharedInputGraph(), clk.now)
	at, ok := h.RefreshedAt()
	if !ok || !at.Equal(clk.t) {
		t.Fatalf("a boot-built graph is the serving snapshot and must stamp its install time, got (%v, %v)", at, ok)
	}
	clk.advance(90 * time.Second)
	if age, ok := h.Age(); !ok || age != 90*time.Second {
		t.Fatalf("age must be now - install, got (%v, %v)", age, ok)
	}
}

func TestANilPlaceholderReadsAsNeverInstalled(t *testing.T) {
	clk := &tickingClock{t: time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)}
	h := NewHolderWithClock(nil, clk.now)
	if h.Graph() == nil {
		t.Fatal("the placeholder graph must still be non-nil (the prediction path must not panic)")
	}
	if _, ok := h.RefreshedAt(); ok {
		t.Fatal("an empty placeholder was never installed from a source — its age must read UNKNOWN, never fresh")
	}
	if _, ok := h.Age(); ok {
		t.Fatal("Age over a never-installed holder must be ok=false")
	}
	var nilHolder *Holder
	if _, ok := nilHolder.Age(); ok {
		t.Fatal("a nil holder must answer ok=false, not panic")
	}
}

// THE SHARED FAILURE MODE, PINNED. A refresh whose every fallible source fails and whose rebuild is empty keeps
// the prior snapshot (the correct outage posture, TG-375). The predictor, the agent seed and the verifier all
// read the same pointer, so all three keep answering from the aging graph: Resolve still resolves the renamed
// or decommissioned guest, BlastRadius still names yesterday's dependents. Nothing in the graph itself says so;
// the ONLY signal is the holder's age, which must grow through the outage.
func TestARejectedRefreshKeepsServingTheStaleSnapshot(t *testing.T) {
	clk := &tickingClock{t: time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)}
	h := NewHolderWithClock(sharedInputGraph(), clk.now)
	installed, _ := h.RefreshedAt()

	clk.advance(3 * time.Hour)
	kept, errs := h.Refresh(context.Background(), []EdgeSource{errSource{}})
	if !kept || len(errs) != 1 {
		t.Fatalf("a failed, empty rebuild must be REJECTED and the prior graph kept, got kept=%v errs=%v", kept, errs)
	}
	// Every consumer of Graph() still sees the stale topology — the same answer for the seed, the predictor
	// and the verifier's site/collateral reads.
	if _, ok := h.Graph().Resolve("web01"); !ok {
		t.Fatal("the kept snapshot must still resolve its hosts — that is what makes the staleness shared")
	}
	if impacts := h.Graph().BlastRadius(Entity{Type: TypeLXC, Name: "web01"}, 3); len(impacts) != 1 || impacts[0].Entity.Name != "app01" {
		t.Fatalf("the kept snapshot must still name yesterday's dependents, got %v", impacts)
	}
	// The stamp must NOT have advanced: the serving snapshot is 3h old and the age says so.
	if at, _ := h.RefreshedAt(); !at.Equal(installed) {
		t.Fatalf("a rejected refresh must not re-stamp the install time (that would launder a stale snapshot as fresh): %v vs %v", at, installed)
	}
	if age, ok := h.Age(); !ok || age != 3*time.Hour {
		t.Fatalf("age must grow through a rejected refresh, got (%v, %v)", age, ok)
	}

	// An ACCEPTED refresh installs a new snapshot and resets the age.
	clk.advance(time.Minute)
	if kept, _ := h.Refresh(context.Background(), []EdgeSource{okSource{}}); kept {
		t.Fatal("a successful rebuild must be installed")
	}
	if age, ok := h.Age(); !ok || age != 0 {
		t.Fatalf("an accepted refresh resets the age to zero, got (%v, %v)", age, ok)
	}
	if _, ok := h.Graph().Resolve("web01"); ok {
		t.Fatal("after an accepted refresh from a source that no longer lists web01, the stale host must be gone")
	}
}

// Set is the other install path (the decay/disproof reconciler publishes through it): it stamps too, so a
// re-published graph never reads older than it is.
func TestSetStampsTheInstallTime(t *testing.T) {
	clk := &tickingClock{t: time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)}
	h := NewHolderWithClock(nil, clk.now)
	clk.advance(time.Hour)
	h.Set(sharedInputGraph())
	if at, ok := h.RefreshedAt(); !ok || !at.Equal(clk.t) {
		t.Fatalf("Set must stamp the install time, got (%v, %v)", at, ok)
	}
	clk.advance(time.Minute)
	h.Set(nil)
	if age, _ := h.Age(); age != time.Minute {
		t.Fatalf("Set(nil) is ignored and must not touch the stamp, got age %v", age)
	}
}
