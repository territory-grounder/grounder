// Package estate is the multi-source causal infrastructure graph — the substrate the prediction gate reasons
// over. It is the faithful re-expression of the predecessor's scripts/lib/infragraph.py (1,207 LOC) + the
// live gateway-state.db graph (725 entities / 701 edges / 348 predictions), replacing TG's hollow
// `NewDependencyGraph(map[string][]string{})` — the empty graph the port-fidelity audit flagged as TG
// re-importing the predecessor's #1 "wired-but-disconnected dead capability" failure mode.
//
// Model (grounded in the live schema): typed Entity identity = (Type, Name); a directed Edge means
// FROM depends-on TO (an `lxc -runs_on-> pve_node` is the lxc depending on its host). blast_radius(H) walks
// edges INTO H (who is affected if H fails); deps(H) walks OUT (what H needs). Multi-source truth is
// confidence-graded (tunnel 1.00 > pve 0.95 > vsphere 0.94 > netbox/librenms 0.90 > declared 0.85 > slurpit
// 0.82 > learned <=0.75 — vsphere is a live-hypervisor peer of pve (TG-91); slurpit discovered-inventory
// slots between declared and learned but stays ABOVE the 0.80 suppression cutoff, so it counts as observed
// ground truth; only learned is capped below it), merged by a
// MAX-confidence ratchet on (From,To,Rel) that never downgrades a better-evidenced edge, and self-expiring
// (live edges carry a valid_until).
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

// EntityType is a node kind in the graph (the live graph carries 14).
type EntityType string

const (
	TypePhysicalHost  EntityType = "physical_host"
	TypePVENode       EntityType = "pve_node"
	TypeVM            EntityType = "vm"
	TypeLXC           EntityType = "lxc"
	TypeNetworkDevice EntityType = "network_device"
	// TypeStorageAppliance is a dedicated storage device (a Synology DSM NAS) — the storage-plane sibling
	// of TypeNetworkDevice (TG-78 storage slice). Stamped by the LibreNMS topology source from the os the
	// API already returns (os=dsm); DomainOf routes every alert family on such a host to storage
	// competence the way a PVE node routes to Proxmox — the appliance's identity dominates the symptom.
	TypeStorageAppliance EntityType = "storage_appliance"
	TypeTunnel           EntityType = "tunnel"
	TypeSite             EntityType = "site"
	TypeService          EntityType = "service"
	// TypeCluster is a virtualization / placement GROUP (e.g. a NetBox cluster) — a LOGICAL grouping, never a
	// physical hypervisor. It exists because emitting a cluster as TypePVENode let a placement group (even a
	// Synology DSM cluster) impersonate a Proxmox node, carry 133 children, and keep HasGroundTruth true when
	// the real per-node edge was gone (TG-390). It is deliberately NOT a sibling-parent and NOT ground truth.
	TypeCluster EntityType = "cluster"
	TypeHost    EntityType = "host"
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
	SourceTunnel Source = "tunnel"
	SourcePVE    Source = "pve"
	// SourceVsphere is VMware vCenter VM placement (TG-91) — a live-hypervisor authority ALONGSIDE pve. It is
	// DISCOVERED reality: vCenter is the source of truth for which host a VM runs on, exactly as pve is for
	// Proxmox. Its confidence (0.94, in SourceConfidence below) sits one notch below pve (0.95) — DISTINCT per
	// the tie-contract documented there — while staying well inside the ground-truth band (>0.80). vSphere and
	// pve write DISJOINT guests (vCenter VMs vs Proxmox guests), so their ranks never compete on one edge.
	SourceVsphere  Source = "vsphere"
	SourceNetbox   Source = "netbox"
	SourceLibreNMS Source = "librenms"
	SourceDeclared Source = "declared"
	// SourceSlurpit is Slurp'it network-device discovery/inventory (TG-91). It is DISCOVERED reality (the
	// tool logs into real devices and records their inventory + site), so it outranks a heuristic guess, but
	// it is a periodic config scrape subject to planned-vs-actual drift rather than a live control-plane
	// probe — so it sits just below declared. Its confidence (0.82) is in SourceConfidence below.
	SourceSlurpit  Source = "slurpit"
	SourceIncident Source = "incident" // learned, capped
	SourceChaos    Source = "chaos"
)

