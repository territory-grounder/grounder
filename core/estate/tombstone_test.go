package estate

import (
	"context"
	"testing"
	"time"
)

// TG-375: a full-rebuild estate refresh deletes a dead hypervisor's guest→node parent edges the instant its
// PVE API goes silent — absence of evidence written as evidence of absence, into the ONE structure that folds
// N guest-down alerts into a single hypervisor incident. These oracles pin the tombstone-not-delete fix and
// the migration/destroyed discriminators that keep it from over-retaining.

var tsNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

func clockAt(t time.Time) Option { return WithClock(func() time.Time { return t }) }

func pveRunsOn(guest, node string) Edge {
	return Edge{
		From: Entity{Type: TypeLXC, Name: guest},
		To:   Entity{Type: TypePVENode, Name: node},
		Rel:  RelRunsOn, Source: SourcePVE, Confidence: 0.95,
	}
}

func edgeIn(g *Graph, guest, node string) (*Edge, bool) {
	e, ok := g.edges[edgeKey(Entity{Type: TypeLXC, Name: guest}, Entity{Type: TypePVENode, Name: node}, RelRunsOn)]
	return e, ok
}

// Headline: pve03 goes silent (all its guests vanish from the rebuild), pve01 stays up. The pve03 parents
// must SURVIVE with a reduced confidence, and pve03's blast radius must be non-empty again. Killing mutation:
// drop the Upsert in carryForwardUnreachable (or the call in Holder.Refresh) → the edges vanish → RED.
func TestTombstonePreservesUnreachableHypervisorEdges(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	for _, g := range []string{"guestA", "guestB", "guestC"} {
		prior.Upsert(pveRunsOn(g, "dc1pve03"))
	}
	prior.Upsert(pveRunsOn("guestX", "dc1pve01"))

	// The rebuild: pve03 answered nothing (its guests are gone), pve01 still reports guestX.
	next := NewGraph(clockAt(tsNow))
	next.Upsert(pveRunsOn("guestX", "dc1pve01"))

	kept := carryForwardUnreachable(prior, next, SourcePVE, false, indexPlacements(next))
	if kept != 3 {
		t.Fatalf("all 3 pve03 parents must be tombstoned when the node goes silent, kept=%d", kept)
	}
	for _, g := range []string{"guestA", "guestB", "guestC"} {
		e, ok := edgeIn(next, g, "dc1pve03")
		if !ok {
			t.Fatalf("%s→pve03 was DELETED — the topology is lost at the moment correlation needs it", g)
		}
		if e.Confidence != TombstoneConfidence {
			t.Errorf("%s→pve03 confidence = %v, want the reduced %v (unconfirmed, not a live read)", g, e.Confidence, TombstoneConfidence)
		}
		if e.ValidUntil.IsZero() || !e.ValidUntil.After(tsNow) {
			t.Errorf("%s→pve03 must carry a bounded future ValidUntil (the decommission clock), got %v", g, e.ValidUntil)
		}
	}
	// The actual point: pve03's blast radius is non-empty again — the 3 guests are affected if it fails.
	if imps := next.BlastRadius(Entity{Type: TypePVENode, Name: "dc1pve03"}, 3); len(imps) < 3 {
		t.Fatalf("pve03 blast radius = %d, want ≥3 — the tombstoned parents must fold the guest-down alerts into one incident", len(imps))
	}
}

// A guest that MIGRATED (reappears on another node) is a genuine delete, not a tombstone: the new-node edge
// supersedes the old, so the stale parent must NOT be carried.
func TestTombstoneDeletesMigratedGuest(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(pveRunsOn("guestM", "dc1pve01"))

	next := NewGraph(clockAt(tsNow))
	next.Upsert(pveRunsOn("guestM", "dc1pve02")) // migrated

	if kept := carryForwardUnreachable(prior, next, SourcePVE, false, indexPlacements(next)); kept != 0 {
		t.Fatalf("a migrated guest must not tombstone its old parent, kept=%d", kept)
	}
	if _, ok := edgeIn(next, "guestM", "dc1pve01"); ok {
		t.Error("guestM→pve01 (superseded by the migration) must be gone, not retained")
	}
}

