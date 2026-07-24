package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// adminSurfaceRig drives the REAL router: browser sessions + the admin tier + the write deps, all
// with in-memory fakes. Tokens are generated in-test. It returns the router, the session cookie
// (already admin-elevated when elevate=true), and the fakes.
type fakeConfigWriter struct {
	lastKey, lastValue, lastRationale, lastOperator string
	err                                             error
}

func (f *fakeConfigWriter) WriteConfig(_ context.Context, key, value, rationale, operator string) (ConfigWriteOutcome, error) {
	if f.err != nil {
		return ConfigWriteOutcome{}, f.err
	}
	f.lastKey, f.lastValue, f.lastRationale, f.lastOperator = key, value, rationale, operator
	return ConfigWriteOutcome{Key: key, Value: value, Source: "console", LedgerSeq: 41}, nil
}

type fakeSecretWriter struct {
	lastName, lastValue, lastPurpose, lastRationale, lastOperator string
	err                                                           error
}

func (f *fakeSecretWriter) PutSecret(_ context.Context, name, value, purpose, rationale, operator string) (SecretPutOutcome, error) {
	if f.err != nil {
		return SecretPutOutcome{}, f.err
	}
	f.lastName, f.lastValue, f.lastPurpose, f.lastRationale, f.lastOperator = name, value, purpose, rationale, operator
	return SecretPutOutcome{Name: name, Ref: "store:" + name, LedgerSeq: 42}, nil
}

type fakeSealedList struct{ rows []SealedSecretInfo }

func (f fakeSealedList) SealedSecrets(_ context.Context) ([]SealedSecretInfo, error) {
	return f.rows, nil
}

func adminSurfaceRig(t *testing.T, d Deps, elevate bool) (*auth.Router, *http.Cookie) {
	t.Helper()
	adminWriteLimits = voteLimiter{} // a fresh per-test window for the shared write limiter
	v := &auth.Verifier{}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), auth.NewMemSessionStore(),
		auth.MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte("op-token-test"))}}, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v.EnableBrowserSessions(sa)
	aa, err := auth.NewAdminAuthenticator(
		auth.MemOperators{"root-admin": {Name: "root-admin", TokenSHA256: sha256.Sum256([]byte("admin-token-test"))}}, 15*time.Minute)
	if err != nil {
		t.Fatalf("admin authenticator: %v", err)
	}
	v.EnableAdminSessions(aa)
	d.Sessions = sa
	d.AdminSessions = aa
	rt := auth.NewRouter(v)
	Register(rt, d)

	cookie, _, err := sa.Login(context.Background(), "kyriakos", "op-token-test", "192.0.2.9:1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if elevate {
		req := httptest.NewRequest(http.MethodPost, "/v1/session/elevate", nil)
		req.RemoteAddr = "192.0.2.9:1"
		req.AddCookie(cookie)
		req.Header.Set(auth.AdminHeaderName, "root-admin")
		req.Header.Set("Authorization", "Bearer admin-token-test")
		rec := httptest.NewRecorder()
		rt.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("elevate: got %d (%s)", rec.Code, rec.Body.String())
		}
		var grant AdminGrant
		if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil || grant.AdminUntil.IsZero() {
			t.Fatalf("elevate grant malformed: %s", rec.Body.String())
		}
	}
	return rt, cookie
}

func adminPostJSON(rt *auth.Router, c *http.Cookie, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	return rec
}

func TestConfigWriteHappyPathUsesSessionIdentity(t *testing.T) {
	fw := &fakeConfigWriter{}
	rt, c := adminSurfaceRig(t, Deps{ConfigWrite: fw}, true)
	rec := adminPostJSON(rt, c, "/v1/config/session.ttl", `{"value":"8h","rationale":"shorter sessions"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("write: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out ConfigWriteOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.Key != "session.ttl" || out.Source != "console" || out.LedgerSeq != 41 {
		t.Fatalf("outcome = %+v", out)
	}
	if fw.lastOperator != "kyriakos" {
		t.Fatalf("operator identity must be the SERVER-authenticated session principal, got %q", fw.lastOperator)
	}
}

func TestConfigWriteRefusesLawKeyWith422(t *testing.T) {
	fw := &fakeConfigWriter{}
	rt, c := adminSurfaceRig(t, Deps{ConfigWrite: fw}, true)
	for _, key := range []string{"safety.mutation_enabled", "safety.never_auto_floor", "safety.predict_then_verify"} {
		rec := adminPostJSON(rt, c, "/v1/config/"+key, `{"value":"on","rationale":"try the clamp"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("LAW key %s: got %d, want 422 — the clamp is the law", key, rec.Code)
		}
	}
	if fw.lastKey != "" {
		t.Fatalf("a LAW write reached the backend: %q", fw.lastKey)
	}
}

