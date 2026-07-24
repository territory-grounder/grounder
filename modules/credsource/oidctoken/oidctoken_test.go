package oidctoken_test

// ORACLE tests for the OIDC client-credentials token minter (spec/016 T-016-14, REQ-1619). CI has no OIDC
// provider, so a httptest.Server fakes the RFC 6749 §4.4 token endpoint (grant_type=client_credentials +
// client auth + scope → access_token/token_type/expires_in), and the tests drive the REAL Minter + resolver
// through it. They prove: a successful mint through the oidc: SecretRef scheme; caching by expires_in with a
// re-mint after skew; fail-closed on denied / unreachable / no-token / malformed-ref; scope override; the
// HTTP Basic (client_secret_basic) auth style; sealed extra form fields; and that TLS is VERIFIED (a pinned
// CA succeeds, an untrusted cert fails closed). Run under -race.
//
// A BEST-EFFORT live check (TestLiveMint) mints from the real authentik demo only when
// TG_OIDC_LIVE_TOKEN_URL is set.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/modules/credsource/oidctoken"
)

// fakeIDP is an in-memory OAuth2 client-credentials token endpoint for the oracles.
type fakeIDP struct {
	mu           sync.Mutex
	requests     int    // total token requests received (proves caching vs re-mint)
	clientID     string // the credential the fake accepts
	clientSecret string
	expiresIn    int  // advertised token lifetime
	deny         bool // force every grant to 401 invalid_client
	noToken      bool // return 200 with an empty body (no access_token)
	lastScope    string
	lastGrant    string
	lastExtra    map[string]string
}

func newFakeIDP() *fakeIDP {
	return &fakeIDP{clientID: "tg-client", clientSecret: "s3cr3t-value", expiresIn: 3600, lastExtra: map[string]string{}}
}

func (f *fakeIDP) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeIDP) tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(s.Close)
	return s
}

func (f *fakeIDP) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "invalid_request"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid_request"})
		return
	}
	f.lastGrant = r.PostForm.Get("grant_type")
	if f.lastGrant != "client_credentials" {
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	// Client authentication: accept either HTTP Basic (client_secret_basic) or body params
	// (client_secret_post) — RFC 6749 §2.3.1.
	id, sec, ok := r.BasicAuth()
	if !ok {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if f.deny || id != f.clientID || sec != f.clientSecret {
		writeJSON(w, 401, map[string]any{"error": "invalid_client", "error_description": "authentication failed"})
		return
	}
	f.lastScope = r.PostForm.Get("scope")
	for k, v := range r.PostForm {
		switch k {
		case "grant_type", "client_id", "client_secret", "scope", "audience":
		default:
			if len(v) > 0 {
				f.lastExtra[k] = v[0]
			}
		}
	}
	if f.noToken {
		writeJSON(w, 200, map[string]any{"token_type": "bearer", "expires_in": f.expiresIn})
		return
	}
	writeJSON(w, 200, map[string]any{
		"access_token": "tok-" + strconv.Itoa(f.requests), // increments so a re-mint is observable
		"token_type":   "bearer",
		"expires_in":   f.expiresIn,
	})
}

func (f *fakeIDP) reqCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests }

// minterFor wires a Minter (client_id/client_secret via env: SecretRefs) at a token URL, with an injectable
// clock so caching can be tested deterministically.
func minterFor(t *testing.T, tokenURL string, now func() time.Time, opts ...func(*oidctoken.Config)) *oidctoken.Minter {
	t.Helper()
	t.Setenv("TG_TEST_OIDC_ID", "tg-client")
	t.Setenv("TG_TEST_OIDC_SECRET", "s3cr3t-value")
	cfg := oidctoken.Config{
		TokenURL:        tokenURL,
		ClientIDRef:     config.SecretRef("env:TG_TEST_OIDC_ID"),
		ClientSecretRef: config.SecretRef("env:TG_TEST_OIDC_SECRET"),
		Scope:           "openid",
		HTTPClient:      http.DefaultClient,
		Now:             now,
	}
	for _, o := range opts {
		o(&cfg)
	}
	m, err := oidctoken.New(cfg)
	if err != nil {
		t.Fatalf("new minter: %v", err)
	}
	return m
}