// A guest DESTROYED on a still-up node (the node keeps hosting other guests) is a genuine delete — the API's
// silence about this one guest is positive evidence it ended, because its node clearly still answers.
func TestTombstoneDeletesDestroyedGuestOnLiveNode(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(pveRunsOn("guestGone", "dc1pve01"))
	prior.Upsert(pveRunsOn("guestStays", "dc1pve01"))

	next := NewGraph(clockAt(tsNow))
	next.Upsert(pveRunsOn("guestStays", "dc1pve01")) // pve01 up, still hosting guestStays

	if kept := carryForwardUnreachable(prior, next, SourcePVE, false, indexPlacements(next)); kept != 0 {
		t.Fatalf("a guest gone from a still-up node must be deleted, not tombstoned, kept=%d", kept)
	}
	if _, ok := edgeIn(next, "guestGone", "dc1pve01"); ok {
		t.Error("guestGone→pve01 must be deleted — the node is up, so the guest genuinely ended")
	}
}

// A re-emitted edge (steady state) is left exactly as the live read placed it — no tombstone, no downgrade.
func TestTombstoneIgnoresReEmittedEdge(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(pveRunsOn("guestS", "dc1pve01"))
	next := NewGraph(clockAt(tsNow))
	next.Upsert(pveRunsOn("guestS", "dc1pve01"))

	if kept := carryForwardUnreachable(prior, next, SourcePVE, false, indexPlacements(next)); kept != 0 {
		t.Fatalf("a re-emitted edge must not be tombstoned, kept=%d", kept)
	}
	if e, _ := edgeIn(next, "guestS", "dc1pve01"); e.Confidence != 0.95 {
		t.Errorf("a live re-read must keep 0.95, got %v", e.Confidence)
	}
}

// The decommission clock is anchored at FIRST disappearance and not reset each cycle: an already-tombstoned
// edge past its ValidUntil is dropped (treated as decommissioned); one still within it is carried with the
// SAME ValidUntil (not extended) so a node silent forever eventually drops.
func TestTombstoneDropsAfterTTLAndHoldsSteadyBefore(t *testing.T) {
	// Already-tombstoned, EXPIRED: ValidUntil in the past relative to the refresh clock → drop.
	expired := pveRunsOn("guestOld", "dc1pve03")
	expired.Confidence = TombstoneConfidence
	expired.ValidUntil = tsNow.Add(-time.Hour)
	priorExpired := NewGraph(clockAt(tsNow))
	priorExpired.Upsert(expired)
	// A partial picture (pve01 answered) so the TTL-expiry path is exercised, not the honest-empty early-out.
	nextA := NewGraph(clockAt(tsNow))
	nextA.Upsert(pveRunsOn("guestLive", "dc1pve01"))
	if kept := carryForwardUnreachable(priorExpired, nextA, SourcePVE, false, indexPlacements(nextA)); kept != 0 {
		t.Fatalf("a tombstone past its TTL must drop (decommissioned), kept=%d", kept)
	}

	// Already-tombstoned, STILL VALID: carried with the SAME ValidUntil, not re-extended to now+TTL.
	anchored := tsNow.Add(48 * time.Hour)
	valid := pveRunsOn("guestOld", "dc1pve03")
	valid.Confidence = TombstoneConfidence
	valid.ValidUntil = anchored
	priorValid := NewGraph(clockAt(tsNow))
	priorValid.Upsert(valid)
	nextB := NewGraph(clockAt(tsNow))
	nextB.Upsert(pveRunsOn("guestLive", "dc1pve01"))
	if kept := carryForwardUnreachable(priorValid, nextB, SourcePVE, false, indexPlacements(nextB)); kept != 1 {
		t.Fatalf("a tombstone still within its TTL must be carried, kept=%d", kept)
	}
	e, _ := edgeIn(nextB, "guestOld", "dc1pve03")
	if !e.ValidUntil.Equal(anchored) {
		t.Errorf("ValidUntil must be preserved (clock anchored at first disappearance), got %v want %v", e.ValidUntil, anchored)
	}
}

