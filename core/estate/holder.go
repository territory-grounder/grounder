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
// produced a NON-empty graph, or if every source succeeded (a legitimately empty estate). If sources
// errored AND the rebuild came back empty (so the emptiness is a transient outage, not truth), the
// previous good graph is KEPT — a topology blip must never blank the estate and make every prediction
// vacuous. The swap decision is made here, not by the caller.
//
// THE GUARD DECIDES ON THE RESULT, NOT ON A SOURCE COUNT, and that is a correction rather than a
// refinement. It used to read:
//
//	allFailed := len(sources) > 0 && len(errs) == len(sources)
//
// which is ARITHMETICALLY UNSATISFIABLE at the only production caller. That caller appends
// learner.LearnedSource() to the source list, and LearnedSource.Edges has no error path at all — it
// cannot fail. So len(errs) could never reach len(sources), allFailed was never true, and the entire
// outage protection was dead code. A total CMDB and LibreNMS outage swapped in the near-empty graph
// while the caller logged "(kept prior edges)", which was the opposite of what happened.
//
// An unfailable source in the list cannot break a result-based predicate the way it broke a
// count-based one: if every fallible source is down and the survivor contributes no edges, the rebuild
// is empty and the prior graph stands.
//
// It returns kept=true when the rebuild was REJECTED and the prior graph still stands, so the caller can
// say which of the two things happened instead of asserting one of them.
func (h *Holder) Refresh(ctx context.Context, sources []EdgeSource, opts ...Option) (kept bool, errs []SourceError) {
	prior := h.Graph()
	g, errs := Build(ctx, sources, opts...)
	if len(errs) > 0 && g.Len() == 0 {
		// Something failed and the result is empty: treat the emptiness as the outage, not as truth.
		return true, errs
	}
	// A hypervisor that went SILENT has its guests' authoritative parent edges carried forward from the prior
	// graph (tombstoned with a decayed confidence) rather than deleted — a full rebuild otherwise reads the
	// API's silence about a down host as "that host has no guests", losing the topology at the exact moment
	// correlation needs it (TG-375). A migration or a still-up host is still a genuine delete. Run PER
	// live-hypervisor source (TG-521): Proxmox and vCenter have the identical failure shape, and vSphere
	// without this reads a silently-dark vCenter's VMs as reachLive until their TTL lapses.
	idx := indexPlacements(g) // ONE shared placement snapshot for every source's pass (TG-521): so one source's
	// just-written tombstones never feed the next source's migration/still-up evidence — the passes stay order-independent.
	carryForwardUnreachable(prior, g, SourcePVE, sourceFailed(errs, SourcePVE), idx)
	carryForwardUnreachable(prior, g, SourceVsphere, sourceFailed(errs, SourceVsphere), idx)
	h.Set(g)
	return false, errs
}
