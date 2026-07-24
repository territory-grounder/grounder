// Package gencontracts generates Territory Grounder's wire contracts (openapi.yaml / asyncapi.yaml /
// JSON Schemas) from ONE canonical source — the authenticated router's registered routes and the typed
// schema-version registry — never a hand-maintained parallel copy. Every generated artifact embeds a
// non-null generated_at, a deterministic source hash, and a coverage scope; CI regenerates and fails on
// drift, an uncovered routed endpoint, or a hand-written count.
//
// Provenance: [O] INV-15 (one generated source of truth per entity; 100%-endpoint coverage with
// declared auth/error schemas + provenance), spec/006 REQ-501b. This is a library so the acceptance
// oracle can drive Generate/BuildModel directly; the tool main (cmd/gencontracts) is a thin wrapper.
package gencontracts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/schema"
)

// Route is one routed endpoint in the canonical model.
type Route struct {
	Method string
	Path   string
	Auth   string // the declared auth scheme (every route is authenticated — INV-01)
}

// Entity is one governed, schema-stamped table in the canonical model.
type Entity struct {
	Table   string
	Version int
}

// Model is the single canonical source the contracts derive from.
type Model struct {
	Routes   []Route
	Entities []Entity
}

// BuildModel assembles the canonical model from the authenticated router's registered routes and the
// governed schema-version registry. The routes come from the SAME httpapi.Register the server uses, so
// the contract cannot drift from the served surface. Deps carries a throwaway session authenticator so
// the CONDITIONAL browser-path routes (login/logout/vote) register too — without it the drift gate was
// blind to them (they were served in production but absent from the contract, an INV-15 gap). The
// handlers are wired but never invoked here.
func BuildModel() (Model, error) {
	rt := auth.NewRouter(&auth.Verifier{})
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("c", 32)), auth.NewMemSessionStore(), auth.MemOperators{}, time.Hour)
	if err != nil {
		return Model{}, fmt.Errorf("gencontracts: throwaway session authenticator: %w", err)
	}
	// The throwaway ADMIN authenticator registers the conditional admin-tier routes too (task #27
	// Phase B–D: elevate + config/secret writes) — without it the drift gate would be blind to them.
	aa, err := auth.NewAdminAuthenticator(auth.MemOperators{}, time.Hour)
	if err != nil {
		return Model{}, fmt.Errorf("gencontracts: throwaway admin authenticator: %w", err)
	}
	httpapi.Register(rt, httpapi.Deps{Sessions: sa, AdminSessions: aa})

	// The canonical method per route is the one its handler actually serves — declared at registration
	// (rt.Handle's httpMethods) and enforced internally by the handler's own r.Method guard. We emit THAT,
	// not chi's all-method catch-all: the router registers every route for all methods so the auth
	// middleware runs first (a session POST to a read route is the auth layer's 403, not a routing 405),
	// which made chi.Walk report connect/delete/get/… for every path. Every route reaching registration
	// went through core/auth, which panics on auth=none, so a declared auth scheme is guaranteed (INV-01).
	var rs []Route
	declaredPaths := make(map[string]bool)
	for _, d := range rt.DeclaredRoutes() {
		rs = append(rs, Route{Method: d.Method, Path: d.Pattern, Auth: "tgHMAC"})
		declaredPaths[d.Pattern] = true
	}

	// Coverage safety net (INV-15): every path the router actually serves MUST carry a method declaration,
	// or the contract would silently omit a live endpoint. Walk the served route table and fail closed on
	// any served-but-undeclared path — the same guarantee the drift gate gives, kept as routes are added.
	routes, ok := rt.Mux().(chi.Routes)
	if !ok {
		return Model{}, fmt.Errorf("gencontracts: router does not expose chi.Routes")
	}
	seen := make(map[string]bool)
	var undeclared []string
	err = chi.Walk(routes, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !declaredPaths[route] && !seen[route] {
			undeclared = append(undeclared, route)
			seen[route] = true
		}
		return nil
	})
	if err != nil {
		return Model{}, err
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		return Model{}, fmt.Errorf("gencontracts: routed path(s) %v carry no declared HTTP method — pass the method to rt.Handle so the contract lists the real verb (INV-15)", undeclared)
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Path != rs[j].Path {
			return rs[i].Path < rs[j].Path
		}
		return rs[i].Method < rs[j].Method
	})

	tables := schema.Tables()
	sort.Slice(tables, func(i, j int) bool { return tables[i] < tables[j] })
	var es []Entity
	for _, t := range tables {
		v, err := schema.Current(t)
		if err != nil {
			return Model{}, err
		}
		es = append(es, Entity{Table: string(t), Version: int(v)})
	}
	return Model{Routes: rs, Entities: es}, nil
}

