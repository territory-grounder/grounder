package runner

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/ingest"
)

// TG-86 slice 2b: estateBlockSource folds the operator's estate-doc grounding into the <estate> block source,
// where InvestigateActivity then screens + delimiter-neutralizes + budgets it like every other untrusted block.
// OFF (nil EstateDocs) must be byte-identical to the graph context alone; armed, the docs must actually reach
// the block. KILLING MUTATION: drop the estateDocContext fold in estateBlockSource ⇒ the armed case loses the
// docs and reddens.
func TestEstateBlockSourceFoldsDocGroundingWhenArmed(t *testing.T) {
	env := ingest.IncidentEnvelope{ExternalRef: "inc-1", Host: "librespeed01", AlertRule: "DiskFull", Summary: "disk filling"}
	graph := func(host string) string { return "ESTATE GRAPH: parents of " + host }
	grounding := func(host, summary string) string {
		return "ESTATE DOCUMENTATION: " + host + " is the speedtest service\n"
	}

	// OFF (nil EstateDocs): byte-identical to the graph context alone.
	off := &Activities{D: Deps{EstateSeed: graph}}
	if got, want := off.estateBlockSource(env), off.estateContext(env); got != want {
		t.Fatalf("grounding OFF must equal the graph context alone (byte-identical): %q vs %q", got, want)
	}
	if strings.Contains(off.estateBlockSource(env), "ESTATE DOCUMENTATION") {
		t.Error("OFF must carry no estate-doc grounding")
	}

	// Armed: BOTH the graph and the docs are present in the block source.
	on := &Activities{D: Deps{EstateSeed: graph, EstateDocs: grounding}}
	src := on.estateBlockSource(env)
	if !strings.Contains(src, "ESTATE GRAPH: parents of librespeed01") {
		t.Errorf("armed must still carry the graph context, got %q", src)
	}
	if !strings.Contains(src, "ESTATE DOCUMENTATION: librespeed01 is the speedtest service") {
		t.Fatalf("armed must fold the estate-doc grounding into the block, got %q", src)
	}

	// A graph-less host with grounding armed yields EXACTLY the docs (no leading separator noise).
	docOnly := &Activities{D: Deps{EstateDocs: grounding}}
	if got, want := docOnly.estateBlockSource(env), grounding(env.Host, env.Summary); got != want {
		t.Errorf("graph-less + armed must yield exactly the docs, got %q want %q", got, want)
	}
}

// estateDocContext passes the incident host + summary through to the provider, and nil ⇒ "" (the OFF guard).
func TestEstateDocContextPassthroughAndOff(t *testing.T) {
	env := ingest.IncidentEnvelope{Host: "librespeed01", Summary: "disk filling"}
	var gotHost, gotSummary string
	probe := &Activities{D: Deps{EstateDocs: func(h, s string) string { gotHost, gotSummary = h, s; return "X-BLOCK" }}}
	if got := probe.estateDocContext(env); got != "X-BLOCK" {
		t.Errorf("estateDocContext must return the provider's block, got %q", got)
	}
	if gotHost != "librespeed01" || gotSummary != "disk filling" {
		t.Errorf("the provider must receive the incident host+summary, got host=%q summary=%q", gotHost, gotSummary)
	}
	if got := (&Activities{D: Deps{}}).estateDocContext(env); got != "" {
		t.Errorf("nil EstateDocs must yield \"\", got %q", got)
	}
}
