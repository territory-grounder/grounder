package librenms

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/estate"
)

type devDoer struct {
	body string
	err  error
	auth string
}

func (d *devDoer) Do(req *http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	d.auth = req.Header.Get("X-Auth-Token")
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(d.body)), Header: make(http.Header)}, nil
}

// The reader emits depends_on edges from dependency_parent_hostname (CSV supported), skips devices with no
// parent, and builds a coherent blast radius — a parent switch cascades to the devices behind it (P0-1).
func TestLibreNMSDependencyParentEdges(t *testing.T) {
	t.Setenv("TG_TEST_LNMS_TOKEN", "lnms_secret")
	body := `{"status":"ok","devices":[
		{"hostname":"host-a","dependency_parent_hostname":"coreswitch"},
		{"hostname":"host-b","dependency_parent_hostname":"coreswitch,distswitch"},
		{"hostname":"coreswitch","dependency_parent_hostname":""},
		{"hostname":"loopy","dependency_parent_hostname":"loopy"}
	]}`
	d := &devDoer{body: body}
	src := NewEstateSource(
		[]Deployment{{Site: "nl", BaseURL: "https://lnms.example/", TokenRef: "env:TG_TEST_LNMS_TOKEN"}},
		WithTopologyHTTPClient(d), WithExpectedAlerts("DeviceDown"),
	)

	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges must succeed: %v", err)
	}
	if d.auth != "lnms_secret" {
		t.Fatalf("the resolved token must be sent as X-Auth-Token, got %q", d.auth)
	}
	// host-a→coreswitch, host-b→coreswitch, host-b→distswitch = 3 edges; coreswitch (no parent) and the
	// self-loop are skipped.
	if len(edges) != 3 {
		t.Fatalf("expected 3 depends_on edges, got %d: %+v", len(edges), edges)
	}
	for _, e := range edges {
		if e.Rel != estate.RelDependsOn || e.Source != estate.SourceLibreNMS {
			t.Fatalf("edge shape wrong: %+v", e)
		}
	}

	g, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 0 {
		t.Fatalf("build reported errors: %v", errs)
	}
	sw, ok := g.Resolve("coreswitch")
	if !ok {
		t.Fatal("coreswitch must resolve once devices depend on it")
	}
	names := map[string]bool{}
	for _, imp := range g.BlastRadius(sw, 3) {
		names[imp.Entity.Name] = true
	}
	if !names["host-a"] || !names["host-b"] {
		t.Fatalf("coreswitch's blast radius must include the devices behind it, got %+v", names)
	}
}

// An IP-literal dependency parent is dead weight (a phantom host node the estate can never triage), so its
// edges are dropped — matching the predecessor's seed-time `re.fullmatch(r"[\d.]+")` guard. Without this,
// cam-01 and cam-02 sharing an IP-literal parent would be materialized as a phantom node and emitted as
// common-cause siblings.
func TestLibreNMSSkipsIPLiteralParents(t *testing.T) {
	t.Setenv("TG_TEST_LNMS_TOKEN", "lnms_secret")
	body := `{"status":"ok","devices":[
		{"hostname":"cam-01","dependency_parent_hostname":"192.168.2.1"},
		{"hostname":"cam-02","dependency_parent_hostname":"coreswitch,192.168.2.1"}
	]}`
	src := NewEstateSource(
		[]Deployment{{Site: "nl", BaseURL: "https://lnms.example/", TokenRef: "env:TG_TEST_LNMS_TOKEN"}},
		WithTopologyHTTPClient(&devDoer{body: body}),
	)
	edges, err := src.Edges(context.Background())
	if err != nil {
		t.Fatalf("Edges must succeed: %v", err)
	}
	// Only cam-02→coreswitch survives; both →192.168.2.1 IP-literal edges are dropped.
	if len(edges) != 1 || edges[0].From.Name != "cam-02" || edges[0].To.Name != "coreswitch" {
		t.Fatalf("IP-literal parents must be skipped, got %d edges: %+v", len(edges), edges)
	}
}

// A deployment fetch error aborts the whole LibreNMS contribution and surfaces as one source error — never
// a silent partial topology.
func TestLibreNMSFetchErrorSurfaces(t *testing.T) {
	src := NewEstateSource(
		[]Deployment{{Site: "nl", BaseURL: "https://lnms.example/", TokenRef: "env:TG_TEST_LNMS_TOKEN_X"}},
		WithTopologyHTTPClient(&devDoer{err: errors.New("connection refused")}),
	)
	t.Setenv("TG_TEST_LNMS_TOKEN_X", "x")
	_, errs := estate.Build(context.Background(), []estate.EdgeSource{src})
	if len(errs) != 1 || errs[0].Source != estate.SourceLibreNMS {
		t.Fatalf("a fetch failure must surface as one librenms source error, got %v", errs)
	}
}
