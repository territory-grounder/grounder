// Package estate is the multi-source causal infrastructure graph — the substrate the prediction gate reasons
// over. It is the faithful re-expression of the predecessor's scripts/lib/infragraph.py (1,207 LOC) + the
// live gateway-state.db graph (725 entities / 701 edges / 348 predictions), replacing TG's hollow
// `NewDependencyGraph(map[string][]string{})` — the empty graph the port-fidelity audit flagged as TG
// re-importing the predecessor's #1 "wired-but-disconnected dead capability" failure mode.
//
// Model (grounded in the live schema): typed Entity identity = (Type, Name); a directed Edge means
// FROM depends-on TO (an `lxc -runs_on-> pve_node` is the lxc depending on its host). blast_radius(H) walks
// edges INTO H (who is affected if H fails); deps(H) walks OUT (what H needs). Multi-source truth is
// confidence-graded (tunnel 1.00 > pve 0.95 > netbox/librenms 0.90 > declared 0.85 > learned <=0.75, capped
// below the 0.80 suppression cutoff), merged by a MAX-confidence ratchet on (From,To,Rel) that never
// downgrades a better-evidenced edge, and self-expiring (live edges carry a valid_until).
//
// Provenance: [F] infragraph.py {upsert_edge MAX-ratchet, resolve_entity, traverse path-product, siblings
// 0.6x} re-expressed under the typed spine; the source→confidence table verified against the live
// infragraph_dynamics distribution. See docs/PORT-FIDELITY-AUDIT.md §1 + docs/SYSTEM-MAP.md.
package estate

import (
	"sort"
	"strings"
	"time"
)

// EntityType is a node kind in the graph (the live graph carries 13).
type EntityType string

const (
	TypePhysicalHost  EntityType = "physical_host"
	TypePVENode       EntityType = "pve_node"
	TypeVM            EntityType = "vm"
	TypeLXC           EntityType = "lxc"
	TypeNetworkDevice EntityType = "network_device"
	TypeTunnel        EntityType = "tunnel"
	TypeSite          EntityType = "site"
	TypeService       EntityType = "service"
	TypeHost          EntityType = "host"
)

// RelType is a dependency-edge kind. FROM depends-on TO in every case.
type RelType string

const (
	RelRunsOn    RelType = "runs_on"   // a guest depends on its hypervisor
	RelMemberOf  RelType = "member_of" // a device belongs to a site
	RelDependsOn RelType = "depends_on"
	RelRoutesVia RelType = "routes_via"
)

// Source is where an edge's evidence came from — its provenance.
type Source string

const (
	SourceTunnel   Source = "tunnel"
	SourcePVE      Source = "pve"
	SourceNetbox   Source = "netbox"
	SourceLibreNMS Source = "librenms"
	SourceDeclared Source = "declared"
	SourceIncident Source = "incident" // learned, capped
	SourceChaos    Source = "chaos"
)

// SourceConfidence is the sourcing policy: the fixed confidence each seeded source stamps on an edge
// (verified against the live infragraph_dynamics distribution — pve 0.95, netbox/librenms 0.90, declared
// 0.85, tunnel/chaos 1.0/0.90). Learned incident edges are NOT here — they use LearnedConfidence.
var SourceConfidence = map[Source]float64{
	SourceTunnel:   1.00,
	SourcePVE:      0.95,
	SourceNetbox:   0.90,
	SourceLibreNMS: 0.90,
	SourceDeclared: 0.85,
	SourceChaos:    0.90,
}

// LearnedConfidence is the incident-co-occurrence confidence: min(0.75, 0.4 + 0.05·count). It is HARD-CAPPED
// at 0.75, deliberately below the 0.80 suppression cutoff, so a heuristic edge can never outrank ground
// truth or trigger suppression on its own.
func LearnedConfidence(count int) float64 {
	c := 0.4 + 0.05*float64(count)
	if c > 0.75 {
		return 0.75
	}
	if c < 0 {
		return 0
	}
	return c
}

// LaplaceConfidence is the BASE-RATE-AWARE co-occurrence confidence: the Laplace-smoothed fraction of the
// primary's incidents in which the dependent also alerted, (hits+1)/(trials+2). Unlike the count-only ramp it
// penalizes a pair that co-occurs RARELY relative to how often the primary alerts — a dependent that follows
// the primary 5/5 times is a real dependency; 5/50 is coincidence, and the smoothing keeps the +1/+2 prior
// from over-trusting a thin sample. HARD-CAPPED at 0.75 like all learned evidence. With no trials recorded it
// falls back to the count-only LearnedConfidence.
func LaplaceConfidence(hits, trials int) float64 {
	if trials <= 0 {
		return LearnedConfidence(hits)
	}
	if hits < 0 {
		hits = 0
	}
	if hits > trials {
		hits = trials
	}
	c := float64(hits+1) / float64(trials+2)
	if c > 0.75 {
		return 0.75
	}
	return c
}

