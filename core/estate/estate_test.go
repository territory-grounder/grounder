package estate

import (
	"context"
	"strings"
	"testing"
	"time"
)

func pveNode(n string) Entity { return Entity{Type: TypePVENode, Name: n} }
func lxc(n string) Entity     { return Entity{Type: TypeLXC, Name: n} }

// Grounded in the live graph: lxc -runs_on-> pve_node (pve, 0.95). If the pve_node fails, every guest that
// runs_on it is in the blast radius.
func TestBlastRadiusWalksEdgesIntoTarget(t *testing.T) {
	g := NewGraph()
	for _, guest := range []string{"n8n01", "litellm01", "grafana01"} {
		g.Upsert(Edge{From: lxc(guest), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: SourceConfidence[SourcePVE], Source: SourcePVE})
	}
	imp := g.BlastRadius(pveNode("dc1pve01"), 3)
	if len(imp) != 3 {
		t.Fatalf("all 3 guests must be in the blast radius, got %d", len(imp))
	}
	for _, i := range imp {
		if i.Distance != 1 || i.Confidence != 0.95 {
			t.Errorf("direct guest %s: distance/conf = %d/%v, want 1/0.95", i.Entity.Name, i.Distance, i.Confidence)
		}
	}
}

// Parents is the one-hop upstream walk (edges OUT of the target): the hypervisor a guest runs_on, the site
// it is member_of — with the REL preserved, best-confidence per parent, ordered confidence-descending. An
// expired edge is filtered.
func TestParentsReturnsDirectUpstreamWithRel(t *testing.T) {
	now := time.Now()
	g := NewGraph(WithClock(func() time.Time { return now }))
	g.Upsert(Edge{From: lxc("app01"), To: pveNode("pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: lxc("app01"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelMemberOf, Confidence: 0.90, Source: SourceNetbox})
	g.Upsert(Edge{From: lxc("app01"), To: Entity{Type: TypeService, Name: "expired-svc"}, Rel: RelDependsOn,
		Confidence: 0.85, Source: SourceDeclared, ValidUntil: now.Add(-time.Hour)})
	g.Upsert(Edge{From: lxc("other"), To: pveNode("pve02"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})

	ps := g.Parents(lxc("app01"))
	if len(ps) != 2 {
		t.Fatalf("want 2 fresh parents (expired filtered, other host's parent excluded), got %d: %+v", len(ps), ps)
	}
	if ps[0].Entity.Name != "pve01" || ps[0].Rel != RelRunsOn || ps[0].Confidence != 0.95 {
		t.Errorf("best parent must be the runs_on hypervisor: %+v", ps[0])
	}
	if ps[1].Entity.Name != "nl" || ps[1].Rel != RelMemberOf {
		t.Errorf("second parent must be the member_of site: %+v", ps[1])
	}
}

// Path-product confidence decays multiplicatively along a two-hop chain (lxc -> pve_node -> site).
func TestPathProductConfidenceDecays(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("app01"), To: pveNode("pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	g.Upsert(Edge{From: pveNode("pve01"), To: Entity{Type: TypeSite, Name: "nl"}, Rel: RelMemberOf, Confidence: 0.90, Source: SourceNetbox})
	imp := g.BlastRadius(Entity{Type: TypeSite, Name: "nl"}, 3)
	// site fails → pve01 (0.90) → app01 (0.90*0.95 = 0.855)
	var app, pve *Impact
	for i := range imp {
		switch imp[i].Entity.Name {
		case "app01":
			app = &imp[i]
		case "pve01":
			pve = &imp[i]
		}
	}
	if pve == nil || pve.Confidence != 0.90 || pve.Distance != 1 {
		t.Fatalf("pve01 must be a direct dependent at 0.90, got %+v", pve)
	}
	if app == nil || app.Distance != 2 || app.Confidence != 0.8550 {
		t.Fatalf("app01 must be reached at distance 2 with path-product 0.855, got %+v", app)
	}
}

// Common-cause siblings: guests sharing a pve_node with the target are surfaced at 0.6× confidence — the
// co-failure signal a pure who-depends-on-me walk misses.
// Siblings fire only through INFRASTRUCTURE parents whose silent failure cascades. Two devices that merely
// share a SITE (co-location) or a logical SERVICE are NOT common-cause siblings (the predecessor's
// infrastructure-parent filter); sharing a network device / hypervisor still makes them siblings.
func TestSiblingsGatedToInfrastructureParent(t *testing.T) {
	host := func(n string) Entity { return Entity{Type: TypeHost, Name: n} }

	// (a) shared SITE parent → NO siblings (co-location is not co-failure).
	gs := NewGraph()
	site := Entity{Type: TypeSite, Name: "nl"}
	gs.Upsert(Edge{From: host("devA"), To: site, Rel: RelMemberOf, Confidence: 0.9, Source: SourceNetbox})
	gs.Upsert(Edge{From: host("devB"), To: site, Rel: RelMemberOf, Confidence: 0.9, Source: SourceNetbox})
	if sibs := gs.Siblings(host("devA")); len(sibs) != 0 {
		t.Fatalf("devices sharing only a SITE must NOT be common-cause siblings, got %+v", sibs)
	}

	// (b) shared SERVICE parent → NO siblings (a monitored logical dependency is not a silent common cause).
	gv := NewGraph()
	svc := Entity{Type: TypeService, Name: "auth"}
	gv.Upsert(Edge{From: host("app1"), To: svc, Rel: RelDependsOn, Confidence: 0.9, Source: SourceNetbox})
	gv.Upsert(Edge{From: host("app2"), To: svc, Rel: RelDependsOn, Confidence: 0.9, Source: SourceNetbox})
	if sibs := gv.Siblings(host("app1")); len(sibs) != 0 {
		t.Fatalf("devices sharing only a SERVICE must NOT be common-cause siblings, got %+v", sibs)
	}

	// (c) shared NETWORK DEVICE parent → siblings (a switch failure genuinely cascades) — no regression.
	gn := NewGraph()
	sw := Entity{Type: TypeNetworkDevice, Name: "sw-core"}
	gn.Upsert(Edge{From: host("camA"), To: sw, Rel: RelDependsOn, Confidence: 0.9, Source: SourceLibreNMS})
	gn.Upsert(Edge{From: host("camB"), To: sw, Rel: RelDependsOn, Confidence: 0.9, Source: SourceLibreNMS})
	if sibs := gn.Siblings(host("camA")); len(sibs) != 1 || sibs[0].Entity.Name != "camB" {
		t.Fatalf("devices behind a shared network device MUST be siblings, got %+v", sibs)
	}
}

func TestSiblingsShareParentAtPenalty(t *testing.T) {
	g := NewGraph()
	for _, guest := range []string{"a", "b", "c"} {
		g.Upsert(Edge{From: lxc(guest), To: pveNode("pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	}
	sibs := g.Siblings(lxc("a"))
	if len(sibs) != 2 { // b and c, not a itself
		t.Fatalf("a must have 2 siblings (b,c), got %d", len(sibs))
	}
	for _, s := range sibs {
		if s.Entity.Name == "a" {
			t.Fatal("an entity is not its own sibling")
		}
		if s.Confidence != 0.57 { // 0.6 × 0.95
			t.Errorf("sibling %s confidence = %v, want 0.57 (0.6×0.95)", s.Entity.Name, s.Confidence)
		}
	}
}

// The MAX-confidence ratchet: a re-seed never downgrades a better-evidenced edge, and the winning
// confidence's provenance is stored (fixing the predecessor's misattribution bug).
func TestUpsertRatchetsConfidenceUpOnly(t *testing.T) {
	g := NewGraph()
	// netbox first (0.85 cable-derived), then librenms strengthens it (0.90).
	g.Upsert(Edge{From: lxc("x"), To: pveNode("p"), Rel: RelDependsOn, Confidence: 0.85, Source: SourceNetbox})
	g.Upsert(Edge{From: lxc("x"), To: pveNode("p"), Rel: RelDependsOn, Confidence: 0.90, Source: SourceLibreNMS})
	e := g.edges[edgeKey(lxc("x"), pveNode("p"), RelDependsOn)]
	if e.Confidence != 0.90 || e.Source != SourceLibreNMS {
		t.Fatalf("stronger source must win with its provenance, got %v/%s", e.Confidence, e.Source)
	}
	// a weaker re-seed must NOT downgrade.
	g.Upsert(Edge{From: lxc("x"), To: pveNode("p"), Rel: RelDependsOn, Confidence: 0.60, Source: SourceIncident})
	if e.Confidence != 0.90 {
		t.Fatalf("a weaker source must not downgrade a better-evidenced edge, got %v", e.Confidence)
	}
}

// Expired edges are invisible to traversal — a dead source degrades to "no edge after its TTL," never a
// silently-wrong stale edge.
func TestExpiredEdgesAreInvisible(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := NewGraph(WithClock(func() time.Time { return now }))
	g.Upsert(Edge{From: lxc("g"), To: pveNode("p"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE, ValidUntil: now.Add(-time.Hour)})
	if imp := g.BlastRadius(pveNode("p"), 3); len(imp) != 0 {
		t.Fatalf("an expired edge must be invisible to blast radius, got %d impacts", len(imp))
	}
}

// Learned confidence is hard-capped below the 0.80 suppression cutoff.
func TestLearnedConfidenceCapped(t *testing.T) {
	if c := LearnedConfidence(100); c != 0.75 {
		t.Fatalf("learned confidence must cap at 0.75, got %v", c)
	}
	if c := LearnedConfidence(2); c != 0.5 { // 0.4 + 0.05*2
		t.Fatalf("learned confidence(2) = %v, want 0.50", c)
	}
	if LearnedConfidence(100) >= 0.80 {
		t.Fatal("learned confidence must stay below the 0.80 suppression cutoff")
	}
}

// Entity resolution returns the concrete typed node, never the generic 'host' twin.
func TestResolvePrefersConcreteNodeOverHostTwin(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: lxc("z"), To: pveNode("dc1pve01"), Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	// register a generic host twin of the same name (as the predecessor's GraphRAG side does).
	g.register(Entity{Type: TypeHost, Name: "dc1pve01"})
	got, ok := g.Resolve("dc1pve01.example.net")
	if !ok {
		t.Fatal("must resolve a known name (domain stripped)")
	}
	if got.Type != TypePVENode {
		t.Fatalf("must resolve to the concrete pve_node, not the host twin, got %s", got.Type)
	}
	if _, ok := g.Resolve("unknown-host"); ok {
		t.Fatal("an unknown name must not resolve")
	}
}

// Operator-declared edges parse, seed the graph, and are OUT-RANKED by a live source on the same edge — so
// "live devices state is the source of truth" holds by construction while declared fills gaps (P0-1 / the
// operator-defined-topology requirement).
func TestDeclaredSourceParsesAndIsOutrankedByLive(t *testing.T) {
	js := `[
		{"from":"svc-api","to":"db01","rel":"depends_on","expected_alerts":["ApiDown"]},
		{"from":"n8n01","from_type":"lxc","to":"dc1pve01","to_type":"pve_node","rel":"runs_on"}
	]`
	edges, err := ParseDeclared(strings.NewReader(js))
	if err != nil {
		t.Fatalf("ParseDeclared must succeed: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 declared edges, got %d", len(edges))
	}
	for _, e := range edges {
		if e.Source != SourceDeclared {
			t.Fatalf("declared edges must carry SourceDeclared, got %s", e.Source)
		}
	}
	// A LIVE PVE edge on the SAME (n8n01 runs_on pve01) key must out-rank the declared one via the MAX-ratchet.
	g := NewGraph()
	decl := NewDeclaredSource(edges)
	de, _ := decl.Edges(context.Background())
	for _, e := range de {
		if e.Confidence == 0 {
			e.Confidence = SourceConfidence[SourceDeclared]
		}
		g.Upsert(e)
	}
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "n8n01"}, To: Entity{Type: TypePVENode, Name: "dc1pve01"}, Rel: RelRunsOn, Confidence: SourceConfidence[SourcePVE], Source: SourcePVE})
	node, ok := g.Resolve("dc1pve01")
	if !ok {
		t.Fatal("pve node must resolve")
	}
	for _, imp := range g.BlastRadius(node, 3) {
		if imp.Entity.Name == "n8n01" && imp.Confidence < SourceConfidence[SourcePVE] {
			t.Fatalf("the live PVE confidence (0.95) must win over declared (0.85), got %.2f", imp.Confidence)
		}
	}
}

// A malformed declared edge is rejected loudly, never silently dropped.
func TestDeclaredSourceRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		`[{"from":"","to":"x","rel":"depends_on"}]`,      // empty endpoint
		`[{"from":"a","to":"b","rel":"teleports_to"}]`,   // unknown rel
		`[{"from":"a","from_type":"wormhole","to":"b"}]`, // unknown type
		`[{"from":"a","to":"b","bogus":1}]`,              // unknown field
	} {
		if _, err := ParseDeclared(strings.NewReader(bad)); err == nil {
			t.Errorf("a malformed declared edge must be rejected: %s", bad)
		}
	}
}

// The learned tier: repeated incident co-occurrence becomes a depends_on edge capped at 0.75 — below any
// live source AND the 0.80 suppression cutoff, so it enriches prediction but a live edge always out-ranks it.
func TestLearnedSourceCappedAndOutrankedByLive(t *testing.T) {
	obs := []CoOccurrence{
		{Primary: "db01", Dependent: "app01", Count: 12},  // well-observed → learned edge
		{Primary: "db01", Dependent: "flaky01", Count: 1}, // one coincidence → NOT an edge
		{Primary: "x", Dependent: "x", Count: 9},          // self-loop → skipped
	}
	src := NewLearnedSource(obs)
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Fatalf("only the well-observed non-self pair becomes an edge, got %d: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.From.Name != "app01" || e.To.Name != "db01" || e.Rel != RelDependsOn || e.Source != SourceIncident {
		t.Fatalf("learned edge shape wrong: %+v", e)
	}
	if e.Confidence > 0.75 {
		t.Fatalf("learned confidence must be hard-capped at 0.75, got %.3f", e.Confidence)
	}
	// a LIVE PVE edge on the same key must out-rank the learned one via the MAX-ratchet.
	g := NewGraph()
	for _, le := range edges {
		g.Upsert(le)
	}
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "app01"}, To: Entity{Type: TypeHost, Name: "db01"}, Rel: RelDependsOn, Confidence: SourceConfidence[SourcePVE], Source: SourcePVE})
	db, _ := g.Resolve("db01")
	for _, imp := range g.BlastRadius(db, 3) {
		if imp.Entity.Name == "app01" && imp.Confidence < SourceConfidence[SourcePVE] {
			t.Fatalf("a live edge (0.95) must win over the learned edge (<=0.75), got %.3f", imp.Confidence)
		}
	}
}

func TestParseCoOccurrencesRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		`[{"primary":"","dependent":"b","count":5}]`,
		`[{"primary":"a","dependent":"b","count":-1}]`,
		`[{"primary":"a","dependent":"b","count":5,"bogus":1}]`,
	} {
		if _, err := ParseCoOccurrences(strings.NewReader(bad)); err == nil {
			t.Errorf("malformed co-occurrence must be rejected: %s", bad)
		}
	}
	if obs, err := ParseCoOccurrences(strings.NewReader(`[{"primary":"a","dependent":"b","count":5}]`)); err != nil || len(obs) != 1 {
		t.Fatalf("a well-formed co-occurrence must parse: %v %+v", err, obs)
	}
}