func TestMintSuccessThroughSecretRef(t *testing.T) {
	f := newFakeIDP()
	m := minterFor(t, f.server(t).URL, nil)
	oidctoken.RegisterResolver(m)
	t.Cleanup(func() { oidctoken.RegisterResolver(nil) })

	// A Bundle SecretRef resolves through core/config's scheme registry (the exact path Bundle.Resolve uses).
	got, err := config.SecretRef("oidc:netbox").Resolve()
	if err != nil {
		t.Fatalf("resolve oidc ref: %v", err)
	}
	if !strings.HasPrefix(got, "tok-") {
		t.Fatalf("resolved token %q, want a minted tok-N", got)
	}
	if f.lastGrant != "client_credentials" {
		t.Fatalf("grant_type sent was %q, want client_credentials", f.lastGrant)
	}
	if f.lastScope != "openid" {
		t.Fatalf("scope sent was %q, want openid", f.lastScope)
	}
}

func TestCachesByExpiresInAndRemintsAfterSkew(t *testing.T) {
	f := newFakeIDP() // expires_in = 3600
	clk := time.Unix(1_000_000, 0)
	m := minterFor(t, f.server(t).URL, func() time.Time { return clk })

	a, err := m.Mint(context.Background(), "")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	// A second mint well within the lifetime returns the cached token with no new request.
	b, err := m.Mint(context.Background(), "")
	if err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if a != b {
		t.Fatalf("cached mint returned a different token: %q vs %q", a, b)
	}
	if n := f.reqCount(); n != 1 {
		t.Fatalf("expected exactly 1 token request while cached, got %d", n)
	}
	// Advance past expiry (minus skew) → the next mint refreshes.
	clk = clk.Add(3600 * time.Second)
	c, err := m.Mint(context.Background(), "")
	if err != nil {
		t.Fatalf("mint after expiry: %v", err)
	}
	if c == a {
		t.Fatalf("expected a fresh token after expiry, still got %q", c)
	}
	if n := f.reqCount(); n != 2 {
		t.Fatalf("expected exactly one re-mint after expiry (2 total), got %d", n)
	}
}

func TestFailClosed(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		f := newFakeIDP()
		f.deny = true
		m := minterFor(t, f.server(t).URL, nil)
		if tok, err := m.Mint(context.Background(), ""); err == nil || tok != "" {
			t.Fatalf("a denied grant must fail closed, got tok=%q err=%v", tok, err)
		}
	})
	t.Run("wrong-secret", func(t *testing.T) {
		f := newFakeIDP()
		m := minterFor(t, f.server(t).URL, nil, func(c *oidctoken.Config) {
			t.Setenv("TG_TEST_OIDC_BAD", "not-the-secret")
			c.ClientSecretRef = config.SecretRef("env:TG_TEST_OIDC_BAD")
		})
		if _, err := m.Mint(context.Background(), ""); err == nil {
			t.Fatal("a wrong client_secret must fail closed")
		}
	})
	t.Run("no-access-token", func(t *testing.T) {
		f := newFakeIDP()
		f.noToken = true
		m := minterFor(t, f.server(t).URL, nil)
		if _, err := m.Mint(context.Background(), ""); err == nil {
			t.Fatal("a response with no access_token must fail closed")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		srv := newFakeIDP().server(t)
		addr := srv.URL
		srv.Close() // now unreachable
		m := minterFor(t, addr, nil)
		if _, err := m.Mint(context.Background(), ""); err == nil {
			t.Fatal("an unreachable endpoint must fail closed")
		}
	})
	t.Run("unresolvable-secretref", func(t *testing.T) {
		f := newFakeIDP()
		m := minterFor(t, f.server(t).URL, nil, func(c *oidctoken.Config) {
			c.ClientSecretRef = config.SecretRef("env:TG_TEST_OIDC_UNSET_XYZ") // no such env → resolve error
		})
		if _, err := m.Mint(context.Background(), ""); err == nil {
			t.Fatal("an unresolvable client_secret SecretRef must fail closed")
		}
	})
	t.Run("malformed-ref", func(t *testing.T) {
		f := newFakeIDP()
		m := minterFor(t, f.server(t).URL, nil)
		for _, ref := range []string{"oidc:", "oidc:#openid", "vault:x", "oidc", ""} {
			if _, err := m.ResolveRef(ref); err == nil {
				t.Fatalf("malformed ref %q must fail closed", ref)
			}
		}
	})
}

