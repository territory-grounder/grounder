package knowledge

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// rerankFakeBase records the k it was asked for (to prove the wide pull) and returns a fixed hit list cut to k.
type rerankFakeBase struct {
	hits  []Hit
	lastK int
}

func (b *rerankFakeBase) Retrieve(_ Query, k int) []Hit {
	b.lastK = k
	if k >= 0 && k < len(b.hits) {
		return append([]Hit(nil), b.hits[:k]...)
	}
	return append([]Hit(nil), b.hits...)
}

func rrHit(ref string) Hit {
	return Hit{Incident: Incident{ExternalRef: ref, AlertRule: "R", Summary: ref}, Score: 1, Reasons: []string{"base"}}
}

func rrRefs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Incident.ExternalRef)
	}
	return out
}

// rerankFake scores by a supplied index→score map (missing indices unscored) or returns err.
type rerankFake struct {
	byIndex  map[int]float64
	err      error
	gotQuery string
}

func (f *rerankFake) Rerank(_ context.Context, query string, texts []string) ([]RerankScore, error) {
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	var out []RerankScore
	for i := range texts {
		if s, ok := f.byIndex[i]; ok {
			out = append(out, RerankScore{Index: i, Score: s})
		}
	}
	return out, nil
}

func TestRerankOffIsBaseRanking(t *testing.T) {
	base := &rerankFakeBase{hits: []Hit{rrHit("A"), rrHit("B"), rrHit("C")}}
	r := &RerankRetriever{Base: base, Reranker: nil}
	if got, want := rrRefs(r.Retrieve(Query{}, 2)), []string{"A", "B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("nil Reranker must serve the base ranking (top-2), got %v", got)
	}
	if base.lastK != 2 {
		t.Errorf("OFF must retrieve exactly k from base (no widen), got k=%d", base.lastK)
	}
}

// The core: rerank reorders to the cross-encoder's top-k, and it WIDENS the base pull first. KILLING
// MUTATIONS: sort ascending ⇒ wrong top-k; retrieve k instead of widen ⇒ base.lastK==2.
func TestRerankReordersAndWidens(t *testing.T) {
	base := &rerankFakeBase{hits: []Hit{rrHit("A"), rrHit("B"), rrHit("C"), rrHit("D")}}
	f := &rerankFake{byIndex: map[int]float64{2: 0.9, 0: 0.5, 1: 0.1, 3: 0.05}} // C highest, then A
	r := &RerankRetriever{Base: base, Reranker: f, WidenTo: 10}
	if got, want := rrRefs(r.Retrieve(Query{AlertRule: "DiskFull", Host: "db1", Summary: "out of disk"}, 2)), []string{"C", "A"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rerank must reorder to the cross-encoder's top-k, got %v", got)
	}
	if base.lastK != 10 {
		t.Errorf("rerank must retrieve a WIDE set from base (WidenTo=10), got base k=%d", base.lastK)
	}
	if f.gotQuery != "DiskFull. db1. out of disk" {
		t.Errorf("rerank query text must be the incident identity with no embed prefix, got %q", f.gotQuery)
	}
}

func TestRerankDegradesToBaseOnError(t *testing.T) {
	base := &rerankFakeBase{hits: []Hit{rrHit("A"), rrHit("B"), rrHit("C")}}
	r := &RerankRetriever{Base: base, Reranker: &rerankFake{err: errors.New("reranker down")}}
	if got, want := rrRefs(r.Retrieve(Query{}, 2)), []string{"A", "B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a reranker error must degrade to the base ranking, got %v", got)
	}
}

// A reranker that scores only SOME candidates must never DROP the rest — they keep base order after the scored.
func TestRerankNeverDropsUnscoredCandidate(t *testing.T) {
	base := &rerankFakeBase{hits: []Hit{rrHit("A"), rrHit("B"), rrHit("C")}}
	r := &RerankRetriever{Base: base, Reranker: &rerankFake{byIndex: map[int]float64{1: 0.9}}, WidenTo: 10}
	if got, want := rrRefs(r.Retrieve(Query{}, 3)), []string{"B", "A", "C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a reranker scoring only B must not drop A/C (base order after B), got %v", got)
	}
}
