package retrievaleval

import (
	"fmt"
	"math"
	"testing"

	knowledge "github.com/territory-grounder/grounder/core/knowledge"
)

// itoaRef builds a stable, distinctive ExternalRef for a crafted fixture row.
func itoaRef(prefix string, i int) string { return fmt.Sprintf("%s-%02d", prefix, i) }

// refsOf lists the ExternalRefs of a hit slice, for failure messages.
func refsOf(hits []knowledge.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Incident.ExternalRef
	}
	return out
}

// TG-508 arming evidence — the measurement the ticket's must-resolve-before-arming set requires: run
// leave-one-out resolution-recall over the SHIPPED seed with IDF-tag weighting OFF (shipped) vs ON (armed)
// and confirm, on the REAL corpus (not just the hand-built unit fixture), that arming does not REGRESS the
// honest retrieval-quality metric.
//
// The honest bar is NO-REGRESSION, not a lift. IDF weighting exists to stop BOILERPLATE tag-sets outranking
// rare curated ones — and boilerplate rows carry no resolution, so they never enter resolution-recall's
// denominator (idf_tag_weight_test.go). The metric is therefore largely BLIND to the class of rows IDF
// re-ranks; a neutral result here is expected and is itself the finding ("IDF helps a class this metric
// cannot see; it must at least not hurt the class it can"). A regression, by contrast, would be a real
// reason not to arm. So: assert ON >= OFF (within tolerance) and hold the same of-findable floor; report the
// delta honestly either way.
func TestResolutionRecall_IDFArmingDoesNotRegressRealCorpus(t *testing.T) {
	corpus := loadSeedCorpus(t)
	if len(corpus) < 10 {
		t.Fatalf("seed corpus too small to measure: %d rows", len(corpus))
	}

	const k, thr = 3, 0.5
	off := ResolutionRecall(corpus, k, thr)
	on := ResolutionRecallIDF(corpus, k, thr)

	t.Logf("TG-508 IDF arming — resolution-recall@%d (thr %.2f) over %d-row shipped seed:", k, thr, len(corpus))
	t.Logf("  FLAT (shipped, OFF): of-findable=%.4f recall=%.4f retrieved=%d/findable=%d", off.OfFindable, off.Recall, off.Retrieved, off.Findable)
	t.Logf("  IDF  (armed,   ON ): of-findable=%.4f recall=%.4f retrieved=%d/findable=%d", on.OfFindable, on.Recall, on.Retrieved, on.Findable)
	t.Logf("  DELTA of-findable = %+.4f  (>=0 required to arm; boilerplate rows IDF re-ranks are invisible to this metric)", on.OfFindable-off.OfFindable)

	// No-regression is the arming gate: IDF must not lower the honest, resolution-derived quality number.
	if on.OfFindable < off.OfFindable-1e-9 {
		t.Errorf("IDF arming REGRESSED of-findable resolution-recall: %.4f (ON) < %.4f (OFF) — do not arm", on.OfFindable, off.OfFindable)
	}
	// And it must still clear the standing ratchet floor (the flat path's guard, restated so arming can't
	// slip the retriever under it via a re-rank).
	const ofFindableFloor = 0.90
	if on.OfFindable < ofFindableFloor {
		t.Errorf("IDF-armed of-findable %.4f below the %.2f ratchet floor", on.OfFindable, ofFindableFloor)
	}
}