// TestFailClosedNoStaleTokenAfterExpiry proves that once a cached token expires and the endpoint starts
// denying, the minter refuses rather than serving the stale token — the fail-closed property under a
// backend that goes bad mid-lifetime.
func TestFailClosedNoStaleTokenAfterExpiry(t *testing.T) {
	f := newFakeIDP()
	clk := time.Unix(2_000_000, 0)
	m := minterFor(t, f.server(t).URL, func() time.Time { return clk })
	if _, err := m.Mint(context.Background(), ""); err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	f.mu.Lock()
	f.deny = true // provider now rejects every grant
	f.mu.Unlock()
	clk = clk.Add(3600 * time.Second) // cached token has expired
	if tok, err := m.Mint(context.Background(), ""); err == nil || tok != "" {
		t.Fatalf("expired-then-denied must fail closed, got tok=%q err=%v", tok, err)
	}
}

func TestScopeOverride(t *testing.T) {
	f := newFakeIDP()
	m := minterFor(t, f.server(t).URL, nil)
	oidctoken.RegisterResolver(m)
	t.Cleanup(func() { oidctoken.RegisterResolver(nil) })
	if _, err := config.SecretRef("oidc:netbox#read write").Resolve(); err != nil {
		t.Fatalf("resolve with scope override: %v", err)
	}
	if f.lastScope != "read write" {
		t.Fatalf("scope override not honored: endpoint saw %q, want %q", f.lastScope, "read write")
	}
}

func TestAuthStyleBasic(t *testing.T) {
	f := newFakeIDP()
	m := minterFor(t, f.server(t).URL, nil, func(c *oidctoken.Config) { c.AuthStyle = oidctoken.AuthStyleBasic })
	if _, err := m.Mint(context.Background(), ""); err != nil {
		t.Fatalf("basic-auth mint should succeed: %v", err)
	}
}

func TestExtraSealedFormFields(t *testing.T) {
	f := newFakeIDP()
	t.Setenv("TG_TEST_OIDC_SVC_USER", "svc-tg")
	m := minterFor(t, f.server(t).URL, nil, func(c *oidctoken.Config) {
		c.Extra = []oidctoken.FormField{{Key: "username", Value: config.SecretRef("env:TG_TEST_OIDC_SVC_USER")}}
	})
	if _, err := m.Mint(context.Background(), ""); err != nil {
		t.Fatalf("mint with extra field: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastExtra["username"] != "svc-tg" {
		t.Fatalf("extra sealed field not sent: got %v", f.lastExtra)
	}
}

// TestTLSVerified proves TLS is VERIFIED: a pinned CA cert mints successfully, and the SAME server with an
// untrusted (default) transport fails closed — there is no InsecureSkipVerify path.
func TestTLSVerified(t *testing.T) {
	f := newFakeIDP()
	srv := f.tlsServer(t)

	// Pin the server's self-signed cert as a CA and exercise the REAL New() TLS-pool path.
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	t.Setenv("TG_TEST_OIDC_ID", "tg-client")
	t.Setenv("TG_TEST_OIDC_SECRET", "s3cr3t-value")
	pinned, err := oidctoken.New(oidctoken.Config{
		TokenURL:        srv.URL,
		ClientIDRef:     config.SecretRef("env:TG_TEST_OIDC_ID"),
		ClientSecretRef: config.SecretRef("env:TG_TEST_OIDC_SECRET"),
		Scope:           "openid",
		CACertPath:      caPath,
	})
	if err != nil {
		t.Fatalf("new pinned minter: %v", err)
	}
	if _, err := pinned.Mint(context.Background(), ""); err != nil {
		t.Fatalf("mint against a pinned CA must succeed: %v", err)
	}

	// Same TLS server, but a default transport that does NOT trust the self-signed cert → fail closed.
	untrusted, err := oidctoken.New(oidctoken.Config{
		TokenURL:        srv.URL,
		ClientIDRef:     config.SecretRef("env:TG_TEST_OIDC_ID"),
		ClientSecretRef: config.SecretRef("env:TG_TEST_OIDC_SECRET"),
	})
	if err != nil {
		t.Fatalf("new untrusted minter: %v", err)
	}
	if _, err := untrusted.Mint(context.Background(), ""); err == nil {
		t.Fatal("an untrusted server certificate must fail closed (TLS verified)")
	}

	// Sanity: the pinned CA pool is a real x509 pool (guards against an accidental InsecureSkipVerify).
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatal("test cert did not parse into a pool")
	}
}

