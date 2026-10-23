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

// TestTraceReadRole drives the REAL Router (spec/020 T-020-11, REQ-2014): the elevated trace surface admits an
// admin-eligible session (the tg-admins tier) but REFUSES a plain read-only operator session — the same
// session that satisfies the AuthReadOnly console surface — so the trace-read role is genuinely DISTINCT and
// more elevated than read-only. Fail-closed: an unauthenticated caller is refused before the handler.
func TestTraceReadRole(t *testing.T) {
	store := NewMemSessionStore()
	ops := MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"), store, ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v := &Verifier{}
	v.EnableBrowserSessions(sa)

	rt := NewRouter(v)
	ok := func(w http.ResponseWriter, _ *http.Request, _ Principal) { w.WriteHeader(http.StatusOK) }
	rt.Handle("/v1/sessions", AuthReadOnly, ok, http.MethodGet)                 // the read-only console surface
	rt.Handle("/v1/sessions/{external_ref}", AuthTraceRead, ok, http.MethodGet) // the elevated trace surface
	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()

	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// A plain read-only session IS admitted to the AuthReadOnly console surface.
	if got := traceReadGet(t, srv.URL+"/v1/sessions", cookie); got != http.StatusOK {
		t.Fatalf("plain session on read-only surface: got %d, want 200 (a read-only session must satisfy AuthReadOnly)", got)
	}
	// ...but is REFUSED at the elevated trace surface (403 trace-read required — DISTINCT from read-only).
	if got := traceReadGet(t, srv.URL+"/v1/sessions/ext-1", cookie); got != http.StatusForbidden {
		t.Fatalf("plain session on trace surface: got %d, want 403 (trace-read role required)", got)
	}

	// The SAME session, once admin-eligible (an LDAP tg-admins member), IS admitted to the trace surface.
	id, _, _ := strings.Cut(cookie.Value, ".")
	sa.markAdminEligible(id)
	if got := traceReadGet(t, srv.URL+"/v1/sessions/ext-1", cookie); got != http.StatusOK {
		t.Fatalf("admin-eligible session on trace surface: got %d, want 200 (elevated trace-read admitted)", got)
	}

	// An unauthenticated request is refused before the handler (401), never opened.
	if got := traceReadGet(t, srv.URL+"/v1/sessions/ext-1", nil); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated on trace surface: got %d, want 401", got)
	}
}

func traceReadGet(t *testing.T, url string, cookie *http.Cookie) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
