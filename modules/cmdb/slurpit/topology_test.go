package slurpit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// fixedSource is an in-test estate.EdgeSource that contributes a stamped edge set — used to seed a
// higher-confidence source the slurpit source must not downgrade.
type fixedSource struct {
	src   estate.Source
	edges []estate.Edge
}

func (f fixedSource) Source() estate.Source                        { return f.src }
func (f fixedSource) Edges(context.Context) ([]estate.Edge, error) { return f.edges, nil }

// serveDevices spins a plain-HTTP fake Slurp'it that answers /api/devices with a fixed JSON body.
func serveDevices(t *testing.T, body string) *EstateSource {
	t.Helper()
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "t")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"), WithExpectedAlerts("DeviceDown"))
}

// TestSlurpitDevicesBecomeNodesAtDiscoveredTier is the positive oracle: a discovered device becomes an estate
// node via its site membership, and its parent becomes a dependency edge — both stamped SourceSlurpit at the
// 0.82 discovered-inventory tier (Build stamps the source default).
func TestSlurpitDevicesBecomeNodesAtDiscoveredTier(t *testing.T) {
	src := serveDevices(t, `[{"hostname":"sw-acc-01","site":"NL","parent":"sw-core-01"}]`)

	g, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build reported source errors: %v", errs)
	}
	node, ok := g.Resolve("sw-acc-01")
	if !ok {
		t.Fatal("the discovered device must resolve as an estate node")
	}

	parents := g.Parents(node)
	var site, dep *estate.Parent
	for i := range parents {
		switch parents[i].Rel {
		case estate.RelMemberOf:
			site = &parents[i]
		case estate.RelDependsOn:
			dep = &parents[i]
		}
	}
	if site == nil || site.Entity.Name != "NL" || site.Source != estate.SourceSlurpit || site.Confidence != 0.82 {
		t.Fatalf("device must be member_of its site at slurpit@0.82, got %+v", site)
	}
	if dep == nil || dep.Entity.Name != "sw-core-01" || dep.Source != estate.SourceSlurpit || dep.Confidence != 0.82 {
		t.Fatalf("device must depend_on its parent at slurpit@0.82, got %+v", dep)
	}
}

// TestSlurpitFactDoesNotDowngradeHigherConfidenceSource is the MAX-ratchet oracle. A live source (PVE 0.95)
// already knows an adjacency; the slurpit source then discovers the SAME edge at 0.82. The merged edge must
// keep PVE's confidence and provenance — a discovered-inventory fact never downgrades observed live truth.
//
// RED-PROVE: this pins slurpit's tier BELOW pve. If SourceConfidence[SourceSlurpit] were raised to >=0.95,
// the slurpit edge would win the ratchet and Source would flip to slurpit — RED. (Confirmed by construction:
// 0.82 < 0.95.)
func TestSlurpitFactDoesNotDowngradeHigherConfidenceSource(t *testing.T) {
	src := serveDevices(t, `[{"hostname":"sw-acc-01","parent":"sw-core-01"}]`) // slurpit: sw-acc-01 depends_on sw-core-01 @0.82

	pve := fixedSource{src: estate.SourcePVE, edges: []estate.Edge{{
		From:   estate.Entity{Type: estate.TypeHost, Name: "sw-acc-01"},
		To:     estate.Entity{Type: estate.TypeHost, Name: "sw-core-01"},
		Rel:    estate.RelDependsOn,
		Source: estate.SourcePVE,
		// Confidence left 0 ⇒ Build stamps SourcePVE's 0.95.
	}}}

	g, errs := estate.Build(context.Background(), []estate.EdgeSource{pve, src})
	if len(errs) != 0 {
		t.Fatalf("build reported source errors: %v", errs)
	}
	node, _ := g.Resolve("sw-acc-01")
	parents := g.Parents(node)
	var dep *estate.Parent
	for i := range parents {
		if parents[i].Rel == estate.RelDependsOn && parents[i].Entity.Name == "sw-core-01" {
			dep = &parents[i]
		}
	}
	if dep == nil {
		t.Fatalf("the shared adjacency must be present, got parents %+v", parents)
	}
	if dep.Source != estate.SourcePVE || dep.Confidence != 0.95 {
		t.Fatalf("a slurpit@0.82 fact must NOT downgrade the pve@0.95 edge — expected pve/0.95, got %s/%.2f",
			dep.Source, dep.Confidence)
	}
}