// The tunnel tier: a cross-site host routing through a firewall tunnel is placed in the firewall's blast
// radius at the TOP confidence (1.0) — the reason the verifier must not exclude an unknown-site cascade.
func TestTunnelSourceTopConfidence(t *testing.T) {
	tunnels := []Tunnel{
		{Endpoint: "dc1fw01", Routes: []string{"notrf01vps01", "chzrh01vps01", "dc1fw01"}, ExpectedAlerts: []string{"HostUnreachable"}},
	}
	src := NewTunnelSource(tunnels)
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 { // two remotes; the self-route (fw01→fw01) is skipped
		t.Fatalf("expected 2 routes_via edges (self-route skipped), got %d: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Rel != RelRoutesVia || e.To.Name != "dc1fw01" || e.Source != SourceTunnel {
			t.Fatalf("tunnel edge shape wrong: %+v", e)
		}
	}
	g, errs := Build(context.Background(), []EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build errors: %v", errs)
	}
	fw, ok := g.Resolve("dc1fw01")
	if !ok {
		t.Fatal("the tunnel endpoint must resolve")
	}
	var found bool
	for _, imp := range g.BlastRadius(fw, 3) {
		if imp.Entity.Name == "notrf01vps01" {
			found = true
			if imp.Confidence < SourceConfidence[SourceTunnel] {
				t.Fatalf("a tunnel edge must carry the top confidence (1.0), got %.3f", imp.Confidence)
			}
		}
	}
	if !found {
		t.Fatal("the VPS routing through the firewall must be in its blast radius")
	}
}

func TestParseTunnelsRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		`[{"endpoint":"","routes":["a"]}]`,
		`[{"endpoint":"fw","routes":[]}]`,
		`[{"endpoint":"fw","routes":["a"],"bogus":1}]`,
	} {
		if _, err := ParseTunnels(strings.NewReader(bad)); err == nil {
			t.Errorf("malformed tunnel must be rejected: %s", bad)
		}
	}
}

// Cross-source coherence: NetBox (physical_host) and PVE (pve_node) and LibreNMS (host) describe the SAME
// machine under different entity types. Their edge sets must MERGE into one blast radius, not sit on
// disconnected typed twins — otherwise multi-source is illusory.
func TestBlastRadiusMergesCrossTypeSources(t *testing.T) {
	g := NewGraph()
	// PVE: an LXC runs on pve_node:core01
	g.Upsert(Edge{From: Entity{Type: TypeLXC, Name: "n8n01"}, To: Entity{Type: TypePVENode, Name: "core01"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	// NetBox: a VM placed on the SAME machine, but typed physical_host:core01
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "app01"}, To: Entity{Type: TypePhysicalHost, Name: "core01"}, Rel: RelRunsOn, Confidence: 0.90, Source: SourceNetbox})
	// LibreNMS: a device depends on the SAME machine, typed host:core01
	g.Upsert(Edge{From: Entity{Type: TypeHost, Name: "sensor01"}, To: Entity{Type: TypeHost, Name: "core01"}, Rel: RelDependsOn, Confidence: 0.90, Source: SourceLibreNMS})

	core, ok := g.Resolve("core01")
	if !ok {
		t.Fatal("core01 must resolve")
	}
	names := map[string]bool{}
	for _, imp := range g.BlastRadius(core, 3) {
		names[imp.Entity.Name] = true
	}
	// all three dependents, contributed under three different To-types, must appear in ONE blast radius.
	for _, want := range []string{"n8n01", "app01", "sensor01"} {
		if !names[want] {
			t.Errorf("cross-type sources must merge: %s missing from core01's blast radius (%v)", want, names)
		}
	}
}