// TG-508 degenerate-regime oracle — the shape the ticket flagged as UNPROVEN by the unit fixture (which uses
// near-zero-IDF BOILERPLATE tags): several MODERATELY-common tags (df ~= N/2, idf ~= ln 2 ~= 0.69 each) on
// one candidate versus ONE truly-rare curated tag on another. The rare curated tag is the stronger
// fault-class signal, so the precedent carrying it must NOT be buried under a bag of merely-common tags. This
// is a BLACK-BOX test over the public Retrieve API — it lives in eval/ (not the gated core/knowledge/ tree)
// on purpose (see the package doc): it consumes the retriever, it does not alter it.
func TestIDFTagWeighting_RareCuratedTagBeatsSeveralModerateTags(t *testing.T) {
	var corpus []knowledge.Incident
	// 6 fillers carry the three moderate tags; 6 unrelated rows dilute N so df(moderate) ~= N/2.
	for i := 0; i < 6; i++ {
		corpus = append(corpus, knowledge.Incident{ExternalRef: itoaRef("mod-filler", i), Tags: []string{"mod1", "mod2", "mod3"}})
	}
	for i := 0; i < 6; i++ {
		corpus = append(corpus, knowledge.Incident{ExternalRef: itoaRef("misc", i), Tags: []string{itoaRef("uniq", i)}})
	}
	corpus = append(corpus,
		knowledge.Incident{ExternalRef: "generic", Tags: []string{"mod1", "mod2", "mod3"}}, // 3 moderate matches
		knowledge.Incident{ExternalRef: "precedent", Tags: []string{"rare1"}},              // 1 rare curated match
	)
	// N = 14; df(mod*) = 7 (6 fillers + generic) ~= N/2; df(rare1) = 1.
	q := knowledge.Query{Tags: []string{"mod1", "mod2", "mod3", "rare1"}}

	hits := knowledge.NewLexicalRetriever(corpus).SetIDFTags(true).Retrieve(q, 14)
	rank := func(ref string) int {
		for i, h := range hits {
			if h.Incident.ExternalRef == ref {
				return i
			}
		}
		return -1
	}
	pr, gr := rank("precedent"), rank("generic")
	var ps, gs float64
	for _, h := range hits {
		if h.Incident.ExternalRef == "precedent" {
			ps = h.Score
		}
		if h.Incident.ExternalRef == "generic" {
			gs = h.Score
		}
	}
	t.Logf("IDF moderate-regime: precedent(1 rare tag) score=%.4f rank=%d ; generic(3 moderate tags) score=%.4f rank=%d", ps, pr, gs, gr)

	if pr < 0 {
		t.Fatalf("precedent (rare curated tag) was not retrieved at all: %v", refsOf(hits))
	}
	// The single rare curated tag must weigh at least as much as the bag of three moderate tags — the rare
	// tag is the stronger same-fault-class signal, and a naive flat OR a cap-saturated sum would invert this.
	if gr >= 0 && gr < pr {
		t.Fatalf("moderate-tag 'generic' (rank %d) outranked the rare-curated 'precedent' (rank %d) under IDF — the degenerate misrank TG-508 must avoid", gr, pr)
	}
	if ps < gs-1e-9 {
		t.Fatalf("rare-curated precedent score %.4f < moderate-bag generic score %.4f under IDF", ps, gs)
	}
}

