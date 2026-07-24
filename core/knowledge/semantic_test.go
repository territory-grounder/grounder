package knowledge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// --- fakes -----------------------------------------------------------------------------------------------

type fakeEmbedder struct {
	vec   []float32
	err   error
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}

type fakeSearcher struct {
	matches []SemanticMatch
	err     error
	calls   int
}

func (f *fakeSearcher) SearchSimilar(context.Context, []float32, int) ([]SemanticMatch, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.matches, nil
}

func semCorpus() []Incident {
	return []Incident{
		{ExternalRef: "TG-200", Host: "web01", AlertRule: "NginxDown", Site: "nl", Summary: "nginx worker crashed under load", Resolution: "restart nginx", Tags: []string{"web"}},
		{ExternalRef: "TG-201", Host: "web02", AlertRule: "NginxDown", Site: "nl", Summary: "nginx oom killed", Resolution: "raise memory limit", Tags: []string{"web"}},
		{ExternalRef: "TG-202", Host: "db01", AlertRule: "DiskFull", Site: "gr", Summary: "postgres wal filled the disk", Resolution: "prune wal archives", Tags: []string{"db"}},
	}
}

// --- the fusion oracle -----------------------------------------------------------------------------------

// A precedent ranked in BOTH channels must outrank precedents ranked in only one — the RRF agreement
// property the fusion exists for — and the fused hit carries both channels' reasons.
func TestFusedRetrieverRRFAgreementWins(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	q := Query{Host: "web01", AlertRule: "NginxDown", Summary: "nginx crashed"}
	lex := h.Retrieve(q, 5) // TG-200 (host+rule+summary) then TG-201 (rule)
	if len(lex) != 2 || lex[0].Incident.ExternalRef != "TG-200" {
		t.Fatalf("precondition: lexical must rank TG-200 first, got %+v", lex)
	}
	// The semantic channel ranks TG-201 first (a paraphrase match) and TG-202 second; TG-200 is absent.
	f := &FusedRetriever{
		Base:  h,
		Embed: &fakeEmbedder{vec: []float32{1, 0}},
		Index: &fakeSearcher{matches: []SemanticMatch{{ExternalRef: "TG-201", Similarity: 0.9}, {ExternalRef: "TG-202", Similarity: 0.8}}},
	}
	hits := f.Retrieve(q, 5)
	if len(hits) != 3 {
		t.Fatalf("expected 3 fused hits, got %d: %+v", len(hits), hits)
	}
	// TG-201 is rank 2 lexically AND rank 1 semantically: 1/62 + 1/61 beats TG-200's single 1/61.
	if hits[0].Incident.ExternalRef != "TG-201" {
		t.Fatalf("the both-channels precedent must fuse to the top, got %s", hits[0].Incident.ExternalRef)
	}
	if hits[1].Incident.ExternalRef != "TG-200" || hits[2].Incident.ExternalRef != "TG-202" {
		t.Fatalf("single-channel order wrong: %+v", hits)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("fused scores must order the ranking: %.4f !> %.4f", hits[0].Score, hits[1].Score)
	}
	// Explainability survives fusion: lexical reasons + the semantic similarity reason.
	joined := strings.Join(hits[0].Reasons, "; ")
	if !strings.Contains(joined, "same alert rule") || !strings.Contains(joined, "semantic similarity 0.90") {
		t.Fatalf("fused reasons must carry both channels, got %q", joined)
	}
	// The semantic-only hit is explainable too.
	if joined := strings.Join(hits[2].Reasons, "; "); !strings.Contains(joined, "semantic similarity 0.80") {
		t.Fatalf("semantic-only reasons missing, got %q", joined)
	}
}

