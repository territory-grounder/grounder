package auth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// adminTestRig builds a router with browser sessions + the admin tier configured, one operator and
// one admin credential, and returns the pieces the tests drive. Test tokens are generated in-test —
// no literal secrets.
func adminTestRig(t *testing.T, adminTTL time.Duration) (*Router, *Verifier, *SessionAuthenticator, *AdminAuthenticator, func() *http.Cookie) {
	t.Helper()
	v := &Verifier{}
	sa, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), NewMemSessionStore(),
		MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte("op-token-for-test"))}}, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v.EnableBrowserSessions(sa)
	aa, err := NewAdminAuthenticator(
		MemOperators{"root-admin": {Name: "root-admin", TokenSHA256: sha256.Sum256([]byte("admin-token-for-test"))}}, adminTTL)
	if err != nil {
		t.Fatalf("admin authenticator: %v", err)
	}
	v.EnableAdminSessions(aa)
	rt := NewRouter(v)
	login := func() *http.Cookie {
		c, _, err := sa.Login(context.Background(), "kyriakos", "op-token-for-test", "192.0.2.9:1234")
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		return c
	}
	return rt, v, sa, aa, login
}

func adminWriteRoute(t *testing.T, rt *Router) {
	t.Helper()
	rt.Handle("/v1/config/{key}", AuthAdminSession, func(w http.ResponseWriter, r *http.Request, p Principal) {
		if !p.Admin {
			t.Errorf("admin route served a principal without Admin=true: %+v", p)
		}
		w.WriteHeader(http.StatusOK)
	})
	rt.Handle("/v1/session/elevate", AuthAdminElevate, func(w http.ResponseWriter, r *http.Request, p Principal) {
		w.WriteHeader(http.StatusOK)
	})
}

func elevate(rt *Router, c *http.Cookie, name, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/session/elevate", nil)
	req.RemoteAddr = "192.0.2.9:1234"
	if c != nil {
		req.AddCookie(c)
	}
	if name != "" {
		req.Header.Set(AdminHeaderName, name)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	return rec
}

func postAdmin(rt *Router, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/config/session.ttl", strings.NewReader(`{}`))
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	return rec
}

func TestAdminElevationGrantsWriteTier(t *testing.T) {
	rt, _, _, _, login := adminTestRig(t, 15*time.Minute)
	adminWriteRoute(t, rt)
	c := login()

	// A plain (non-elevated) session is refused on the admin route.
	if rec := postAdmin(rt, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("plain session on admin route: got %d, want 401", rec.Code)
	}
	// Step-up with the admin credential succeeds…
	if rec := elevate(rt, c, "root-admin", "admin-token-for-test"); rec.Code != http.StatusOK {
		t.Fatalf("elevate: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// …and the SAME session now satisfies AuthAdminSession with Admin=true.
	if rec := postAdmin(rt, c); rec.Code != http.StatusOK {
		t.Fatalf("elevated session on admin route: got %d, want 200", rec.Code)
	}
}

func TestAdminElevationRefusalsAreUniform(t *testing.T) {
	rt, _, _, _, login := adminTestRig(t, 15*time.Minute)
	adminWriteRoute(t, rt)
	c := login()

	cases := map[string]*httptest.ResponseRecorder{
		"no session cookie":     elevate(rt, nil, "root-admin", "admin-token-for-test"),
		"wrong admin token":     elevate(rt, c, "root-admin", "wrong-token"),
		"unknown admin account": elevate(rt, c, "nobody", "admin-token-for-test"),
		"missing credential":    elevate(rt, c, "", ""),
	}
	for name, rec := range cases {
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want uniform 401", name, rec.Code)
		}
	}
	// None of those attempts may have elevated the session.
	if rec := postAdmin(rt, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session elevated by a failed attempt: got %d, want 401", rec.Code)
	}
}

func TestAdminElevationExpiresByTTL(t *testing.T) {
	rt, _, sa, aa, login := adminTestRig(t, 10*time.Minute)
	adminWriteRoute(t, rt)
	now := time.Now()
	clock := func() time.Time { return now }
	sa.WithClock(clock)
	aa.WithClock(clock)
	c := login()
	if rec := elevate(rt, c, "root-admin", "admin-token-for-test"); rec.Code != http.StatusOK {
		t.Fatalf("elevate: got %d", rec.Code)
	}
	if rec := postAdmin(rt, c); rec.Code != http.StatusOK {
		t.Fatalf("fresh elevation refused: got %d", rec.Code)
	}
	now = now.Add(11 * time.Minute) // the elevation lapses; the session itself (1h) is still valid
	if rec := postAdmin(rt, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired elevation still admitted: got %d, want 401", rec.Code)
	}
}

func TestAdminElevationIsRateLimited(t *testing.T) {
	rt, _, _, _, login := adminTestRig(t, 10*time.Minute)
	adminWriteRoute(t, rt)
	c := login()
	for i := 0; i < 5; i++ {
		if rec := elevate(rt, c, "root-admin", "bad-guess"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, rec.Code)
		}
	}
	// The 6th attempt inside the window is refused BEFORE comparison — even with the right token.
	if rec := elevate(rt, c, "root-admin", "admin-token-for-test"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget elevation: got %d, want 429", rec.Code)
	}
}

func TestAdminTierFailsClosedWhenUnconfigured(t *testing.T) {
	// Sessions configured, admin tier NOT: elevate- and admin-class routes reject everything.
	v := &Verifier{}
	sa, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), NewMemSessionStore(),
		MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte("op-token-for-test"))}}, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v.EnableBrowserSessions(sa)
	rt := NewRouter(v)
	adminWriteRoute(t, rt)
	c, _, err := sa.Login(context.Background(), "kyriakos", "op-token-for-test", "192.0.2.9:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if rec := elevate(rt, c, "root-admin", "admin-token-for-test"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured elevate: got %d, want 401", rec.Code)
	}
	if rec := postAdmin(rt, c); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured admin route: got %d, want 401", rec.Code)
	}
}

func TestMachinePrincipalCannotSatisfyAdminRoute(t *testing.T) {
	rt, _, _, _, _ := adminTestRig(t, 10*time.Minute)
	adminWriteRoute(t, rt)
	// A request wearing HMAC headers but no cookie: the admin class never inspects HMAC material,
	// so this is a plain 401 — machine lanes and the admin tier stay disjoint by construction.
	req := httptest.NewRequest(http.MethodPost, "/v1/config/session.ttl", strings.NewReader(`{}`))
	req.Header.Set("X-TG-Source", "librenms")
	req.Header.Set("X-TG-Signature", "deadbeef")
	req.Header.Set("X-TG-Timestamp", "1")
	req.Header.Set("X-TG-Nonce", "n1")
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("HMAC caller on admin route: got %d, want 401", rec.Code)
	}
}

func TestAdminDropRevokesElevation(t *testing.T) {
	_, _, sa, aa, _ := adminTestRig(t, 10*time.Minute)
	_ = sa
	if _, err := aa.Elevate(context.Background(), "sess-1", "root-admin", "admin-token-for-test", "192.0.2.9:99"); err != nil {
		t.Fatalf("elevate: %v", err)
	}
	if _, ok := aa.Elevated("sess-1"); !ok {
		t.Fatalf("elevation missing after grant")
	}
	aa.Drop("sess-1")
	if _, ok := aa.Elevated("sess-1"); ok {
		t.Fatalf("elevation survived Drop")
	}
}