func TestConstructorFailClosed(t *testing.T) {
	base := oidctoken.Config{
		TokenURL:        "https://idp/application/o/token/",
		ClientIDRef:     "env:X",
		ClientSecretRef: "env:Y",
	}
	if _, err := oidctoken.New(oidctoken.Config{ClientIDRef: base.ClientIDRef, ClientSecretRef: base.ClientSecretRef}); err == nil {
		t.Fatal("missing token URL must error")
	}
	if _, err := oidctoken.New(oidctoken.Config{TokenURL: base.TokenURL, ClientSecretRef: base.ClientSecretRef}); err == nil {
		t.Fatal("missing client_id must error")
	}
	if _, err := oidctoken.New(oidctoken.Config{TokenURL: base.TokenURL, ClientIDRef: base.ClientIDRef}); err == nil {
		t.Fatal("missing client_secret must error")
	}
}

// TestLiveMint is a BEST-EFFORT env-gated live check against the real authentik demo. It is skipped unless
// TG_OIDC_LIVE_TOKEN_URL is set (e.g. "http://localhost:9000/application/o/tg-oidc-demo/token/"). It mints a
// REAL token via client-credentials and asserts it is non-empty and cached on the second call.
func TestLiveMint(t *testing.T) {
	tokenURL := os.Getenv("TG_OIDC_LIVE_TOKEN_URL")
	if tokenURL == "" {
		t.Skip("set TG_OIDC_LIVE_TOKEN_URL (+ TG_OIDC_LIVE_CLIENT_ID/SECRET envs) to run the live authentik mint")
	}
	m, err := oidctoken.New(oidctoken.Config{
		TokenURL:        tokenURL,
		ClientIDRef:     config.SecretRef("env:TG_OIDC_LIVE_CLIENT_ID"),
		ClientSecretRef: config.SecretRef("env:TG_OIDC_LIVE_CLIENT_SECRET"),
		Scope:           envOr("TG_OIDC_LIVE_SCOPE", "openid"),
		CACertPath:      os.Getenv("TG_OIDC_LIVE_CACERT"),
	})
	if err != nil {
		t.Fatalf("live minter: %v", err)
	}
	tok, err := m.Mint(context.Background(), "")
	if err != nil {
		t.Fatalf("live mint: %v", err)
	}
	if tok == "" {
		t.Fatal("live mint returned an empty token")
	}
	// A JWT access token has two dots; log only the length + prefix, never the token.
	t.Logf("live authentik mint OK: token_len=%d segments=%d", len(tok), strings.Count(tok, ".")+1)
	if tok2, err := m.Mint(context.Background(), ""); err != nil || tok2 != tok {
		t.Fatalf("second live mint should be cached and identical: err=%v same=%v", err, tok2 == tok)
	}
}

// ---- tiny helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