// Entity is a graph node; its identity is (Type, Name).
type Entity struct {
	Type EntityType
	Name string
}

func (e Entity) key() string { return string(e.Type) + "\x00" + e.Name }

// Edge is a directed dependency: From depends-on To, with a confidence, its winning provenance, an optional
// expiry (zero = open-ended), and the alerts a cascade along it is expected to fire.
type Edge struct {
	From           Entity
	To             Entity
	Rel            RelType
	Confidence     float64
	Source         Source
	ValidUntil     time.Time
	ExpectedAlerts []string
}

func edgeKey(from, to Entity, rel RelType) string {
	return from.key() + "|" + string(rel) + "|" + to.key()
}

// Impact is one entity reached from a blast-radius or sibling walk, with the path-product confidence and hop
// distance at which it was reached.
type Impact struct {
	Entity         Entity
	Confidence     float64
	Distance       int
	ExpectedAlerts []string
}

// Option configures a Graph.
type Option func(*Graph)

// WithClock injects the freshness clock so expiry is deterministic in tests.
func WithClock(now func() time.Time) Option { return func(g *Graph) { g.now = now } }

// Graph is the causal estate graph. It is built once per refresh from the connector adapters and read by the
// prediction gate; it holds no mutable per-request state.
type Graph struct {
	edges     map[string]*Edge   // (from,rel,to) → edge, for the MAX-ratchet upsert
	inName    map[string][]*Edge // canonical To NAME → edges pointing at that host (the blast-radius direction)
	names     map[string][]Entity
	aliasNorm map[string]Entity // normalized name (lower-cased canonName) + registered aliases → best entity (the alias/fuzzy resolution tiers — see resolve.go)
	now       func() time.Time
}

// NewGraph returns an empty graph.
func NewGraph(opts ...Option) *Graph {
	g := &Graph{edges: map[string]*Edge{}, inName: map[string][]*Edge{}, names: map[string][]Entity{}, aliasNorm: map[string]Entity{}, now: time.Now}
	for _, o := range opts {
		o(g)
	}
	return g
}

// canonName is the name-identity used across sources: a domain-stripped, trimmed hostname. Two edges about
// the same machine seen by different sources under different entity TYPES (NetBox physical_host, PVE pve_node,
// LibreNMS host) share one canonical name, so the blast-radius walk merges their edge sets instead of leaving
// each source's contribution on a disconnected typed twin.
func canonName(name string) string {
	return strings.SplitN(strings.TrimSpace(name), ".", 2)[0]
}

// canonical resolves a name to its most-specific typed entity (the Resolve rule); if the name is unknown it
// returns the given entity unchanged so a caller always has a concrete node to report.
func (g *Graph) canonical(e Entity) Entity {
	if best, ok := g.Resolve(e.Name); ok {
		return best
	}
	return e
}

// Upsert adds or STRENGTHENS an edge. Confidence is ratcheted upward only — MAX(existing, new) — so a
// re-seed from any source never downgrades a better-evidenced edge, and the provenance stored is that of the
// WINNING confidence (fixing the predecessor's misattribution bug, PORT-FIDELITY-AUDIT §1.7-1), except that
// chaos-grade evidence always overwrites the source label because it outranks the seed bucket.
func (g *Graph) Upsert(e Edge) {
	g.register(e.From)
	g.register(e.To)
	k := edgeKey(e.From, e.To, e.Rel)
	cur, ok := g.edges[k]
	if !ok {
		cp := e
		g.edges[k] = &cp
		g.inName[canonName(e.To.Name)] = append(g.inName[canonName(e.To.Name)], &cp)
		return
	}
	if e.Source == SourceChaos || e.Confidence > cur.Confidence {
		if e.Confidence > cur.Confidence {
			cur.Confidence = e.Confidence
			cur.Source = e.Source // provenance of the winning confidence
		} else if e.Source == SourceChaos {
			cur.Source = SourceChaos // chaos outranks the seed label even at equal confidence
		}
	}
	if e.ValidUntil.IsZero() || (!cur.ValidUntil.IsZero() && e.ValidUntil.After(cur.ValidUntil)) {
		cur.ValidUntil = e.ValidUntil // refresh (open-ended wins; else the later expiry)
	}
	if len(e.ExpectedAlerts) > 0 {
		cur.ExpectedAlerts = e.ExpectedAlerts
	}
}

