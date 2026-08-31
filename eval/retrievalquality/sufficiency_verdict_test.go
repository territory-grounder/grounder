package retrievalquality

import (
	"testing"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// The retrieval-sufficiency verdict (TG-214, the CRAG-analog stage after the TG-50 min-relevance floor) is
// exercised here END-TO-END against the REAL LexicalRetriever over the same labeled fixture the retrieval-
// quality floor uses — not hand-built hits. This is the lock-free correctness evidence for the verdict: the
// seed-render behavior it drives is behind TG_RETRIEVE_SUFFICIENCY and its effect on proposal quality is
// measured separately by the behavior eval gate, but the verdict's TWO load-bearing properties are provable
// deterministically here.

func refsOfHits(hits []knowledge.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Incident.ExternalRef)
	}
	return out
}

// PROPERTY 1 — never suppress a genuinely-relevant precedent. Every labeled case has a real precedent in the
// corpus that the retriever surfaces (the floor test proves recall≥0.80); the sufficiency verdict must judge
// each such set ADEQUATE, so arming the signal can never replace a real precedent with "no adequate precedent".
func TestSufficiencyVerdictNeverSuppressesRelevantPrecedent(t *testing.T) {
	r := knowledge.NewLexicalRetriever(fixtureCorpus())
	const k = 3
	for _, c := range fixtureCases() {
		hits := r.Retrieve(c.Query, k)
		if !knowledge.HasAdequatePrecedent(c.Query, hits, 0) {
			t.Errorf("case %q: a labeled-relevant precedent was retrieved (%v) but the sufficiency verdict judged the set INADEQUATE — arming it would wrongly suppress a real precedent",
				c.Name, refsOfHits(hits))
		}
	}
}

// PROPERTY 2 — fire on a genuinely novel incident. Neither the rule (CacheEviction) nor the host (cache09)
// exists in the corpus, so retrieval surfaces only weak site+tag overlap. The verdict must judge that
// INADEQUATE so the seed says "no adequate precedent" instead of padding with an off-target restart-nginx
// row. The len(hits)>0 guard keeps the assertion non-vacuous: an empty retrieval is trivially "inadequate",
// which would prove nothing about the weak-hits-present path this test exists to cover.
func TestSufficiencyVerdictFiresOnNovelIncident(t *testing.T) {
	r := knowledge.NewLexicalRetriever(fixtureCorpus())
	const k = 3
	novel := knowledge.Query{Host: "cache09", AlertRule: "CacheEviction", Site: "nl", Tags: []string{"web"}}
	hits := r.Retrieve(novel, k)
	if len(hits) == 0 {
		t.Fatal("fixture expectation broken: the novel query should still retrieve weak site/tag overlap hits — otherwise this test proves nothing about the inadequate-set path")
	}
	if knowledge.HasAdequatePrecedent(novel, hits, 0) {
		t.Errorf("a novel incident retrieving only weak site/tag overlap (%v) must be judged INADEQUATE (no shared rule/host, no strong semantic match), got adequate",
			refsOfHits(hits))
	}
}
