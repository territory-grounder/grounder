// semantic.go is the SEMANTIC channel of the retrieval plane (spec/012 REQ-1110/REQ-1111): a query
// embedding is matched against stored precedent embeddings (cosine top-K over the pgvector index) and the
// result is FUSED with the transparent lexical channel by Reciprocal Rank Fusion — so paraphrased incidents
// ("nginx OOM-killed" vs "web server ran out of memory") surface the precedent lexical token overlap misses,
// while an exact same-rule/same-host precedent keeps its lexical rank.
//
// The honesty contract (INV-08-shaped): the semantic channel only ever ADDS recall on top of the lexical
// baseline. No embedder configured, no index, an embed/search failure, or zero embedded rows ⇒ the retriever
// returns EXACTLY the lexical result — never a fabricated vector, never a failed retrieval because embedding
// is down. A min-similarity threshold keeps junk out of the agent seed, and every fused hit still carries
// human-readable reasons ("semantic similarity 0.83"), so "why was this precedent surfaced?" stays answerable.
package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// DefaultEmbedDim is the embedding dimension the migration provisions (vector(768) in migration 0013) —
// the native dimension of nomic-embed-text, the sane default TG_EMBED_MODEL. TG_EMBED_DIM must match the
// migrated column; the worker refuses a mismatch at boot rather than writing truncated/padded vectors.
const DefaultEmbedDim = 768

// DefaultMinSimilarity is the default cosine-similarity floor (TG_EMBED_MIN_SIMILARITY): a semantic match
// below it never enters the seed, so weak nearest-neighbors (there is ALWAYS a nearest neighbor) do not
// become fabricated precedent.
const DefaultMinSimilarity = 0.5

// rrfK is the standard Reciprocal Rank Fusion constant (k=60, Cormack et al.): each channel contributes
// 1/(k + rank) per document, which rewards agreement between channels without letting either channel's raw
// score scale dominate.
const rrfK = 60

// defaultSemanticTimeout bounds the per-query embed+search round trip; past it the query degrades to the
// lexical channel alone (retrieval must never stall the investigation on a slow embedding backend).
const defaultSemanticTimeout = 10 * time.Second

// Embedder produces one embedding vector per input text, in input order. The production implementation is
// the LiteLLM gateway's OpenAI-compatible /v1/embeddings surface (adapters/model.Embedder); tests inject a
// fake. Output is untrusted numeric DATA (INV-08): it is compared, never executed or interpolated.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// SemanticMatch is one nearest-neighbor hit from the vector index: the precedent's identity plus its cosine
// similarity to the query (1 = identical direction, 0 = orthogonal).
type SemanticMatch struct {
	ExternalRef string
	Similarity  float64
}

// SemanticSearcher is the read seam over the vector index (pgx-backed over knowledge_embedding in
// production, a fake in tests): cosine top-k most-similar embedded precedents for a query vector.
type SemanticSearcher interface {
	SearchSimilar(ctx context.Context, vec []float32, k int) ([]SemanticMatch, error)
}

// EmbedText renders an incident into the ONE deterministic text form that is embedded and content-hashed.
// Field-labelled and newline-delimited so the embedding sees structure, and stable so the same precedent
// always hashes (and embeds) identically — the idempotency key of the whole write path.
func EmbedText(inc Incident) string {
	var b strings.Builder
	writeField(&b, "alert_rule", inc.AlertRule)
	writeField(&b, "host", inc.Host)
	writeField(&b, "site", inc.Site)
	writeField(&b, "tags", strings.Join(inc.Tags, ", "))
	writeField(&b, "summary", inc.Summary)
	writeField(&b, "resolution", inc.Resolution)
	return b.String()
}

// QueryText renders a query into the same labelled form as EmbedText (minus the resolution a new incident
// does not have), so query and corpus vectors live in the same representation space.
func QueryText(q Query) string {
	var b strings.Builder
	writeField(&b, "alert_rule", q.AlertRule)
	writeField(&b, "host", q.Host)
	writeField(&b, "site", q.Site)
	writeField(&b, "tags", strings.Join(q.Tags, ", "))
	writeField(&b, "summary", q.Summary)
	return b.String()
}

