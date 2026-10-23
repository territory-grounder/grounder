package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

func credentialsFixture() *MemCredentialsReader {
	return &MemCredentialsReader{
		Sources: []CredentialSource{
			// deliberately out of order to prove the reader sorts plane then source_id.
			{SourceID: "awx", Plane: "machine", LastSyncedAt: "2026-07-19T10:00:00Z", Added: 1, Changed: 0, Removed: 2, Drifted: true, CoveredTargets: 12, Outcome: "ok"},
			{SourceID: "ldap", Plane: "human", LastSyncedAt: "2026-07-19T09:00:00Z", Outcome: "failed", Err: "ldap unreachable"},
			{SourceID: "native-hostdiag", Plane: "machine", LastSyncedAt: "2026-07-19T10:05:00Z", CoveredTargets: 40, Outcome: "ok"},
		},
		Resolutions: []CredentialResolution{
			{Target: "librespeed01", Plane: "machine", Outcome: "resolved", Source: "native-hostdiag", Native: true, ResolvedUser: "root", Scheme: "ssh", KeyRefScheme: "file", CreatedAt: "2026-07-19T10:10:00Z"},
			{Target: "dc2fw01", Plane: "machine", Outcome: "unresolved", Err: "no source covers target", CreatedAt: "2026-07-19T10:09:00Z"},
			{Target: "librespeed01", Plane: "machine", Outcome: "ambiguous", Shadowed: []string{"awx", "ldap"}, Err: "equal-precedence sources matched", CreatedAt: "2026-07-19T10:08:00Z"},
		},
		Coverage: CredentialCoverage{
			WindowDays: 30,
			ByPlane:    []CredentialOutcomeCounts{{Key: "machine", Resolved: 5, Unresolved: 2, Ambiguous: 1, Total: 8}},
			BySource:   []CredentialOutcomeCounts{{Key: "native", Resolved: 5, Total: 5}, {Key: "unresolved", Unresolved: 2, Total: 2}},
			RecentResolved: []CredentialTargetOutcome{{Target: "librespeed01", Outcome: "resolved", Source: "native-hostdiag", CreatedAt: "2026-07-19T10:10:00Z"}},
			RecentRefused:  []CredentialTargetOutcome{{Target: "dc2fw01", Outcome: "unresolved", CreatedAt: "2026-07-19T10:09:00Z"}},
		},
	}
}