// A semantic match below the similarity floor never enters the seed: with only junk neighbors the fused
// result is EXACTLY the lexical result.
func TestFusedRetrieverThresholdExcludesJunk(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	q := Query{Host: "web01", AlertRule: "NginxDown"}
	lex := h.Retrieve(q, 5)
	f := &FusedRetriever{
		Base:  h,
		Embed: &fakeEmbedder{vec: []float32{1, 0}},
		Index: &fakeSearcher{matches: []SemanticMatch{{ExternalRef: "TG-202", Similarity: 0.31}}}, // below 0.5
	}
	if got := f.Retrieve(q, 5); !reflect.DeepEqual(got, lex) {
		t.Fatalf("below-threshold matches must leave the result exactly lexical:\n got %+v\nwant %+v", got, lex)
	}
	// An explicit higher floor excludes what the default admits.
	f.Index = &fakeSearcher{matches: []SemanticMatch{{ExternalRef: "TG-202", Similarity: 0.6}}}
	f.MinSim = 0.75
	if got := f.Retrieve(q, 5); !reflect.DeepEqual(got, lex) {
		t.Fatalf("a configured floor must exclude sub-floor matches:\n got %+v\nwant %+v", got, lex)
	}
}

// The disabled path is the exact lexical behavior: no embedder or no index configured means the fused
// retriever returns what the lexical retriever returns, hit for hit — the INV-08-honest total fallback.
func TestFusedRetrieverDisabledIsExactlyLexical(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	for _, q := range []Query{
		{Host: "web01", AlertRule: "NginxDown", Summary: "nginx crashed", Tags: []string{"web"}},
		{AlertRule: "DiskFull"},
		{Host: "zzz", AlertRule: "UnheardOf"},
	} {
		want := h.Retrieve(q, 3)
		for name, f := range map[string]*FusedRetriever{
			"no embedder": {Base: h, Index: &fakeSearcher{}},
			"no index":    {Base: h, Embed: &fakeEmbedder{vec: []float32{1}}},
			"neither":     {Base: h},
		} {
			if got := f.Retrieve(q, 3); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: disabled fused retrieval must equal lexical:\n got %+v\nwant %+v", name, got, want)
			}
		}
	}
}

// A transient embed or search failure degrades THAT query to the lexical result — retrieval never fails
// because embedding is down, and no vector is ever fabricated.
func TestFusedRetrieverDegradesPerQueryOnFailure(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	q := Query{AlertRule: "NginxDown"}
	want := h.Retrieve(q, 5)
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }

	embed := &fakeEmbedder{err: errors.New("gateway 502")}
	f := &FusedRetriever{Base: h, Embed: embed, Index: &fakeSearcher{matches: []SemanticMatch{{ExternalRef: "TG-202", Similarity: 0.9}}}, Logf: logf}
	if got := f.Retrieve(q, 5); !reflect.DeepEqual(got, want) {
		t.Fatalf("embed failure must serve lexical:\n got %+v\nwant %+v", got, want)
	}
	search := &fakeSearcher{err: errors.New("index offline")}
	f = &FusedRetriever{Base: h, Embed: &fakeEmbedder{vec: []float32{1}}, Index: search, Logf: logf}
	if got := f.Retrieve(q, 5); !reflect.DeepEqual(got, want) {
		t.Fatalf("search failure must serve lexical:\n got %+v\nwant %+v", got, want)
	}
	if len(logged) != 2 {
		t.Fatalf("each degrade must be logged once, got %d", len(logged))
	}
	// Recovery is per-query: the next query with a healthy channel fuses again.
	embed.err = nil
	embed.vec = []float32{1}
	f = &FusedRetriever{Base: h, Embed: embed, Index: &fakeSearcher{matches: []SemanticMatch{{ExternalRef: "TG-202", Similarity: 0.9}}}, Logf: logf}
	if got := f.Retrieve(q, 5); len(got) != 3 {
		t.Fatalf("recovered channel must fuse again, got %+v", got)
	}
}

