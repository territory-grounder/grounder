package knowledge

import (
	"context"
	"strings"
	"testing"
)

// recordingEmbedder captures the text it was asked to embed — the seam under test.
type recordingEmbedder struct {
	got string
	vec []float32
}

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if len(texts) > 0 {
		r.got = texts[0]
	}
	return [][]float32{r.vec}, nil
}

type oneMatchSearcher struct{ ref string }

func (s *oneMatchSearcher) SearchSimilar(context.Context, []float32, int) ([]SemanticMatch, error) {
	return []SemanticMatch{{ExternalRef: s.ref, Similarity: 0.99}}, nil
}

// TG-214 HyDE: with a Hypothetical seam armed, the semantic channel embeds the hypothetical RESOLUTION as a
// DOCUMENT; OFF (nil) it embeds the raw query; an empty hypothetical (LLM failure) falls back to the raw query.
func TestFusedRetrieverHyDEEmbedsHypotheticalAsDocument(t *testing.T) {
	corpus := []Incident{{ExternalRef: "P1", Host: "h", AlertRule: "R", Resolution: "grow the volume"}}
	h := NewHolder(NewLexicalRetriever(corpus))
	q := Query{Host: "db1", AlertRule: "DiskFull", Summary: "out of space"}

	// OFF (nil Hypothetical): the RAW query text is embedded (search_query side), byte-identical to pre-HyDE.
	off := &recordingEmbedder{vec: []float32{1, 0}}
	(&FusedRetriever{Base: h, Index: &oneMatchSearcher{ref: "P1"}, Embed: off}).Retrieve(q, 3)
	if off.got != QueryText(q) {
		t.Fatalf("OFF must embed the raw query text; got %q want %q", off.got, QueryText(q))
	}

	// Armed: the HYPOTHETICAL is embedded, as a DOCUMENT (so it matches corpus resolutions). KILLING MUTATION:
	// drop the HyDE branch (always QueryText) ⇒ this reddens; drop embedDocumentPrefix ⇒ the prefix check reddens.
	on := &recordingEmbedder{vec: []float32{1, 0}}
	(&FusedRetriever{Base: h, Index: &oneMatchSearcher{ref: "P1"}, Embed: on,
		Hypothetical: func(context.Context, Query) string { return "grow the disk on the postgres node" }}).Retrieve(q, 3)
	if !strings.Contains(on.got, "grow the disk on the postgres node") {
		t.Fatalf("armed must embed the hypothetical resolution; got %q", on.got)
	}
	if !strings.HasPrefix(on.got, embedDocumentPrefix) {
		t.Errorf("the hypothetical must be embedded as a DOCUMENT (%q prefix); got %q", embedDocumentPrefix, on.got)
	}

	// Empty hypothetical (an LLM failure returns ""): fall back to the raw query.
	fb := &recordingEmbedder{vec: []float32{1, 0}}
	(&FusedRetriever{Base: h, Index: &oneMatchSearcher{ref: "P1"}, Embed: fb,
		Hypothetical: func(context.Context, Query) string { return "   " }}).Retrieve(q, 3)
	if fb.got != QueryText(q) {
		t.Errorf("an empty hypothetical must fall back to the raw query; got %q", fb.got)
	}
}
