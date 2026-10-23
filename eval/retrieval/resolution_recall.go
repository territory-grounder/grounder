// Package retrievaleval measures the knowledge retriever's quality from OUTSIDE the package, using only
// its public API (NewLexicalRetriever/Query/Retrieve/Hit). It lives under eval/ — not core/knowledge/ —
// on purpose: it is a measurement that CONSUMES the retrieval plane, not part of the agent-behavior plane
// itself, so it must never be able to change what the agent retrieves. Keeping it here also keeps it out
// of the eval-evidence behavior gate honestly (it cannot regress judged quality), while its ratchet still
// fires on any real retriever regression, because that regression lands in the gated core/knowledge/ tree.
package retrievaleval

// Leave-one-out RESOLUTION-RECALL — TG-491's honest, no-hand-label retrieval-quality metric.
//
// THE PROBLEM TG-491 DEFENDS AGAINST. You cannot machine-invent per-query relevance gold labels
// (`must_retrieve_any_of`) without the number measuring the LABELLER instead of the retriever — and
// outcome-derived (op_class) labels were measured 96% recoverable from (host, alert_rule) and rejected
// before shipping (docs/PREDECESSOR-MECHANISM-INVENTORY.md). So the honest posture kept
// production-queries.json's labels deliberately empty, leaving only tie-saturation (distinctness), which
// measures the corpus, not the retriever.
//
// THE UNLOCK. There is a ground-truth signal that (1) no one invented and (2) the retriever never scores
// on: each incident's own recorded human RESOLUTION. LexicalRetriever.Retrieve ranks on
// rule/host/site/tags/summary + recency; `Resolution` is rendered to the agent but is NOT a scoring
// channel. So the question "did the retriever surface a prior incident carrying the SAME fix as the
// held-out one?" is EARNED, never circular — the retriever cannot see the answer it is being graded on.
// (This is the same "human resolution as the un-invented label" move the predecessor's
// session-outcome-truth uses, and the "outcome-anchored, not gold-labelled" path TG's own
// docs/TESTING-AND-BENCHMARK.md endorses.)
//
// THE METRIC. Leave-one-out over the corpus: for each incident I that carries a resolution, remove I,
// query the retriever with I's own (host, rule, site, summary, tags), and check whether any top-k hit's
// resolution matches I's. This measures the thing the corpus exists to do — "when this alert fires again,
// would memory surface a past incident with the applicable fix?" — with zero hand-labels and full
// reproducibility.
//
// WHY IT PUNISHES (NOT REWARDS) THE KNOWN TIE BUG. 92.5% of production top-k cuts were decided by
// alphabetical ExternalRef order among same-(host,rule) ties (retriever.go, ResolvedAt note). Where those
// tied rows carry DIFFERENT resolutions, the alphabetical cut can drop the applicable one — and
// resolution-recall drops with it. Unlike an op_class label (which rewarded keeping the tie bug), this
// metric moves the right way when the retriever improves.

import (
	"sort"
	"strings"

	knowledge "github.com/territory-grounder/grounder/core/knowledge"
)

// ResolutionRecallResult is the honest retrieval-quality readout. Every field is derived from recorded
// resolutions the retriever never scored on — no invented gold label anywhere.
type ResolutionRecallResult struct {
	K         int     // top-k the retriever was asked for (production uses 3)
	Threshold float64 // resolution-match token-overlap threshold (stated, not hidden)
	Denom     int     // incidents carrying a non-empty resolution (the population)
	Findable  int     // of Denom, those with >=1 OTHER incident sharing the resolution (a peer exists to find)
	Retrieved int     // of Denom, those where the retriever surfaced a same-resolution peer in top-k

	// Recall = Retrieved/Denom — of ALL resolution-bearing incidents, how many did the retriever help.
	Recall float64
	// Ceiling = Findable/Denom — the BEST any retriever could do over this corpus (context, not a score).
	Ceiling float64
	// OfFindable = Retrieved/Findable — the clean quality number: among the recoverable incidents, the
	// fraction the retriever actually surfaced. Ratchet on THIS.
	OfFindable float64
	// Gap = Ceiling-Recall — the retriever's MISS rate: findable applicable precedent it failed to surface.
	Gap float64

	// TrivialHostRule is the circularity guard: OfFindable for a degenerate retriever that ranks ONLY on
	// (host, rule) exact match (retriever's dominant channels). If the real OfFindable ~= this, resolution
	// recall is dominated by host/rule matching and the summary/tag/recency channels add little; a real
	// OfFindable ABOVE it is signal those channels earn. Reported so the number can never quietly become a
	// restatement of "same host, same rule".
	TrivialHostRule float64

	trivialHits int // accumulator for TrivialHostRule (over findable rows)
}

// sameResolution reports whether two recorded human resolutions describe the same fix. DETERMINISTIC —
// no model judge, no invented opinion — by token overlap over the SMALLER resolution (so a terse fix is
// not penalised for a verbose peer). Empty resolutions never match (unknown is not equal to unknown).
func sameResolution(a, b string, threshold float64) bool {
	ta, tb := tokenSet(a), tokenSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	small, large := ta, tb
	if len(tb) < len(ta) {
		small, large = tb, ta
	}
	inter := 0
	for k := range small {
		if _, ok := large[k]; ok {
			inter++
		}
	}
	return float64(inter)/float64(len(small)) >= threshold
}