// Scope guard: only authoritative PVE runs_on is tombstoned. A learned edge or a NetBox edge that drops is
// left to its own lifecycle — this fix does not resurrect co-occurrence edges.
func TestTombstoneOnlyTouchesAuthoritativePVE(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(Edge{From: Entity{Type: TypeHost, Name: "hA"}, To: Entity{Type: TypeHost, Name: "hB"}, Rel: RelDependsOn, Source: SourceIncident, Confidence: 0.3})
	prior.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "gN"}, To: Entity{Type: TypePVENode, Name: "dc1pve09"}, Rel: RelRunsOn, Source: SourceNetbox, Confidence: 0.9})
	next := NewGraph(clockAt(tsNow))

	if kept := carryForwardUnreachable(prior, next, SourcePVE, false, indexPlacements(next)); kept != 0 {
		t.Fatalf("neither a learned depends_on nor a NetBox runs_on may be tombstoned by the PVE-scoped fix, kept=%d", kept)
	}
}

// Whole-cluster outage: the PVE source ERRORED (not one node), so no PVE placement answered — but the rebuild
// is non-empty because another source (NetBox) contributed. pveUnreadable=true authorises preserving the
// prior PVE parents even with no partial picture.
func TestTombstonePreservesAllWhenPVEReadFailed(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(pveRunsOn("guestP", "dc1pve05"))
	next := NewGraph(clockAt(tsNow))
	next.Upsert(Edge{From: Entity{Type: TypeHost, Name: "hK"}, To: Entity{Type: TypeSite, Name: "nllei"}, Rel: RelMemberOf, Source: SourceNetbox, Confidence: 0.9})

	if kept := carryForwardUnreachable(prior, next, SourcePVE, true, indexPlacements(next)); kept != 1 {
		t.Fatalf("a whole-cluster PVE read failure must preserve the prior parents, kept=%d", kept)
	}
	if _, ok := edgeIn(next, "guestP", "dc1pve05"); !ok {
		t.Error("guestP→pve05 must survive a total PVE API outage")
	}
}

// A healthy, fully-empty rebuild (no error, no placement) is a genuinely empty estate — prior PVE edges must
// NOT be resurrected, or a decommissioned-to-zero estate could never be represented (locks the regression the
// Holder honest-empty test caught).
func TestTombstoneDoesNotResurrectIntoHonestlyEmpty(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(pveRunsOn("guestZ", "dc1pve05"))
	next := NewGraph(clockAt(tsNow)) // healthy empty

	if kept := carryForwardUnreachable(prior, next, SourcePVE, false, indexPlacements(next)); kept != 0 {
		t.Fatalf("a healthy empty rebuild is an empty estate, not a silent node — nothing may be tombstoned, kept=%d", kept)
	}
	if next.Len() != 0 {
		t.Errorf("the empty rebuild must stay empty, got %d edges", next.Len())
	}
}

// Reachability: drive the tombstone through TWO real Holder.Refresh cycles with a SourcePVE EdgeSource — the
// full guest list, then one node's guests missing — and assert the parents survive in the LIVE h.Graph().
// This closes the "implemented ≠ reachable" gap: the unit tests call carryForwardUnreachable directly, so
// only this exercises the two-line wire-up in Holder.Refresh. Killing mutation: remove that call → the second
// refresh drops pve03 and this goes RED.
func TestHolderRefreshTombstonesUnreachablePVENode(t *testing.T) {
	full := []Edge{
		pveRunsOn("guestA", "dc1pve03"),
		pveRunsOn("guestB", "dc1pve03"),
		pveRunsOn("guestX", "dc1pve01"),
	}
	h := NewHolder(NewGraph(clockAt(tsNow)))
	if _, errs := h.Refresh(context.Background(), []EdgeSource{fakeSource{src: SourcePVE, edges: full}}, clockAt(tsNow)); len(errs) != 0 {
		t.Fatalf("first refresh must succeed: %v", errs)
	}
	// pve03 goes silent: its guests vanish from the cluster read while pve01 still answers.
	partial := []Edge{pveRunsOn("guestX", "dc1pve01")}
	if _, errs := h.Refresh(context.Background(), []EdgeSource{fakeSource{src: SourcePVE, edges: partial}}, clockAt(tsNow)); len(errs) != 0 {
		t.Fatalf("second refresh must succeed: %v", errs)
	}
	g := h.Graph()
	for _, guest := range []string{"guestA", "guestB"} {
		if _, ok := edgeIn(g, guest, "dc1pve03"); !ok {
			t.Fatalf("%s→pve03 was deleted across a real Holder.Refresh — the tombstone wiring is not reachable", guest)
		}
	}
	if imps := g.BlastRadius(Entity{Type: TypePVENode, Name: "dc1pve03"}, 3); len(imps) < 2 {
		t.Fatalf("pve03 blast radius = %d after a real refresh cycle, want ≥2 (guest-down alerts fold into one incident)", len(imps))
	}
}