// TestEdgeEmissionShapes pins edgesFrom's guards directly: the fqdn identity fallback, the site membership
// edge carrying NO expected alert (a site is not a cascading parent), the dependency edge carrying the
// configured alert, and the three parents that must be skipped as dead weight (empty, self, and
// numeric/IP-literal references).
func TestEdgeEmissionShapes(t *testing.T) {
	t.Setenv("TG_TEST_SLURPIT_TOKEN", "t")
	src := New("http://slurpit.example", config.SecretRef("env:TG_TEST_SLURPIT_TOKEN"), WithExpectedAlerts("DeviceDown"))

	devices := []slurpitDevice{
		{Hostname: "", Fqdn: ""},                               // no identity → skipped entirely
		{Hostname: "", Fqdn: "host-b.example.com", Site: "GR"}, // identity from fqdn; member_of GR
		{Hostname: "host-c", Site: "NL", Parent: "host-c"},     // self-parent → parent skipped, site kept
		{Hostname: "host-d", Site: "NL", Parent: "192.0.2.1"},   // IP-literal parent → skipped, site kept
		{Hostname: "host-e", Site: "NL", Parent: "42"},         // numeric-id parent → skipped, site kept
		{Hostname: "host-f", Site: "NL", Parent: "sw-core-01"}, // site + real dependency edge
	}
	edges := src.edgesFrom(devices)

	member, depends := 0, 0
	for _, e := range edges {
		if e.Source != estate.SourceSlurpit {
			t.Errorf("every emitted edge must be SourceSlurpit, got %s: %+v", e.Source, e)
		}
		switch e.Rel {
		case estate.RelMemberOf:
			member++
			if e.To.Type != estate.TypeSite {
				t.Errorf("membership must point at a site: %+v", e)
			}
			if len(e.ExpectedAlerts) != 0 {
				t.Errorf("a site edge must carry NO expected alert (a site is not a cascading parent): %+v", e)
			}
		case estate.RelDependsOn:
			depends++
			if e.From.Name != "host-f" || e.To.Name != "sw-core-01" {
				t.Errorf("only host-f's real parent may become a dependency edge, got %+v", e)
			}
			if len(e.ExpectedAlerts) != 1 || e.ExpectedAlerts[0] != "DeviceDown" {
				t.Errorf("a dependency edge must carry the configured cascade alert: %+v", e)
			}
		}
	}
	if member != 5 { // host-b, host-c, host-d, host-e, host-f each carry a site (the identity-less device is skipped)
		t.Errorf("expected 5 site memberships (host-b..host-f), got %d", member)
	}
	if depends != 1 {
		t.Errorf("exactly one dependency edge (host-f→sw-core-01) must survive the guards, got %d", depends)
	}
}

// TestUnresolvableTokenSurfacesOneSourceError proves per-source isolation: an unresolvable token ref fails the
// slurpit contribution loudly as ONE SourceError (never a silent empty topology), without a network call.
func TestUnresolvableTokenSurfacesOneSourceError(t *testing.T) {
	src := New("http://slurpit.invalid", config.SecretRef("env:TG_MISSING_SLURPIT_TOKEN_REF"))
	_, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 1 {
		t.Fatalf("an unresolvable token must surface as exactly one source error, got %v", errs)
	}
	if errs[0].Source != estate.SourceSlurpit {
		t.Errorf("the error must be attributed to the slurpit source, got %s", errs[0].Source)
	}
}
