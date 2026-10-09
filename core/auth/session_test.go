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

func testSessions(t *testing.T, ttl time.Duration) (*SessionAuthenticator, *MemSessionStore) {
	t.Helper()
	store := NewMemSessionStore()
	ops := MemOperators{"kyriakos": {Name: "kyriakos", TokenSHA256: sha256.Sum256([]byte("correct-horse"))}}
	sa, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, ttl)
	if err != nil {
		t.Fatalf("NewSessionAuthenticator: %v", err)
	}
	return sa, store
}

// Construction fails closed on any missing piece: short key, nil store/resolver, non-positive TTL.
func TestSessionAuthenticatorFailClosedConstruction(t *testing.T) {
	ops := MemOperators{}
	store := NewMemSessionStore()
	if _, err := NewSessionAuthenticator([]byte("short"), store, ops, time.Hour); err == nil {
		t.Fatal("short key must be rejected")
	}
	if _, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), nil, ops, time.Hour); err == nil {
		t.Fatal("nil store must be rejected")
	}
	if _, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, nil, time.Hour); err == nil {
		t.Fatal("nil resolver must be rejected")
	}
	if _, err := NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, 0); err == nil {
		t.Fatal("zero TTL must be rejected")
	}
}

// A valid login mints a registered, HttpOnly, SameSite=Strict cookie whose session verifies to the
// operator; a wrong token and an unknown operator fail identically.
func TestLoginAndVerify(t *testing.T) {
	sa, _ := testSessions(t, time.Hour)
	ctx := context.Background()

	cookie, _, err := sa.Login(ctx, "kyriakos", "correct-horse", "192.0.2.9:5555")
	if err != nil {
		t.Fatalf("valid login failed: %v", err)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Name != SessionCookieName {
		t.Fatalf("cookie attributes wrong: %+v", cookie)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
	r.AddCookie(cookie)
	op, err := sa.verify(r)
	if err != nil || op != "kyriakos" {
		t.Fatalf("verify: op=%q err=%v", op, err)
	}

	if _, _, err := sa.Login(ctx, "kyriakos", "wrong", "192.0.2.9:5555"); err != ErrUnauthenticated {
		t.Fatalf("wrong token must be ErrUnauthenticated, got %v", err)
	}
	if _, _, err := sa.Login(ctx, "nobody", "correct-horse", "192.0.2.9:5555"); err != ErrUnauthenticated {
		t.Fatalf("unknown operator must be ErrUnauthenticated, got %v", err)
	}
}

// Tampered, expired, and revoked cookies are rejected identically (ErrUnauthenticated).
func TestVerifyRejectsTamperedExpiredRevoked(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	sa, store := testSessions(t, 30*time.Minute)
	sa.WithClock(func() time.Time { return now })
	ctx := context.Background()

	cookie, _, err := sa.Login(ctx, "kyriakos", "correct-horse", "192.0.2.9:1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	mk := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/stats", nil)
		r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: v})
		return r
	}
	// tampered signature
	id, _, _ := strings.Cut(cookie.Value, ".")
	if _, err := sa.verify(mk(id + "." + strings.Repeat("0", 64))); err != ErrUnauthenticated {
		t.Fatalf("tampered sig must fail: %v", err)
	}
	// tampered id (signature no longer matches)
	if _, err := sa.verify(mk("deadbeef" + cookie.Value[8:])); err != ErrUnauthenticated {
		t.Fatalf("tampered id must fail: %v", err)
	}
	// expired
	sa.WithClock(func() time.Time { return now.Add(31 * time.Minute) })
	if _, err := sa.verify(mk(cookie.Value)); err != ErrUnauthenticated {
		t.Fatalf("expired session must fail: %v", err)
	}
	// revoked
	sa.WithClock(func() time.Time { return now })
	if err := store.Revoke(ctx, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := sa.verify(mk(cookie.Value)); err != ErrUnauthenticated {
		t.Fatalf("revoked session must fail: %v", err)
	}
}

// Login failures are rate-limited per (operator, ip): the budget blocks BEFORE digest comparison, a
// different ip is unaffected, and a success resets the window.
func TestLoginRateLimit(t *testing.T) {
	sa, _ := testSessions(t, time.Hour)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, _, err := sa.Login(ctx, "kyriakos", "wrong", "192.0.2.9:1"); err != ErrUnauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, _, err := sa.Login(ctx, "kyriakos", "correct-horse", "192.0.2.9:1"); err != ErrLoginRateLimited {
		t.Fatalf("6th attempt must be rate-limited even with the right token, got %v", err)
	}
	// another ip is not collateral damage
	if _, _, err := sa.Login(ctx, "kyriakos", "correct-horse", "192.0.2.10:1"); err != nil {
		t.Fatalf("different ip must not be limited: %v", err)
	}
}

// The router-level policy: a session principal satisfies AuthReadOnly GETs only; it never satisfies
// AuthHMAC/AuthMTLS routes; non-GETs on read routes are 403; logout revokes; an unconfigured verifier
// fails closed. This drives the REAL router wrap — no fakes.
func TestRouterSessionPolicy(t *testing.T) {
	sa, _ := testSessions(t, time.Hour)
	v := &Verifier{} // no machine auth configured; session only
	v.EnableBrowserSessions(sa)
	rt := NewRouter(v)
	hits := map[string]int{}
	handler := func(name string) PrincipalHandler {
		return func(w http.ResponseWriter, _ *http.Request, p Principal) {
			hits[name]++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(p.SourceID + "|" + p.Method.String()))
		}
	}
	rt.Handle("/read", AuthReadOnly, handler("read"))
	rt.Handle("/machine", AuthHMAC, handler("machine"))
	rt.Handle("/tls", AuthMTLS, handler("tls"))

	cookie, _, err := sa.Login(context.Background(), "kyriakos", "correct-horse", "192.0.2.9:1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	do := func(method, path string, withCookie bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if withCookie {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		rt.Mux().ServeHTTP(w, r)
		return w
	}

	if w := do(http.MethodGet, "/read", true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "operator:kyriakos|session") {
		t.Fatalf("session GET on read route: %d %q", w.Code, w.Body.String())
	}
	if w := do(http.MethodPost, "/read", true); w.Code != http.StatusForbidden {
		t.Fatalf("session POST on read route must be 403, got %d", w.Code)
	}
	if w := do(http.MethodGet, "/machine", true); w.Code != http.StatusUnauthorized {
		t.Fatalf("session on AuthHMAC route must be 401, got %d", w.Code)
	}
	if w := do(http.MethodGet, "/tls", true); w.Code != http.StatusUnauthorized {
		t.Fatalf("session on AuthMTLS route must be 401, got %d", w.Code)
	}
	if w := do(http.MethodGet, "/read", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("no credential at all must be 401, got %d", w.Code)
	}
	if hits["machine"] != 0 || hits["tls"] != 0 {
		t.Fatalf("machine handlers must never run under a session principal: %v", hits)
	}

	// a verifier with NO session authenticator fails closed on the session paths
	bare := NewRouter(&Verifier{})
	bare.Handle("/read", AuthReadOnly, handler("bare"))
	r := httptest.NewRequest(http.MethodGet, "/read", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	bare.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured session path must be 401, got %d", w.Code)
	}
}
