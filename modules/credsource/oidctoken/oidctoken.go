// Package oidctoken is the native-Go OIDC machine-plane token MINTER — the oidc: SecretRef scheme
// (spec/016 T-016-14, REQ-1619, epic TG-107). It is a SIBLING of the vault:/bao: connectors
// (modules/credsource/vault, REQ-1613): where those DEREFERENCE a stored KV v2 secret, this one MINTS a
// short-lived Bearer access token on demand via the OAuth2 client-credentials grant (RFC 6749 §4.4)
// against an OIDC provider's token endpoint, and caches it by its advertised expires_in.
//
// It belongs to the machine → host / machine → service credential plane (credential.PlaneMachine), NOT the
// human → console approver plane. REQ-1614 binds the LDAP/OIDC *user/group* sync to the human plane
// (approvers feeding spec/015 approve_by); that is a different capability (T-016-10, modules/credsource/
// oidc). A client-credentials token minter authenticates TG-as-a-service to a machine target and therefore
// lives on the machine plane alongside vault:/bao: — REQ-1619 binds it there explicitly.
//
// A Bundle SecretRef like "oidc:netbox#openid" is dereferenced at USE TIME through
// core/config.RegisterSchemeResolver: on Resolve the Minter POSTs
// grant_type=client_credentials (+scope) to the configured token endpoint, authenticates the client
// (client_secret_post by default, or HTTP Basic / client_secret_basic per RFC 6749 §2.3.1), reads the
// access_token, and returns it. It FAILS CLOSED — an unreachable endpoint, a denied grant (4xx), a server
// error (5xx), a TLS-verification failure, or a response with no access_token → an error, never a blank or
// default token. The client_id and client_secret are core/config.SecretRefs (env:/file:/store:), never
// literals (INV-13); optional provider-specific extra form fields (e.g. authentik's service-account
// username/app-password variant) are ALSO sealed SecretRefs.
//
// The client is DISTROLESS-SAFE: net/http + crypto/tls with an optional pinned CA cert (the demo authentik
// presents a self-signed cert); NO subprocess, no vendor CLI (INV-02). TLS is always VERIFIED — there is no
// InsecureSkipVerify anywhere in this package; an untrusted server cert fails closed.
//
// Grounded in the OAuth2 docs read before build: RFC 6749 §4.4 (client-credentials request:
// application/x-www-form-urlencoded, grant_type=client_credentials + optional scope; §2.3.1 client auth) and
// the authentik client_credentials docs (goauthentik.io: POST /application/o/token/ with
// grant_type=client_credentials + client_id + client_secret + scope; authentik auto-provisions the
// ak-<provider>-client_credentials service account; the response is a signed-JWT access_token with
// token_type + expires_in).
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1619), TG-107.
package oidctoken

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "oidc-token"

// Scheme is the SecretRef scheme this connector mints for.
const Scheme = "oidc"

// defaultTimeout bounds a single token-mint round-trip so a hung provider fails closed promptly rather than
// blocking the actuation-critical Resolve indefinitely.
const defaultTimeout = 15 * time.Second

// defaultSkew re-mints slightly before a token actually expires, so an in-flight request never races expiry
// (mirrors the vault connector's leaseSkew).
const defaultSkew = 30 * time.Second

// Doer is the minimal HTTP contract; *http.Client satisfies it, and the oracle injects a fake token endpoint.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// AuthStyle selects how the client authenticates to the token endpoint (RFC 6749 §2.3.1).
type AuthStyle int

const (
	// AuthStylePost sends client_id + client_secret as form body parameters (client_secret_post). This is
	// the style authentik's client_credentials docs use, so it is the default.
	AuthStylePost AuthStyle = iota
	// AuthStyleBasic sends the client_id:client_secret pair as an HTTP Basic Authorization header
	// (client_secret_basic, the RFC-preferred method). No client credentials appear in the body.
	AuthStyleBasic
)

// FormField is an extra provider-specific form parameter whose value is a sealed SecretRef (never a literal)
// — e.g. authentik's alternative username/app-password client-credentials variant. Optional.
type FormField struct {
	Key   string
	Value config.SecretRef
}