// ─── vSphere (TG-521): the identical tombstone for a silently-dark vCenter ───

func vsphereRunsOn(vm, host string) Edge {
	return Edge{
		From: Entity{Type: TypeVM, Name: vm},
		To:   Entity{Type: TypePhysicalHost, Name: host},
		Rel:  RelRunsOn, Source: SourceVsphere, Confidence: 0.94,
	}
}

// vsphereEdgeIn is edgeIn for the vSphere edge shape (TypeVM→TypePhysicalHost), since edgeIn hardcodes the pve
// shape (TypeLXC→TypePVENode).
func vsphereEdgeIn(g *Graph, vm, host string) (*Edge, bool) {
	e, ok := g.edges[edgeKey(Entity{Type: TypeVM, Name: vm}, Entity{Type: TypePhysicalHost, Name: host}, RelRunsOn)]
	return e, ok
}

// A silently-dark ESXi host has its VMs' runs_on parents carried forward, exactly as pve03 does — the failure
// shape vSphere shares (TG-521). Killing mutation: revert the scope line in carryForwardUnreachable to a
// hardcoded SourcePVE → the SourceVsphere pass tombstones nothing → kept=0 → RED.
func TestTombstoneCarriesSilentVsphereHost(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	for _, vm := range []string{"web-01", "web-02", "db-01"} {
		prior.Upsert(vsphereRunsOn(vm, "esxi-a"))
	}
	prior.Upsert(vsphereRunsOn("app-01", "esxi-b"))

	// esxi-a went silent (its VMs vanished); esxi-b still reports app-01.
	next := NewGraph(clockAt(tsNow))
	next.Upsert(vsphereRunsOn("app-01", "esxi-b"))

	kept := carryForwardUnreachable(prior, next, SourceVsphere, false, indexPlacements(next))
	if kept != 3 {
		t.Fatalf("all 3 esxi-a VM parents must be tombstoned when the host goes silent, kept=%d", kept)
	}
	for _, vm := range []string{"web-01", "web-02", "db-01"} {
		e, ok := vsphereEdgeIn(next, vm, "esxi-a")
		if !ok {
			t.Fatalf("%s→esxi-a was DELETED — a silently-dark vCenter's topology is lost at the moment correlation needs it", vm)
		}
		if e.Confidence != TombstoneConfidence {
			t.Errorf("%s→esxi-a confidence = %v, want the reduced %v (unconfirmed, not a live read)", vm, e.Confidence, TombstoneConfidence)
		}
		if e.ValidUntil.IsZero() || !e.ValidUntil.After(tsNow) {
			t.Errorf("%s→esxi-a must carry a bounded future ValidUntil (the decommission clock), got %v", vm, e.ValidUntil)
		}
	}
	if imps := next.BlastRadius(Entity{Type: TypePhysicalHost, Name: "esxi-a"}, 3); len(imps) < 3 {
		t.Fatalf("esxi-a blast radius = %d, want ≥3 — the tombstoned parents must fold the VM-down alerts into one incident", len(imps))
	}
}

