package estate

import (
	"slices"
	"testing"
)

// TG-180: the observation census must count EVERY host-like entity the graph knows, not only TypeHost. A
// PVE guest the graph carries as TypeLXC/TypeVM and a hypervisor carried as TypePVENode were invisible to
// the TypeHost-only census — neither observed nor counted unobservable. FreshObservableNames widens the
// denominator to the same concrete-machine set siblingParentEligible uses; FreshHostNames is unchanged.
// KILLING MUTATION: filter FreshObservableNames on TypeHost — the guest and the node vanish and this fails.
func TestFreshObservableNamesCountsGuestsAndNodes(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("app01"), To: pveNode("pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	obs := g.FreshObservableNames()
	for _, want := range []string{"app01", "pve01"} {
		if !slices.Contains(obs, want) {
			t.Fatalf("FreshObservableNames = %v, missing %q — a guest/node the graph knows is invisible to the census denominator", obs, want)
		}
	}
	if hosts := g.FreshHostNames(); slices.Contains(hosts, "app01") || slices.Contains(hosts, "pve01") {
		t.Fatalf("FreshHostNames must stay TypeHost-only (its other callers depend on it), got %v", hosts)
	}
	if !slices.IsSorted(obs) {
		t.Fatalf("names must be sorted for a deterministic census, got %v", obs)
	}
}