// A domain-qualified endpoint merges with its bare form (Resolve strips domains, so must the blast walk).
func TestBlastRadiusMergesDomainQualifiedNames(t *testing.T) {
	g := NewGraph()
	g.Upsert(Edge{From: Entity{Type: TypeVM, Name: "app01"}, To: Entity{Type: TypePVENode, Name: "core01.dc.example"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	core, ok := g.Resolve("core01")
	if !ok {
		t.Fatal("core01 must resolve from its domain-qualified form")
	}
	got := g.BlastRadius(core, 3)
	if len(got) != 1 || got[0].Entity.Name != "app01" {
		t.Fatalf("the domain-qualified node must carry its dependent, got %+v", got)
	}
}

// LaplaceConfidence is base-rate-aware: a pair that co-occurs a large FRACTION of the primary's incidents is
// more confident than one with the same raw count but a much larger denominator — and it is still capped 0.75.
func TestLaplaceConfidence(t *testing.T) {
	always := LaplaceConfidence(5, 5) // 5/5 → (5+1)/(5+2)=0.857 → capped 0.75
	rare := LaplaceConfidence(5, 50)  // 5/50 → (5+1)/(52)=0.115
	if always <= rare {
		t.Fatalf("a high-base-rate pair must outrank a low-base-rate one: always=%.3f rare=%.3f", always, rare)
	}
	if always > 0.75 {
		t.Fatalf("laplace confidence must be capped at 0.75, got %.3f", always)
	}
	if rare > 0.2 {
		t.Fatalf("a 5/50 pair must be low-confidence, got %.3f", rare)
	}
	// no trials → falls back to the count-only ramp
	if LaplaceConfidence(3, 0) != LearnedConfidence(3) {
		t.Fatal("zero trials must fall back to LearnedConfidence")
	}
}

// The holder swaps the graph atomically on a good refresh, but KEEPS the last good graph when every source
// errors (a topology blip must never blank the estate into vacuous predictions).
func TestHolderRefreshKeepsLastGoodOnTotalOutage(t *testing.T) {
	// initial good graph with one placement edge.
	good := NewGraph()
	good.Upsert(Edge{From: Entity{Type: TypeVM, Name: "app01"}, To: Entity{Type: TypePVENode, Name: "pve01"}, Rel: RelRunsOn, Confidence: 0.95, Source: SourcePVE})
	h := NewHolder(good)
	if _, ok := h.Graph().Resolve("pve01"); !ok {
		t.Fatal("initial graph must resolve pve01")
	}

	// a refresh where the ONLY source errors → keep the last good graph.
	errs := h.Refresh(context.Background(), []EdgeSource{errSource{}})
	if len(errs) != 1 {
		t.Fatalf("the source error must be reported, got %v", errs)
	}
	if _, ok := h.Graph().Resolve("pve01"); !ok {
		t.Fatal("a total-outage refresh must KEEP the last good graph, not blank it")
	}

	// a refresh with a healthy source → swap in the new graph.
	h.Refresh(context.Background(), []EdgeSource{okSource{}})
	if _, ok := h.Graph().Resolve("db01"); !ok {
		t.Fatal("a healthy refresh must swap in the new graph")
	}
	// nil holder init never returns nil.
	if NewHolder(nil).Graph() == nil {
		t.Fatal("Graph() must never be nil")
	}
}

type errSource struct{}

func (errSource) Source() Source                        { return SourcePVE }
func (errSource) Edges(context.Context) ([]Edge, error) { return nil, context.DeadlineExceeded }

type okSource struct{}

func (okSource) Source() Source { return SourceNetbox }
func (okSource) Edges(context.Context) ([]Edge, error) {
	return []Edge{{From: Entity{Type: TypeVM, Name: "db01"}, To: Entity{Type: TypePhysicalHost, Name: "hostA"}, Rel: RelRunsOn, Source: SourceNetbox}}, nil
}