// ResolutionRecall runs the leave-one-out resolution-recall eval over corpus at top-k, using threshold as
// the resolution-match cutoff, with the retriever's SHIPPED (flat tag) scoring. It is pure and deterministic.
func ResolutionRecall(corpus []knowledge.Incident, k int, threshold float64) ResolutionRecallResult {
	return resolutionRecall(corpus, k, threshold, false)
}

// ResolutionRecallIDF is ResolutionRecall with TG-508 IDF-tag weighting ARMED on the retriever — the same
// leave-one-out measurement, so a caller can compare the flat and IDF rankings over the SAME corpus and read
// the real-corpus lift (or its absence) the TG-508 arming step must confirm. IDF weighting demotes
// boilerplate tag-sets; whether that moves resolution-recall depends on whether the rows it re-ranks carry
// resolutions the metric can see — which is exactly what this comparison measures, honestly, either way.
func ResolutionRecallIDF(corpus []knowledge.Incident, k int, threshold float64) ResolutionRecallResult {
	return resolutionRecall(corpus, k, threshold, true)
}

// resolutionRecall is the shared engine. O(n^2) in the corpus size (fine at seed scale); each query rebuilds
// a retriever over corpus-minus-I so the held-out row can never match itself. idf arms the retriever's
// TG-508 tag-rarity weighting for this run; false is the shipped flat behaviour.
func resolutionRecall(corpus []knowledge.Incident, k int, threshold float64, idf bool) ResolutionRecallResult {
	res := ResolutionRecallResult{K: k, Threshold: threshold}
	if k <= 0 || len(corpus) < 2 {
		return res
	}
	for i, held := range corpus {
		if len(tokenSet(held.Resolution)) == 0 {
			continue // no ground truth on this row
		}
		res.Denom++

		// corpus minus the held-out row.
		rest := make([]knowledge.Incident, 0, len(corpus)-1)
		for j, inc := range corpus {
			if j != i {
				rest = append(rest, inc)
			}
		}

		// Is the answer even recoverable — does a peer with the same resolution exist at all?
		findable := false
		for _, inc := range rest {
			if sameResolution(inc.Resolution, held.Resolution, threshold) {
				findable = true
				break
			}
		}
		if findable {
			res.Findable++
		}

		// Did the REAL retriever surface a same-resolution peer in its top-k?
		q := knowledge.Query{Host: held.Host, AlertRule: held.AlertRule, Site: held.Site, Summary: held.Summary, Tags: held.Tags}
		hits := knowledge.NewLexicalRetriever(rest).SetIDFTags(idf).Retrieve(q, k)
		for _, h := range hits {
			if sameResolution(h.Incident.Resolution, held.Resolution, threshold) {
				res.Retrieved++
				break
			}
		}

		// Circularity guard: would a host+rule-ONLY ranker have recalled it? (Counts only over findable
		// rows, matching OfFindable's denominator.)
		if findable {
			if trivialHostRuleHit(rest, held, k, threshold) {
				res.trivialHits++
			}
		}
	}

	if res.Denom > 0 {
		res.Recall = float64(res.Retrieved) / float64(res.Denom)
		res.Ceiling = float64(res.Findable) / float64(res.Denom)
	}
	if res.Findable > 0 {
		res.OfFindable = float64(res.Retrieved) / float64(res.Findable)
		res.TrivialHostRule = float64(res.trivialHits) / float64(res.Findable)
	}
	res.Gap = res.Ceiling - res.Recall
	return res
}

// trivialHostRuleHit is the circularity baseline: rank rest by (host==, rule==) only, ties broken by
// ExternalRef (mirrors the retriever's tiebreak), take top-k, and ask the same resolution-match question.
func trivialHostRuleHit(rest []knowledge.Incident, held knowledge.Incident, k int, threshold float64) bool {
	type sc struct {
		inc   knowledge.Incident
		score int
	}
	scored := make([]sc, 0, len(rest))
	for _, inc := range rest {
		s := 0
		if eqFold(inc.AlertRule, held.AlertRule) {
			s += 2
		}
		if eqFold(inc.Host, held.Host) {
			s++
		}
		if s > 0 {
			scored = append(scored, sc{inc, s})
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].inc.ExternalRef < scored[j].inc.ExternalRef
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	for _, s := range scored {
		if sameResolution(s.inc.Resolution, held.Resolution, threshold) {
			return true
		}
	}
	return false
}

// tokenSet and eqFold are byte-identical copies of the knowledge package's unexported helpers (retriever.go).
// They are duplicated rather than exported because this metric's resolution-MATCH tokenisation is its own
// concern — the retriever never tokenises `Resolution` (it is not a scoring channel), so coupling the two
// would be wrong, not DRY. Copying keeps the measured numbers identical to the in-package prototype.
func tokenSet(s string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(tok) >= 3 { // skip trivial tokens
			set[tok] = struct{}{}
		}
	}
	return set
}

func eqFold(a, b string) bool { return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) }
