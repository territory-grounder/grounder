package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The PRODUCTION QUERY SET: 227 distinct (host, alert_rule) shapes transcribed from session_triage,
// covering 3,199 real incidents TG has actually triaged. It is the query half of TG's retrieval
// instrument; the relevance half (must_retrieve_any_of) is deliberately empty and needs an operator.
//
// WHY IT IS NOT DERIVED FROM eval/corpus.json. That fixture's sites are all "dc1" where the retrieval
// corpus uses "nl"/"gr" — vocabulary intersection EMPTY — and 14 of its 16 hosts do not exist in the
// corpus at all. Every query built on it would exercise a scorer with weightSite structurally dead and
// weightHost dead for 88% of queries: 2 of 5 signals, measured against a baseline already broken. A
// retrieval number computed there would have been confidently wrong, which is worse than absent.

type productionQuery struct {
	Host              string   `json:"host"`
	AlertRule         string   `json:"alert_rule"`
	Site              string   `json:"site"`
	ObservedIncidents int      `json:"observed_incidents"`
	MustRetrieveAnyOf []string `json:"must_retrieve_any_of"`
	NearMisses        []string `json:"adversarial_near_misses"`
	NearMissTotal     int      `json:"near_miss_total"`
	ResolvableInSeed  bool     `json:"resolvable_in_seed"`
	CutIsATie         bool     `json:"cut_is_a_tie"`
}

func loadProductionQueries(t *testing.T) []productionQuery {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "eval", "retrieval", "production-queries.json"))
	if err != nil {
		t.Fatalf("read production query set: %v", err)
	}
	var doc struct {
		Queries []productionQuery `json:"queries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse production query set: %v", err)
	}
	return doc.Queries
}

// TestProductionQueriesUseProductionVocabulary is the conformance guard that stops the eval/corpus.json
// trap from being reintroduced.
//
// KILLING MUTATION: add a query with site "dc1" (the fixture's vocabulary), or a host whose prefix
// disagrees with its site. RED — because such a query silently disables weightSite and measures a
// scorer that is not the one production runs.
func TestProductionQueriesUseProductionVocabulary(t *testing.T) {
	qs := loadProductionQueries(t)
	if len(qs) < 50 {
		t.Fatalf("vacuity floor: only %d queries — a set this small certifies nothing about a 3,199-incident population", len(qs))
	}
	for _, q := range qs {
		if q.Host == "" || q.AlertRule == "" {
			t.Errorf("query with empty host/rule: %+v", q)
			continue
		}
		// Production emits sites "NL"/"GR" (modules/ingest/librenms/normalize.go); the corpus stores them
		// lowercased. Anything else — notably the fixture's "dc1" — is a vocabulary the scorer cannot match.
		if q.Site != "nl" && q.Site != "gr" && q.Site != "" {
			t.Errorf("query %s/%s carries site %q, which no corpus row uses: weightSite is dead for it",
				q.Host, q.AlertRule, q.Site)
		}
		// The site must agree with the host's own prefix, or the row is internally inconsistent.
		switch {
		case strings.HasPrefix(q.Host, "nllei") && q.Site != "nl":
			t.Errorf("host %s implies site nl, got %q", q.Host, q.Site)
		case strings.HasPrefix(q.Host, "grskg") && q.Site != "gr":
			t.Errorf("host %s implies site gr, got %q", q.Host, q.Site)
		}
	}
}

// TestProductionQueryLabelsAreHonestlyAbsent pins the deliberate hole, so nobody later reads an empty
// label set as "the retriever passed" — and so that filling it becomes a visible, intentional act.
//
// KILLING MUTATION: populate must_retrieve_any_of with machine-invented labels. This test then fails,
// forcing whoever did it to say so — because an LLM-invented gold label turns every later hit@k into a
// measurement of the labeller rather than of the retriever.
func TestProductionQueryLabelsAreHonestlyAbsent(t *testing.T) {
	qs := loadProductionQueries(t)
	labelled := 0
	for _, q := range qs {
		if len(q.MustRetrieveAnyOf) > 0 {
			labelled++
		}
	}
	if labelled > 0 {
		t.Fatalf("%d queries now carry relevance labels. If those came from a human, DELETE THIS TEST in "+
			"the same commit and say who labelled them. If they were machine-generated, they are not "+
			"ground truth and every hit@k computed from them measures the labeller.", labelled)
	}
	t.Logf("relevance labels: 0/%d by design — the query half of the instrument exists, the relevance "+
		"half needs an operator", len(qs))
}

// TestRealQueryTieSaturation measures the ratchet over REAL production queries rather than corpus
// leave-one-out, and is the answer to the n=1 limit of the first live observation.
//
// KILLING MUTATION: same as the LOO ratchet — neutralise a continuous channel in retriever.go; the
// number rises and clears the ceiling.
func TestRealQueryTieSaturation(t *testing.T) {
	corpus := loadSeedCorpus(t)
	r := NewLexicalRetriever(corpus)
	qs := loadProductionQueries(t)

	var evaluated, tied, volume, tiedVolume int
	for _, q := range qs {
		hits := r.Retrieve(Query{Host: q.Host, AlertRule: q.AlertRule, Site: q.Site}, retrieveK+1)
		if len(hits) <= retrieveK {
			continue
		}
		evaluated++
		volume += q.ObservedIncidents
		if hits[retrieveK-1].Score == hits[retrieveK].Score {
			tied++
			tiedVolume += q.ObservedIncidents
		}
	}
	if evaluated == 0 {
		t.Fatal("vacuity floor: no production query produced more than k candidates against the shipped seed")
	}
	got := float64(tied) / float64(evaluated)
	t.Logf("REAL-QUERY TIE SATURATION (shipped seed, n=%d resolvable of %d shapes): %d/%d = %.3f; "+
		"weighted by observed incidents %d/%d = %.3f", evaluated, len(qs), tied, evaluated, got,
		tiedVolume, volume, float64(tiedVolume)/float64(volume))
	t.Logf("AGAINST THE DEPLOYED CORPUS (seed+maintained, measured 2026-08-01): 149/179 = 0.832 per " +
		"shape, 2865/3121 = 0.918 weighted by real incident volume — two independent methods agreeing")

	if got > saturationCeiling {
		t.Fatalf("real-query tie saturation ROSE to %.3f (ceiling %.2f): retrieval became MORE "+
			"alphabetical on the queries production actually asks", got, saturationCeiling)
	}
}