// SourceConfidence is the sourcing policy: the fixed confidence each seeded source stamps on an edge
// (verified against the live infragraph_dynamics distribution — pve 0.95, netbox/librenms 0.90, declared
// 0.85, tunnel/chaos 1.0/0.90). Learned incident edges are NOT here — they use LearnedConfidence.
//
// SLURPIT SITS AT 0.82 — BETWEEN DECLARED (0.85) AND THE 0.80 GROUND-TRUTH CUTOFF (TG-91). Two facts fix the
// number. (1) It is DISCOVERED, not declared: Slurp'it logs into real network devices and records their
// inventory + site membership, which is observed reality, not a co-occurrence heuristic — so it must clear
// GroundTruthCutoff (0.80) and count as ground truth, and 0.82 does. (2) It is INVENTORY, not a live probe:
// a Slurp'it record is a periodic config scrape carrying planned-vs-actual drift, weaker evidence than a
// deliberately-curated declared dependency (0.85) or the live hypervisor/CMDB sources above it — so it sits
// below declared. The ticket's own guidance ("discovered-topology likely between declared and learned")
// lands here. 0.82 is also deliberately DISTINCT from every other writer (1.00/0.95/0.90/0.85/<=0.75): the
// Upsert tie-contract (see Upsert, the ExpectedAlerts/DelaySeconds/RecoverySeconds blocks) warns that a NEW
// source landing at a COINCIDENT confidence that also sets those fields must revisit last-writer ambiguity —
// a distinct rank keeps slurpit clear of that, its ExpectedAlerts strictly lose to declared/pve above and
// strictly win over learned below, never tie.
var SourceConfidence = map[Source]float64{
	SourceTunnel:   1.00,
	SourcePVE:      0.95,
	SourceVsphere:  0.94,
	SourceNetbox:   0.90,
	SourceLibreNMS: 0.90,
	SourceDeclared: 0.85,
	SourceSlurpit:  0.82,
	SourceChaos:    0.90,
}

// GroundTruthCutoff is the 0.80 suppression cutoff as a NAMED constant — the line between an edge a source
// observed (pve 0.95 / netbox+librenms 0.90 / declared 0.85) and one a heuristic inferred (learned, capped at
// 0.75). It was documented in prose all over this package and declared nowhere, so a consumer that needed to
// ask "is the graph's knowledge here ground truth or a guess?" had to re-type the literal. The first such
// consumer is the judge's estate grounding (core/judge, TG-202): it may only say "the graph says these two are
// unrelated" about entities the graph knows through observed edges — a pair joined solely by capped
// co-occurrence guesses is knowledge THIN ENOUGH that its silence means nothing.
const GroundTruthCutoff = 0.80

// LearnedConfidenceCap is the ceiling every learned (co-occurrence) confidence saturates at. It was four
// re-typed 0.75 literals until 2026-08-06, when the number turned out to matter for a second reason: on the
// live estate 1,481 of 1,524 learned edges (97.2%) sit EXACTLY here, so a filter threshold is either below
// the cap (removing 43 edges) or above it (removing all 1,524). Naming it lets a consumer ask "is this edge
// at the cap?" without re-typing the constant, which is how the two would drift apart.
//
// It stays deliberately below GroundTruthCutoff so a heuristic edge can never outrank ground truth.
const LearnedConfidenceCap = 0.75

