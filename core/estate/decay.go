package estate

// Decay-on-disproof for the SELF-LEARNING estate tier (spec/018, design-wisdom #11; Gulli ch14 — periodic
// reconciliation). The graph's confidence ratchet only ever goes UP (Upsert MAX-merges), so a learned edge
// that reality later contradicts would linger at full confidence forever. DecayOnDisproof is the down side of
// the ratchet: a fresh observation that the graph MISPREDICTED (verify's surprise-hosts + rule-mismatches,
// off the typed core/verify.VerdictDetail) reduces the confidence of the LEARNED edges incident to those
// hosts, and AGES OUT (expires) any that fall to/below a floor — so the estate tracks reality instead of
// accumulating stale learned edges.
//
// It is deliberately scoped to the self-learning tier (Source == SourceIncident): ground-truth live edges
// (tunnel / pve / netbox / librenms) and operator-declared edges are re-seeded from their systems of record
// every refresh, so a heuristic disproof NEVER decays them. It works on a CLONE and returns a new graph, so a
// published graph other goroutines are reading is never mutated in place (the estate's immutable-after-build
// discipline — Holder swaps atomically). This is a COMPETENCE-plane read-model correction: it ages LEARNED
// state only — it never touches the estate itself and never actuates. Mutation stays OFF.

import (
	"sort"
	"time"
)

// DefaultDecayFactor halves a disproved learned edge's confidence per disproof pass.
const DefaultDecayFactor = 0.5

// Disproof is a fresh observation that CONTRADICTS the learned estate tier. Hosts names the entities the
// observation showed the graph mispredicted around — verify's surprise-hosts (observed, unpredicted) plus its
// rule-mismatch hosts (predicted host, unpredicted rule), both read off the typed core/verify.VerdictDetail
// and mapped to bare hostnames by the caller (so this package stays decoupled from core/verify). At is the
// observation time — the new "as of" stamped on any edge the pass ages out (zero ⇒ the graph clock's now).
type Disproof struct {
	Hosts []string
	At    time.Time
}

// DecayOptions tunes one decay-on-disproof pass.
type DecayOptions struct {
	// Factor multiplies a disproved learned edge's confidence; it must be in (0,1). A value <=0 or >=1 uses
	// DefaultDecayFactor — a decay can only ever REDUCE confidence, never raise it (the ratchet's down side).
	Factor float64
	// Floor is the confidence at/below which a decayed learned edge is aged out (expired). A negative value
	// clamps to 0, so by default an edge is aged out only once it decays to zero.
	Floor float64
}

// DecayReport is the audit of one decay-on-disproof pass — no silent decisions.
type DecayReport struct {
	Decayed  int      // learned edges whose confidence was reduced
	AgedOut  int      // learned edges expired (their decayed confidence reached the floor)
	AgedKeys []string // the (from|rel|to) edge keys aged out, sorted — for logging
}

// DecayOnDisproof returns a graph in which every LEARNED (incident-sourced) edge incident to a disproved host
// has had its confidence multiplied by the decay factor, and any that reached the floor has been aged out
// (its ValidUntil set to the observation time, so the existing freshness filter excludes it from every
// traversal without deleting it — a later re-observation can re-establish it through the normal learned
// path). It NEVER decays ground-truth live edges or operator-declared edges, and it NEVER mutates the receiver
// (it works on a clone) — so a concurrent prediction read of the published graph is race-free. When no host
// resolves to a disproof, the receiver is returned unchanged (rep.Decayed == 0), so the caller can skip the
// atomic swap. This ages LEARNED state only; it never touches the estate itself and never actuates.
func (g *Graph) DecayOnDisproof(dis Disproof, opts DecayOptions) (*Graph, DecayReport) {
	factor := opts.Factor
	if factor <= 0 || factor >= 1 {
		factor = DefaultDecayFactor
	}
	floor := opts.Floor
	if floor < 0 {
		floor = 0
	}
	disproved := make(map[string]struct{}, len(dis.Hosts))
	for _, h := range dis.Hosts {
		if n := canonName(h); n != "" {
			disproved[n] = struct{}{}
		}
	}
	if len(disproved) == 0 {
		return g, DecayReport{}
	}
	at := dis.At
	if at.IsZero() {
		at = g.now()
	}
	out := g.clone()
	var rep DecayReport
	for k, e := range out.edges {
		if e.Source != SourceIncident {
			continue // only the self-learning tier decays; ground truth is re-seeded from reality each refresh
		}
		if !out.fresh(e) {
			continue // already expired — nothing to age
		}
		_, fromHit := disproved[canonName(e.From.Name)]
		_, toHit := disproved[canonName(e.To.Name)]
		if !fromHit && !toHit {
			continue
		}
		e.Confidence = round4(e.Confidence * factor)
		rep.Decayed++
		if e.Confidence <= floor {
			e.ValidUntil = at // age it out: the freshness filter now excludes it from every walk
			rep.AgedOut++
			rep.AgedKeys = append(rep.AgedKeys, k)
		}
	}
	sort.Strings(rep.AgedKeys)
	if rep.Decayed == 0 {
		return g, rep // nothing matched a learned edge — hand back the receiver, no swap needed
	}
	return out, rep
}

// clone returns a deep copy of the graph — independent Edge values and a rebuilt blast-radius index — so a
// caller can produce a modified snapshot (e.g. a decay pass) without mutating a published graph other
// goroutines are reading. The name index is copied and the freshness clock is shared (a func value).
func (g *Graph) clone() *Graph {
	c := &Graph{
		edges:     make(map[string]*Edge, len(g.edges)),
		inName:    make(map[string][]*Edge, len(g.inName)),
		names:     make(map[string][]Entity, len(g.names)),
		aliasNorm: make(map[string]Entity, len(g.aliasNorm)),
		now:       g.now,
	}
	for k, e := range g.edges {
		cp := *e
		c.edges[k] = &cp
		c.inName[canonName(cp.To.Name)] = append(c.inName[canonName(cp.To.Name)], &cp)
	}
	for n, ents := range g.names {
		c.names[n] = append([]Entity(nil), ents...)
	}
	for n, e := range g.aliasNorm {
		c.aliasNorm[n] = e // carry the alias/fuzzy resolution tiers into the decayed snapshot (resolve.go)
	}
	return c
}
