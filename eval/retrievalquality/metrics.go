// Package retrievalquality is a RAGAS-analog retrieval-quality eval for the knowledge retrieval plane
// (TG-50): context precision@k, recall@k, and mean-reciprocal-rank over a LABELED corpus, so a change to the
// retriever's scoring can be MEASURED — did it surface the relevant precedent, and how much noise did it
// admit — instead of asserted by a single golden string (the only retrieval test that exists today).
//
// It lives in eval/ (a consumer package), imports core/knowledge READ-ONLY, and changes no behavior path:
// the measurement is deliberately separate from the plane it measures, so the eval can be extended and
// re-run without touching — or triggering the eval gate on — the retriever itself. It is the baseline the
// later advanced-RAG stages (a configurable min-relevance threshold, multi-query + RRF fusion, rerank) are
// validated against.
package retrievalquality

import (
	"github.com/territory-grounder/grounder/core/knowledge"
)

// Labeled is one evaluation case: a query and the GROUND-TRUTH set of precedent ExternalRefs a correct
// retrieval should surface for it. Relevance is the label author's judgment, held here as data so the metric
// is reproducible and the corpus reviewable.
type Labeled struct {
	Name     string
	Query    knowledge.Query
	Relevant []string // the ExternalRefs genuinely relevant to Query
}

// Result is the aggregate retrieval quality over a labeled set at a given k.
type Result struct {
	K             int
	Cases         int
	MeanPrecision float64 // mean over cases of |retrieved ∩ relevant| / retrievedCount
	MeanRecall    float64 // mean over cases of |retrieved ∩ relevant| / |relevant|
	MeanMRR       float64 // mean over cases of 1 / (rank of the first relevant hit), 0 if none retrieved
}

// Evaluate runs each labeled case through the retriever at top-k and aggregates precision, recall, and MRR.
// A case with no declared relevant refs is skipped — it cannot contribute a recall — and Cases counts only
// the scored ones. The result is exactly the retriever's own ordering: deterministic, no sampling.
func Evaluate(r knowledge.Retriever, cases []Labeled, k int) Result {
	res := Result{K: k}
	var sumP, sumR, sumRR float64
	scored := 0
	for _, c := range cases {
		if len(c.Relevant) == 0 {
			continue
		}
		rel := make(map[string]bool, len(c.Relevant))
		for _, ref := range c.Relevant {
			rel[ref] = true
		}
		hits := r.Retrieve(c.Query, k)
		inter, firstRank := 0, 0
		for i, h := range hits {
			if rel[h.Incident.ExternalRef] {
				inter++
				if firstRank == 0 {
					firstRank = i + 1
				}
			}
		}
		if len(hits) > 0 {
			sumP += float64(inter) / float64(len(hits))
		}
		sumR += float64(inter) / float64(len(c.Relevant))
		if firstRank > 0 {
			sumRR += 1.0 / float64(firstRank)
		}
		scored++
	}
	res.Cases = scored
	if scored > 0 {
		res.MeanPrecision = sumP / float64(scored)
		res.MeanRecall = sumR / float64(scored)
		res.MeanMRR = sumRR / float64(scored)
	}
	return res
}