func TestConfigWriteHonestErrors(t *testing.T) {
	fw := &fakeConfigWriter{}
	rt, c := adminSurfaceRig(t, Deps{ConfigWrite: fw}, true)

	if rec := adminPostJSON(rt, c, "/v1/config/no.such.key", `{"value":"v","rationale":"r"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key: got %d, want 404", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/config/net.public_addr", `{"value":":1","rationale":"r"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("boot-only key: got %d, want 422", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/config/session.ttl", `{"value":"8h"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing rationale: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/config/session.ttl", `{"value":"","rationale":"r"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty value: got %d, want 400", rec.Code)
	}
}

func TestConfigWriteNilBackendIs503(t *testing.T) {
	rt, c := adminSurfaceRig(t, Deps{}, true)
	rec := adminPostJSON(rt, c, "/v1/config/session.ttl", `{"value":"8h","rationale":"r"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
}

func TestConfigWriteCrossOriginRejected(t *testing.T) {
	fw := &fakeConfigWriter{}
	rt, c := adminSurfaceRig(t, Deps{ConfigWrite: fw}, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/config/session.ttl",
		strings.NewReader(`{"value":"8h","rationale":"r"}`))
	req.AddCookie(c)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin write: got %d, want 403", rec.Code)
	}
}

func TestConfigWriteRequiresElevation(t *testing.T) {
	fw := &fakeConfigWriter{}
	rt, c := adminSurfaceRig(t, Deps{ConfigWrite: fw}, false) // logged in, NOT elevated
	rec := adminPostJSON(rt, c, "/v1/config/session.ttl", `{"value":"8h","rationale":"r"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain session on config write: got %d, want 401", rec.Code)
	}
}

func TestSecretPutIsWriteOnly(t *testing.T) {
	fw := &fakeSecretWriter{}
	rt, c := adminSurfaceRig(t, Deps{SecretsWrite: fw}, true)
	rec := adminPostJSON(rt, c, "/v1/secrets/librenms.token",
		`{"value":"material-generated-in-test","purpose":"librenms API","rationale":"initial provisioning"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("put: got %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "material-generated-in-test") {
		t.Fatalf("the response echoed the secret material: %s", rec.Body.String())
	}
	var out SecretPutOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.Ref != "store:librenms.token" || out.LedgerSeq != 42 {
		t.Fatalf("outcome = %+v", out)
	}
	if fw.lastOperator != "kyriakos" || fw.lastValue != "material-generated-in-test" {
		t.Fatalf("backend call = %+v", fw)
	}
}

func TestSecretPutValidation(t *testing.T) {
	fw := &fakeSecretWriter{}
	rt, c := adminSurfaceRig(t, Deps{SecretsWrite: fw}, true)
	if rec := adminPostJSON(rt, c, "/v1/secrets/Bad..Name!", `{"value":"v","rationale":"r"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/secrets/ok.name", `{"value":"v"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing rationale: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/secrets/ok.name", `{"value":"","rationale":"r"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty value: got %d, want 400", rec.Code)
	}
}

func TestSecretsReadListsSealedNamesOnly(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	d := Deps{
		SecretsRead: staticSecretRefs{},
		SealedRead: fakeSealedList{rows: []SealedSecretInfo{
			{Name: "librenms.token", Ref: "store:librenms.token", Purpose: "poller", CreatedAt: now, UpdatedAt: now},
		}},
	}
	rt, c := adminSurfaceRig(t, d, false)
	req := httptest.NewRequest(http.MethodGet, "/v1/secrets", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("secrets read: got %d", rec.Code)
	}
	var page SecretsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("page decode: %v", err)
	}
	if len(page.Sealed) != 1 || page.Sealed[0].Ref != "store:librenms.token" {
		t.Fatalf("sealed section = %+v", page.Sealed)
	}
	// Belt and braces: the wire body carries no field named "value" anywhere.
	if strings.Contains(rec.Body.String(), `"value"`) {
		t.Fatalf("a value field appeared on the secrets read surface: %s", rec.Body.String())
	}
}

// staticSecretRefs is a minimal SecretsReader fake for the read test.
type staticSecretRefs struct{}

func (staticSecretRefs) SecretRefs(_ context.Context, _ auth.Principal) ([]SecretRefStatus, error) {
	return []SecretRefStatus{{Ref: "env:TG_SESSION_KEY", Purpose: "session cookie signing key", Resolved: true}}, nil
}

// TestAdminRoutesAbsentWithoutAdminAuthenticator proves the fail-closed registration: with sessions
// configured but NO admin tier, the elevate and write routes do not exist (404/405 from chi, never a
// handler).
func TestAdminRoutesAbsentWithoutAdminAuthenticator(t *testing.T) {
	v := &auth.Verifier{}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), auth.NewMemSessionStore(),
		auth.MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte("op-token-test"))}}, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v.EnableBrowserSessions(sa)
	rt := auth.NewRouter(v)
	Register(rt, Deps{Sessions: sa, ConfigWrite: &fakeConfigWriter{}})
	c, _, err := sa.Login(context.Background(), "kyriakos", "op-token-test", "192.0.2.9:1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	for _, path := range []string{"/v1/session/elevate", "/v1/config/session.ttl", "/v1/secrets/name"} {
		rec := adminPostJSON(rt, c, path, `{"value":"v","rationale":"r"}`)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s registered without an admin authenticator: got %d", path, rec.Code)
		}
	}
}