// Minter mints and caches short-lived Bearer tokens via the OAuth2 client-credentials grant. It is safe for
// concurrent use.
type Minter struct {
	tokenURL     string
	clientIDRef  config.SecretRef
	clientSecret config.SecretRef
	scope        string      // default space-separated scope; a ref may override it after '#'
	audience     string      // optional RFC 8693 / provider audience parameter
	authStyle    AuthStyle   // client_secret_post (default) or client_secret_basic
	extra        []FormField // optional provider-specific sealed form fields
	http         Doer
	skew         time.Duration
	now          func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken // keyed by effective scope
}

// cachedToken is a minted token and the instant at which it must be re-minted.
type cachedToken struct {
	token  string
	expiry time.Time // when the cached token becomes unusable (skew already applied by the reader)
}

// Config constructs a Minter. TokenURL, ClientIDRef, and ClientSecretRef are required. CACertPath, if set,
// is read into the TLS root pool for the default HTTP client (the demo authentik presents a self-signed
// cert). HTTPClient, if set, overrides transport entirely (the oracle injects a fake). Skew and Now default
// to defaultSkew and time.Now.
type Config struct {
	TokenURL        string
	ClientIDRef     config.SecretRef
	ClientSecretRef config.SecretRef
	Scope           string
	Audience        string
	AuthStyle       AuthStyle
	Extra           []FormField
	CACertPath      string
	HTTPClient      Doer
	Skew            time.Duration
	Now             func() time.Time
}

// New builds a Minter from a Config. It fails closed if the token URL or either client-credential reference
// is missing, or if a configured CA cert cannot be loaded.
func New(cfg Config) (*Minter, error) {
	if strings.TrimSpace(cfg.TokenURL) == "" {
		return nil, fmt.Errorf("oidctoken: token endpoint URL is required")
	}
	if strings.TrimSpace(string(cfg.ClientIDRef)) == "" {
		return nil, fmt.Errorf("oidctoken: client_id SecretRef is required")
	}
	if strings.TrimSpace(string(cfg.ClientSecretRef)) == "" {
		return nil, fmt.Errorf("oidctoken: client_secret SecretRef is required")
	}
	m := &Minter{
		tokenURL:     cfg.TokenURL,
		clientIDRef:  cfg.ClientIDRef,
		clientSecret: cfg.ClientSecretRef,
		scope:        strings.TrimSpace(cfg.Scope),
		audience:     strings.TrimSpace(cfg.Audience),
		authStyle:    cfg.AuthStyle,
		extra:        cfg.Extra,
		http:         cfg.HTTPClient,
		skew:         cfg.Skew,
		now:          cfg.Now,
		cache:        map[string]cachedToken{},
	}
	if m.skew <= 0 {
		m.skew = defaultSkew
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("oidctoken: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("oidctoken: CA cert %q contains no valid certificate", cfg.CACertPath)
			}
			// TLS is always verified against the pinned CA — never InsecureSkipVerify (fail closed on a bad cert).
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		m.http = hc
	}
	return m, nil
}

