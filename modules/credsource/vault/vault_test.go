package vault_test

// ORACLE tests for the OpenBao/Vault connector (spec/016 T-016-9, REQ-1613). CI has no OpenBao, so a
// httptest.Server fakes the KV v2 read/list + AppRole/JWT/token login endpoints, and the tests drive the
// REAL client + resolver + CredentialSource through it. They prove: a successful KV v2 read; list→sync→
// resolvable entries; fail-closed on 403 / 404 / unreachable / expired-token→re-login; and that no plaintext
// secret is stored (bundles carry vault: refs). Run under -race.
//
// A BEST-EFFORT live check (TestLiveHealth) hits the real openbao01 only when TG_OPENBAO_LIVE_ADDR is set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/modules/credsource/vault"
)

// fakeBao is an in-memory OpenBao/Vault KV v2 + auth backend for the oracles.
type fakeBao struct {
	mu         sync.Mutex
	logins     int    // number of successful logins issued
	lastToken  string // the most-recently issued token
	minToken   int    // reads require a token >= this counter (raise it to simulate expiry)
	kv         map[string]map[string]any
	list       map[string][]string
	denyReads  bool // force every read to 403 (permanent denial)
	notFound   bool // force every read to 404
	healthDown bool
}

func newFakeBao() *fakeBao {
	return &fakeBao{
		kv: map[string]map[string]any{
			"secret/data/hosts/hostA": {"user": "vaultuser", "ssh_key": "PRIVATE-KEY-A", "port": "2222"},
			"secret/data/hosts/hostB": {"user": "opsuser", "ssh_key": "PRIVATE-KEY-B"},
		},
		list: map[string][]string{
			"secret/metadata/hosts": {"hostA", "hostB", "sub/"},
		},
	}
}

func (f *fakeBao) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeBao) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")

	// auth logins (AppRole / JWT) — issue an incrementing token.
	if r.Method == http.MethodPost && strings.HasPrefix(path, "auth/") && strings.HasSuffix(path, "/login") {
		f.logins++
		f.lastToken = "tok-" + itoa(f.logins)
		writeJSON(w, 200, map[string]any{
			"auth": map[string]any{"client_token": f.lastToken, "lease_duration": 3600, "renewable": true},
		})
		return
	}
	if path == "sys/health" {
		if f.healthDown {
			w.WriteHeader(503)
			return
		}
		writeJSON(w, 200, map[string]any{"initialized": true, "sealed": false})
		return
	}

	// everything else requires a valid token.
	tok := r.Header.Get("X-Vault-Token")
	if !f.tokenValid(tok) {
		writeJSON(w, 403, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	switch {
	case r.Method == "LIST":
		keys, ok := f.list[path]
		if !ok {
			writeJSON(w, 404, map[string]any{"errors": []string{}})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"keys": keys}})
	case r.Method == http.MethodGet:
		if f.denyReads {
			writeJSON(w, 403, map[string]any{"errors": []string{"permission denied"}})
			return
		}
		data, ok := f.kv[path]
		if !ok || f.notFound {
			writeJSON(w, 404, map[string]any{"errors": []string{}})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"data": data, "metadata": map[string]any{"version": 1}}})
	default:
		w.WriteHeader(405)
	}
}

// tokenValid: the token must be the current issued token AND its counter must be >= minToken (so raising
// minToken simulates the cached token expiring server-side).
func (f *fakeBao) tokenValid(tok string) bool {
	if tok == "" || !strings.HasPrefix(tok, "tok-") {
		return false
	}
	n := atoi(strings.TrimPrefix(tok, "tok-"))
	return n >= f.minToken && tok == f.lastToken
}

