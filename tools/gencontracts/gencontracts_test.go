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

// A PATH SERVING TWO VERBS MUST EMIT ONE MAPPING KEY (TG-261).
//
// The generator emitted one block per route, so a multi-verb path produced a DUPLICATE YAML mapping key —
// invalid OpenAPI, and silently destructive: a parser keeps whichever entry wins, so one verb disappears
// from the published contract while the server still serves it. Latent until /v1/config/{key} gained
// DELETE beside POST and became the first multi-verb path in the router.
//
// KILLING MUTATION: revert to printing the path header on every route. RED.
func TestAMultiVerbPathEmitsOneMappingKey(t *testing.T) {
	m := Model{Routes: []Route{
		{Method: "DELETE", Path: "/v1/thing/{id}", Auth: "tgHMAC"},
		{Method: "POST", Path: "/v1/thing/{id}", Auth: "tgHMAC"},
		{Method: "GET", Path: "/v1/other", Auth: "tgHMAC"},
	}}
	oa := Generate(m, "2026-08-03T00:00:00Z").OpenAPI
	if n := strings.Count(oa, "  /v1/thing/{id}:\n"); n != 1 {
		t.Fatalf("the two-verb path emitted %d mapping keys, want exactly 1 — a duplicate key makes the "+
			"contract invalid and drops one verb silently:\n%s", n, oa)
	}
	for _, verb := range []string{"    delete:\n", "    post:\n"} {
		if !strings.Contains(oa, verb) {
			t.Fatalf("verb %q missing from the emitted contract:\n%s", strings.TrimSpace(verb), oa)
		}
	}
	if n := strings.Count(oa, "  /v1/other:\n"); n != 1 {
		t.Fatalf("single-verb path emitted %d keys, want 1", n)
	}
}

// THE COVERAGE VERIFIER USED TO BE INCAPABLE OF FAILING (TG-249).
//
// It ran `strings.Contains(a.OpenAPI, r.Auth)` — a substring search for a scheme name in a document the
// GENERATOR had just written from that same string. Verification proved the generator echoed its own
// input. The audit demonstrated it by setting a route's scheme to `tgTotallyUndefinedScheme`, which still
// returned nil.
//
// This pins the two failures a real generator bug produces: a scheme that is referenced but never defined,
// and a route whose own security block carries a DIFFERENT scheme than the model says. The second is the
// original defect — every route emitted `tgHMAC` while fifteen of them accept only a session.
func TestVerifyCoverageRejectsAnUndefinedScheme(t *testing.T) {
	m := Model{Routes: []Route{{Method: "GET", Path: "/v1/thing", Auth: "tgGhost"}}}
	a := Artifacts{
		GeneratedAt: "t", SourceHash: "h", CoverageScope: "s",
		OpenAPI: "components:\n  securitySchemes:\n    tgHMAC:\n      type: apiKey\n" +
			"paths:\n  /v1/thing:\n    get:\n      security:\n        - tgGhost: []\n",
	}
	if err := VerifyCoverage(m, a); err == nil {
		t.Fatal("a route referencing a scheme that components.securitySchemes never defines was accepted — " +
			"the published contract's security refs would dangle and no reader could satisfy them")
	}
}

func TestVerifyCoverageRejectsTheWrongSchemeOnTheRoute(t *testing.T) {
	// The model says this route needs a session; the emitted document says HMAC. That is exactly the shape
	// that shipped: a client reading the contract signs the request and is rejected 401 by the server.
	m := Model{Routes: []Route{{Method: "POST", Path: "/v1/config/{key}", Auth: "tgSession"}}}
	a := Artifacts{
		GeneratedAt: "t", SourceHash: "h", CoverageScope: "s",
		OpenAPI: "components:\n  securitySchemes:\n    tgHMAC:\n      type: apiKey\n" +
			"    tgSession:\n      type: apiKey\n" +
			"paths:\n  /v1/config/{key}:\n    post:\n      security:\n        - tgHMAC: []\n",
	}
	if err := VerifyCoverage(m, a); err == nil {
		t.Fatal("a route whose emitted security block carries a DIFFERENT scheme than the model declares " +
			"was accepted — this is the TG-249 defect, and the verifier that missed it was searching the " +
			"whole document rather than the route's own block")
	}
}

// And it must still PASS on a correct document, or the guard would just be an always-fail.
func TestVerifyCoverageAcceptsACorrectlySchemedRoute(t *testing.T) {
	m := Model{Routes: []Route{{Method: "POST", Path: "/v1/config/{key}", Auth: "tgSession"}}}
	a := Artifacts{
		GeneratedAt: "t", SourceHash: "h", CoverageScope: "s",
		OpenAPI: "components:\n  securitySchemes:\n    tgSession:\n      type: apiKey\n" +
			"paths:\n  /v1/config/{key}:\n    post:\n      security:\n        - tgSession: []\n",
	}
	if err := VerifyCoverage(m, a); err != nil {
		t.Fatalf("a correctly-schemed route was rejected: %v", err)
	}
}
