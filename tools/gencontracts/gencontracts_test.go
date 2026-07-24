package gencontracts

import (
	"net/http"
	"strings"
	"testing"
)

func TestBuildModelHasRoutesAndEntities(t *testing.T) {
	m, err := BuildModel()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Routes) == 0 {
		t.Fatal("model must enumerate the registered routes")
	}
	if len(m.Entities) == 0 {
		t.Fatal("model must enumerate the governed entities")
	}
	// the two read-only surfaces must appear
	var stats, replay bool
	for _, r := range m.Routes {
		if strings.Contains(r.Path, "/v1/stats") {
			stats = true
		}
		if strings.Contains(r.Path, "/replay") {
			replay = true
		}
	}
	if !stats || !replay {
		t.Fatalf("both /v1/stats and the replay route must be covered: %+v", m.Routes)
	}
}

func TestGenerateCoversEveryRouteWithProvenance(t *testing.T) {
	m, _ := BuildModel()
	a := Generate(m, "2026-07-15T00:00:00Z")

	if a.GeneratedAt == "" || a.SourceHash == "" || a.CoverageScope == "" {
		t.Fatalf("artifact must carry non-null provenance: %+v", a)
	}
	if err := VerifyCoverage(m, a); err != nil {
		t.Fatalf("every routed endpoint must be covered: %v", err)
	}
	// every entity gets a JSON Schema
	if len(a.JSONSchemas) != len(m.Entities) {
		t.Fatalf("one JSON schema per entity, got %d for %d entities", len(a.JSONSchemas), len(m.Entities))
	}
}

// methodsForPath returns every HTTP method the model records for a path.
func methodsForPath(m Model, path string) []string {
	var out []string
	for _, r := range m.Routes {
		if r.Path == path {
			out = append(out, r.Method)
		}
	}
	return out
}

// pathBlock returns the generated-OpenAPI YAML for a single path — from its "  <path>:" line up to the
// next top-level path — so a method assertion cannot leak into a neighbouring route.
func pathBlock(oapi, path string) string {
	marker := "  " + path + ":\n"
	i := strings.Index(oapi, marker)
	if i < 0 {
		return ""
	}
	rest := oapi[i+len(marker):]
	if j := strings.Index(rest, "\n  /"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestContractListsOnlyRealMethodsPerRoute is the regression guard for the bug: the contract used to list
// EVERY HTTP verb (connect/delete/get/…) on every path because the router registers an all-method
// catch-all and chi.Walk enumerated them all. A GET-only read route must now list ONLY get, and a write
// route ONLY post — the real verb each handler serves.
func TestContractListsOnlyRealMethodsPerRoute(t *testing.T) {
	m, err := BuildModel()
	if err != nil {
		t.Fatal(err)
	}

	// A read-only route lists exactly GET — never the phantom all-method catch-all.
	if got := methodsForPath(m, "/v1/alerts"); len(got) != 1 || got[0] != http.MethodGet {
		t.Fatalf("/v1/alerts (read-only) must list only [GET], got %v", got)
	}
	if got := methodsForPath(m, "/v1/stats"); len(got) != 1 || got[0] != http.MethodGet {
		t.Fatalf("/v1/stats (read-only) must list only [GET], got %v", got)
	}
	// A write/ingest/vote route lists exactly POST.
	if got := methodsForPath(m, "/v1/ingest/{source_type}"); len(got) != 1 || got[0] != http.MethodPost {
		t.Fatalf("/v1/ingest/{source_type} must list only [POST], got %v", got)
	}
	if got := methodsForPath(m, "/v1/vote"); len(got) != 1 || got[0] != http.MethodPost {
		t.Fatalf("/v1/vote must list only [POST], got %v", got)
	}

	// The rendered OpenAPI for /v1/alerts must carry the get operation and NONE of the phantom verbs.
	a := Generate(m, "2026-07-19T00:00:00Z")
	block := pathBlock(a.OpenAPI, "/v1/alerts")
	if !strings.Contains(block, "    get:") {
		t.Fatalf("/v1/alerts OpenAPI block must contain the get operation, got:\n%s", block)
	}
	for _, phantom := range []string{"    post:", "    put:", "    patch:", "    delete:", "    connect:", "    head:", "    options:", "    trace:"} {
		if strings.Contains(block, phantom) {
			t.Fatalf("/v1/alerts (read-only) must NOT list %q, got:\n%s", strings.TrimSpace(phantom), block)
		}
	}
}

func TestVerifyCoverageDetectsAGap(t *testing.T) {
	m, _ := BuildModel()
	a := Generate(m, "t")
	// add a route the artifact does not cover ⇒ coverage must fail
	m.Routes = append(m.Routes, Route{Method: "GET", Path: "/v1/uncovered", Auth: "tgHMAC"})
	if err := VerifyCoverage(m, a); err == nil {
		t.Fatal("an uncovered route must fail the coverage check")
	}
}

func TestSourceHashDeterministicAndTimestampIndependent(t *testing.T) {
	m, _ := BuildModel()
	// two generations at different timestamps yield the same source hash (drift key excludes generated_at)
	if Generate(m, "t1").SourceHash != Generate(m, "t2").SourceHash {
		t.Fatal("source hash must be independent of generated_at")
	}
	// a changed model changes the hash
	m2 := m
	m2.Entities = append(append([]Entity(nil), m.Entities...), Entity{Table: "new_table", Version: 1})
	if m.SourceHash() == m2.SourceHash() {
		t.Fatal("a changed model must change the source hash")
	}
}