func writeField(b *strings.Builder, name, value string) {
	if v := strings.TrimSpace(value); v != "" {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
}

// ContentHash is the sha256 (hex) of an incident's EmbedText — stored beside the embedding so a changed
// precedent re-embeds (its row's vector is nulled on hash change) and an unchanged one never re-embeds.
func ContentHash(inc Incident) string {
	sum := sha256.Sum256([]byte(EmbedText(inc)))
	return hex.EncodeToString(sum[:])
}

// FusedRetriever fuses the lexical channel (Base) with the semantic channel (Embed + Index) by Reciprocal
// Rank Fusion. It implements Retriever, so the Runner's seed path needs no change. All degradation paths
// return the lexical result UNCHANGED — the fallback is the exact pre-semantic behavior, not an
// approximation of it.
type FusedRetriever struct {
	Base    *Holder          // the lexical channel + the live corpus the semantic refs resolve against
	Index   SemanticSearcher // the vector index read seam; nil ⇒ lexical only
	Embed   Embedder         // the query embedder; nil ⇒ lexical only
	MinSim  float64          // cosine-similarity floor; <=0 ⇒ DefaultMinSimilarity
	Timeout time.Duration    // per-query embed+search budget; <=0 ⇒ defaultSemanticTimeout
	Logf    func(format string, args ...any) // degrade logging; nil ⇒ log.Printf
}

var _ Retriever = (*FusedRetriever)(nil)

// Retrieve returns up to k fused hits. The lexical channel is computed first and is the result whenever the
// semantic channel is unavailable, errors, times out, or matches nothing above the similarity floor —
// per-query, so a transient embedding outage degrades that one retrieval and nothing else.
func (f *FusedRetriever) Retrieve(q Query, k int) []Hit {
	if f.Base == nil || k <= 0 {
		return nil
	}
	lex := f.Base.Retrieve(q, k)
	if f.Index == nil || f.Embed == nil {
		return lex
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = defaultSemanticTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	vecs, err := f.Embed.Embed(ctx, []string{QueryText(q)})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		f.logf("semantic retrieval: query embed failed (%v) — served lexical only for this query", err)
		return lex
	}
	matches, err := f.Index.SearchSimilar(ctx, vecs[0], k)
	if err != nil {
		f.logf("semantic retrieval: index search failed (%v) — served lexical only for this query", err)
		return lex
	}
	minSim := f.MinSim
	if minSim <= 0 {
		minSim = DefaultMinSimilarity
	}
	// Threshold + resolve each ref against the LIVE corpus: a below-floor neighbor never enters the seed,
	// and a stale index row whose precedent left the corpus can never resurrect it (the corpus is truth).
	sem := make([]SemanticMatch, 0, len(matches))
	for _, m := range matches {
		if m.Similarity < minSim {
			continue
		}
		if _, ok := f.Base.ByRef(m.ExternalRef); !ok {
			continue
		}
		sem = append(sem, m)
	}
	if len(sem) == 0 {
		return lex // nothing embedded/similar enough ⇒ exactly the lexical behavior
	}
	return fuseRRF(f.Base, lex, sem, k)
}

// Count passes the novelty-gate signature count through to the lexical corpus (see Holder.Count).
func (f *FusedRetriever) Count(host, alertRule string) int {
	if f.Base == nil {
		return 0
	}
	return f.Base.Count(host, alertRule)
}

func (f *FusedRetriever) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// fuseRRF merges the two ranked channels by Reciprocal Rank Fusion: each channel contributes
// 1/(rrfK + rank) per document (rank starting at 1), so a document ranked in BOTH channels outscores any
// single-channel document at comparable ranks. Ties break deterministically by ExternalRef. The fused Hit
// keeps the existing shape: the incident, a score (the RRF sum, rounded), and the union of reasons.
func fuseRRF(corpus *Holder, lex []Hit, sem []SemanticMatch, k int) []Hit {
	type fused struct {
		inc     Incident
		score   float64
		reasons []string
	}
	docs := map[string]*fused{}
	order := make([]string, 0, len(lex)+len(sem))
	get := func(ref string, inc Incident) *fused {
		if d, ok := docs[ref]; ok {
			return d
		}
		d := &fused{inc: inc}
		docs[ref] = d
		order = append(order, ref)
		return d
	}
	for i, h := range lex {
		d := get(h.Incident.ExternalRef, h.Incident)
		d.score += 1.0 / float64(rrfK+i+1)
		d.reasons = append(d.reasons, h.Reasons...)
	}
	for i, m := range sem {
		inc, ok := corpus.ByRef(m.ExternalRef)
		if !ok {
			continue // filtered upstream; defensive
		}
		d := get(m.ExternalRef, inc)
		d.score += 1.0 / float64(rrfK+i+1)
		d.reasons = append(d.reasons, fmt.Sprintf("semantic similarity %.2f", m.Similarity))
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := docs[order[i]], docs[order[j]]
		if a.score != b.score {
			return a.score > b.score
		}
		return order[i] < order[j]
	})
	if len(order) > k {
		order = order[:k]
	}
	out := make([]Hit, 0, len(order))
	for _, ref := range order {
		d := docs[ref]
		out = append(out, Hit{Incident: d.inc, Score: round4(d.score), Reasons: d.reasons})
	}
	return out
}

// round4 keeps fused RRF scores (order 1/61 ≈ 0.016) distinguishable while staying human-readable.
func round4(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}