// REQ-526: /v1/credentials/sources serves the sync-source drift projection, ordered plane then source_id.
func TestCredentialSourcesShapeAndOrder(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Credentials: credentialsFixture()}.credentialSourcesHandler(w, httptest.NewRequest("GET", "/v1/credentials/sources", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page CredentialSourcesPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Sources) != 3 {
		t.Fatalf("want 3 sources, got %d", len(page.Sources))
	}
	// human plane sorts before machine; within machine, awx before native-hostdiag.
	got := []string{page.Sources[0].SourceID, page.Sources[1].SourceID, page.Sources[2].SourceID}
	want := []string{"ldap", "awx", "native-hostdiag"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if !page.Sources[1].Drifted || page.Sources[1].Removed != 2 {
		t.Fatalf("awx drift not surfaced: %+v", page.Sources[1])
	}
}

// REQ-526: /v1/credentials/resolutions serves the recent history; the ?target= filter and the ?limit= cap
// are applied by the handler and forwarded to the reader.
func TestCredentialResolutionsFilterAndCap(t *testing.T) {
	// unfiltered, default limit.
	f := credentialsFixture()
	w := httptest.NewRecorder()
	Deps{Credentials: f}.credentialResolutionsHandler(w, httptest.NewRequest("GET", "/v1/credentials/resolutions", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var page CredentialResolutionsPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if f.LastLimit != credentialsPageDefault || f.LastTarget != "" {
		t.Fatalf("default read must pass limit=%d target='', got limit=%d target=%q", credentialsPageDefault, f.LastLimit, f.LastTarget)
	}

	// ?target= filters to that target.
	f = credentialsFixture()
	w = httptest.NewRecorder()
	Deps{Credentials: f}.credentialResolutionsHandler(w, httptest.NewRequest("GET", "/v1/credentials/resolutions?target=librespeed01", nil), auth.Principal{})
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if f.LastTarget != "librespeed01" {
		t.Fatalf("target filter not forwarded: %q", f.LastTarget)
	}
	for _, r := range page.Resolutions {
		if r.Target != "librespeed01" {
			t.Fatalf("filter leaked a foreign target: %q", r.Target)
		}
	}

	// ?limit= over the cap is clamped to credentialsPageLimit BEFORE the read.
	f = credentialsFixture()
	w = httptest.NewRecorder()
	Deps{Credentials: f}.credentialResolutionsHandler(w, httptest.NewRequest("GET", "/v1/credentials/resolutions?limit=99999", nil), auth.Principal{})
	if f.LastLimit != credentialsPageLimit {
		t.Fatalf("limit must be capped at %d, got %d", credentialsPageLimit, f.LastLimit)
	}
}

// REQ-526: /v1/credentials/coverage serves the derived coverage summary.
func TestCredentialCoverageShape(t *testing.T) {
	w := httptest.NewRecorder()
	Deps{Credentials: credentialsFixture()}.credentialCoverageHandler(w, httptest.NewRequest("GET", "/v1/credentials/coverage", nil), auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var cov CredentialCoverage
	if err := json.Unmarshal(w.Body.Bytes(), &cov); err != nil {
		t.Fatal(err)
	}
	if cov.WindowDays != 30 || len(cov.ByPlane) != 1 || len(cov.BySource) != 2 {
		t.Fatalf("coverage shape wrong: %+v", cov)
	}
	if len(cov.RecentResolved) != 1 || len(cov.RecentRefused) != 1 {
		t.Fatalf("coverage frontier wrong: %+v", cov)
	}
}

// An empty spine returns an honest empty result on every route — never a fabricated row (INV-15).
func TestCredentialsEmptyIsHonest(t *testing.T) {
	empty := &MemCredentialsReader{}
	for _, tc := range []struct {
		path    string
		handler func(Deps, http.ResponseWriter, *http.Request, auth.Principal)
		wantKey string
	}{
		{"/v1/credentials/sources", Deps.credentialSourcesHandler, `"sources":[]`},
		{"/v1/credentials/resolutions", Deps.credentialResolutionsHandler, `"resolutions":[]`},
		{"/v1/credentials/coverage", Deps.credentialCoverageHandler, `"by_plane":[]`},
	} {
		w := httptest.NewRecorder()
		tc.handler(Deps{Credentials: empty}, w, httptest.NewRequest("GET", tc.path, nil), auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, w.Code)
		}
		if !strings.Contains(w.Body.String(), tc.wantKey) {
			t.Fatalf("%s: empty must serialize %s, got %s", tc.path, tc.wantKey, w.Body.String())
		}
	}
}

// A nil reader is 503 on every credential route (optional wiring; never a fabricated projection).
func TestCredentialsUnavailable(t *testing.T) {
	for _, h := range []func(Deps, http.ResponseWriter, *http.Request, auth.Principal){
		Deps.credentialSourcesHandler, Deps.credentialResolutionsHandler, Deps.credentialCoverageHandler,
	} {
		w := httptest.NewRecorder()
		h(Deps{}, w, httptest.NewRequest("GET", "/v1/credentials/x", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("nil reader: status = %d, want 503", w.Code)
		}
	}
}

// A reader error fails closed to 503 on every route, never a partial/fabricated body.
func TestCredentialsReaderErrorFailsClosed(t *testing.T) {
	bad := &MemCredentialsReader{Err: errCredsBoom}
	for _, tc := range []struct {
		path    string
		handler func(Deps, http.ResponseWriter, *http.Request, auth.Principal)
	}{
		{"/v1/credentials/sources", Deps.credentialSourcesHandler},
		{"/v1/credentials/resolutions", Deps.credentialResolutionsHandler},
		{"/v1/credentials/coverage", Deps.credentialCoverageHandler},
	} {
		w := httptest.NewRecorder()
		tc.handler(Deps{Credentials: bad}, w, httptest.NewRequest("GET", tc.path, nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: reader error status = %d, want 503", tc.path, w.Code)
		}
	}
}

var errCredsBoom = &credsErr{}

type credsErr struct{}

func (*credsErr) Error() string { return "boom" }

// CRUCIAL secret-leak guard (INV-13): no credential response may contain a key-material / ref-value / token
// field. The DTOs carry only non-secret columns by construction; this asserts the serialized JSON exposes
// NONE of the forbidden field names, only the allowed non-secret provenance keys.
func TestCredentialsNoSecretFieldEverLeaks(t *testing.T) {
	forbidden := []string{
		"secret", "password", "passphrase", "private_key", "privatekey", "private",
		"token", "bearer", "material", "key_material", "credential", "ref_value", "ref_path",
		"secret_ref", "secretref", "value", "path",
	}
	d := Deps{Credentials: credentialsFixture()}
	for _, tc := range []struct {
		path    string
		handler func(Deps, http.ResponseWriter, *http.Request, auth.Principal)
	}{
		{"/v1/credentials/sources", Deps.credentialSourcesHandler},
		{"/v1/credentials/resolutions?target=librespeed01", Deps.credentialResolutionsHandler},
		{"/v1/credentials/coverage", Deps.credentialCoverageHandler},
	} {
		w := httptest.NewRecorder()
		tc.handler(d, w, httptest.NewRequest("GET", tc.path, nil), auth.Principal{})
		// Walk the decoded JSON object graph and assert no key matches a forbidden name (case-insensitive).
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		assertNoForbiddenKey(t, tc.path, v, forbidden)
		// key_ref_scheme is ALLOWED (it is a scheme label, not a value) — sanity that the guard did not
		// over-match: it must not have tripped on the substring "ref" inside key_ref_scheme.
	}
}

func assertNoForbiddenKey(t *testing.T, where string, v any, forbidden []string) {
	t.Helper()
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			// allow-list the two non-secret scheme fields whose names contain forbidden substrings.
			if k == "key_ref_scheme" || k == "scheme" {
				assertNoForbiddenKey(t, where, child, forbidden)
				continue
			}
			lk := strings.ToLower(k)
			for _, bad := range forbidden {
				if lk == bad {
					t.Fatalf("%s: response JSON exposes forbidden field %q — a credential read must never carry a secret", where, k)
				}
			}
			assertNoForbiddenKey(t, where, child, forbidden)
		}
	case []any:
		for _, child := range node {
			assertNoForbiddenKey(t, where, child, forbidden)
		}
	}
}

// The three routes register AuthReadOnly through httpapi.Register (a route with no auth cannot exist —
// auth.Router.Handle panics on auth=none, INV-01). This proves they are declared read-only GET routes.
func TestCredentialsRoutesRegisteredReadOnlyGet(t *testing.T) {
	rt := auth.NewRouter(&auth.Verifier{})
	Register(rt, Deps{Credentials: credentialsFixture()})
	want := map[string]bool{
		"/v1/credentials/sources":     false,
		"/v1/credentials/resolutions": false,
		"/v1/credentials/coverage":    false,
	}
	for _, dr := range rt.DeclaredRoutes() {
		if _, ok := want[dr.Pattern]; ok {
			if dr.Method != http.MethodGet {
				t.Fatalf("%s declared method %q, want GET", dr.Pattern, dr.Method)
			}
			want[dr.Pattern] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Fatalf("route %s was not registered as a declared GET route", p)
		}
	}
}