// LearnedConfidence is the incident-co-occurrence confidence: min(0.75, 0.4 + 0.05·count). It is HARD-CAPPED
// at 0.75, deliberately below the 0.80 suppression cutoff, so a heuristic edge can never outrank ground
// truth or trigger suppression on its own.
func LearnedConfidence(count int) float64 {
	c := 0.4 + 0.05*float64(count)
	if c > LearnedConfidenceCap {
		return LearnedConfidenceCap
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
	if c > LearnedConfidenceCap {
		return LearnedConfidenceCap
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
	// DelaySeconds is the learned mean PROPAGATION delay along this dependency: how long, on average, after the
	// To (root) alerts does the From (dependent) alert. The co-occurrence learner already measures this per pair
	// (CoOccurrence.MeanDelaySeconds, TG-188) but had nowhere to carry it — so it was computed and discarded.
	// Carried here it rides the graph (Export/relay) and is available to a timing-aware consumer. Zero means
	// UNLEARNED (a source that measures no delay, e.g. ground-truth PVE/NetBox), never "instantaneous" — the same
	// discipline MeanDelaySeconds uses; a consumer must treat 0 as "no estimate".
	DelaySeconds float64
	// RecoverySeconds is the learned mean RECOVERY time (MTTR) along this dependency: how long, on average,
	// after the From (dependent) alerts does it then recover. Fed by the chaos ground-truth tier (the
	// injection's observed downstream recovery, TG-188 slice 2c); the co-occurrence learner does NOT measure it
	// (it never sees clears). Carried here it rides the graph (Export/relay) for a recovery-aware consumer.
	// Zero means UNLEARNED (no observed recovery), never "instantaneous" — the same discipline DelaySeconds
	// uses; a consumer must treat 0 as "no estimate".
	RecoverySeconds float64
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
	// Learned is true when this impact rests on at least one SourceIncident (co-occurrence) edge — a GUESS
	// capped at LearnedConfidenceCap, never ground truth. It is carried because provenance CANNOT be recovered
	// from the number here the way a single Parent edge's can: BlastRadius confidence is a path PRODUCT and
	// Siblings multiplies by a penalty, so a rendered 0.57 sibling or a 0.81 four-hop authoritative path both
	// sit below GroundTruthCutoff for reasons that have nothing to do with being learned. A renderer that
	// presents a guess as a fact is the TG-391 defect (37 fabricated parents shown identically to PVE truth).
	Learned bool
}

// Option configures a Graph.
type Option func(*Graph)

// WithClock injects the freshness clock so expiry is deterministic in tests.
func WithClock(now func() time.Time) Option { return func(g *Graph) { g.now = now } }

// WithDefaultEdgeSchema attaches a FRESH observe-only edge-triple validator (TG-207) to the graph, so every
// Upsert checks its (FromType, Rel, ToType) against DefaultEdgeSchema and an unlisted triple is COUNTED —
// never dropped. Fresh per Build, so the count reflects the CURRENT graph's edges rather than accumulating
// across refreshes. Until a caller opts in with this, g.schema stays nil and Check is a no-op — which is how
// the validator shipped in !1044 was PRESENT-BUT-UNWIRED (defined, unit-tested, never attached to a
// production graph, so nothing was ever validated). Read the count via Schema().UnknownCount().
func WithDefaultEdgeSchema() Option {
	return func(g *Graph) { g.schema = NewEdgeSchema(DefaultEdgeSchema()) }
}

// Graph is the causal estate graph. It is built once per refresh from the connector adapters and read by the
// prediction gate; it holds no mutable per-request state.
type Graph struct {
	edges     map[string]*Edge   // (from,rel,to) → edge, for the MAX-ratchet upsert
	inName    map[string][]*Edge // canonical To NAME → edges pointing at that host (the blast-radius direction)
	names     map[string][]Entity
	aliasNorm map[string]Entity // normalized name (lower-cased canonName) + registered aliases → best entity (the alias/fuzzy resolution tiers — see resolve.go)
	now       func() time.Time
	// schema validates the (FromType, Rel, ToType) triple at WRITE time (TG-207). OBSERVE-ONLY: an unlisted
	// triple is counted and reported, never dropped — see EdgeSchema for why rejection cannot be the day-one
	// behaviour. nil means no validation at all, which is what every existing caller gets until it opts in.
	schema *EdgeSchema
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
	// SCHEMA CHECK AT WRITE TIME (TG-207). The only type-legality logic used to run at READ time inside the
	// Siblings walk, so a malformed adapter installed an illegal edge that then participated in blast-radius
	// prediction. This records; it does not refuse — dropping edges against an unproven table would corrupt
	// the very model it protects.
	g.schema.Check(e.From.Type, e.Rel, e.To.Type)
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
	// TG-188 (chaos-measured ExpectedAlerts): the expected-alert set follows the WINNING provenance on the same
	// rule as DelaySeconds/RecoverySeconds below — a lower-confidence writer (e.g. a learned re-seed at 0.75)
	// must not stomp a chaos-MEASURED set on a shared edge key, which the previous unconditional replacement
	// allowed. A source at least as authoritative as the incumbent (chaos, or confidence >= the already-ratcheted
	// current) with a NON-EMPTY set replaces it; a same-source re-seed refreshes; an empty set never clobbers
	// (absent is "no measurement", not "expect nothing"). AFTER the confidence ratchet, so cur.Confidence is the
	// winner's.
	//
	// TIE CONTRACT (this block and the two below): the non-strict >= exists FOR the same-source re-seed. At a
	// confidence tie between two DIFFERENT sources these fields would be last-writer-wins — order-dependent,
	// unlike the Confidence field's strict ratchet. That cannot fire today (the writers of these fields sit at
	// distinct ranks: chaos 0.90 / declared 0.85 / learned <=0.75); a NEW source landing at a coincident
	// confidence that also sets one of these fields must revisit this contract (fresh-eyes review note, TG-188).
	if len(e.ExpectedAlerts) > 0 && (e.Source == SourceChaos || e.Confidence >= cur.Confidence) {
		cur.ExpectedAlerts = e.ExpectedAlerts
	}
	// TG-188 s2d: the delay follows the WINNING provenance, not the last writer — a lower-confidence learned
	// delay must not overwrite a ground-truth chaos delay on a shared edge key. A source at least as
	// authoritative as the incumbent (chaos, or confidence >= the already-ratcheted current) with a measured
	// delay sets it; a same-source re-seed refreshes; 0 = unlearned, never clobbers. Placed AFTER the confidence
	// ratchet, so cur.Confidence is the winner's — e.Confidence >= cur.Confidence is true iff e just won or tied.
	if e.DelaySeconds > 0 && (e.Source == SourceChaos || e.Confidence >= cur.Confidence) {
		cur.DelaySeconds = e.DelaySeconds
	}
	// RecoverySeconds ratchets exactly like DelaySeconds (TG-188 slice 2c): a source at least as authoritative
	// as the incumbent (chaos, or confidence >= the already-ratcheted current) with a measured recovery sets it;
	// a same-source re-seed refreshes; 0 = unlearned, never clobbers. AFTER the confidence ratchet so
	// cur.Confidence is the winner's.
	if e.RecoverySeconds > 0 && (e.Source == SourceChaos || e.Confidence >= cur.Confidence) {
		cur.RecoverySeconds = e.RecoverySeconds
	}
}

// reconcileInferredEdges drops INFERRED `depends_on` edges whose direction is unsupportable, measured on the
// 2026-08-06 pve03 cascade (TG-379) and extended to the chaos tier (TG-188). Two inferred tiers are eligible:
// the learned tier (SourceIncident, direction fabricated from correlation ordering) and the chaos tier
// (SourceChaos, root is ground truth but the DEPENDENT is still inferred from what co-alarmed). Authoritative
// edges are never touched.
//
//   - BACKWARDS CAUSALITY (both inferred tiers). Co-alarm is symmetric — everything on a failed host alarms at
//     once — so an inferred `depends_on` arrow can INVERT the authoritative containment: "pve03 depends_on
//     <a guest it hosts>". If an authoritative (non-learned) `runs_on` says the To of the depends_on runs on
//     its From (To is a guest of From), then From is the HOST and cannot depend on its guest. Reject. For chaos
//     this is the shared-resource artifact — inject a fault on a GUEST and its host alarms (a disk/CPU they
//     share), which co-alarm reads as "host depends_on guest"; admitting that at chaos's 0.90 would invert the
//     blast-radius hierarchy. A real host→guest resource coupling is modeled explicitly, never by inverting
//     runs_on.
//   - CROSS-SITE co-occurrence (LEARNED ONLY). Two hosts in different sites cannot depend on each other in any
//     physical sense (dc2pve02 vs dc1pve03); co-occurrence merely alarmed in one correlation window
//     that spans sites, so a learned cross-site depends_on is rejected. CHAOS IS EXEMPT: a cross-site chaos
//     cascade is a fault we actually injected and then observed propagate across the boundary — exactly the
//     ground-truth cross-site coupling (over a tunnel/route) that co-occurrence could never justify.
//
// Runs after Build. Rejecting here (rather than at Upsert, which is deliberately record-only, TG-207) keeps the
// drop scoped to the tiers that infer direction.
func (g *Graph) reconcileInferredEdges() int {
	sites := g.siteIndex()
	runsOn := g.authoritativeRunsOn()
	var dropped int
	for k, e := range g.edges {
		if e.Rel != RelDependsOn {
			continue
		}
		// Only the inferred tiers are removable — authoritative edges are never dropped.
		if e.Source != SourceIncident && e.Source != SourceChaos {
			continue
		}
		// (1) inverts an authoritative containment edge: To runs_on From ⇒ From is the host, and a host does
		// not depend on its guest. Applies to BOTH inferred tiers. Matched on canonical NAME, not the entity
		// key — the inference stamps TypeHost while PVE stamps TypeLXC/TypeVM→pve_node for the same machine, so
		// an edge-key compare would miss the very inversion this exists to catch.
		if runsOn[canonName(e.To.Name)+"\x00"+canonName(e.From.Name)] {
			g.removeEdge(k)
			dropped++
			continue
		}
		// (2) spans two KNOWN, DIFFERENT sites — LEARNED ONLY (chaos cross-site is injected-and-observed ground
		// truth, not a correlation-window storm, so it is kept).
		if e.Source == SourceIncident {
			sf, okf := sites[canonName(e.From.Name)]
			st, okt := sites[canonName(e.To.Name)]
			if okf && okt && sf != st {
				g.removeEdge(k)
				dropped++
			}
		}
	}
	return dropped
}

// authoritativeRunsOn indexes non-learned runs_on containment by canonical (from,to) name, so the reconcile
// recognises an inversion regardless of the entity TYPE each source stamped for the same machine.
func (g *Graph) authoritativeRunsOn() map[string]bool {
	out := map[string]bool{}
	for _, e := range g.edges {
		if e.Rel == RelRunsOn && e.Source != SourceIncident {
			out[canonName(e.From.Name)+"\x00"+canonName(e.To.Name)] = true
		}
	}
	return out
}

// IsGuest reports whether name is a virtualization GUEST — the From of an AUTHORITATIVE runs_on edge (a guest
// runs_on its hypervisor, RelRunsOn). Learned (co-occurrence) runs_on edges are excluded: guest-ness is a
// containment FACT from an inventory source (pve/netbox/vsphere), never a co-alarm inference — the same
// discipline siteIndex applies to member_of. The skill-domain classifier uses this to route a guest-DOWN
// incident to Proxmox competence regardless of which sensor detected it (intake-independent), so it must be a
// fact, not a guess. A nil graph (unseeded estate) is not a guest — fail closed to the domain-unknown default.
func (g *Graph) IsGuest(name string) bool {
	if g == nil {
		return false
	}
	cn := canonName(name)
	for _, e := range g.edges {
		// A From that is ITSELF a PVE node does not count: the live estate carries at least one
		// authoritative pve_node runs_on edge (netbox models dc1pve02 as running on the Synology it
		// boots from), and counting it made a HYPERVISOR classify as a guest — a node-DOWN alert then
		// composed the GUEST-lifecycle frame ("a reversible start is a candidate") for the one host class
		// where that frame is dangerous (TG-78 node-plane slice). The exclusion is BY TYPE, not by a
		// vm/lxc allowlist: netbox types some real guests as plain hosts (dc1redis03), and an
		// allowlist would silently de-guest them.
		if e.Rel == RelRunsOn && e.Source != SourceIncident && e.From.Type != TypePVENode && canonName(e.From.Name) == cn {
			return true
		}
	}
	return false
}

// IsPveNode reports whether name is a Proxmox HYPERVISOR NODE — an entity typed TypePVENode at either end
// of an AUTHORITATIVE edge (guests run_on it; the node itself may carry edges to hardware or storage).
// Same discipline as IsGuest: an inventory fact, never a co-alarm inference, and a nil graph fails closed.
// The skill-domain classifier routes EVERY alert on a node to Proxmox competence: the host's identity as a
// hypervisor dominates — whatever the symptom, the never-touch-host floor and the name-the-plane doctrine
// apply (TG-78 node-plane slice).
func (g *Graph) IsPveNode(name string) bool {
	if g == nil {
		return false
	}
	cn := canonName(name)
	for _, e := range g.edges {
		if e.Source == SourceIncident {
			continue
		}
		if (e.From.Type == TypePVENode && canonName(e.From.Name) == cn) || (e.To.Type == TypePVENode && canonName(e.To.Name) == cn) {
			return true
		}
	}
	return false
}

// typedAtEitherEnd is the shared body of the per-class host signals (IsPveNode predates it and keeps its
// own copy verbatim): does ANY authoritative edge carry an endpoint of the given type under this canonical
// name? An inventory fact, never a co-alarm inference; a nil graph fails closed.
func (g *Graph) typedAtEitherEnd(name string, t EntityType) bool {
	if g == nil {
		return false
	}
	cn := canonName(name)
	for _, e := range g.edges {
		if e.Source == SourceIncident {
			continue
		}
		if (e.From.Type == t && canonName(e.From.Name) == cn) || (e.To.Type == t && canonName(e.To.Name) == cn) {
			return true
		}
	}
	return false
}

// IsNetworkDevice reports whether name is a NETWORK DEVICE (switch/router/AP/firewall — an entity typed
// TypeNetworkDevice on an authoritative edge; the LibreNMS topology source stamps it from the device os:
// ios/iosxe/ciscosb/asa). The skill-domain classifier routes EVERY alert family on such a host to the
// cisco pack's network competence (TG-78 network slice): there is no honest rule prefix — the measured
// vocabulary is generic SNMP rules shared with guests and PVE nodes — so the device's estate identity is
// the only signal that does not steal from another lane.
func (g *Graph) IsNetworkDevice(name string) bool { return g.typedAtEitherEnd(name, TypeNetworkDevice) }

// IsStorageAppliance reports whether name is a dedicated STORAGE APPLIANCE (a Synology DSM NAS — typed
// TypeStorageAppliance, stamped from os=dsm). Routes every alert family on the appliance to storage
// competence (TG-78 storage slice) — the appliance identity dominates the symptom, exactly the IsPveNode
// discipline. NOTE the near-alias hazard this signal must never trip on: netbox models a hypervisor
// boot-from-NAS edge against the HYPHENATED name (dc1-syno01, typed pve_node), which canonName keeps
// distinct from the alerting hostname dc1syno01 — pinned by TestSynoAliasNeverLeaksAcrossNames.
func (g *Graph) IsStorageAppliance(name string) bool { return g.typedAtEitherEnd(name, TypeStorageAppliance) }

// siteIndex maps each device's canonical name to its site, from the authoritative member_of edges (a learned
// member_of, were one ever emitted, is ignored — site membership is not a co-occurrence fact).
func (g *Graph) siteIndex() map[string]string {
	out := map[string]string{}
	for _, e := range g.edges {
		if e.Rel == RelMemberOf && e.To.Type == TypeSite && e.Source != SourceIncident {
			out[canonName(e.From.Name)] = canonName(e.To.Name)
		}
	}
	return out
}

// removeEdge deletes an edge from both the primary map and the inName (blast-radius) index, so a dropped edge
// cannot still be reached by a To-name walk.
func (g *Graph) removeEdge(k string) {
	e, ok := g.edges[k]
	if !ok {
		return
	}
	delete(g.edges, k)
	cn := canonName(e.To.Name)
	kept := g.inName[cn][:0]
	for _, p := range g.inName[cn] {
		if edgeKey(p.From, p.To, p.Rel) != k {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		delete(g.inName, cn)
	} else {
		g.inName[cn] = kept
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
	e, _, ok := g.ResolveTiered(name)
	return e, ok
}

// ResolveTiered is Resolve plus the WITNESS of which tier answered — exact, alias or fuzzy. Resolve is
// defined in terms of it, so there is one resolution ladder and not two that can drift.
//
// The tier matters to any caller that would treat a resolution as an IDENTITY CLAIM rather than a lookup
// convenience. The fuzzy tier folds separators (`dc1-pve01` == `dc1pve01`): excellent for recovering
// a host whose reference form differs, and NOT a confirmed identity — it is a well-founded guess. The judge's
// estate grounding (core/judge, TG-202) therefore refuses to score an axis, in EITHER direction, off a
// fuzzy-tier resolution: it will neither penalise a diagnosis for naming a host it only guessed at, nor
// reward one. Callers that just want the best node keep calling Resolve and are unaffected.
func (g *Graph) ResolveTiered(name string) (Entity, Tier, bool) {
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
	return best, TierExact, true
}

func typeRank(t EntityType) int {
	switch t {
	case TypePVENode, TypeVM, TypeLXC, TypePhysicalHost, TypeNetworkDevice, TypeStorageAppliance:
		return 2 // concrete placement nodes
	case TypeService, TypeTunnel, TypeSite, TypeCluster:
		return 1 // groupings and logical nodes — a concrete placement node (rank 2) wins a name collision
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
		e       Entity
		conf    float64
		d       int
		learned bool
	}
	targetName := canonName(target.Name)
	visited := map[string]bool{targetName: true}
	frontier := []frontierNode{{target, 1.0, 0, false}}
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
				// A PATH is a guess if ANY edge along it is learned: a blast radius routed through a
				// co-occurrence edge is not a topology fact even if the reached node has authoritative edges too.
				learned := fn.learned || ed.Source == SourceIncident
				imp := Impact{Entity: dep, Confidence: round4(conf), Distance: fn.d + 1, ExpectedAlerts: ed.ExpectedAlerts, Learned: learned}
				if cur, ok := best[depName]; !ok || imp.Distance < cur.Distance || (imp.Distance == cur.Distance && imp.Confidence > cur.Confidence) {
					best[depName] = imp
				}
				if !visited[depName] {
					visited[depName] = true
					next = append(next, frontierNode{dep, conf, fn.d + 1, learned})
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
			// The sibling inference rests on TWO edges — target→parent (ed) and sibling→parent (sib). If either
			// is a co-occurrence edge, "these two share a parent" is a guess, not a topology fact.
			learned := ed.Source == SourceIncident || sib.Source == SourceIncident
			imp := Impact{Entity: g.canonical(sib.From), Confidence: conf, Distance: 1, ExpectedAlerts: sib.ExpectedAlerts, Learned: learned}
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
	// Source is the provenance of the winning edge — WHO SAID SO. A caller that reports an adjacency as a
	// FACT (the judge's estate grounding, TG-202) has to be able to say whether it rests on PVE ground truth
	// or on a capped incident co-occurrence, because those two are not the same claim about the world.
	Source Source
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
		p := Parent{Entity: g.canonical(ed.To), Rel: ed.Rel, Confidence: ed.Confidence, Source: ed.Source}
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

// InDegree reports how many DISTINCT estate entities depend on e — the count of FRESH edges pointing INTO
// e's canonical name, de-duplicated by dependent name. This is the causal weight the cascade-collapse
// election reads (TG-385): the node the most things fall WITH is the one to investigate, and a hypervisor
// carrying dozens of guests has a high in-degree while each guest has ~none. Read-only; O(edges into e).
//
// It counts DEPENDENTS, matched by canonical name so a host's dependents seen by different sources under
// different entity types are counted once, not once per typed twin — the same discipline BlastRadius walks.
// An entity the graph does not know returns 0, which the election treats as "no causal weight", never as a
// claim of centrality.
func (g *Graph) InDegree(e Entity) int {
	seen := map[string]bool{}
	for _, ed := range g.inName[canonName(e.Name)] {
		if !g.fresh(ed) {
			continue
		}
		dep := canonName(ed.From.Name)
		if dep == canonName(e.Name) {
			continue // a self-edge is not a dependent
		}
		seen[dep] = true
	}
	return len(seen)
}

// RunsOnParent returns e's best-confidence FRESH runs_on parent — the cascading infrastructure node it is
// placed on (a hypervisor / physical host) — and whether one exists. It is the authoritative CONTAINMENT
// edge TG-375 restored, read by the election's second tie-break ("the member whose host is a parent of the
// most other members"). The parent TYPE is gated by siblingParentEligible (the same allow-list the sibling
// walk uses) so a malformed runs_on edge into a non-cascading type — possible because the edge schema is
// observe-only, not enforced — cannot be read as a placement parent. Not-found (ok=false) for a host with
// no fresh runs_on containment, never an invented one.
func (g *Graph) RunsOnParent(e Entity) (Entity, bool) {
	for _, p := range g.Parents(e) { // confidence-descending; the first eligible runs_on edge is the placement
		if p.Rel == RelRunsOn && siblingParentEligible(p.Entity.Type) {
			return p.Entity, true
		}
	}
	return Entity{}, false
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
	// EXPLICIT ALLOW-LIST (TG-390): only a concrete infrastructure node whose SILENT failure cascades to its
	// dependents can make two dependents common-cause siblings. Flipping this from the old deny-list (Site,
	// Service) to an allow-list means an UNRECOGNISED parent type is ineligible BY DEFAULT — notably a
	// virtualization TypeCluster (a logical placement group, not a physical hypervisor), which as a deny-list
	// omission had silently become an eligible common-cause parent with 133 children. The eligible set is the
	// same concrete/placement/network types the old deny-list left in (Host/VM/LXC still parent shared
	// services), minus the logical groupings; the ONLY behavioural change is that Cluster and any future type
	// are now excluded until someone decides they belong.
	switch t {
	case TypePhysicalHost, TypePVENode, TypeNetworkDevice, TypeStorageAppliance, TypeTunnel, TypeHost, TypeVM, TypeLXC:
		return true
	default:
		return false // TypeSite, TypeService, TypeCluster, and any unrecognised new type
	}
}

// HasGroundTruth reports whether the graph holds LIVE, OBSERVED topology touching this entity: at least one
// unexpired edge at or above GroundTruthCutoff, in either direction, on its canonical name.
//
// ★ IT EXISTS TO SEPARATE "THE GRAPH SAYS NO" FROM "THE GRAPH DOES NOT KNOW" (TG-202). Resolve answering
// ok=true only means the NAME is in the index — a name can be there because one expired edge once mentioned
// it, or because a co-occurrence heuristic guessed at it. Treating those as knowledge would let an absent
// path be read as a positive claim of unrelatedness, which is exactly the failure mode a stale or partial
// estate produces: the sources are down, the graph thins out, and every diagnosis suddenly looks
// topologically impossible. An entity this returns false for is one the graph must stay SILENT about.
func (g *Graph) HasGroundTruth(e Entity) bool {
	n := canonName(e.Name)
	for _, ed := range g.edges {
		if ed.Confidence < GroundTruthCutoff || !g.fresh(ed) {
			continue
		}
		// A virtualization CLUSTER edge is a logical placement group, not observed topology (TG-390). A guest
		// whose only remaining parent is its cluster pseudo-node KNOWS nothing about where it runs, so a 0.90
		// NetBox cluster edge must not mask that hole — excluding it here is what lets HasGroundTruth fall to
		// false when the authoritative per-node edge is gone, reaching TG-202's "stay silent" state instead of
		// asserting a confident placement TG does not actually have.
		if ed.From.Type == TypeCluster || ed.To.Type == TypeCluster {
			continue
		}
		if canonName(ed.From.Name) == n || canonName(ed.To.Name) == n {
			return true
		}
	}
	return false
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

// FreshNodeCount reports the number of distinct entities that are an endpoint of at least one FRESH edge —
// the live node set the mutation gate actually reasons over. It differs from len(Export().Nodes), which
// counts every entity that has EVER been an endpoint: Upsert never physically removes an expired edge (it
// only stops counting it as fresh at read time, via Parents()/BlastRadius), so a node whose every edge has
// aged out lingers in Export().Nodes forever. During exactly the failure class this matters for — an ingest
// source going quiet so its edges age out — a node count off Export() holds high while the fresh graph
// shrinks, and the two diverge with no signal. This counts the SAME freshness the read paths enforce, so a
// gauge derived from it moves with the graph the gate sees (TG-449). O(edges), read-only.
func (g *Graph) FreshNodeCount() int {
	seen := make(map[string]bool, len(g.edges))
	for _, ed := range g.edges {
		if !g.fresh(ed) {
			continue
		}
		seen[ed.From.key()] = true
		seen[ed.To.key()] = true
	}
	return len(seen)
}

// FreshHostNames returns the distinct names of HOST entities that are an endpoint of at least one FRESH edge —
// FreshObservableNames is FreshHostNames widened to EVERY host-like entity type — physical hosts, PVE nodes,
// network devices, tunnels, VMs and LXCs as well as plain hosts (the same set commonCauseEligible treats as
// concrete machines). The observation census (TG-180) needs this denominator: a PVE guest the graph knows
// only as TypeVM/TypeLXC, or a switch known as TypeNetworkDevice, was neither observed nor counted as
// unobservable by the TypeHost-only census — invisible to the instrument that exists to find blind spots.
// Same freshness rule, same deterministic ordering, read-only.
func (g *Graph) FreshObservableNames() []string {
	seen := make(map[string]bool, len(g.edges))
	for _, ed := range g.edges {
		if !g.fresh(ed) {
			continue
		}
		for _, e := range []Entity{ed.From, ed.To} {
			if e.Name != "" && siblingParentEligible(e.Type) {
				seen[e.Name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// the live host set, by the SAME freshness the read paths enforce (TG-449). A census over it counts the hosts
// the gate actually sees, not the aged-out ghosts that linger in Export().Nodes forever after their ingest
// source goes quiet. Only TypeHost entities are returned: the observation census (TG-180) matches these against
// fired-alert history keyed on hostname. Names are sorted for a deterministic census. O(edges), read-only.
func (g *Graph) FreshHostNames() []string {
	seen := make(map[string]bool, len(g.edges))
	for _, ed := range g.edges {
		if !g.fresh(ed) {
			continue
		}
		for _, e := range []Entity{ed.From, ed.To} {
			if e.Type == TypeHost && e.Name != "" {
				seen[e.Name] = true
			}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
