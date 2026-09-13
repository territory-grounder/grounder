package knowledge

import (
	"fmt"
	"testing"
)

// TG-508: with IDF-tag weighting ON, a BOILERPLATE-tagged row (tags carried by much of the corpus) must NOT
// outrank a real precedent that matches on a RARE, curated tag — even when the boilerplate row is a FULLER flat
// match. The flat weightTag*Jaccard (the shipped OFF path, and a naive weightTag bump) ranks the boilerplate
// full-match ABOVE the curated partial-match, and does so INVISIBLY to resolution_recall (boilerplate rows carry
// no resolution → never enter its denominator). Summing per-shared-tag IDF fixes it: a tag on ~all rows scores ~0.
//
// KILLING MUTATION: make the ON path flat (drop the r.idfTags branch, or SetIDFTags a no-op) → the boilerplate
// row ranks first under the "ON" retriever too → the second assertion goes RED.
func TestIDFTagsBoilerplateDoesNotOutrankCuratedPrecedent(t *testing.T) {
	var corpus []Incident
	// 15 filler rows make gov1/gov2/gov3 corpus-wide BOILERPLATE (near-zero IDF).
	for i := 0; i < 15; i++ {
		corpus = append(corpus, Incident{ExternalRef: fmt.Sprintf("filler-%02d", i), Tags: []string{"gov1", "gov2", "gov3"}})
	}
	corpus = append(corpus,
		Incident{ExternalRef: "boilerplate", Tags: []string{"gov1", "gov2", "gov3"}}, // a FULL boilerplate match
		Incident{ExternalRef: "precedent", Tags: []string{"nginx-oom", "gov1"}},      // the real precedent: a RARE curated tag
	)
	q := Query{Tags: []string{"gov1", "gov2", "gov3", "nginx-oom"}}

	firstRef := func(hits []Hit) string {
		if len(hits) == 0 {
			return "<none>"
		}
		return hits[0].Incident.ExternalRef
	}
	rankOf := func(hits []Hit, ref string) int {
		for i, h := range hits {
			if h.Incident.ExternalRef == ref {
				return i
			}
		}
		return -1
	}

	// Precondition — the bug: flat scoring ranks the boilerplate full-match FIRST (Jaccard 3/4 > precedent's 2/4).
	if got := firstRef(NewLexicalRetriever(corpus).Retrieve(q, 5)); got != "boilerplate" {
		t.Fatalf("precondition: flat scoring should rank 'boilerplate' first (the TG-508 bug), got %q", got)
	}

	// Fix — IDF-tag weighting: the rare curated tag wins; the boilerplate row does not outrank the precedent.
	idf := NewLexicalRetriever(corpus).SetIDFTags(true).Retrieve(q, 5)
	if got := firstRef(idf); got != "precedent" {
		t.Fatalf("IDF-tag weighting must rank the curated 'precedent' first, got %q", got)
	}
	if p, b := rankOf(idf, "precedent"), rankOf(idf, "boilerplate"); b >= 0 && (p < 0 || b < p) {
		t.Fatalf("boilerplate must not outrank the precedent under IDF: precedent rank=%d boilerplate rank=%d", p, b)
	}
}

// TG-508: the flag defaults OFF — a fresh retriever must score tags identically to the pre-TG-508 flat path,
// so the merge is dormant (behaviour changes only when SetIDFTags(true) is armed).
func TestIDFTagsDefaultOffIsFlatBehaviour(t *testing.T) {
	corpus := []Incident{
		{ExternalRef: "a", Host: "h1", Tags: []string{"x", "y"}},
		{ExternalRef: "b", Host: "h1", Tags: []string{"x"}},
	}
	q := Query{Host: "h1", Tags: []string{"x", "y"}}
	r := NewLexicalRetriever(corpus) // default: idfTags == false
	hits := r.Retrieve(q, 5)
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	// a matches both tags (jaccard 1.0) + host; b matches one tag (jaccard 0.5) + host → a ranks first, flat.
	if hits[0].Incident.ExternalRef != "a" {
		t.Fatalf("flat default must rank the fuller tag match 'a' first, got %q", hits[0].Incident.ExternalRef)
	}
	// exact flat score: weightHost(3) + weightTag(2)*jaccard(1) = 5.00 for a
	if hits[0].Score != 5.00 {
		t.Fatalf("flat default score drift: want a=5.00, got %.2f", hits[0].Score)
	}
}