// Zero embedded rows (an empty index) and stale index refs both leave the result exactly lexical: the
// corpus is truth, and an index row whose precedent left the corpus can never resurrect it.
func TestFusedRetrieverEmptyIndexAndStaleRefs(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	q := Query{AlertRule: "NginxDown"}
	want := h.Retrieve(q, 5)
	f := &FusedRetriever{Base: h, Embed: &fakeEmbedder{vec: []float32{1}}, Index: &fakeSearcher{}} // no matches
	if got := f.Retrieve(q, 5); !reflect.DeepEqual(got, want) {
		t.Fatalf("an empty index must serve lexical exactly:\n got %+v\nwant %+v", got, want)
	}
	f.Index = &fakeSearcher{matches: []SemanticMatch{{ExternalRef: "TG-999", Similarity: 0.99}}} // not in corpus
	if got := f.Retrieve(q, 5); !reflect.DeepEqual(got, want) {
		t.Fatalf("a stale ref must never surface:\n got %+v\nwant %+v", got, want)
	}
	// Guard rails: k<=0 and a nil base retrieve nothing and never call the channels.
	embed := &fakeEmbedder{vec: []float32{1}}
	f = &FusedRetriever{Base: h, Embed: embed, Index: &fakeSearcher{}}
	if got := f.Retrieve(q, 0); got != nil {
		t.Fatalf("k=0 must retrieve nothing, got %+v", got)
	}
	if embed.calls != 0 {
		t.Fatal("k=0 must not embed")
	}
	if got := (&FusedRetriever{}).Retrieve(q, 3); got != nil {
		t.Fatalf("a nil base retrieves nothing, got %+v", got)
	}
}

// The novelty-gate count passes through the fused retriever unchanged.
func TestFusedRetrieverCountPassthrough(t *testing.T) {
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	f := &FusedRetriever{Base: h}
	if got := f.Count("web01", "NginxDown"); got != 1 {
		t.Fatalf("count passthrough: want 1, got %d", got)
	}
	if got := (&FusedRetriever{}).Count("web01", "NginxDown"); got != 0 {
		t.Fatalf("nil base counts zero, got %d", got)
	}
}

// EmbedText/QueryText are deterministic, field-labelled, and hash-stable; a content change (the fields a
// precedent is cited for) changes the hash, and cosmetic identity (same fields) does not.
func TestEmbedTextAndContentHash(t *testing.T) {
	inc := semCorpus()[0]
	if EmbedText(inc) != EmbedText(inc) || ContentHash(inc) != ContentHash(inc) {
		t.Fatal("embed text/hash must be deterministic")
	}
	txt := EmbedText(inc)
	for _, want := range []string{"alert_rule: NginxDown", "host: web01", "summary: nginx worker crashed under load", "resolution: restart nginx"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("embed text missing %q:\n%s", want, txt)
		}
	}
	changed := inc
	changed.Resolution = "reinstall nginx"
	if ContentHash(changed) == ContentHash(inc) {
		t.Fatal("a changed resolution must change the content hash (it re-embeds)")
	}
	// The query renders the same labelled space minus the resolution a new incident cannot have.
	qt := QueryText(Query{Host: "web01", AlertRule: "NginxDown", Summary: "nginx crashed"})
	if !strings.Contains(qt, "alert_rule: NginxDown") || strings.Contains(qt, "resolution:") {
		t.Fatalf("query text malformed:\n%s", qt)
	}
	// ByRef resolves live refs and refuses unknown/blank ones.
	h := NewHolder(NewLexicalRetriever(semCorpus()))
	if got, ok := h.ByRef("TG-201"); !ok || got.Host != "web02" {
		t.Fatalf("ByRef must resolve TG-201, got %+v ok=%v", got, ok)
	}
	if _, ok := h.ByRef("TG-999"); ok {
		t.Fatal("ByRef must refuse an unknown ref")
	}
	if snap := h.Snapshot(); len(snap) != 3 {
		t.Fatalf("snapshot must copy the corpus, got %d", len(snap))
	}
}
