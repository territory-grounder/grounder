package knowledge

import (
	"testing"
	"time"
)

// TG-502: a tracker-import row must not DISPLACE a same-shape verified/inherited precedent out of the
// agent's top-k. Two paths could let it: (1) imports carry a real ResolvedAt while the incumbent corpus is
// UNDATED (0 of ~670 rows timestamped), so crediting import recency floats them ~+0.25 above the incumbent
// on a channel it cannot contest; (2) up to TG_TRACKER_IMPORT_LIMIT imports of one shape crowd the top-k on
// an exact score tie. The fix withholds recency from imports AND breaks score-ties in favour of non-imports.
// The existing TestTrackerImportProvenanceIsRetrievalInert holds recency CONSTANT to isolate the provenance
// tag, so by construction it cannot see this — the defect lives in the variable that test fixes.
//
// KILLING MUTATIONS (either alone reddens this):
//   (a) drop `&& inc.Source != ProvenanceTrackerImport` from the recency guard → imports float, displace.
//   (b) drop the import tiebreak from the sort → the low-sorting verified ref is crowded out on the tie.
func TestTrackerImportDoesNotDisplaceVerifiedPrecedent(t *testing.T) {
	now := time.Now()
	// The verified incumbent is UNDATED (like the real corpus) and its ref sorts LAST, so ONLY the non-import
	// tiebreak — not ref order — can keep it in the cut. The imports are RECENT and their refs sort FIRST.
	corpus := []Incident{
		{ExternalRef: "z-verified", AlertRule: "Service up/down", Host: "app01", Source: ProvenanceVerifiedResolution},
		{ExternalRef: "a-import-1", AlertRule: "Service up/down", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
		{ExternalRef: "a-import-2", AlertRule: "Service up/down", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
		{ExternalRef: "a-import-3", AlertRule: "Service up/down", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
		{ExternalRef: "a-import-4", AlertRule: "Service up/down", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
	}
	hits := NewLexicalRetriever(corpus).Retrieve(Query{Host: "app01", AlertRule: "Service up/down"}, 3)
	refs := make([]string, len(hits))
	inTop := false
	for i, h := range hits {
		refs[i] = h.Incident.ExternalRef
		if h.Incident.ExternalRef == "z-verified" {
			inTop = true
		}
	}
	if !inTop {
		t.Fatalf("verified precedent 'z-verified' was DISPLACED out of the top-3 by tracker-import rows: %v. "+
			"Imports must neither float on recency the undated incumbent cannot earn, nor crowd it out on a tie.", refs)
	}
	t.Logf("verified precedent survives the top-3 against 4 recent same-shape imports: %v", refs)
}

// TG-502 (fairness): the fix must strip only the UNEARNED advantage (the recency float + tie-crowding),
// never real relevance — a genuinely better-matching import still outranks a weaker incumbent.
//
// KILLING MUTATION: make the import tiebreak unconditional ("imports always last", not tie-only) → the
// better-matching import sinks below the weaker incumbent → RED.
func TestTrackerImportStillWinsOnRealRelevance(t *testing.T) {
	corpus := []Incident{
		{ExternalRef: "incumbent", AlertRule: "Service up/down", Host: "app01", Source: ProvenanceVerifiedResolution},
		// The import matches rule+host+SITE; the incumbent only rule+host — the import is genuinely more relevant.
		{ExternalRef: "import", AlertRule: "Service up/down", Host: "app01", Site: "dc1", Source: ProvenanceTrackerImport},
	}
	hits := NewLexicalRetriever(corpus).Retrieve(Query{Host: "app01", AlertRule: "Service up/down", Site: "dc1"}, 1)
	if len(hits) != 1 || hits[0].Incident.ExternalRef != "import" {
		t.Fatalf("a better-matching import (same site too) failed to outrank a weaker incumbent: got %v — the fix "+
			"must strip only the unearned recency/tie advantage, not real relevance.", hits)
	}
}
