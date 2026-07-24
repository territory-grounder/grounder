package knowledge

import "sync/atomic"

// Holder wraps a retriever behind an atomic pointer so the precedent corpus can be RELOADED at runtime
// without a restart — an operator (or the lessons feed) appending a new resolved incident takes effect on the
// next refresh. It implements Retriever, so callers hold a stable reference while the corpus behind it is
// swapped. A reload that fails to parse keeps the last good corpus (the caller decides not to Set on error).
type Holder struct{ r atomic.Pointer[LexicalRetriever] }

// NewHolder wraps an initial retriever. A nil initial retriever is replaced with an empty one, so Retrieve
// never dereferences nil.
func NewHolder(r *LexicalRetriever) *Holder {
	if r == nil {
		r = NewLexicalRetriever(nil)
	}
	h := &Holder{}
	h.r.Store(r)
	return h
}

var _ Retriever = (*Holder)(nil)

// Retrieve reads the current corpus snapshot. Safe for concurrent reads.
func (h *Holder) Retrieve(q Query, k int) []Hit { return h.r.Load().Retrieve(q, k) }

// Count returns the prior-incident count for a (host, alert_rule) signature over the current corpus snapshot
// (see LexicalRetriever.Count) — the novelty gate's data source. Safe for concurrent reads.
func (h *Holder) Count(host, alertRule string) int { return h.r.Load().Count(host, alertRule) }

// ByRef resolves a precedent by ExternalRef over the current corpus snapshot — the semantic channel's join
// back onto the live corpus. Safe for concurrent reads.
func (h *Holder) ByRef(ref string) (Incident, bool) { return h.r.Load().ByRef(ref) }

// Snapshot returns a copy of the current corpus — the semantic index sync's input. Safe for concurrent reads.
func (h *Holder) Snapshot() []Incident { return h.r.Load().Snapshot() }

// Set atomically swaps in a reloaded retriever. A nil retriever is ignored (the previous corpus stands).
func (h *Holder) Set(r *LexicalRetriever) {
	if r != nil {
		h.r.Store(r)
	}
}