// SourceHash is a deterministic hash over the canonical model — the drift key. It excludes generated_at,
// so regenerating an unchanged model yields the same hash (a wall-clock timestamp never reads as drift).
func (m Model) SourceHash() string {
	h := sha256.New()
	for _, r := range m.Routes {
		fmt.Fprintf(h, "route\x00%s\x00%s\x00%s\n", r.Method, r.Path, r.Auth)
	}
	for _, e := range m.Entities {
		fmt.Fprintf(h, "entity\x00%s\x00%d\n", e.Table, e.Version)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Artifacts is the generated contract set plus its provenance.
type Artifacts struct {
	OpenAPI       string
	AsyncAPI      string
	JSONSchemas   map[string]string
	GeneratedAt   string
	SourceHash    string
	CoverageScope string
	RouteCount    int
	EntityCount   int
}

// Generate emits the contract set from the model. generatedAt is injected (the caller stamps the wall
// clock) so generation is otherwise deterministic. Every routed endpoint is emitted with a declared
// security scheme and error responses (401/404); every entity gets a JSON Schema; the provenance fields
// are always populated (REQ-501b). [O] INV-15.
func Generate(m Model, generatedAt string) Artifacts {
	src := m.SourceHash()
	cov := fmt.Sprintf("routes=%d;entities=%d", len(m.Routes), len(m.Entities))

	var oa strings.Builder
	fmt.Fprintf(&oa, "openapi: 3.0.3\n")
	fmt.Fprintf(&oa, "info:\n  title: Territory Grounder API\n  version: \"1\"\n")
	fmt.Fprintf(&oa, "  x-provenance:\n    generated_at: %q\n    source_hash: %q\n    coverage_scope: %q\n", generatedAt, src, cov)
	fmt.Fprintf(&oa, "components:\n  securitySchemes:\n    tgHMAC:\n      type: apiKey\n      in: header\n      name: X-TG-Signature\n")
	fmt.Fprintf(&oa, "paths:\n")
	for _, r := range m.Routes {
		fmt.Fprintf(&oa, "  %s:\n    %s:\n", oapiPath(r.Path), strings.ToLower(r.Method))
		fmt.Fprintf(&oa, "      security:\n        - %s: []\n", r.Auth)
		fmt.Fprintf(&oa, "      responses:\n        \"200\": {description: ok}\n        \"401\": {description: unauthenticated}\n        \"404\": {description: not found}\n")
	}

	var aa strings.Builder
	fmt.Fprintf(&aa, "asyncapi: 2.6.0\n")
	fmt.Fprintf(&aa, "info:\n  title: Territory Grounder events\n  version: \"1\"\n")
	fmt.Fprintf(&aa, "  x-provenance:\n    generated_at: %q\n    source_hash: %q\n    coverage_scope: %q\n", generatedAt, src, cov)
	fmt.Fprintf(&aa, "channels:\n  triage.requested:\n    subscribe:\n      message:\n        payload:\n          $ref: '#/components/schemas/IncidentEnvelope'\n")

	schemas := make(map[string]string, len(m.Entities))
	for _, e := range m.Entities {
		schemas[e.Table] = fmt.Sprintf("{\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"title\":%q,\"type\":\"object\",\"x-schema-version\":%d,\"x-source-hash\":%q}\n", e.Table, e.Version, src)
	}

	return Artifacts{
		OpenAPI:       oa.String(),
		AsyncAPI:      aa.String(),
		JSONSchemas:   schemas,
		GeneratedAt:   generatedAt,
		SourceHash:    src,
		CoverageScope: cov,
		RouteCount:    len(m.Routes),
		EntityCount:   len(m.Entities),
	}
}

// oapiPath converts a chi route pattern ("/v1/sessions/{external_ref}/replay") to an OpenAPI path (chi
// and OpenAPI both use {param}, so this is identity today; kept as the single conversion point).
func oapiPath(p string) string { return p }

// CoverageError reports a routed endpoint missing from the generated OpenAPI.
type CoverageError struct{ Route Route }

func (e CoverageError) Error() string {
	return fmt.Sprintf("gencontracts: route %s %s is not covered by the generated OpenAPI", e.Route.Method, e.Route.Path)
}

// VerifyCoverage checks that every routed endpoint in the model appears in the generated OpenAPI with a
// declared security scheme, and that the provenance fields are non-null (REQ-501b). It returns the first
// coverage gap, or nil.
func VerifyCoverage(m Model, a Artifacts) error {
	if a.GeneratedAt == "" || a.SourceHash == "" || a.CoverageScope == "" {
		return fmt.Errorf("gencontracts: generated artifact is missing provenance (generated_at/source_hash/coverage_scope)")
	}
	for _, r := range m.Routes {
		if !strings.Contains(a.OpenAPI, oapiPath(r.Path)) {
			return CoverageError{Route: r}
		}
		if !strings.Contains(a.OpenAPI, r.Auth) {
			return fmt.Errorf("gencontracts: route %s has no declared security scheme", r.Path)
		}
	}
	return nil
}

// VerifyNoDrift reports whether a freshly built model's source hash matches a committed hash — the CI
// drift gate. A mismatch means the served surface diverged from the committed contract.
func VerifyNoDrift(committedSourceHash string) (bool, error) {
	m, err := BuildModel()
	if err != nil {
		return false, err
	}
	return m.SourceHash() == committedSourceHash, nil
}