// appRoleClient wires an AppRole client (role_id/secret_id via env: SecretRefs) against the fake.
func appRoleClient(t *testing.T, base string) *vault.Client {
	t.Helper()
	t.Setenv("TG_TEST_ROLE_ID", "test-role-id")
	t.Setenv("TG_TEST_SECRET_ID", "test-secret-id")
	c, err := vault.New(vault.Config{
		BaseURL: base,
		Auth: vault.AppRole{
			RoleIDRef:   config.SecretRef("env:TG_TEST_ROLE_ID"),
			SecretIDRef: config.SecretRef("env:TG_TEST_SECRET_ID"),
		},
		HTTPClient: base2Client(base),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// base2Client returns an http.Client (the httptest default is fine; kept explicit for clarity).
func base2Client(string) vault.Doer { return http.DefaultClient }

func TestReadKVSuccess(t *testing.T) {
	srv := newFakeBao().server(t)
	c := appRoleClient(t, srv.URL)
	data, err := c.ReadKV(context.Background(), "secret/data/hosts/hostA")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if data["ssh_key"] != "PRIVATE-KEY-A" || data["user"] != "vaultuser" {
		t.Fatalf("unexpected data: %v", data)
	}
}

func TestResolveRefThroughSecretRef(t *testing.T) {
	srv := newFakeBao().server(t)
	c := appRoleClient(t, srv.URL)
	vault.RegisterResolver(c)
	t.Cleanup(func() { vault.RegisterResolver(nil) })

	// A Bundle SecretRef resolves through core/config's scheme registry (the same path Bundle.Resolve uses).
	got, err := config.SecretRef("vault:secret/data/hosts/hostA#ssh_key").Resolve()
	if err != nil {
		t.Fatalf("resolve vault ref: %v", err)
	}
	if got != "PRIVATE-KEY-A" {
		t.Fatalf("resolved %q, want PRIVATE-KEY-A", got)
	}
}

func TestResolveRefFailClosed(t *testing.T) {
	t.Run("denied-403", func(t *testing.T) {
		f := newFakeBao()
		f.denyReads = true
		c := appRoleClient(t, f.server(t).URL)
		if _, err := c.ResolveRef("vault:secret/data/hosts/hostA#ssh_key"); err == nil {
			t.Fatal("denied read must fail closed, got nil error")
		}
	})
	t.Run("not-found-404", func(t *testing.T) {
		c := appRoleClient(t, newFakeBao().server(t).URL)
		if _, err := c.ResolveRef("vault:secret/data/hosts/absent#ssh_key"); err == nil {
			t.Fatal("missing path must fail closed")
		}
	})
	t.Run("missing-field", func(t *testing.T) {
		c := appRoleClient(t, newFakeBao().server(t).URL)
		if _, err := c.ResolveRef("vault:secret/data/hosts/hostB#api_token"); err == nil {
			t.Fatal("missing field must fail closed")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := newFakeBao().server(t)
		addr := srv.URL
		srv.Close() // now unreachable
		c := appRoleClient(t, addr)
		if _, err := c.ResolveRef("vault:secret/data/hosts/hostA#ssh_key"); err == nil {
			t.Fatal("unreachable backend must fail closed")
		}
	})
	t.Run("malformed-ref", func(t *testing.T) {
		c := appRoleClient(t, newFakeBao().server(t).URL)
		for _, ref := range []string{"vault:secret/data/hosts/hostA", "vault:", "vault:#field"} {
			if _, err := c.ResolveRef(ref); err == nil {
				t.Fatalf("malformed ref %q must fail closed", ref)
			}
		}
	})
}

func TestExpiredTokenRelogin(t *testing.T) {
	f := newFakeBao()
	c := appRoleClient(t, f.server(t).URL)

	// first read logs in (tok-1) and succeeds.
	if _, err := c.ReadKV(context.Background(), "secret/data/hosts/hostA"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	f.mu.Lock()
	loginsAfterFirst := f.logins
	f.minToken = 2 // simulate tok-1 expiring server-side → the cached token now yields 403
	f.mu.Unlock()

	// next read presents the stale token, gets 403, re-logins (tok-2), and succeeds.
	if _, err := c.ReadKV(context.Background(), "secret/data/hosts/hostA"); err != nil {
		t.Fatalf("read after expiry should recover via re-login: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins != loginsAfterFirst+1 {
		t.Fatalf("expected exactly one re-login, logins went %d -> %d", loginsAfterFirst, f.logins)
	}
}

func TestPermanentDenialFailsClosed(t *testing.T) {
	f := newFakeBao()
	f.denyReads = true
	c := appRoleClient(t, f.server(t).URL)
	if _, err := c.ReadKV(context.Background(), "secret/data/hosts/hostA"); err == nil {
		t.Fatal("a permanently-denied read must fail closed even after re-login")
	}
}

func TestSyncListReadResolvable(t *testing.T) {
	srv := newFakeBao().server(t)
	c := appRoleClient(t, srv.URL)
	vault.RegisterResolver(c)
	t.Cleanup(func() { vault.RegisterResolver(nil) })

	src, err := vault.NewSource(vault.SourceConfig{ID: "openbao01", Client: c, Mount: "secret", Prefix: "hosts"})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	if src.Plane() != credential.PlaneMachine || src.ID() != "openbao01" {
		t.Fatalf("source metadata wrong: plane=%s id=%s", src.Plane(), src.ID())
	}

	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// two flat hosts (the "sub/" folder is skipped).
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	byHost := map[string]credential.SourceEntry{}
	for _, e := range entries {
		byHost[e.Selector.Pattern] = e
	}
	a, ok := byHost["hostA"]
	if !ok {
		t.Fatalf("hostA not synced: %v", byHost)
	}
	// NO plaintext secret stored: the bundle carries a vault: REFERENCE, never the key material.
	ref := string(a.Bundle.SSHKeyRef())
	if !strings.HasPrefix(ref, "vault:") {
		t.Fatalf("bundle secret is not a vault ref: %q", ref)
	}
	if strings.Contains(ref, "PRIVATE-KEY") {
		t.Fatalf("plaintext secret leaked into the bundle ref: %q", ref)
	}
	if a.Bundle.User() != "vaultuser" || a.Bundle.Port() != 2222 {
		t.Fatalf("non-secret metadata wrong: user=%s port=%d", a.Bundle.User(), a.Bundle.Port())
	}
	// the ref dereferences to the real value at use time.
	res, err := a.Bundle.Resolve()
	if err != nil {
		t.Fatalf("resolve synced bundle: %v", err)
	}
	if res.SSHKey != "PRIVATE-KEY-A" {
		t.Fatalf("synced ref resolved to %q, want PRIVATE-KEY-A", res.SSHKey)
	}
}

func TestSyncFailClosedOnListError(t *testing.T) {
	f := newFakeBao()
	f.denyReads = true // list still works (token valid) but reads 403 → sync aborts
	c := appRoleClient(t, f.server(t).URL)
	src, _ := vault.NewSource(vault.SourceConfig{ID: "x", Client: c, Mount: "secret", Prefix: "hosts"})
	if _, err := src.Sync(context.Background()); err == nil {
		t.Fatal("sync must fail closed when an entry read is denied")
	}
}

// TestSyncEmptyNamespaceIsOK proves the live openbao01 regression: an EMPTY KV v2 namespace under a valid
// mount returns 404 on the metadata LIST, and that must be reported as ok-with-0-entries (SyncEngine records
// outcome=ok, coverage honestly empty), NOT a failed sync. The fake returns 404 for any un-populated LIST
// path (see handle: no `list` entry → 404), so a mount/prefix with no keys reproduces the field condition.
func TestSyncEmptyNamespaceIsOK(t *testing.T) {
	f := newFakeBao() // "secret/metadata/tg" has no keys → the fake LISTs it as 404 (empty namespace)
	c := appRoleClient(t, f.server(t).URL)
	src, err := vault.NewSource(vault.SourceConfig{ID: "openbao", Client: c, Mount: "secret", Prefix: "tg"})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("empty namespace must sync ok (0 entries), got error: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty namespace must yield 0 entries, got %d", len(entries))
	}
}

// TestSyncEmptyPrefixIsDisabled proves an UNCONFIGURED (empty) prefix disables the source entirely: it
// returns 0 entries with no error WITHOUT listing the mount root, so a shared substrate's other tenants or
// flat non-bundle secrets can never choke it. We point at a CLOSED server — a real LIST would fail closed
// (see TestSyncFailClosedOnUnreachableList) — so 0/nil here proves the guard short-circuits before dialing.
func TestSyncEmptyPrefixIsDisabled(t *testing.T) {
	srv := newFakeBao().server(t)
	addr := srv.URL
	srv.Close() // any backend call would now error; the guard must return before touching the network
	c := appRoleClient(t, addr)
	src, err := vault.NewSource(vault.SourceConfig{ID: "openbao", Client: c, Mount: "secret", Prefix: ""})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	entries, err := src.Sync(context.Background())
	if err != nil {
		t.Fatalf("empty-prefix source must be a no-op (0 entries, nil err), got: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty-prefix source must yield 0 entries (disabled), got %d", len(entries))
	}
}

// TestSyncFailClosedOnDeniedList proves a DENIED list (401/403) still fails closed — a not-found is
// ok-empty, but a denial is a real error and must NOT be swallowed as an empty namespace.
func TestSyncFailClosedOnDeniedList(t *testing.T) {
	f := newFakeBao()
	f.minToken = 99 // every token the client presents is rejected (403) → the LIST itself is denied
	c := appRoleClient(t, f.server(t).URL)
	src, _ := vault.NewSource(vault.SourceConfig{ID: "openbao", Client: c, Mount: "secret", Prefix: "hosts"})
	if _, err := src.Sync(context.Background()); err == nil {
		t.Fatal("a denied LIST must fail closed, not be treated as an empty namespace")
	}
}

// TestSyncFailClosedOnUnreachableList proves an unreachable backend on the LIST fails closed (never ok-empty).
func TestSyncFailClosedOnUnreachableList(t *testing.T) {
	srv := newFakeBao().server(t)
	addr := srv.URL
	srv.Close() // now unreachable
	c := appRoleClient(t, addr)
	src, _ := vault.NewSource(vault.SourceConfig{ID: "openbao", Client: c, Mount: "secret", Prefix: "hosts"})
	if _, err := src.Sync(context.Background()); err == nil {
		t.Fatal("an unreachable backend on LIST must fail closed, not be treated as an empty namespace")
	}
}

func TestJWTAndTokenAuth(t *testing.T) {
	// JWT auth.
	srv := newFakeBao().server(t)
	t.Setenv("TG_TEST_JWT", "test-jwt-value")
	jc, err := vault.New(vault.Config{
		BaseURL:    srv.URL,
		Auth:       vault.JWT{JWTRef: config.SecretRef("env:TG_TEST_JWT"), Role: "tg-worker"},
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("jwt client: %v", err)
	}
	if _, err := jc.ReadKV(context.Background(), "secret/data/hosts/hostA"); err != nil {
		t.Fatalf("jwt read: %v", err)
	}

	// Static-token auth: the token is presented directly. The fake only accepts issued "tok-N", so a static
	// token yields 403 — proving direct-token auth is wired AND that a bad token fails closed (no fall-open).
	t.Setenv("TG_TEST_TOKEN", "tok-static")
	tc, err := vault.New(vault.Config{
		BaseURL:    srv.URL,
		Auth:       vault.Token{TokenRef: config.SecretRef("env:TG_TEST_TOKEN")},
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("token client: %v", err)
	}
	if _, err := tc.ReadKV(context.Background(), "secret/data/hosts/hostA"); err == nil {
		t.Fatal("a token the backend rejects must fail closed")
	}
}

func TestConstructorFailClosed(t *testing.T) {
	if _, err := vault.New(vault.Config{Auth: vault.Token{TokenRef: "env:X"}}); err == nil {
		t.Fatal("missing base URL must error")
	}
	if _, err := vault.New(vault.Config{BaseURL: "https://x:8200"}); err == nil {
		t.Fatal("missing authenticator must error")
	}
}

// TestLiveHealth is a BEST-EFFORT env-gated live check against the real openbao01. It is skipped unless
// TG_OPENBAO_LIVE_ADDR is set (e.g. "https://dc1k8s-openbao01.example.net:8200"). It asserts
// only the unauthenticated health endpoint (TG may not hold real AppRole creds in this environment).
func TestLiveHealth(t *testing.T) {
	addr := os.Getenv("TG_OPENBAO_LIVE_ADDR")
	if addr == "" {
		t.Skip("set TG_OPENBAO_LIVE_ADDR to run the live openbao01 health check")
	}
	c, err := vault.New(vault.Config{
		BaseURL:    addr,
		Auth:       vault.Token{TokenRef: config.SecretRef("env:TG_OPENBAO_LIVE_TOKEN")},
		CACertPath: os.Getenv("BAO_CACERT"),
	})
	if err != nil {
		t.Fatalf("live client: %v", err)
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("live openbao01 health: %v", err)
	}
}

// ---- tiny helpers (no strconv import churn in the fake) ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return -1
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}