// The generalization is SOURCE-SCOPED: a carryForwardUnreachable pass for one hypervisor source must NOT
// tombstone another source's edges, or a silent vCenter cycle would resurrect stale pve edges (and vice versa)
// and cross-contaminate the two planes' outage accounting. Killing mutation: drop the `e.Source != source`
// guard → the SourceVsphere pass also carries the vanished pve edge → kept=2 → RED.
func TestTombstoneIsSourceScoped(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(vsphereRunsOn("web-01", "esxi-a")) // a vsphere edge that vanished
	prior.Upsert(pveRunsOn("guestA", "pve01"))      // a pve edge that ALSO vanished

	next := NewGraph(clockAt(tsNow))
	next.Upsert(vsphereRunsOn("app-01", "esxi-b")) // esxi-b answers → a partial picture

	// The vSphere pass must tombstone ONLY the vsphere edge, never the pve one.
	kept := carryForwardUnreachable(prior, next, SourceVsphere, false, indexPlacements(next))
	if kept != 1 {
		t.Fatalf("the vSphere pass tombstoned %d edges, want exactly 1 (its own) — it must not touch pve", kept)
	}
	if _, ok := edgeIn(next, "guestA", "pve01"); ok {
		t.Fatal("the vSphere pass tombstoned a SourcePVE edge — the source scope is broken; the two planes cross-contaminate")
	}
	if _, ok := vsphereEdgeIn(next, "web-01", "esxi-a"); !ok {
		t.Fatal("the vSphere pass did not carry its OWN silent-host edge")
	}
}

// Reachability: the vSphere tombstone must survive TWO real Holder.Refresh cycles — proving the SECOND wire-up
// line in Holder.Refresh (carryForwardUnreachable for SourceVsphere) is actually reached, the "implemented ≠
// reachable" guard for TG-521. Killing mutation: remove that holder line → the second refresh drops esxi-a → RED.
func TestHolderRefreshTombstonesUnreachableVsphereHost(t *testing.T) {
	full := []Edge{
		vsphereRunsOn("web-01", "esxi-a"),
		vsphereRunsOn("web-02", "esxi-a"),
		vsphereRunsOn("app-01", "esxi-b"),
	}
	h := NewHolder(NewGraph(clockAt(tsNow)))
	if _, errs := h.Refresh(context.Background(), []EdgeSource{fakeSource{src: SourceVsphere, edges: full}}, clockAt(tsNow)); len(errs) != 0 {
		t.Fatalf("first refresh must succeed: %v", errs)
	}
	// esxi-a goes silent: its VMs vanish from the vCenter read while esxi-b still answers.
	partial := []Edge{vsphereRunsOn("app-01", "esxi-b")}
	if _, errs := h.Refresh(context.Background(), []EdgeSource{fakeSource{src: SourceVsphere, edges: partial}}, clockAt(tsNow)); len(errs) != 0 {
		t.Fatalf("second refresh must succeed: %v", errs)
	}
	g := h.Graph()
	for _, vm := range []string{"web-01", "web-02"} {
		if _, ok := vsphereEdgeIn(g, vm, "esxi-a"); !ok {
			t.Fatalf("%s→esxi-a was deleted across a real Holder.Refresh — the vSphere tombstone wiring is not reachable", vm)
		}
	}
	if imps := g.BlastRadius(Entity{Type: TypePhysicalHost, Name: "esxi-a"}, 3); len(imps) < 2 {
		t.Fatalf("esxi-a blast radius = %d after a real refresh cycle, want ≥2 (VM-down alerts fold into one incident)", len(imps))
	}
}