func (g *Graph) register(e Entity) {
	g.indexAlias(e)         // self-populate the alias/fuzzy resolution tiers from the multi-source names (resolve.go)
	cn := canonName(e.Name) // index by canonical name so a domain-qualified node is findable by its bare form
	for _, x := range g.names[cn] {
		if x.Type == e.Type {
			return
		}
	}
	g.names[cn] = append(g.names[cn], e)
}

// Resolve maps a bare hostname to an existing typed entity — the single canonical node. A dropped resolution
// is a silent correctness bug: an edge written against the wrong typed node lands on a "disconnected twin"
// invisible to traversal. When several typed nodes share a name, the most specific placement type wins
// (pve_node/vm/lxc/physical_host before the generic host).
//
// It is a HYBRID resolver (design-wisdom #11): the EXACT tier below is byte-identical to the original and
// returns first, so a name that resolves today resolves to the SAME entity (no regression); only when the
// exact tier MISSES does it fall through to the alias/fuzzy tiers (resolve.go), which recover a case /
// domain-qualified / separator / registered-alias / IP variant of a machine that IS in the graph but whose
// reference form the exact index does not carry. ok=false only when NO tier resolves.
func (g *Graph) Resolve(name string) (Entity, bool) {
	cands := g.names[canonName(name)] // domain-stripped identity, matching how edges are indexed
	if len(cands) == 0 {
		return g.resolveHybrid(name) // exact miss — try the alias then fuzzy tiers before giving up
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if typeRank(c.Type) > typeRank(best.Type) {
			best = c
		}
	}
	return best, true
}

func typeRank(t EntityType) int {
	switch t {
	case TypePVENode, TypeVM, TypeLXC, TypePhysicalHost, TypeNetworkDevice:
		return 2 // concrete placement nodes
	case TypeService, TypeTunnel, TypeSite:
		return 1
	default:
		return 0 // generic host — the twin to avoid
	}
}

func (g *Graph) fresh(e *Edge) bool {
	return e.ValidUntil.IsZero() || e.ValidUntil.After(g.now())
}

// BlastRadius returns the transitive set of entities affected if target fails — the sources of edges pointing
// INTO target, walked up to maxDepth. Confidence is a PATH PRODUCT (it decays multiplicatively along the
// path); a cycle is prevented by the visited set; expired edges are filtered; and each reached entity is
// reduced to its shortest path, then to the highest confidence at that distance.
func (g *Graph) BlastRadius(target Entity, maxDepth int) []Impact {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	best := map[string]Impact{} // canonical dependent name → best impact
	type frontierNode struct {
		e    Entity
		conf float64
		d    int
	}
	targetName := canonName(target.Name)
	visited := map[string]bool{targetName: true}
	frontier := []frontierNode{{target, 1.0, 0}}
	for len(frontier) > 0 {
		var next []frontierNode
		for _, fn := range frontier {
			if fn.d >= maxDepth {
				continue
			}
			// Walk every edge pointing at ANY typed node sharing fn.e's name — so a host's dependents seen by
			// different sources under different entity types are all traversed, not just one source's twin.
			for _, ed := range g.inName[canonName(fn.e.Name)] {
				if !g.fresh(ed) {
					continue
				}
				dep := g.canonical(ed.From) // the entity that depends on fn.e, reduced to its canonical node
				depName := canonName(dep.Name)
				if depName == targetName {
					continue // an edge back to the target itself is not part of its own blast radius
				}
				conf := fn.conf * ed.Confidence
				imp := Impact{Entity: dep, Confidence: round4(conf), Distance: fn.d + 1, ExpectedAlerts: ed.ExpectedAlerts}
				if cur, ok := best[depName]; !ok || imp.Distance < cur.Distance || (imp.Distance == cur.Distance && imp.Confidence > cur.Confidence) {
					best[depName] = imp
				}
				if !visited[depName] {
					visited[depName] = true
					next = append(next, frontierNode{dep, conf, fn.d + 1})
				}
			}
		}
		frontier = next
	}
	return sortedImpacts(best)
}

