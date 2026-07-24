package netbox

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

// vmDoer serves a two-page NetBox virtual-machines list: page 1 links to page 2 via `next`, exercising
// pagination. One VM is unplaced (no device) and must be skipped.
type vmDoer struct{ hits int }

func (d *vmDoer) Do(req *http.Request) (*http.Response, error) {
	d.hits++
	body := "{}"
	switch {
	case strings.Contains(req.URL.RawQuery, "offset=200"):
		body = `{"next":null,"results":[
			{"name":"grafana01","device":{"name":"dc1pve02"}},
			{"name":"orphan-vm","device":null}
		]}`
	case strings.Contains(req.URL.Path, "/api/virtualization/virtual-machines/"):
		// n8n01/litellm01 are device-placed; cluster-vm has no device but a cluster (the common
		// real-world shape) and MUST still produce a runs_on edge to its cluster.
		body = `{"next":"https://netbox.example/api/virtualization/virtual-machines/?limit=200&offset=200","results":[
			{"name":"n8n01","device":{"name":"dc1pve01"}},
			{"name":"litellm01","device":{"name":"dc1pve01"}},
			{"name":"cluster-vm","device":null,"cluster":{"name":"dc2-pve"}}
		]}`
	default:
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("nf")), Header: make(http.Header)}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestEstateSourceEmitsRunsOnEdges(t *testing.T) {
	t.Setenv("TG_TEST_NETBOX_TOKEN", "nb_secret")
	d := &vmDoer{}
	m := New("https://netbox.example/", config.SecretRef("env:TG_TEST_NETBOX_TOKEN"), WithHTTPClient(d))
	src := NewEstateSource(m, "HostDown")

	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges must succeed: %v", err)
	}
	if d.hits != 2 {
		t.Fatalf("both pages must be fetched, got %d requests", d.hits)
	}
	if len(edges) != 4 { // n8n01, litellm01, grafana01 (device) + cluster-vm (cluster); orphan-vm skipped
		t.Fatalf("expected 4 runs_on edges (orphan skipped, cluster-vm included), got %d: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Rel != estate.RelRunsOn || e.Source != estate.SourceNetbox || len(e.ExpectedAlerts) != 1 {
			t.Fatalf("edge shape wrong: %+v", e)
		}
	}
	// the cluster-placed VM must produce a runs_on edge to its cluster (a pve_node grouping).
	var clusterEdge bool
	for _, e := range edges {
		if e.From.Name == "cluster-vm" && e.To.Name == "dc2-pve" && e.To.Type == estate.TypePVENode {
			clusterEdge = true
		}
	}
	if !clusterEdge {
		t.Fatalf("cluster-placed VM must depend on its cluster: %+v", edges)
	}

	// build a graph from the source and confirm a pve node now has a NON-empty, correct blast radius.
	g, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build reported source errors: %v", errs)
	}
	pve01, ok := g.Resolve("dc1pve01")
	if !ok {
		t.Fatal("the pve node must resolve once its guests are placed on it")
	}
	blast := g.BlastRadius(pve01, 3)
	names := map[string]bool{}
	for _, imp := range blast {
		names[imp.Entity.Name] = true
	}
	if !names["n8n01"] || !names["litellm01"] {
		t.Fatalf("pve01's blast radius must include its two guests, got %+v", names)
	}
	if names["grafana01"] {
		t.Fatalf("grafana01 runs on pve02, not pve01 — it must not be in pve01's blast radius")
	}
}

// A source fetch error is surfaced (per-source-isolated), not silently swallowed as an empty topology.
func TestEstateSourceFetchErrorSurfaces(t *testing.T) {
	t.Setenv("TG_TEST_NETBOX_TOKEN", "nb_secret")
	m := New("https://netbox.example/", config.SecretRef("env:TG_TEST_NETBOX_TOKEN"), WithHTTPClient(&vmDoer{}))
	// force a 404 by pointing at a path the fake does not serve is not possible here; instead use a doer that errors.
	_, errs := estate.Build(context.Background(), []estate.EdgeSource{NewEstateSource(New("https://x/", config.SecretRef("env:MISSING_TOKEN_REF")))})
	if len(errs) != 1 {
		t.Fatalf("an unresolvable token must surface as one source error, got %v", errs)
	}
	_ = m
}
