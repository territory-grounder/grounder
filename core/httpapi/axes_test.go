package httpapi

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// TG-480: GET /v1/axes serves the serialized axis scorecard (the axisscore -json shape) read-only, and the
// bytes pass through UNTOUCHED — the grounder-side reader is the one authority; a body reshaped here would
// be a second shape to drift. Nil backend fails closed (the empty-input mutation): 503, never an empty 200
// that reads as a scorecard of zeros.
func TestAxesServesSerializedScorecardReadOnly(t *testing.T) {
	body := []byte(`{"window":"168h0m0s","incidents":42,"axes_not_live_measurable":[{"axis":"A1","name":"detection recall","missing_input":"no injected fault in window"}]}`)

	ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), auth.NewMemSessionStore(), ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)

	rt := auth.NewRouter(v)
	Register(rt, Deps{Sessions: sa, Axes: &MemAxesReader{Body: body}})

	// Vacuity floor: the route must actually be registered, or these assertions grade a 404.
	seen := false
	for _, dr := range rt.DeclaredRoutes() {
		if dr.Pattern == "/v1/axes" && dr.Method == http.MethodGet {
			seen = true
		}
	}
	if !seen {
		t.Fatal("VACUITY FLOOR: GET /v1/axes is not registered — this test would grade a 404")
	}

	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()
	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	get := func() (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/axes", nil)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code, got := get()
	if code != http.StatusOK {
		t.Fatalf("axes: got %d (%s)", code, got)
	}
	if got != string(body) {
		t.Fatalf("body must pass through untouched (one authority), got %q", got)
	}
}

func TestAxesNilBackendIs503(t *testing.T) {
	ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), auth.NewMemSessionStore(), ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)
	rt := auth.NewRouter(v)
	Register(rt, Deps{Sessions: sa})

	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()
	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/axes", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", resp.StatusCode)
	}
}
