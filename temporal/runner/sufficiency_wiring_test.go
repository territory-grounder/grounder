package runner

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/ingest"
	"github.com/territory-grounder/grounder/core/knowledge"
)

// TG-214: the retrieval-sufficiency verdict wired through the PRODUCTION call site — Activities.precedent()
// over the real LexicalRetriever — the layer that core/knowledge's unit tests and eval/retrievalquality's
// verdict tests deliberately do NOT reach (they exercise HasAdequatePrecedent / NoAdequatePrecedentBlock
// directly, never through the wired `if a.D.Sufficiency && len(kept) > 0 && !HasAdequatePrecedent(...)`).
//
// KILLING MUTATIONS these catch — each compiles and passes 100% of the OTHER shipped tests, and reddens ONLY
// here: negate the guard (`!knowledge.HasAdequatePrecedent` → `knowledge.HasAdequatePrecedent`), delete the
// whole `if`, hardcode `a.D.SufficiencyMinCosine` to a value, or default `Deps.Sufficiency` to true.

func sufwireCorpus() []knowledge.Incident {
	return []knowledge.Incident{
		{ExternalRef: "SW-1", Host: "web01", AlertRule: "NginxDown", Site: "nl", Tags: []string{"web"}, Resolution: "restart nginx"},
		{ExternalRef: "SW-2", Host: "web02", AlertRule: "NginxDown", Site: "nl", Tags: []string{"web"}, Resolution: "restart nginx"},
	}
}

// sufwireNovelEnv: neither the rule (CacheEviction) nor the host (cache09) is in the corpus; only the SITE
// overlaps, so retrieval surfaces WEAK, off-target hits — the sufficiency-fire case.
func sufwireNovelEnv() ingest.IncidentEnvelope {
	return ingest.IncidentEnvelope{ExternalRef: "sw-novel", Host: "cache09", AlertRule: "CacheEviction", Site: "nl", Summary: "cache evictions spiking on cache09"}
}

// sufwireAdequateEnv shares the corpus rule AND host — an adequate precedent exists.
func sufwireAdequateEnv() ingest.IncidentEnvelope {
	return ingest.IncidentEnvelope{ExternalRef: "sw-web01", Host: "web01", AlertRule: "NginxDown", Site: "nl", Summary: "nginx down on web01"}
}

// OFF is byte-identical: the zero value and an explicit false both leave precedent()'s block untouched, on
// BOTH an inadequate and an adequate incident. This is the regression the composition-root flag rests on.
func TestPrecedentSufficiencyOffIsByteIdentical(t *testing.T) {
	corpus := sufwireCorpus()
	zero := &Activities{D: Deps{Retriever: knowledge.NewLexicalRetriever(corpus)}} // Sufficiency zero value = false
	explicitOff := &Activities{D: Deps{Retriever: knowledge.NewLexicalRetriever(corpus), Sufficiency: false}}
	for _, env := range []ingest.IncidentEnvelope{sufwireNovelEnv(), sufwireAdequateEnv()} {
		bZero, _, _ := zero.precedent(env)
		bOff, _, _ := explicitOff.precedent(env)
		if bZero != bOff {
			t.Fatalf("Sufficiency=false must be byte-identical to the zero value (env host %s): %q vs %q", env.Host, bZero, bOff)
		}
	}
}

// ON fires on an inadequate set: an incident whose retrieval surfaces only weak (site-only) precedent gets
// the explicit "no adequate precedent" signal instead of the padded block.
func TestPrecedentSufficiencyFiresOnInadequateSet(t *testing.T) {
	corpus := sufwireCorpus()
	off := &Activities{D: Deps{Retriever: knowledge.NewLexicalRetriever(corpus)}}
	on := &Activities{D: Deps{Retriever: knowledge.NewLexicalRetriever(corpus), Sufficiency: true}}

	// VACUITY FLOOR: the novel incident must retrieve a NON-empty weak block with sufficiency OFF — otherwise
	// "fired" is indistinguishable from "retrieved nothing", and the ON assertion would pass vacuously.
	offBlock, _, _ := off.precedent(sufwireNovelEnv())
	if offBlock == "" {
		t.Fatal("vacuity: the novel incident must retrieve a weak (site-overlap) precedent block with sufficiency OFF; got empty — this test would prove nothing")
	}
	if strings.Contains(offBlock, "no adequate precedent") {
		t.Fatalf("the OFF block must not already carry the signal, got %q", offBlock)
	}

	onBlock, _, _ := on.precedent(sufwireNovelEnv())
	if !strings.Contains(onBlock, "no adequate precedent") {
		t.Fatalf("Sufficiency=true on an inadequate set (no rule/host match, weak overlap only) must render the explicit signal, got %q", onBlock)
	}
}

// ON never suppresses an adequate precedent: when a same-rule+same-host precedent exists, the signal must NOT
// fire — the real precedent is rendered as usual. This is the safety half (arming must never hide precedent).
func TestPrecedentSufficiencyNeverSuppressesAdequatePrecedent(t *testing.T) {
	corpus := sufwireCorpus()
	on := &Activities{D: Deps{Retriever: knowledge.NewLexicalRetriever(corpus), Sufficiency: true}}
	block, _, _ := on.precedent(sufwireAdequateEnv())
	if strings.Contains(block, "no adequate precedent") {
		t.Fatalf("Sufficiency must NOT fire when an adequate same-rule/same-host precedent exists — it would suppress a real precedent; got %q", block)
	}
	if !strings.Contains(block, "SW-1") {
		t.Fatalf("the adequate precedent (SW-1, same rule+host) must still be rendered, got %q", block)
	}
}
