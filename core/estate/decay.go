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
	// Hosts is the flat set of disproved hostnames. Scoping by this alone decays EVERY learned edge incident
	// to ANY of them, which is the defect TG-206 names: a capture that mispredicted `web7` from target
	// `pve01` also decays an unrelated `web7 -> cache2` edge that no prediction ever got wrong, and it decays
	// edges out of `web7` that were CORRECT for other incidents. Retained because a caller that supplies only
	// this gets exactly the pre-TG-206 behaviour — narrowing silently would change the blast-radius model
	// under deployments that never asked for it.
	Hosts []string
	// Paths is the SCOPED form: one entry per captured misprediction, naming the target the prediction was
	// made FROM and the hosts it was surprised BY. When present it takes precedence, and only edges that
	// actually connect a target to one of ITS OWN surprise hosts decay — the edge that should have carried
	// the prediction and did not.
	//
	// The information was always there. core/falsify.DiscoveryRecord carries TargetHost beside SurpriseHosts
	// and the pair was collapsed to a flat []string before it reached the graph.
	Paths []DisproofPath
	At    time.Time
}

// DisproofPath is one misprediction: the target a prediction was made from, and what surprised it. DeviationKey
// and ActionID carry the CONTRADICTION IDENTITY (core/falsify.DiscoveryRecord's reproduction signature + the
// committed action id) so a decayed edge can be attributed to the exact misprediction that disproved it — the
// durable per-edge disproof record TG-206(a) preserves. Both are optional: a legacy caller that supplies only
// Target/Surprised decays exactly as before and simply produces an unattributed (blank-key) disproof record.
type DisproofPath struct {
	Target       string
	Surprised    []string
	DeviationKey string
	ActionID     string
}

// pairAttr is the contradiction identity carried on a scoped disproof pair, so a matched edge's durable
// disproof record can name the misprediction that disproved it (TG-206a).
type pairAttr struct {
	target       string
	deviationKey string
	actionID     string
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
	Decayed   int            // learned edges whose confidence was reduced
	AgedOut   int            // learned edges expired (their decayed confidence reached the floor)
	AgedKeys  []string       // the (from|rel|to) edge keys aged out, sorted — for logging
	Disproofs []EdgeDisproof // one per decayed learned edge, attributed to the misprediction that disproved it (TG-206a)
}

// EdgeDisproof is the durable, attributable record of ONE learned edge decayed by ONE misprediction: the edge's
// identity (key + endpoints + relation), the contradiction that disproved it (DeviationKey + ActionID, carried
// from the DisproofPath that matched), the confidence the edge was decayed TO this pass, and whether that
// decay aged it out. It is the "attach the contradiction to the edge" record (spec/018, TG-206a): rather than
// silently lowering a number and discarding the report, the losing reading is retained so a later verdict can
// vindicate or refute it, and the learned-tier lifecycle (TG-388) has a durable disproof substrate to consult.
// NON-SECRET: only host/relation slugs, hashes, and a confidence — no argv, credential, or token.
type EdgeDisproof struct {
	EdgeKey      string  // the (from|rel|to) edge key
	From         string  // the edge's source entity name (non-secret)
	Rel          string  // the relation type
	To           string  // the edge's destination entity name (non-secret)
	Target       string  // the target the disproving prediction was made FROM
	DeviationKey string  // the misprediction reproduction signature (blank for a legacy flat-host disproof)
	ActionID     string  // the committed action id of the disproving prediction (blank if not carried)
	// DecayedTo is the graph edge confidence immediately after THIS decay pass (oldConfidence * factor) — a
	// DECAY-TIME SNAPSHOT for the audit record, NOT the edge's live confidence (TG-444). Since TG-388 the
	// 5-minute estate refresh rebuilds every learned edge's confidence from the SOURCE counts
	// (LaplaceConfidence over the learner-decayed counts), a different formula and magnitude, so a persisted
	// DecayedTo no longer equals what the refresh will render. Read it as "what the graph decay computed at
	// the disproof instant", never as the current confidence.
	DecayedTo float64
	AgedOut   bool // true if the decayed confidence reached the floor and the edge was expired
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
	// SCOPED (path) form wins when supplied; otherwise the flat legacy set, unchanged. The pair map carries the
	// disproving path's CONTRADICTION IDENTITY (TG-206a) so a matched edge can be attributed to the exact
	// misprediction — first path wins for a pair two mispredictions share, deterministic in the caller's path
	// order, so the durable disproof record is stable.
	pairs := make(map[string]pairAttr)
	addPair := func(a, b string, at pairAttr) {
		if _, seen := pairs[a+"\x00"+b]; !seen {
			pairs[a+"\x00"+b] = at
		}
	}
	for _, p := range dis.Paths {
		t := canonName(p.Target)
		if t == "" {
			continue
		}
		at := pairAttr{target: p.Target, deviationKey: p.DeviationKey, actionID: p.ActionID}
		for _, h := range p.Surprised {
			if n := canonName(h); n != "" && n != t {
				addPair(t, n, at)
				addPair(n, t, at) // an edge implicating the pair points either way
			}
		}
	}
	disproved := make(map[string]struct{}, len(dis.Hosts))
	for _, h := range dis.Hosts {
		if n := canonName(h); n != "" {
			disproved[n] = struct{}{}
		}
	}
	if len(pairs) == 0 && len(disproved) == 0 {
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
		from, to := canonName(e.From.Name), canonName(e.To.Name)
		var attr pairAttr
		if len(pairs) > 0 {
			// THE MISPREDICTED PATH ONLY. Both endpoints must belong to the SAME capture — an edge between
			// two hosts that were each surprised by different incidents was never on either path.
			a, ok := pairs[from+"\x00"+to]
			if !ok {
				continue
			}
			attr = a
		} else {
			_, fromHit := disproved[from]
			_, toHit := disproved[to]
			if !fromHit && !toHit {
				continue
			}
		}
		e.Confidence = round4(e.Confidence * factor)
		rep.Decayed++
		agedOut := e.Confidence <= floor
		if agedOut {
			e.ValidUntil = at // age it out: the freshness filter now excludes it from every walk
			rep.AgedOut++
			rep.AgedKeys = append(rep.AgedKeys, k)
		}
		// Attach the contradiction to the edge (TG-206a): a durable, attributable disproof record instead of a
		// silently-lowered number and a discarded report.
		rep.Disproofs = append(rep.Disproofs, EdgeDisproof{
			EdgeKey: k, From: e.From.Name, Rel: string(e.Rel), To: e.To.Name,
			Target: attr.target, DeviationKey: attr.deviationKey, ActionID: attr.actionID,
			DecayedTo: e.Confidence, AgedOut: agedOut,
		})
	}
	sort.Strings(rep.AgedKeys)
	// Deterministic order: the edge map iterates randomly, so sort the disproof records by edge key before they
	// reach the durable store (a stable audit and a stable oracle).
	sort.Slice(rep.Disproofs, func(i, j int) bool { return rep.Disproofs[i].EdgeKey < rep.Disproofs[j].EdgeKey })
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