// Siblings returns the common-cause siblings of target — entities that share an infrastructure parent (the
// same To via the same rel) with target — scored at SiblingPenalty × the edge confidence. This catches
// co-failure where the shared parent itself never alerts (the 2026-05-08 pattern: 4 VMs flap on one PVE node
// while the node stays silent) — a signal a pure who-depends-on-me walk misses entirely.
func (g *Graph) Siblings(target Entity) []Impact {
	const SiblingPenalty = 0.6
	best := map[string]Impact{}
	targetName := canonName(target.Name)
	// find target's parents (edges OUT of target: target depends-on parent), matched by canonical name so a
	// parent seen under a different type still counts.
	for _, ed := range g.edges {
		if canonName(ed.From.Name) != targetName || !g.fresh(ed) {
			continue
		}
		// The shared parent must be INFRASTRUCTURE whose silent failure cascades to its dependents — the
		// predecessor's siblings() `entity_type IN (infrastructure)` filter. A shared SITE (co-location is not
		// co-failure) or a shared logical SERVICE (a monitored dependency that would itself alert) never makes
		// two dependents common-cause siblings. Resolve the parent's authoritative type (a concrete node type
		// beats a generic host) before gating.
		parentType := ed.To.Type
		if resolved, ok := g.Resolve(ed.To.Name); ok {
			parentType = resolved.Type
		}
		if !siblingParentEligible(parentType) {
			continue
		}
		for _, sib := range g.inName[canonName(ed.To.Name)] {
			sibName := canonName(sib.From.Name)
			if !g.fresh(sib) || sibName == targetName || sib.Rel != ed.Rel {
				continue
			}
			conf := round4(SiblingPenalty * sib.Confidence)
			imp := Impact{Entity: g.canonical(sib.From), Confidence: conf, Distance: 1, ExpectedAlerts: sib.ExpectedAlerts}
			if cur, ok := best[sibName]; !ok || imp.Confidence > cur.Confidence {
				best[sibName] = imp
			}
		}
	}
	return sortedImpacts(best)
}

// Parent is one direct upstream dependency of an entity — the thing it runs on, routes via, or otherwise
// depends on — with the relation kind preserved (a runs_on hypervisor and a routes_via switch warrant
// different triage moves, so Impact's rel-less shape is not enough here).
type Parent struct {
	Entity     Entity
	Rel        RelType
	Confidence float64
}

// Parents returns target's DIRECT upstream dependencies (edges OUT of target, one hop): its hypervisor,
// upstream network device, site, and declared/learned dependencies. This is the "who could be the shared
// cause" set a triage session probes when it suspects a cascade. Expired edges are filtered; each parent is
// reduced to its best-confidence edge; ordering is confidence-descending then name (deterministic — the
// exact float compare is safe because confidences are discrete table values, never computed sums).
func (g *Graph) Parents(target Entity) []Parent {
	targetName := canonName(target.Name)
	best := map[string]Parent{}
	for _, ed := range g.edges {
		if canonName(ed.From.Name) != targetName || !g.fresh(ed) {
			continue
		}
		p := Parent{Entity: g.canonical(ed.To), Rel: ed.Rel, Confidence: ed.Confidence}
		key := canonName(p.Entity.Name)
		if cur, ok := best[key]; !ok || p.Confidence > cur.Confidence {
			best[key] = p
		}
	}
	out := make([]Parent, 0, len(best))
	for _, v := range best {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Entity.Name < out[j].Entity.Name
	})
	return out
}

// siblingParentEligible reports whether a shared parent may produce common-cause siblings: only a physical
// infrastructure / compute node whose SILENT failure cascades to its dependents (a PVE node, a physical/
// hypervisor host, a VM/LXC, a network device, a tunnel, or a generic host that stands in for one). An
// organizational or logical grouping — a shared SITE (co-location is not co-failure) or a shared SERVICE (a
// monitored logical dependency that would itself alert, so it is not a silent common cause) — never does.
// This is the predecessor siblings() infrastructure-parent filter (`entity_type IN ('pve_node',
// 'network_device','tunnel')`), adapted to TG's richer type model: TG types hypervisors as physical_host and
// LibreNMS network parents as the generic host, both of which DO cascade, where the predecessor had only
// pve_node — so those stay eligible (no regression) while the non-infrastructure groupings are excluded.
func siblingParentEligible(t EntityType) bool {
	switch t {
	case TypeSite, TypeService:
		return false
	default:
		return true
	}
}

func sortedImpacts(m map[string]Impact) []Impact {
	out := make([]Impact, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Distance != out[j].Distance {
			return out[i].Distance < out[j].Distance
		}
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Entity.Name < out[j].Entity.Name
	})
	return out
}

func round4(f float64) float64 { return float64(int(f*10000+0.5)) / 10000 }

// Len reports the number of distinct edges in the graph.
func (g *Graph) Len() int { return len(g.edges) }
