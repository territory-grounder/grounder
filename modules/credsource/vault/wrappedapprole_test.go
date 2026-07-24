package vault_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/credsource/vault"
)

// wrapFake stands up the three endpoints the wrapped-AppRole flow touches: single-use unwrap, AppRole login,
// and a KV read (to prove the resolved token works end-to-end).
type wrapFake struct {
	mu           sync.Mutex
	wrapToken    string // the valid wrapping token
	unwrapped    bool   // single-use: flips true on first unwrap
	secretID     string // the secret_id the unwrap yields
	seenSecretID string // the secret_id the login received (asserts the flow threaded it)
}

func (f *wrapFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case path == "sys/wrapping/unwrap":
			// The wrapping token authenticates the unwrap; it is single-use (a second unwrap fails — the
			// tamper-alarm property).
			if r.Header.Get("X-Vault-Token") != f.wrapToken || f.unwrapped {
				writeErr(w, 400, "wrapping token is not valid or already used")
				return
			}
			f.unwrapped = true
			writeOK(w, map[string]any{"data": map[string]any{"secret_id": f.secretID}})
		case path == "auth/approle/login":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			f.seenSecretID = body["secret_id"]
			if body["role_id"] == "" || body["secret_id"] == "" {
				writeErr(w, 400, "missing role_id/secret_id")
				return
			}
			writeOK(w, map[string]any{"auth": map[string]any{"client_token": "issued-token", "lease_duration": 3600, "renewable": true}})
		case r.Method == http.MethodGet && path == "secret/data/tg/x":
			if r.Header.Get("X-Vault-Token") != "issued-token" {
				writeErr(w, 403, "permission denied")
				return
			}
			writeOK(w, map[string]any{"data": map[string]any{"data": map[string]any{"field": "the-secret-value"}}})
		default:
			w.WriteHeader(404)
		}
	})
}

func writeOK(w http.ResponseWriter, v any) { w.WriteHeader(200); json.NewEncoder(w).Encode(v) }
func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"errors": []string{msg}})
}

// The wrapped-AppRole flow: unwrap the single-use wrapping token → get the secret_id → AppRole login → the
// resolved token reads a KV field. No durable secret_id ever on disk (only the spent wrapping token).
func TestWrappedAppRoleResolvesEndToEnd(t *testing.T) {
	f := &wrapFake{wrapToken: "wrap-abc", secretID: "the-secret-id"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	t.Setenv("TG_TEST_ROLE_ID", "role-123")
	t.Setenv("TG_TEST_WRAP", "wrap-abc")

	c, err := vault.New(vault.Config{
		BaseURL:    srv.URL,
		HTTPClient: http.DefaultClient,
		Auth: vault.WrappedAppRole{
			RoleIDRef:    config.SecretRef("env:TG_TEST_ROLE_ID"),
			WrapTokenRef: config.SecretRef("env:TG_TEST_WRAP"),
		},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.ResolveRef("bao:secret/data/tg/x#field")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "the-secret-value" {
		t.Fatalf("resolved value = %q, want the-secret-value", got)
	}
	if f.seenSecretID != "the-secret-id" {
		t.Fatalf("login must receive the UNWRAPPED secret_id, got %q", f.seenSecretID)
	}
}

// Single-use tamper property: if the wrapping token was already spent (an attacker unwrapped first), TG's
// unwrap FAILS closed — no token issued, resolution errors.
func TestWrappedAppRoleFailsClosedOnSpentToken(t *testing.T) {
	f := &wrapFake{wrapToken: "wrap-abc", secretID: "sid", unwrapped: true} // already spent
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	t.Setenv("TG_TEST_ROLE_ID", "role-123")
	t.Setenv("TG_TEST_WRAP", "wrap-abc")
	c, _ := vault.New(vault.Config{
		BaseURL: srv.URL, HTTPClient: http.DefaultClient,
		Auth: vault.WrappedAppRole{RoleIDRef: config.SecretRef("env:TG_TEST_ROLE_ID"), WrapTokenRef: config.SecretRef("env:TG_TEST_WRAP")},
	})
	if _, err := c.ResolveRef("bao:secret/data/tg/x#field"); err == nil {
		t.Fatal("a spent/tampered wrapping token must fail closed, got nil error")
	}
}

// An empty/unresolvable wrapping token fails closed (never a blank auth).
func TestWrappedAppRoleFailsClosedOnEmptyWrapToken(t *testing.T) {
	srv := httptest.NewServer((&wrapFake{wrapToken: "x", secretID: "y"}).handler())
	defer srv.Close()
	t.Setenv("TG_TEST_ROLE_ID", "role-123")
	t.Setenv("TG_TEST_WRAP", "") // empty
	c, _ := vault.New(vault.Config{
		BaseURL: srv.URL, HTTPClient: http.DefaultClient,
		Auth: vault.WrappedAppRole{RoleIDRef: config.SecretRef("env:TG_TEST_ROLE_ID"), WrapTokenRef: config.SecretRef("env:TG_TEST_WRAP")},
	})
	if _, err := c.ResolveRef("bao:secret/data/tg/x#field"); err == nil {
		t.Fatal("an empty wrapping token must fail closed")
	}
}