// TG-508 real-corpus grounding for the degenerate regime: the sum-of-IDF is capped (tagIDFCap) but the SHARED
// TAG COUNT is not, so in principle enough moderate tags could cap-saturate and outrank a rare tag. This
// measures the shipped seed's actual tag-DF distribution and the MOST tags any incident shares with another,
// so the regime the oracle pins is grounded in what the real corpus can produce — not an imagined shape.
func TestIDFTagWeighting_RealCorpusTagDistributionGroundsTheRegime(t *testing.T) {
	corpus := loadSeedCorpus(t)
	n := len(corpus)
	df := map[string]int{}
	maxTagsPerIncident := 0
	for _, inc := range corpus {
		seen := map[string]bool{}
		cnt := 0
		for _, tag := range inc.Tags {
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			df[tag]++
			cnt++
		}
		if cnt > maxTagsPerIncident {
			maxTagsPerIncident = cnt
		}
	}
	// Rarest / most-common curated tags, and how many distinct curated tags exist.
	minDF, maxDF := 1<<30, 0
	for _, d := range df {
		if d < minDF {
			minDF = d
		}
		if d > maxDF {
			maxDF = d
		}
	}
	t.Logf("TG-508 seed tag-DF: N=%d incidents, distinct tags=%d, max tags/incident=%d, df range=[%d..%d]", n, len(df), maxTagsPerIncident, minDF, maxDF)
	if len(df) == 0 {
		t.Skip("no tags in the seed corpus")
	}
	// Ground the cap-saturation worry: the largest POSSIBLE moderate-only IDF sum an incident could score is
	// bounded by (max tags it shares) * (IDF of a median-common tag). Report it against a single rarest-tag
	// IDF so the reader sees whether cap-saturation by common tags is even reachable on this corpus.
	idf := func(d int) float64 { return math.Log(float64(n) / float64(d)) }
	medianCommonIDF := idf(maxDF) // the most common tag = the weakest signal
	rarestIDF := idf(minDF)       // the rarest tag = the strongest signal
	worstModerateSum := float64(maxTagsPerIncident) * medianCommonIDF
	t.Logf("  IDF(most-common df=%d)=%.3f  IDF(rarest df=%d)=%.3f  worst all-common sum over %d tags=%.3f",
		maxDF, medianCommonIDF, minDF, rarestIDF, maxTagsPerIncident, worstModerateSum)
	t.Logf("  (a single rarest curated tag scores %.3f; the retriever scales by weightTagIDF=0.5 and caps at tagIDFCap=4.0)", rarestIDF)

	// Sanity invariant, not a quality bar: the distribution must be coherent (rarest <= most common in df).
	if minDF > maxDF {
		t.Fatalf("incoherent tag-DF distribution: minDF %d > maxDF %d", minDF, maxDF)
	}
}

// TG-508 safety invariant — the property the tagIDFCap exists to preserve, and the one that actually bounds
// the degenerate common-tag-flood regime regardless of how many tags an incident shares: a same-RULE match
// must outrank a full-tag IDF match. IDF tag scoring is capped (tagIDFCap=4.0) strictly below the rule weight
// (weightRule=5.0), so even a row matching EVERY query tag (capped tag score) cannot displace a bare same-rule
// row. This holds at any tag count — including the real corpus's max of 9 — so the un-asserted 2.69-vs-2.47
// common-vs-rare margin the grounding test surfaces is moot: neither can reach the rule channel. Black-box.
func TestIDFTagWeighting_RuleMatchDominatesAnyTagFlood(t *testing.T) {
	var corpus []knowledge.Incident
	// A "tagflood" row carrying MANY (rare) tags the query also carries — the strongest possible tag score.
	flood := make([]string, 0, 12)
	q := knowledge.Query{AlertRule: "DiskWillFill", Tags: make([]string, 0, 12)}
	for i := 0; i < 12; i++ {
		tag := itoaRef("rare-tag", i) // each appears only on the flood row → maximal IDF
		flood = append(flood, tag)
		q.Tags = append(q.Tags, tag)
	}
	corpus = append(corpus,
		knowledge.Incident{ExternalRef: "tagflood", Tags: flood},                  // matches all 12 rare tags, NO rule
		knowledge.Incident{ExternalRef: "rulematch", AlertRule: "DiskWillFill"},    // matches the rule only, NO tags
	)
	// Dilute so the flood tags are genuinely rare (df=1) rather than corpus-wide.
	for i := 0; i < 6; i++ {
		corpus = append(corpus, knowledge.Incident{ExternalRef: itoaRef("filler", i), Tags: []string{itoaRef("other", i)}})
	}

	hits := knowledge.NewLexicalRetriever(corpus).SetIDFTags(true).Retrieve(q, 8)
	if len(hits) < 2 || hits[0].Incident.ExternalRef != "rulematch" {
		t.Fatalf("a same-rule match must outrank a 12-rare-tag IDF flood (tagIDFCap<weightRule); got order %v", refsOf(hits))
	}
}
