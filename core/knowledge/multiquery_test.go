package knowledge

import (
	"strings"
	"testing"
)

func mqHits(refs ...string) []Hit {
	out := make([]Hit, 0, len(refs))
	for _, r := range refs {
		out = append(out, Hit{Incident: Incident{ExternalRef: r}})
	}
	return out
}

func mqRefs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Incident.ExternalRef
	}
	return out
}

// fakeBase returns MORE for a host-relaxed query than for the exact-host one — modelling the production fused
// retriever, where the broadened query embeds to different semantic neighbours and surfaces a same-fault-class
// precedent the exact query missed.
type fakeBase struct{ counted int }

func (b *fakeBase) Retrieve(q Query, _ int) []Hit {
	if strings.TrimSpace(q.Host) != "" {
		return mqHits("TG-A") // exact-host query: only the exact match
	}
	return mqHits("TG-A", "TG-B") // host-relaxed: surfaces the cross-host precedent too
}
func (b *fakeBase) Count(string, string) int { b.counted++; return 7 }

// The core value: multi-query fuses the broadened variant's extra recall into the top-k, surfacing a precedent
// the exact query alone never returned. KILLING MUTATION: make queryVariants return only the original → TG-B
// never surfaces and this fails.
func TestMultiQuerySurfacesBroadenedRecall(t *testing.T) {
	m := &MultiQueryRetriever{Base: &fakeBase{}}
	got := m.Retrieve(Query{Host: "web01", AlertRule: "NginxDown"}, 5)
	refs := mqRefs(got)
	if len(got) != 2 || refs[0] != "TG-A" {
		t.Fatalf("got %v, want [TG-A TG-B] — TG-A (in both variants) first, TG-B lifted from the host-relaxed variant", refs)
	}
	foundB := false
	for _, r := range refs {
		if r == "TG-B" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("TG-B (only in the host-relaxed variant) was not surfaced — multi-query added no recall: %v", refs)
	}
}

// A query with no host has nothing to relax, so multi-query is a NO-OP: it returns exactly the base ranking.
func TestMultiQueryNoHostReducesToBase(t *testing.T) {
	base := &fakeBase{}
	m := &MultiQueryRetriever{Base: base}
	// no host → queryVariants yields just the original → the single-variant path returns base.Retrieve verbatim.
	got := m.Retrieve(Query{AlertRule: "NginxDown"}, 5)
	if want := mqRefs(base.Retrieve(Query{AlertRule: "NginxDown"}, 5)); strings.Join(mqRefs(got), ",") != strings.Join(want, ",") {
		t.Fatalf("no-host query: multi-query = %v, want the base ranking %v", mqRefs(got), want)
	}
}

// rrfMergeHits fuses ranks: a doc surfaced by TWO lists outscores one surfaced by a single list at the same rank.
func TestRRFMergeHitsFusesRanks(t *testing.T) {
	lists := [][]Hit{
		mqHits("TG-A", "TG-B"), // A rank1, B rank2
		mqHits("TG-A", "TG-C"), // A rank1, C rank2
	}
	got := mqRefs(rrfMergeHits(lists, 5))
	// A: 1/61 + 1/61; B: 1/62; C: 1/62. A first; B before C by ExternalRef tiebreak.
	if len(got) != 3 || got[0] != "TG-A" {
		t.Fatalf("got %v, want TG-A first (in both lists)", got)
	}
	if got[1] != "TG-B" || got[2] != "TG-C" {
		t.Fatalf("got %v, want [TG-A TG-B TG-C] (B before C by ref tiebreak on equal RRF)", got)
	}
}

// Count is delegated to the base (query broadening does not change a signature's corpus count).
func TestMultiQueryCountDelegates(t *testing.T) {
	base := &fakeBase{}
	m := &MultiQueryRetriever{Base: base}
	if got := m.Count("web01", "NginxDown"); got != 7 {
		t.Fatalf("Count = %d, want the base's 7 (delegated)", got)
	}
	if base.counted != 1 {
		t.Fatalf("Count did not delegate to the base (base.counted=%d)", base.counted)
	}
}
