package knowledge_test

// TestSeedCorpusRoundTrip is THE guard for the seeded predecessor corpus (TG-125), the same regression-trap
// oracle as core/lessons.TestNoveltyWritebackRoundTrip: the WRITER key (what the seed tool stored) MUST
// equal the READER key (what the classifier's novelty gate passes to knowledge.Count). The gate calls
// Count(host, env.AlertRule) where env.AlertRule = librenms.SlugifyRule(rawRuleName). So this test parses
// the COMMITTED seed and re-derives every lookup through the EXACT SAME librenms.SlugifyRule the live
// ingester uses. If the seed had slugged a rule any differently than the ingester does, the corpus would
// look populated but Count would stay 0 — a silent no-op (the aci-validation-must-match-reader failure).
// This proves it does not: for the six guinea-pig shapes the live ingester will actually post, Count > 0.
//
// It reads no DB and needs no sqlite driver — it validates the committed artifact, so it runs in `make test`.

import (
	"os"
	"testing"

	knowledge "github.com/territory-grounder/grounder/core/knowledge"
	librenms "github.com/territory-grounder/grounder/modules/ingest/librenms"
)

const seedPath = "../../deploy/knowledge/corpus.seed.json"

func loadSeed(t *testing.T) []knowledge.Incident {
	t.Helper()
	f, err := os.Open(seedPath)
	if err != nil {
		t.Fatalf("open seed corpus %s: %v", seedPath, err)
	}
	defer f.Close()
	corpus, err := knowledge.ParseCorpus(f) // DisallowUnknownFields — proves the file carries ONLY the 7 allowed fields
	if err != nil {
		t.Fatalf("parse seed corpus: %v", err)
	}
	if len(corpus) == 0 {
		t.Fatal("seed corpus is empty — the seed must carry the predecessor's known incident shapes")
	}
	return corpus
}

// TestSeedSlugIdempotence: every stored alert_rule is already a canonical slug — re-slugging it through the
// ingester's SlugifyRule is a no-op. A row whose stored rule is NOT idempotent (e.g. a raw un-slugged rule
// name accidentally stored) would re-slug differently at ingest and Count would miss it. This catches that.
func TestSeedSlugIdempotence(t *testing.T) {
	for _, inc := range loadSeed(t) {
		if got := librenms.SlugifyRule(inc.AlertRule); got != inc.AlertRule {
			t.Errorf("stored alert_rule %q for %s is not idempotent under the ingester's SlugifyRule (=%q) — "+
				"a live alert would slug to %q and Count would miss the row", inc.AlertRule, inc.ExternalRef, got, got)
		}
	}
}

// TestSeedEveryRowIsQueryable: every seeded row is findable by the novelty gate using the SAME (host,
// re-slugged-rule) shape the classifier passes. Wildcard (host="*") rows are found via the retriever's
// fleet-wide match. A row Count cannot find is dead weight that never de-novels anything.
func TestSeedEveryRowIsQueryable(t *testing.T) {
	corpus := loadSeed(t)
	r := knowledge.NewLexicalRetriever(corpus)
	for _, inc := range corpus {
		if n := r.Count(inc.Host, librenms.SlugifyRule(inc.AlertRule)); n == 0 {
			t.Errorf("row %s (host=%q rule=%q) is not queryable — Count=0 for the exact classifier signature",
				inc.ExternalRef, inc.Host, inc.AlertRule)
		}
	}
}

// TestSeedGuineaPigGoldenPairs is the load-bearing assertion: for each guinea-pig (host, CURRENT-LIVE
// rawRule) the live LibreNMS ingester will actually post, Count > 0 — the incident shape is no longer novel.
//
// rawRule is the CURRENT LIVE rule name (period-less for the up/down family). The plan enumerates SIX
// historical guinea-pig shapes; two are the 2026-04-03 period-drift form "Devices up/down." which the seed
// tool canonicalized to the current-live "Devices up/down" (evidence: the dotted form appears in the
// predecessor DB ONLY on 2026-04-03, the period-less form runs through 2026-07-19 and matches the live rule
// catalog). So the six historical shapes collapse to these five distinct current-live (host, rule) pairs —
// exactly what a live replay (owner's on-box nginx-down for librespeed01 = "Service up/down") will send.
func TestSeedGuineaPigGoldenPairs(t *testing.T) {
	r := knowledge.NewLexicalRetriever(loadSeed(t))
	golden := []struct{ host, rawRule string }{
		{"dc1librespeed01", "Space on / is >= 90% and < 95% in use"},
		{"dc1librespeed01", "Service up/down"},           // owner's on-box replay shape (nginx down)
		{"dc1librespeed01", "Device Down (SNMP unreachable)"},
		{"dc1librespeed01", "Devices up/down"},           // plan shape "Devices up/down." → current-live
		{"dc1myspeed01", "Devices up/down"},              // plan shapes "Devices up/down." + "Devices up/down"
	}
	for _, g := range golden {
		slug := librenms.SlugifyRule(g.rawRule)
		// Idempotence on the raw live name → its slug, per the plan's explicit ask.
		if got := librenms.SlugifyRule(slug); got != slug {
			t.Errorf("SlugifyRule not idempotent for %q: %q != %q", g.rawRule, got, slug)
		}
		if n := r.Count(g.host, slug); n == 0 {
			t.Errorf("guinea-pig shape STILL NOVEL: Count(%q, SlugifyRule(%q)=%q) = 0 — the seed did not de-novel it",
				g.host, g.rawRule, slug)
		}
	}

	// The DELIBERATE non-fabrication (correctness feature): a guinea-pig MEMORY-pressure shape (LibreNMS rule
	// 21) was NOT seeded, so it MUST stay novel — the first time that class is ever seen a human enters the
	// loop. If this ever flips to >0, someone fabricated a memory-pressure precedent that should not exist.
	if n := r.Count("dc1librespeed01", librenms.SlugifyRule("Linux High Memory Usage, >= 90% in use")); n != 0 {
		t.Errorf("librespeed01 memory-pressure must stay NOVEL (not seeded by design), Count=%d want 0", n)
	}
}
