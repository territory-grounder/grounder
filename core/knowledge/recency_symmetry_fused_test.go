package knowledge

import (
	"testing"
	"time"
)

// TG-502 (fused path): the import tie-guard must ALSO hold in fuseRRF. The LIVE retriever is FusedRetriever
// (TG_EMBED_MODEL is set; cmd/worker/main.go wires it, not the bare LexicalRetriever), and RRF's own final
// sort is a SECOND, independent ranking site. RRF fuses channel RANKS, so the lexical recency-withhold
// reaches the fused output via a fair lexical rank — but the SEMANTIC channel is provenance-blind (cosine
// only), so a sem-only import can tie a verified row's best RRF contribution (both at rank 0 ⇒ 1/(rrfK+1))
// and win the ExternalRef tiebreak, crowding the verified precedent out of the fused top-k even with the
// lexical fix fully intact. This is the gap the fresh-eyes review caught AFTER the lexical fix merged.
//
// KILLING MUTATION: remove the import tie-guard from fuseRRF's sort (semantic.go) → the sem-only import wins
// the RRF-score tie and displaces the verified row from the fused top-3 → RED.
func TestTrackerImportDoesNotCrowdVerifiedInFusedPath(t *testing.T) {
	now := time.Now()
	// z-verified: undated (like the real corpus), ref sorts LAST. Three recent same-shape imports whose refs
	// sort FIRST. import-c is engineered to fall OUT of the lexical top-3 (4 rows, k=3, verified takes slot 1),
	// so it reaches fuseRRF ONLY through the semantic channel — the sem-only tie that beats the ExternalRef order.
	corpus := []Incident{
		{ExternalRef: "z-verified", AlertRule: "ServiceDown", Host: "app01", Source: ProvenanceVerifiedResolution},
		{ExternalRef: "import-a", AlertRule: "ServiceDown", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
		{ExternalRef: "import-b", AlertRule: "ServiceDown", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
		{ExternalRef: "import-c", AlertRule: "ServiceDown", Host: "app01", ResolvedAt: now.Add(-24 * time.Hour), Source: ProvenanceTrackerImport},
	}
	h := NewHolder(NewLexicalRetriever(corpus))
	q := Query{Host: "app01", AlertRule: "ServiceDown"}
	// Precondition: the lexical channel (with the merged tie-guard) ranks z-verified first and leaves import-c
	// out of the top-3 — so import-c enters fuseRRF purely on the semantic side.
	if lex := h.Retrieve(q, 3); len(lex) != 3 || lex[0].Incident.ExternalRef != "z-verified" {
		t.Fatalf("precondition: lexical must rank z-verified first, got %+v", lex)
	}
	// Semantic channel ranks the sem-only import-c FIRST; import-a/import-b double-dip both channels.
	f := &FusedRetriever{
		Base:  h,
		Embed: &fakeEmbedder{vec: []float32{1, 0}},
		Index: &fakeSearcher{matches: []SemanticMatch{
			{ExternalRef: "import-c", Similarity: 0.9},
			{ExternalRef: "import-a", Similarity: 0.85},
			{ExternalRef: "import-b", Similarity: 0.8},
		}},
	}
	hits := f.Retrieve(q, 3)
	refs := make([]string, len(hits))
	inTop := false
	for i, hh := range hits {
		refs[i] = hh.Incident.ExternalRef
		if hh.Incident.ExternalRef == "z-verified" {
			inTop = true
		}
	}
	if !inTop {
		t.Fatalf("verified precedent 'z-verified' was crowded out of the FUSED top-3 by tracker-imports: %v. "+
			"The import tie-guard must hold in fuseRRF (the armed production path), not only the lexical sort.", refs)
	}
	t.Logf("verified precedent survives the fused top-3 against sem-ranked imports: %v", refs)
}