// FINDING #1 (TG-521 review): the honest-empty guard must be PER-SOURCE. A source that produced ZERO edges
// without erroring is genuinely decommissioned-to-zero and must NOT have its prior edges resurrected — EVEN
// when the OTHER hypervisor is alive. The pre-fix guard tested cross-source placement, so a retired vCenter
// kept phantom VM→host tombstones for the whole 7-day TTL while pve ran. Killing mutation: revert the guard to
// cross-source (`len(idx.nodeHasGuest) == 0`) → pve's live guest makes it non-empty → the retired vsphere edges
// resurrect → kept=2 → RED. This is the regression the review caught; it ships fixed + pinned.
func TestTombstoneHonorsGenuinelyEmptySource(t *testing.T) {
	prior := NewGraph(clockAt(tsNow))
	prior.Upsert(vsphereRunsOn("web-01", "esxi-a")) // prior vSphere placements, now RETIRED (vCenter decommissioned)
	prior.Upsert(vsphereRunsOn("web-02", "esxi-a"))

	// The rebuild: vSphere produced NOTHING and did NOT error (genuinely retired), while PVE is alive with a guest.
	next := NewGraph(clockAt(tsNow))
	next.Upsert(pveRunsOn("guestX", "dc1pve01"))

	if kept := carryForwardUnreachable(prior, next, SourceVsphere, false, indexPlacements(next)); kept != 0 {
		t.Fatalf("a genuinely-empty (decommissioned) vSphere must NOT resurrect its prior edges while pve is alive, kept=%d — the honest-empty guard is not per-source", kept)
	}
	if _, ok := vsphereEdgeIn(next, "web-01", "esxi-a"); ok {
		t.Fatal("web-01→esxi-a was resurrected as a phantom tombstone — a retired vCenter would pollute blast-radius for a week")
	}

	// THE OTHER SIDE OF THE LINE: a vSphere OUTAGE (sourceUnreadable=true, the whole API errored) is NOT a
	// decommission — its silent edges MUST still be preserved. The per-source guard must not over-correct past this.
	if kept := carryForwardUnreachable(prior, next, SourceVsphere, true, indexPlacements(next)); kept != 2 {
		t.Fatalf("a vSphere OUTAGE (sourceUnreadable) must still tombstone its silent edges, kept=%d want 2 — an errored source is not a decommissioned one", kept)
	}
}

// FINDING #2 (TG-521 review): the two passes share ONE placement snapshot, so the pve pass's just-written
// tombstones never feed the vsphere pass's evidence. The SILENT pve guest and the SILENT vsphere VM here
// deliberately SHARE a canonical name ("shared") — that collision is what makes this test DEPEND on
// order-independence rather than merely prove "both tombstone in isolation" (the re-review flagged the
// collision-free version as a non-killing oracle). If the passes rebuilt their indices per-call from the
// mutated graph, the pve pass's "shared"→pve03 tombstone would make guestPresent["shared"] true for the
// vsphere pass, which would then read "shared"→esxi-a as a MIGRATION and DROP it. Killing mutation: in
// holder.go replace the shared `idx` with a per-call indexPlacements(g) at each site → shared→esxi-a is
// dropped → RED (verified).
func TestHolderRefreshTombstonesBothHypervisorsIndependently(t *testing.T) {
	pve := []Edge{pveRunsOn("shared", "dc1pve03"), pveRunsOn("gX", "dc1pve01")}
	vs := []Edge{vsphereRunsOn("shared", "esxi-a"), vsphereRunsOn("vmX", "esxi-b")}
	h := NewHolder(NewGraph(clockAt(tsNow)))
	srcs := []EdgeSource{fakeSource{src: SourcePVE, edges: pve}, fakeSource{src: SourceVsphere, edges: vs}}
	if _, errs := h.Refresh(context.Background(), srcs, clockAt(tsNow)); len(errs) != 0 {
		t.Fatalf("first refresh: %v", errs)
	}
	// pve03 AND esxi-a both go silent (the same-named guest/VM vanishes from both); pve01 and esxi-b still answer.
	pvePartial := []Edge{pveRunsOn("gX", "dc1pve01")}
	vsPartial := []Edge{vsphereRunsOn("vmX", "esxi-b")}
	srcs2 := []EdgeSource{fakeSource{src: SourcePVE, edges: pvePartial}, fakeSource{src: SourceVsphere, edges: vsPartial}}
	if _, errs := h.Refresh(context.Background(), srcs2, clockAt(tsNow)); len(errs) != 0 {
		t.Fatalf("second refresh: %v", errs)
	}
	g := h.Graph()
	if _, ok := edgeIn(g, "shared", "dc1pve03"); !ok {
		t.Fatal("shared→pve03 (silent pve host) was not tombstoned")
	}
	// THE order-independence assertion: the vsphere 'shared' VM must be tombstoned despite the same-named pve
	// guest being tombstoned first in the same Refresh — a per-pass index rebuild would drop this as a false migration.
	if _, ok := vsphereEdgeIn(g, "shared", "esxi-a"); !ok {
		t.Fatal("shared→esxi-a was dropped — the pve pass's tombstone leaked into the vsphere pass's migration check; the passes are not order-independent")
	}
}
