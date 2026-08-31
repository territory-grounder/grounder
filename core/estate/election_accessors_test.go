package estate

import (
	"fmt"
	"testing"
	"time"
)

// InDegree + RunsOnParent are the two estate reads the cascade-collapse election is built on (TG-385): the
// causal weight of a node (how many things fall with it) and its authoritative placement parent. They must
// answer over the SAME multi-source, canonical-name, freshness-filtered graph every other read uses, or the
// election reasons over a different topology than the prediction gate.
func TestInDegreeAndRunsOnParent_OverContainment(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return now }))

	// A hypervisor with 39 guests running on it — the pve03 cascade in miniature.
	for i := 0; i < 39; i++ {
		g.Upsert(Edge{From: lxc(fmt.Sprintf("vm%02d", i)), To: pveNode("dc1pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	}
	// A second, unrelated node carrying one guest — so in-degree is a real count, not a global constant.
	g.Upsert(Edge{From: lxc("lonely"), To: pveNode("dc1pve04"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	// The hypervisor's causal weight is its guest count; a guest's is ~none.
	if got := g.InDegree(Entity{Name: "dc1pve03"}); got != 39 {
		t.Fatalf("InDegree(pve03) = %d, want 39 (the dependents that fall with it) — the election's primary rule reads this", got)
	}
	if got := g.InDegree(Entity{Name: "dc1pve04"}); got != 1 {
		t.Fatalf("InDegree(pve04) = %d, want 1", got)
	}
	if got := g.InDegree(Entity{Name: "vm00"}); got != 0 {
		t.Fatalf("InDegree(vm00) = %d, want 0 — nothing depends on a guest, so it must not out-rank its host", got)
	}
	if got := g.InDegree(Entity{Name: "ghost-not-in-graph"}); got != 0 {
		t.Fatalf("InDegree(unknown) = %d, want 0 — an unknown host makes no centrality claim", got)
	}

	// The placement parent is the hypervisor, matched by canonical name (domain-qualified reference resolves).
	p, ok := g.RunsOnParent(Entity{Name: "vm07.nllei.lan"})
	if !ok || canonName(p.Name) != "dc1pve03" {
		t.Fatalf("RunsOnParent(vm07) = %v ok=%v, want the pve03 hypervisor — the election's second tie-break reads this", p, ok)
	}
	if _, ok := g.RunsOnParent(Entity{Name: "dc1pve03"}); ok {
		t.Fatal("RunsOnParent(pve03) resolved a parent — a top hypervisor runs on nothing here, so it must be not-found")
	}
}

// A stale (expired) containment edge must not count toward in-degree or a placement parent — the election
// reads the LIVE graph the gate sees, so a hypervisor whose guests all aged out is no longer central. This
// is the freshness discipline TG-449 made load-bearing: a count off ever-seen edges reads healthy exactly
// while the source that fed them has gone quiet.
func TestInDegree_ExcludesExpiredEdges(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	g := NewGraph(WithClock(func() time.Time { return now }))

	g.Upsert(Edge{From: lxc("fresh"), To: pveNode("pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE, ValidUntil: now.Add(time.Hour)})
	g.Upsert(Edge{From: lxc("stale"), To: pveNode("pve03"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE, ValidUntil: now.Add(-time.Hour)})

	if got := g.InDegree(Entity{Name: "pve03"}); got != 1 {
		t.Fatalf("InDegree(pve03) = %d, want 1 — the expired edge must not count, or a dead source inflates causal weight", got)
	}
	if p, ok := g.RunsOnParent(Entity{Name: "stale"}); ok {
		t.Fatalf("RunsOnParent(stale) resolved %v over an EXPIRED edge — the election would place a guest on an aged-out host", p)
	}
}