// tokenResponse is the RFC 6749 §5.1 successful access-token response (only the fields this connector uses).
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// Mint returns a valid Bearer access token for the effective scope (scope override, else the configured
// default), minting a fresh one via client-credentials when none is cached or the cached one is within skew
// of expiry. It fails closed — a mint error returns an error and NEVER a cached, blank, or default token.
func (m *Minter) Mint(ctx context.Context, scope string) (string, error) {
	eff := m.scope
	if s := strings.TrimSpace(scope); s != "" {
		eff = s
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if ct, ok := m.cache[eff]; ok && m.now().Before(ct.expiry.Add(-m.skew)) {
		return ct.token, nil
	}

	tok, ttl, err := m.mintLocked(ctx, eff)
	if err != nil {
		// Fail closed: drop any stale cache entry for this scope and surface the error — never fall open.
		delete(m.cache, eff)
		return "", err
	}
	exp := m.now().Add(ttl)
	if ttl <= 0 {
		// A provider that advertises no (or non-positive) expires_in gets no caching: use the token for this
		// call, but force a fresh mint next time rather than trusting an unknown lifetime.
		exp = m.now()
	}
	m.cache[eff] = cachedToken{token: tok, expiry: exp}
	return tok, nil
}

// mintLocked performs one client-credentials token request. It resolves the client credentials from their
// SecretRefs at call time (INV-13) and returns the token plus its advertised lifetime.
func (m *Minter) mintLocked(ctx context.Context, scope string) (string, time.Duration, error) {
	clientID, err := m.clientIDRef.Resolve()
	if err != nil {
		return "", 0, fmt.Errorf("oidctoken: resolve client_id: %w", err)
	}
	clientSecret, err := m.clientSecret.Resolve()
	if err != nil {
		return "", 0, fmt.Errorf("oidctoken: resolve client_secret: %w", err)
	}
	if clientID == "" || clientSecret == "" {
		return "", 0, fmt.Errorf("oidctoken: resolved client_id/client_secret is empty (fail closed)")
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if scope != "" {
		form.Set("scope", scope)
	}
	if m.audience != "" {
		form.Set("audience", m.audience)
	}
	basic := false
	switch m.authStyle {
	case AuthStyleBasic:
		basic = true
	default: // AuthStylePost
		form.Set("client_id", clientID)
		form.Set("client_secret", clientSecret)
	}
	for _, f := range m.extra {
		v, err := f.Value.Resolve()
		if err != nil {
			return "", 0, fmt.Errorf("oidctoken: resolve extra field %q: %w", f.Key, err)
		}
		form.Set(f.Key, v)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		cred := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))
		req.Header.Set("Authorization", "Basic "+cred)
	}

	resp, err := m.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("oidctoken: token request to %s: %w", redactURL(m.tokenURL), err) // unreachable/TLS → fail closed
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("oidctoken: token endpoint %s returned status %d: %s", redactURL(m.tokenURL), resp.StatusCode, oauthErr(body))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("oidctoken: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, fmt.Errorf("oidctoken: token endpoint %s returned no access_token (fail closed)", redactURL(m.tokenURL))
	}
	return tr.AccessToken, time.Duration(tr.ExpiresIn) * time.Second, nil
}

// ResolveRef dereferences an oidc: SecretRef to a freshly-minted-or-cached Bearer token. ref is the FULL
// reference "oidc:<name>[#<scope>]": <name> is a required, non-empty logical label for the token (recorded,
// not secret) and <scope>, if present, overrides the configured default scope for this mint. It returns the
// RAW access_token value (the caller prefixes "Bearer "). It fails closed — a malformed ref, an unreachable
// or denied endpoint, or a response with no token all return an error, never a blank value.
func (m *Minter) ResolveRef(ref string) (string, error) {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok || scheme != Scheme || rest == "" {
		return "", fmt.Errorf("oidctoken: malformed secret reference %q (want oidc:<name>[#<scope>])", ref)
	}
	name, scope, _ := strings.Cut(rest, "#")
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("oidctoken: reference %q must be oidc:<name>[#<scope>] with a non-empty name", ref)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	return m.Mint(ctx, scope)
}

// RegisterResolver wires this Minter's oidc: scheme into core/config so a Bundle SecretRef resolves through
// it at use time (REQ-1619). Composition-time only. Pass a nil minter to unregister (fail closed).
func RegisterResolver(m *Minter) {
	if m == nil {
		config.RegisterSchemeResolver(Scheme, nil)
		return
	}
	config.RegisterSchemeResolver(Scheme, m.ResolveRef)
}

// ---- small helpers ----------------------------------------------------------------------------------

// oauthErr extracts the RFC 6749 §5.2 {"error":...,"error_description":...} body into a short, secret-free
// message; it falls back to a truncated raw body.
func oauthErr(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		if e.Description != "" {
			return e.Error + ": " + e.Description
		}
		return e.Error
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// redactURL strips any userinfo and query from a URL so an error message never echoes a credential.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<token-endpoint>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
