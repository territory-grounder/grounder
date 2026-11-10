package estate

import (
	"context"
	"sync/atomic"
	"time"
)

// Holder holds the current estate graph behind an atomic pointer, so the worker can atomically REPLACE the
// graph on a periodic refresh — re-reading live topology sources (and later, incorporating newly-learned
// edges) — while in-flight prediction reads see a consistent snapshot and never a half-built graph. The
// prediction closures read through Graph(), so a refresh takes effect WITHOUT a restart. This is the
// primitive the runtime estate-refresh loop is built on; a bad refresh (all sources down) keeps the previous
// good graph rather than swapping in an empty one.
//
// THE SNAPSHOT IS A SHARED INPUT (TG-681). The agent seed's <estate> block, the predictor's blast radius and
// eligibility, the verifier's cross-site filter and the collateral re-check all read THIS pointer. Keeping the
// prior graph through a refresh outage is the right failure posture, but it means every one of those consumers
// answers from the same aging snapshot, and none of them can tell. The holder therefore stamps WHEN the serving
// snapshot was installed (RefreshedAt / Age), so the staleness is at least observable (the worker publishes it
// as tg_estate_graph_age_seconds) instead of invisible by construction.
type Holder struct {
	g atomic.Pointer[Graph]
	// refreshedAt is the UnixNano of the moment the SERVING snapshot was installed — the boot build, a Set, or a
	// Refresh whose rebuild was accepted. 0 means no graph was ever installed (the holder serves the empty
	// placeholder). A rejected refresh (kept=true) deliberately does NOT advance it: the prior snapshot keeps
	// serving, and its age keeps growing.
	refreshedAt atomic.Int64
	now         func() time.Time
}

// NewHolder wraps an initial graph. A nil initial graph is replaced with an empty graph, so Graph() never
// returns nil (a nil graph would panic the prediction path — fail toward an empty, non-vacuous graph instead).
// A non-nil initial graph counts as installed now; a nil one is a placeholder and reads as never installed.
func NewHolder(g *Graph) *Holder { return NewHolderWithClock(g, time.Now) }

// NewHolderWithClock is NewHolder with an injected clock, so the snapshot age is deterministic in tests. A nil
// clock falls back to time.Now.
func NewHolderWithClock(g *Graph, now func() time.Time) *Holder {
	if now == nil {
		now = time.Now
	}
	h := &Holder{now: now}
	if g == nil {
		h.g.Store(NewGraph())
		return h
	}
	h.g.Store(g)
	h.refreshedAt.Store(h.now().UnixNano())
	return h
}

// Graph returns the current snapshot. Safe for concurrent reads.
func (h *Holder) Graph() *Graph { return h.g.Load() }

// Set atomically replaces the graph and stamps the install time. A nil graph is ignored (the previous snapshot
// stands, and its stamp stands with it) — a refresh must never install a nil graph.
func (h *Holder) Set(g *Graph) {
	if g != nil {
		h.g.Store(g)
		h.refreshedAt.Store(h.clock().UnixNano())
	}
}

// RefreshedAt reports when the SERVING snapshot was installed. ok=false when no graph was ever installed — the
// holder was created with nil and every refresh since was rejected — which a consumer must read as "unknown
// age", never as "fresh".
func (h *Holder) RefreshedAt() (time.Time, bool) {
	if h == nil {
		return time.Time{}, false
	}
	ns := h.refreshedAt.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// Age reports how long the serving snapshot has been serving: now minus RefreshedAt. ok=false when
// RefreshedAt is unknown. It grows through a refresh outage, because a rejected rebuild keeps the prior
// snapshot (see Refresh) — that growth IS the staleness signal every consumer of Graph() otherwise lacks.
func (h *Holder) Age() (time.Duration, bool) {
	at, ok := h.RefreshedAt()
	if !ok {
		return 0, false
	}
	age := h.clock().Sub(at)
	if age < 0 {
		age = 0
	}
	return age, true
}

func (h *Holder) clock() time.Time {
	if h == nil || h.now == nil {
		return time.Now()
	}
	return h.now()
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
