package estate

import (
	"context"
	"sync/atomic"
)

// Holder holds the current estate graph behind an atomic pointer, so the worker can atomically REPLACE the
// graph on a periodic refresh — re-reading live topology sources (and later, incorporating newly-learned
// edges) — while in-flight prediction reads see a consistent snapshot and never a half-built graph. The
// prediction closures read through Graph(), so a refresh takes effect WITHOUT a restart. This is the
// primitive the runtime estate-refresh loop is built on; a bad refresh (all sources down) keeps the previous
// good graph rather than swapping in an empty one.
type Holder struct{ g atomic.Pointer[Graph] }

// NewHolder wraps an initial graph. A nil initial graph is replaced with an empty graph, so Graph() never
// returns nil (a nil graph would panic the prediction path — fail toward an empty, non-vacuous graph instead).
func NewHolder(g *Graph) *Holder {
	if g == nil {
		g = NewGraph()
	}
	h := &Holder{}
	h.g.Store(g)
	return h
}

// Graph returns the current snapshot. Safe for concurrent reads.
func (h *Holder) Graph() *Graph { return h.g.Load() }

// Set atomically replaces the graph. A nil graph is ignored (the previous snapshot stands) — a refresh must
// never install a nil graph.
func (h *Holder) Set(g *Graph) {
	if g != nil {
		h.g.Store(g)
	}
}

// Refresh rebuilds the graph from the given sources and atomically swaps it in — but ONLY if the rebuild
// produced a NON-empty graph, or if every source succeeded (a legitimately empty estate). If EVERY source
// errored (so the empty result is a transient outage, not truth), the previous good graph is KEPT and the
// source errors are returned — a topology blip never blanks the estate and makes every prediction vacuous.
// It returns the per-source errors (for logging); the swap decision is made here, not by the caller.
func (h *Holder) Refresh(ctx context.Context, sources []EdgeSource, opts ...Option) []SourceError {
	g, errs := Build(ctx, sources, opts...)
	allFailed := len(sources) > 0 && len(errs) == len(sources)
	if allFailed {
		return errs // every source down — keep the last good graph rather than swapping in an outage-empty one
	}
	h.Set(g)
	return errs
}
