package pve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/estate"
)

type resDoer struct {
	body string
	err  error
	auth string
}

func (d *resDoer) Do(req *http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	d.auth = req.Header.Get("Authorization")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(d.body)), Header: make(http.Header)}, nil
}

// The reader emits runs_on edges (lxc→node, qemu→node) from cluster resources, types guests correctly, skips
// nameless/nodeless entries, and builds a coherent per-node blast radius (P0-1, PVE 0.95 source of truth).
func TestPVEPlacementEdges(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "root@pam!tg=uuid")
	body := `{"data":[
		{"type":"lxc","node":"dc1pve01","name":"n8n01"},
		{"type":"qemu","node":"dc1pve01","name":"win-vm"},
		{"type":"lxc","node":"dc1pve02","name":"grafana01"},
		{"type":"lxc","node":"","name":"unplaced"},
		{"type":"storage","node":"dc1pve01","name":""}
	]}`
	d := &resDoer{body: body}
	src := New("https://dc1pve01:8006/", config.SecretRef("env:TG_TEST_PVE_TOKEN"), WithHTTPClient(d), WithExpectedAlerts("HostDown"))

	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges must succeed: %v", err)
	}
	if d.auth != "PVEAPIToken=root@pam!tg=uuid" {
		t.Fatalf("the resolved token must be sent as PVEAPIToken, got %q", d.auth)
	}
	if len(edges) != 3 { // n8n01, win-vm, grafana01 — unplaced + nameless-storage skipped
		t.Fatalf("expected 3 runs_on edges, got %d: %+v", len(edges), edges)
	}
	var lxc, vm int
	for _, e := range edges {
		if e.Rel != estate.RelRunsOn || e.To.Type != estate.TypePVENode {
			t.Fatalf("edge shape wrong: %+v", e)
		}
		switch e.From.Type {
		case estate.TypeLXC:
			lxc++
		case estate.TypeVM:
			vm++
		}
	}
	if lxc != 2 || vm != 1 {
		t.Fatalf("guest types must be derived from the resource type: lxc=%d vm=%d", lxc, vm)
	}

	g, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build reported errors: %v", errs)
	}
	pve01, ok := g.Resolve("dc1pve01")
	if !ok {
		t.Fatal("the pve node must resolve once guests are placed on it")
	}
	names := map[string]bool{}
	for _, imp := range g.BlastRadius(pve01, 3) {
		names[imp.Entity.Name] = true
	}
	if !names["n8n01"] || !names["win-vm"] || names["grafana01"] {
		t.Fatalf("pve01's blast radius must be exactly its own guests, got %+v", names)
	}
}

// A 2xx body carrying no `data` envelope is NOT an empty cluster: it is what a proxy, an SSO portal or an
// entirely different product answers, and the reader must say so rather than contribute a silent empty
// topology that reads downstream as "this cluster runs nothing". estate.Build isolates the error per source,
// so the loud answer costs the other sources nothing.
func TestPVENonPVEBodyIsAnErrorNotAnEmptyCluster(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "root@pam!tg=uuid")
	src := New("https://x:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"),
		WithHTTPClient(&resDoer{body: `{"error":"unauthorized","login":"required"}`}))

	edges, err := src.Edges(context.Background())
	if err == nil {
		t.Fatalf("a body with no PVE envelope must be an error, got %d edge(s)", len(edges))
	}
	if !strings.Contains(err.Error(), "cluster resources") {
		t.Errorf("the error must name the read that failed, got %q", err)
	}
	// A genuinely empty cluster still parses and is still a success — the two must stay distinguishable.
	empty := New("https://x:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"), WithHTTPClient(&resDoer{body: `{"data":[]}`}))
	if edges, err := empty.Edges(context.Background()); err != nil || len(edges) != 0 {
		t.Fatalf("an empty cluster must parse to zero edges without error, got %d/%v", len(edges), err)
	}
}

// A fetch error surfaces as one PVE source error (per-source-isolated), never a silent empty topology.
func TestPVEFetchErrorSurfaces(t *testing.T) {
	t.Setenv("TG_TEST_PVE_TOKEN", "x")
	src := New("https://x:8006", config.SecretRef("env:TG_TEST_PVE_TOKEN"), WithHTTPClient(&resDoer{err: errors.New("tls handshake")}))
	_, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 1 || errs[0].Source != estate.SourcePVE {
		t.Fatalf("a fetch failure must surface as one pve source error, got %v", errs)
	}
}
