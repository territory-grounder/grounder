// Package vault is the native-Go OpenBao / HashiCorp Vault credential connector (spec/016 T-016-9,
// REQ-1613, TG-90). It provides two read-only capabilities against a KV v2 backend:
//
//  1. The vault: SecretRef scheme (RegisterResolver): a Bundle SecretRef like
//     "vault:secret/data/hosts/hostA#ssh_key" is dereferenced to a KV v2 value at USE TIME through
//     core/config.RegisterSchemeResolver. It FAILS CLOSED — an unreachable backend, a denied read (403),
//     a missing path/field (404), or an expired token → an error, never a blank or default secret.
//  2. A credential.CredentialSource (NewSource): Sync LISTs a configured KV v2 path prefix, reads each
//     entry, and maps it to a SourceEntry whose Bundle carries vault: SecretRefs (NOT raw secret values —
//     the resolver above dereferences them at use time, INV-13). Read-only pull.
//
// The client is DISTROLESS-SAFE: net/http + crypto/tls with an optional CA cert; NO subprocess, no vendor
// CLI (INV-02). Auth is pluggable — AppRole, JWT, or a static token — every auth secret supplied as a
// core/config.SecretRef (env:/file:/store:), never a literal (INV-13). The token is cached and re-login is
// triggered on a 403 / expiry (renewal-aware).
//
// Grounded in the OpenBao/Vault HTTP API docs (openbao.org/api-docs): KV v2 read
// GET /v1/{mount}/data/{path} → .data.data; KV v2 list LIST /v1/{mount}/metadata/{path} → .data.keys;
// AppRole POST /v1/auth/approle/login {role_id,secret_id}; JWT POST /v1/auth/jwt/login {jwt,role};
// each auth response carries .auth.client_token / .auth.lease_duration / .auth.renewable; the token travels
// in the X-Vault-Token header. OpenBao is API-compatible with Vault; the sibling openbao package registers
// the bao: scheme against this same client.
//
// Provenance: [O] INV-13/INV-05/INV-02, spec/016 (REQ-1613), TG-90.
package vault

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
	"github.com/territory-grounder/grounder/core/credential"
)

// SourceType is the vendor slug this connector serves.
const SourceType = "vault"

// Scheme is the SecretRef scheme this connector's client resolves.
const Scheme = "vault"

// defaultTimeout bounds a single read/list/login round-trip so a hung backend fails closed promptly rather
// than blocking the actuation-critical Resolve indefinitely.
const defaultTimeout = 15 * time.Second

// leaseSkew re-logins slightly before a lease actually expires, so an in-flight request never races expiry.
const leaseSkew = 30 * time.Second

// Doer is the minimal HTTP contract; *http.Client satisfies it, and the oracle injects a fake OpenBao.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Authenticator obtains a Vault token. AppRole/JWT/Token implementations return the token plus its lease so
// the client can cache and renew it. A static token has a zero lease (never auto-expires locally); a 403
// still forces a re-login, which for a static token returns the same token and correctly fails closed.
type Authenticator interface {
	// login performs the auth-method login against the client and returns the token, its lease duration
	// (0 = no lease), and whether it is renewable. It fails closed on any transport or denial error.
	login(ctx context.Context, c *Client) (token string, lease time.Duration, renewable bool, err error)
}

// Client is a read-only OpenBao/Vault KV v2 client with a cached, renewal-aware token.
type Client struct {
	baseURL string        // e.g. "https://openbao01:8200" (no trailing slash, no /v1)
	auth    Authenticator // pluggable AppRole/JWT/Token
	http    Doer

	mu       sync.Mutex // guards the cached token
	token    string
	expiry   time.Time // zero = no lease (static token); otherwise when the cached token must be refreshed
	hasToken bool
}

// Config constructs a Client. BaseURL is required; Auth is required. CACertPath, if set, is read into the
// TLS root pool for the default HTTP client (the estate's OpenBao presents a private-CA cert). HTTPClient,
// if set, overrides transport entirely (the oracle injects a fake).
type Config struct {
	BaseURL    string
	Auth       Authenticator
	CACertPath string
	HTTPClient Doer
	// TransportWrap decorates the transport this client builds for itself. It exists because of TG-415.
	//
	// THE PROBLEM IT SOLVES. When a CA path or cert auth is configured, New() must build its own
	// http.Transport to carry the TLS config — and a client with its own Transport does NOT go through
	// http.DefaultTransport, which is where the TG-160 egress meter installs itself. So every OpenBao call
	// from every TG process was invisible to the meter: not counted, not named on first sighting, not
	// blocked, whatever TG_EGRESS_MODE said. Measured on the grounder 2026-08-07: tg_egress_enforcing 1,
	// allowlist_rules 15, and requests_total 0 in the same second the boot log resolved four bao: refs.
	//
	// That is worse than an ordinary coverage gap, because OpenBao is the destination the grounder's whole
	// egress grant exists for — so its meter was enforcing over an empty traffic set, which reads on every
	// dashboard as a clean estate.
	//
	// WHY A FUNCTION FIELD RATHER THAN THE METER ITSELF. This package must not import core/egress; the
	// composition root owns that wiring, and a package-level meter here is the coupling core/egress was
	// extracted to avoid. A nil wrap is the unchanged behaviour, so every existing caller is unaffected.
	//
	// It is applied ONLY on the custom-transport path. Without a CA or cert auth the client leaves
	// Transport nil, which resolves to http.DefaultTransport at CALL time — already the meter.
	TransportWrap func(http.RoundTripper) http.RoundTripper
}

// New builds a Client from a Config. It fails closed if the base URL or authenticator is missing, or if a
// configured CA cert cannot be loaded.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("vault: base URL is required")
	}
	if cfg.Auth == nil {
		return nil, fmt.Errorf("vault: an authenticator (AppRole/JWT/Token) is required")
	}
	c := &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), auth: cfg.Auth, http: cfg.HTTPClient}
	if c.http == nil {
		hc := &http.Client{Timeout: defaultTimeout}
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		custom := false
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("vault: read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("vault: CA cert %q contains no valid certificate", cfg.CACertPath)
			}
			tlsCfg.RootCAs = pool
			custom = true
		}
		// mTLS client-certificate auth (spec/024): present the FreeIPA machine-identity cert in the TLS
		// handshake so OpenBao's cert/login endpoint can authenticate it. Loaded once; it rides only on the
		// login handshake — the subsequent authed KV reads use the returned X-Vault-Token.
		if ca, ok := cfg.Auth.(Cert); ok {
			crt, err := tls.LoadX509KeyPair(ca.CertPath, ca.KeyPath)
			if err != nil {
				return nil, fmt.Errorf("vault: load mTLS client cert/key: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{crt}
			custom = true
		}
		if custom {
			// TG-415: this assignment is what took OpenBao traffic off http.DefaultTransport and therefore
			// out of the egress meter's sight. The transport is still ours to build — the TLS config has to
			// live somewhere — but the composition root now gets to decorate it.
			var rt http.RoundTripper = &http.Transport{TLSClientConfig: tlsCfg}
			if cfg.TransportWrap != nil {
				rt = cfg.TransportWrap(rt)
			}
			hc.Transport = rt
		}
		c.http = hc
	}
	return c, nil
}

// ---- auth methods -----------------------------------------------------------------------------------

// AppRole authenticates with a role_id + secret_id (POST /v1/auth/approle/login). Both are SecretRefs — the
// role_id and secret_id are credentials, never literals (INV-13).
type AppRole struct {
	RoleIDRef   config.SecretRef
	SecretIDRef config.SecretRef
	MountPath   string // auth mount, default "approle"
}

func (a AppRole) login(ctx context.Context, c *Client) (string, time.Duration, bool, error) {
	roleID, err := a.RoleIDRef.Resolve()
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: resolve approle role_id: %w", err)
	}
	secretID, err := a.SecretIDRef.Resolve()
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: resolve approle secret_id: %w", err)
	}
	mount := orDefault(a.MountPath, "approle")
	return c.loginAt(ctx, "auth/"+mount+"/login", map[string]string{"role_id": roleID, "secret_id": secretID})
}

// WrappedAppRole is the SECRET-ZERO-free AppRole variant (spec/024 REQ-2407): instead of a durable
// secret_id on disk, a trusted orchestrator (AWX at deploy) response-wraps a single-use, short-TTL SecretID
// and delivers only the WRAPPING TOKEN to a memory-backed path (file:/run/tg/bao-wrap on tmpfs). At boot TG
// unwraps it (POST /v1/sys/wrapping/unwrap with the wrapping token) to obtain the SecretID, then does the
// normal AppRole login. The wrap is single-use: an attacker who unwraps first makes TG's unwrap fail — a
// fail-closed tamper alarm rather than a silent compromise. The RoleID is a non-secret value (env ref); the
// only on-box material is the spent-on-read wrapping token. This RELOCATES the trust root to the orchestrator
// (named honestly) — it does not eliminate trust, but removes the durable secret-zero from disk.
type WrappedAppRole struct {
	RoleIDRef    config.SecretRef // the AppRole role_id (non-secret; env ref)
	WrapTokenRef config.SecretRef // the single-use response-wrapping token (file: on tmpfs, INV-13)
	MountPath    string           // auth mount, default "approle"
	// SecretIDField names the field the unwrapped data carries the secret_id under (default "secret_id").
	SecretIDField string
}

func (w WrappedAppRole) login(ctx context.Context, c *Client) (string, time.Duration, bool, error) {
	roleID, err := w.RoleIDRef.Resolve()
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: resolve wrapped-approle role_id: %w", err)
	}
	wrapTok, err := w.WrapTokenRef.Resolve()
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: resolve wrapping token: %w", err)
	}
	if strings.TrimSpace(wrapTok) == "" {
		return "", 0, false, fmt.Errorf("vault: wrapping token is empty (fail closed)")
	}
	// Unwrap the single-use wrapped SecretID. The wrapping token authenticates the unwrap; a second unwrap of
	// the same token FAILS (single-use) — so a non-2xx here on a fresh boot is a tamper signal, not a retry.
	raw, err := c.raw(ctx, http.MethodPost, "sys/wrapping/unwrap", nil, wrapTok)
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: unwrap secret_id failed (fail closed — token spent/expired/tampered?): %w", err)
	}
	var uw struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &uw); err != nil {
		return "", 0, false, fmt.Errorf("vault: decode unwrap response: %w", err)
	}
	secretID := uw.Data[orDefault(w.SecretIDField, "secret_id")]
	if strings.TrimSpace(secretID) == "" {
		return "", 0, false, fmt.Errorf("vault: unwrapped data carries no secret_id (fail closed)")
	}
	mount := orDefault(w.MountPath, "approle")
	return c.loginAt(ctx, "auth/"+mount+"/login", map[string]string{"role_id": roleID, "secret_id": secretID})
}

// JWT authenticates with a JWT and a role (POST /v1/auth/jwt/login). The JWT is a SecretRef.
type JWT struct {
	JWTRef    config.SecretRef
	Role      string
	MountPath string // auth mount, default "jwt"
}

func (j JWT) login(ctx context.Context, c *Client) (string, time.Duration, bool, error) {
	jwt, err := j.JWTRef.Resolve()
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: resolve jwt: %w", err)
	}
	if j.Role == "" {
		return "", 0, false, fmt.Errorf("vault: jwt auth requires a role")
	}
	mount := orDefault(j.MountPath, "jwt")
	return c.loginAt(ctx, "auth/"+mount+"/login", map[string]string{"jwt": jwt, "role": j.Role})
}

// Token authenticates with a pre-issued token supplied as a SecretRef (direct token auth). It has no login
// round-trip; the token is used as-is with no local lease.
type Token struct {
	TokenRef config.SecretRef
}

func (t Token) login(_ context.Context, _ *Client) (string, time.Duration, bool, error) {
	tok, err := t.TokenRef.Resolve()
	if err != nil {
		return "", 0, false, fmt.Errorf("vault: resolve token: %w", err)
	}
	if tok == "" {
		return "", 0, false, fmt.Errorf("vault: token is empty (fail closed)")
	}
	return tok, 0, false, nil
}

// Cert authenticates with a TLS CLIENT CERTIFICATE (mTLS) at POST /v1/auth/<mount>/login — the FreeIPA
// machine-identity path (spec/024 Amendment 2026-07-25, REQ-2407). The client cert (a FreeIPA-CA-signed,
// CN-bound identity) is presented in the TLS handshake by New (which loads it into the transport); OpenBao's
// cert auth maps it to a policy. This is an identity the host IS — CA-anchored, certmonger-renewable,
// revocable — not a bearer secret it HOLDS, so there is NO bootstrap token on disk. Name selects the OpenBao
// cert role (optional; "" lets OpenBao match any trusted cert). The certificate is public (a plain path); the
// KEY is the machine identity and must be a root-only file (INV-13 — it is a credential, never a literal).
type Cert struct {
	CertPath  string // PEM client certificate (public; FreeIPA-CA-signed)
	KeyPath   string // PEM private key (the machine identity; root-only file)
	Name      string // OpenBao cert role name (optional)
	MountPath string // auth mount, default "cert"
}

func (ce Cert) login(ctx context.Context, c *Client) (string, time.Duration, bool, error) {
	body := map[string]string{}
	if n := strings.TrimSpace(ce.Name); n != "" {
		body["name"] = n
	}
	return c.loginAt(ctx, "auth/"+orDefault(ce.MountPath, "cert")+"/login", body)
}

// loginAt POSTs an auth-method login body and extracts the token + lease from .auth.
func (c *Client) loginAt(ctx context.Context, apiPath string, body map[string]string) (string, time.Duration, bool, error) {
	// ★ THE LOGIN MUST RETRY A STANDBY TOO, AND FOR EXACTLY THE REASON THE READ DOES.
	//
	// The 2026-07-27 and 2026-07-28 outages were fixed by retrying the authenticated READ. It was not enough,
	// and a THIRD control-plane outage on 2026-07-29 (~9 minutes, console/worker/grounder stuck in `Created`)
	// showed why: authed() resolves a token BEFORE it enters that retry loop, and this login went straight to
	// c.raw with no retry at all. A login request draws a standby with the same p ≈ 2/3 as any other request
	// against the round-robin VIP — so the carefully-sized 19-attempt read budget was never reached. The init
	// died 15 seconds in, not the ~30 seconds the budget implies, which is the arithmetic saying plainly that
	// a different code path failed.
	//
	// This is the "fixed the obvious half" shape: repairing the visible failure moved the same defect one
	// call earlier, where nothing was watching. Both halves now share ONE retry helper, so a future change
	// cannot re-open the gap by fixing one and forgetting the other.
	raw, err := c.retrying(ctx, func() ([]byte, error) {
		return c.raw(ctx, http.MethodPost, apiPath, body, "")
	})
	if err != nil {
		return "", 0, false, err
	}
	var lr struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
			Renewable     bool   `json:"renewable"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &lr); err != nil {
		return "", 0, false, fmt.Errorf("vault: decode login response: %w", err)
	}
	if lr.Auth.ClientToken == "" {
		return "", 0, false, fmt.Errorf("vault: login at %s returned no client token (fail closed)", apiPath)
	}
	return lr.Auth.ClientToken, time.Duration(lr.Auth.LeaseDuration) * time.Second, lr.Auth.Renewable, nil
}

// ---- token cache ------------------------------------------------------------------------------------

// tokenNow returns a valid cached token, logging in if none is cached or the lease is near expiry. force
// discards any cached token first (used after a 403 to re-login exactly once).
func (c *Client) tokenNow(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if force {
		c.hasToken = false
	}
	if c.hasToken && (c.expiry.IsZero() || time.Now().Before(c.expiry.Add(-leaseSkew))) {
		return c.token, nil
	}
	tok, lease, _, err := c.auth.login(ctx, c)
	if err != nil {
		c.hasToken = false
		return "", err
	}
	c.token, c.hasToken = tok, true
	if lease > 0 {
		c.expiry = time.Now().Add(lease)
	} else {
		c.expiry = time.Time{}
	}
	return tok, nil
}

// ---- KV v2 read/list --------------------------------------------------------------------------------

// ReadKV reads a KV v2 secret and returns its data map. apiPath is the path AFTER /v1/ and MUST include the
// KV v2 "data" segment, e.g. "secret/data/hosts/hostA". It fails closed on unreachable/denied/not-found; a
// 403 triggers a single re-login-and-retry (an expired token recovers, a truly-denied one still fails).
func (c *Client) ReadKV(ctx context.Context, apiPath string) (map[string]string, error) {
	raw, err := c.authed(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	var rr struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("vault: decode KV read %s: %w", apiPath, err)
	}
	out := make(map[string]string, len(rr.Data.Data))
	for k, v := range rr.Data.Data {
		out[k] = fmt.Sprint(v)
	}
	return out, nil
}

// ListKV lists the keys directly under a KV v2 path prefix. apiPath is the path AFTER /v1/ and MUST include
// the KV v2 "metadata" segment, e.g. "secret/metadata/hosts". Folders come back suffixed with '/'.
// WriteKV writes a KV v2 secret at apiPath (which already carries the /data/ segment).
//
// It PATCHES the whole data object, which is the KV v2 semantic: the caller supplies the complete field
// set for that path. A partial write would silently drop sibling fields, and a secret that vanishes
// because someone rotated a different field is the kind of outage nobody attributes correctly.
//
// It returns no secret material and logs none — a write path that echoed the value back would put it in
// every caller's error handling.
func (c *Client) WriteKV(ctx context.Context, apiPath string, data map[string]string) error {
	if len(data) == 0 {
		return fmt.Errorf("vault: refusing an empty KV write to %s — that would blank the secret", apiPath)
	}
	body := map[string]any{"data": data}
	if _, err := c.authed(ctx, http.MethodPost, apiPath, body); err != nil {
		return fmt.Errorf("vault: KV write %s: %w", apiPath, err)
	}
	return nil
}

func (c *Client) ListKV(ctx context.Context, apiPath string) ([]string, error) {
	raw, err := c.authed(ctx, "LIST", apiPath, nil)
	if err != nil {
		return nil, err
	}
	var lr struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &lr); err != nil {
		return nil, fmt.Errorf("vault: decode KV list %s: %w", apiPath, err)
	}
	return lr.Data.Keys, nil
}

// ---- HTTP plumbing ----------------------------------------------------------------------------------

// retryBackoff is the delay before each successive UNAVAILABLE retry: bounded, but sized from the ACTUAL
// topology rather than from a feel for "short".
//
// ★ THE FIRST VERSION OF THIS WAS THREE RETRIES, AND IT FAILED IN PRODUCTION A SECOND TIME (2026-07-28,
// ~14 minutes of control-plane downtime, the same shape as the 2026-07-27 outage it was written to fix).
// The reason is arithmetic, not bad luck. `openbao.example.net` resolves to ONE VIP that
// round-robins across three nodes; the leader answers, the two standbys answer 429 to a request they will
// not forward. So each attempt independently draws a standby with p ≈ 2/3, and a budget of 4 total attempts
// fails with p = (2/3)^4 ≈ 19.8% — about one deploy in five, which is exactly the observed rate of two
// outages in two days.
//
// The budget is therefore chosen so that (2/3)^attempts is negligible rather than merely small:
// 19 attempts gives (2/3)^19 ≈ 0.06%, i.e. one stranded deploy per ~1,700. The delays grow to a 2s ceiling
// and total ~30s of waiting — real time on the deploy's critical path, and far cheaper than a control plane
// that is down until a human notices. A hang is still impossible: the budget is finite and the caller's
// context still cancels.
//
// If the OpenBao cluster ever grows or its forwarding behaviour changes, re-derive this from p and the
// target failure probability; TestTheRetryBudgetIsSizedForTheRealTopology pins that arithmetic so the
// number cannot quietly drift back to "short".
var retryBackoff = []time.Duration{
	150 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond, 1200 * time.Millisecond,
	2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second,
	2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second,
	2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second,
}

// authed issues an authenticated request, re-logging-in once on a 403 (an expired/rotated token recovers)
// and retrying a bounded number of times on an UNAVAILABLE status.
//
// The unavailability retry is not a nicety. A Vault/OpenBao name commonly fronts several nodes, and a
// STANDBY answers 429 to a request it will not forward. That is categorically different from "this secret
// does not exist" (404) or "this token may not read it" (403): it is a transport condition that the very
// next request usually resolves. Treating all three identically is what turned one unlucky standby hit into
// a 7-minute control-plane outage on 2026-07-27 — tg-secretenv could not resolve LITELLM_MASTER_KEY, failed
// closed (correctly), and depends_on then left litellm/worker/grounder/console stuck in `Created`. A plain
// `docker compose up -d` moments later resolved all five secrets first try.
//
// FAIL-CLOSED IS PRESERVED EXACTLY. Only 429/503 are retried, and only a bounded number of times; a 404, a
// 403, or an exhausted retry budget still returns an error and still blocks the boot. This makes the
// resolver survive a routine failover — it never makes it hand out a secret it could not read.
func (c *Client) authed(ctx context.Context, method, apiPath string, body any) ([]byte, error) {
	tok, err := c.tokenNow(ctx, false)
	if err != nil {
		return nil, err
	}
	raw, err := c.attempt(ctx, method, apiPath, body, tok)
	if err == nil {
		return raw, nil
	}
	// Fail closed, but recover from a stale token: on a 403 (denied/expired), re-login once and retry.
	if isDenied(err) {
		tok, lerr := c.tokenNow(ctx, true)
		if lerr != nil {
			return nil, lerr
		}
		return c.attempt(ctx, method, apiPath, body, tok)
	}
	return nil, err
}

// attempt performs one authenticated request, retrying only while the substrate reports itself UNAVAILABLE.
func (c *Client) attempt(ctx context.Context, method, apiPath string, body any, tok string) ([]byte, error) {
	return c.retrying(ctx, func() ([]byte, error) {
		return c.raw(ctx, method, apiPath, body, tok)
	})
}

// retrying is the ONE place the standby budget is spent. Both the LOGIN and the authenticated read go
// through it: every request to the VIP draws a standby independently, so any single un-retried request
// re-opens the whole failure mode regardless of how well the others are protected.
//
// Fail-closed is preserved exactly: only 429/503 are retried, only a bounded number of times, and the
// caller's context still cancels. A 404 or 403 returns immediately — those are verdicts, not transport.
func (c *Client) retrying(ctx context.Context, do func() ([]byte, error)) ([]byte, error) {
	out, err := do()
	for i := 0; err != nil && retryable(err) && i < len(retryBackoff); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryBackoff[i]):
		}
		out, err = do()
	}
	return out, err
}

// statusError carries the HTTP status so authed can distinguish a 403 (retry after re-login) from other
// failures. Any non-2xx is still an error — the connector never falls open.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

func isDenied(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusForbidden
	}
	return false
}

// isUnavailable reports whether the status means "this node will not serve you right now", as opposed to a
// verdict about the secret or the caller. 429 is what an OpenBao/Vault STANDBY returns for a request it does
// not forward; 503 is sealed-or-not-yet-ready. Both are transient and the next attempt commonly succeeds.
// Deliberately a CLOSED set: 404 (no such secret) and 403 (not permitted) are real answers and must NEVER be
// retried into a false success.
func isUnavailable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusTooManyRequests || se.status == http.StatusServiceUnavailable
	}
	return false
}

// isTransportStall reports a request that produced NO RESPONSE AT ALL — a per-request timeout or a
// connection-level failure — as opposed to a server that answered with a status.
//
// ★ THIS IS THE THIRD TIME THE BUDGET ABOVE WAS NEVER ENTERED BY THE FAILURE THAT ACTUALLY HAPPENED.
// The file already records two: the read retried but the login did not, and authed() resolved its token
// before the loop. This is the third, and it took the operator console down on 2026-07-29 for ~5 minutes:
//
//	browser sessions disabled: session key bao:secret/data/tg/session#key not resolvable
//	(vault: POST auth/cert/login: ... context deadline exceeded)
//
// isUnavailable matches ONLY a *statusError of 429/503. A timeout carries no status, so retrying() called
// do() exactly once and returned — the carefully-derived 19-attempt budget, sized so that (2/3)^19 is
// negligible, was skipped entirely. The grounder then disabled browser sessions, which un-registers
// /v1/session, and NOBODY COULD LOG IN until a human restarted it. A single unlucky request, one boot log
// line, no recovery.
//
// A stall is the SAME KIND of condition as a 429 and deserves the same treatment: the request never
// reached a decision, so retrying it cannot make the system agree to something it refused. A standby that
// answers 429 and a standby that accepts the connection and never answers are the same event from the
// caller's side.
//
// FAIL-CLOSED IS PRESERVED EXACTLY. This widens WHICH errors re-enter a bounded loop; it does not widen
// what counts as success. A 404, a 403, a TLS/certificate failure, and an exhausted budget all still
// return an error and still block the boot. Note what is deliberately NOT here: a bare
// errors.Is(err, context.DeadlineExceeded) would also fire when the CALLER's context expired, which must
// end the loop rather than extend it — retrying() already returns ctx.Err() from its select, and this
// predicate stays narrow so the two cannot be confused.
func isTransportStall(err error) bool {
	if err == nil {
		return false
	}
	// A status answer is not a stall — isUnavailable owns those, including the ones we must NOT retry.
	var se *statusError
	if errors.As(err, &se) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// http.Client.Timeout surfaces as a *url.Error wrapping context.DeadlineExceeded on some paths.
	return errors.Is(err, context.DeadlineExceeded)
}

// retryable is the single predicate retrying() consults: a server that said "not me, try again" (429/503)
// or a request that never got an answer at all. Kept as one function so a future contributor adding a
// third condition changes one place, not two.
func retryable(err error) bool { return isUnavailable(err) || isTransportStall(err) }

// isNotFound reports whether err is a KV v2 404. On a metadata LIST, OpenBao/Vault returns 404 for a path
// that holds no keys — an empty/absent namespace under a valid mount — which the Sync path treats as
// ok-with-0-entries rather than a failure. It matches ONLY a 404 status: a denial (403), a server error
// (5xx), a transport/decode error, or an unreachable backend is NOT a not-found and still fails closed.
func isNotFound(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.status == http.StatusNotFound
	}
	return false
}

// raw issues one request against /v1/{apiPath} with an optional token, returning the body on 2xx or an
// error (a *statusError for a non-2xx) otherwise. It NEVER returns a body with a nil error on failure.
func (c *Client) raw(ctx context.Context, method, apiPath string, body any, token string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	url := c.baseURL + "/v1/" + strings.TrimLeft(apiPath, "/")
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault: %s %s: %w", method, apiPath, err) // unreachable → fail closed
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &statusError{
			status: resp.StatusCode,
			msg:    fmt.Sprintf("vault: %s %s: status %d: %s", method, apiPath, resp.StatusCode, vaultErrs(out)),
		}
	}
	return out, nil
}

// Health hits GET /v1/sys/health (unauthenticated). A 200 means initialized+unsealed+active. Used by the
// env-gated live integration test as the safe live assertion.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.raw(ctx, http.MethodGet, "sys/health", nil, "")
	return err
}

// ---- SecretRef resolver (REQ-1613) ------------------------------------------------------------------

// ResolveRef dereferences a vault:/bao: SecretRef to a KV v2 value. ref is the FULL reference, e.g.
// "vault:secret/data/hosts/hostA#ssh_key": the part after the scheme is the KV v2 API path (including the
// "data" segment) and, after '#', the field to select. It fails closed — unreachable/denied/missing
// path/missing field all return an error, never a blank value. Read-only.
func (c *Client) ResolveRef(ref string) (string, error) {
	_, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("vault: malformed secret reference %s", config.RedactedRef(ref))
	}
	path, field, ok := strings.Cut(rest, "#")
	if !ok || path == "" || field == "" {
		return "", fmt.Errorf("vault: reference %s must be scheme:<kv-v2-path>#<field>", config.RedactedRef(ref))
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	data, err := c.ReadKV(ctx, path)
	if err != nil {
		return "", err // fail closed on unreachable/denied/not-found
	}
	v, ok := data[field]
	if !ok {
		return "", fmt.Errorf("vault: reference %q: field %q absent at %s (fail closed)", ref, field, path)
	}
	return v, nil
}

// RegisterResolver wires this client's vault: scheme into core/config so a Bundle SecretRef resolves through
// it at use time (REQ-1613). Composition-time only. Pass a nil client to unregister (fail closed).
func RegisterResolver(c *Client) {
	if c == nil {
		config.RegisterSchemeResolver(Scheme, nil)
		return
	}
	config.RegisterSchemeResolver(Scheme, c.ResolveRef)
}

// ---- CredentialSource (REQ-1607/1613) ---------------------------------------------------------------

// Source is a read-only credential.CredentialSource backed by a KV v2 path prefix. Each secret under the
// prefix maps to one host bundle: non-secret fields (user/host/port/scheme) are read at sync time; the
// secret field is carried as a vault: SecretRef (never its value) and dereferenced at use time (INV-13).
type Source struct {
	id     string
	client *Client
	mount  string // KV v2 mount, e.g. "secret"
	prefix string // path prefix under the mount, e.g. "hosts"
	scheme string // scheme label for the emitted refs ("vault" or "bao")
}

// SourceConfig configures a Source. Mount + Prefix name the KV v2 subtree to import; RefScheme is the
// SecretRef scheme the emitted bundles use ("vault" for this package, "bao" for the openbao wrapper).
type SourceConfig struct {
	ID        string
	Client    *Client
	Mount     string
	Prefix    string
	RefScheme string
}

// NewSource builds a Source. It fails closed if the client, id, or mount is missing.
func NewSource(cfg SourceConfig) (*Source, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("vault: source requires a client")
	}
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, fmt.Errorf("vault: source requires an id")
	}
	if strings.TrimSpace(cfg.Mount) == "" {
		return nil, fmt.Errorf("vault: source requires a KV v2 mount")
	}
	return &Source{
		id:     cfg.ID,
		client: cfg.Client,
		mount:  strings.Trim(cfg.Mount, "/"),
		prefix: strings.Trim(cfg.Prefix, "/"),
		scheme: orDefault(cfg.RefScheme, Scheme),
	}, nil
}

// ID implements credential.CredentialSource.
func (s *Source) ID() string { return s.id }

// Plane implements credential.CredentialSource — a Vault source feeds the machine → host plane.
func (s *Source) Plane() credential.Plane { return credential.PlaneMachine }

// Sync performs the READ-ONLY pull (REQ-1607): it LISTs the KV v2 prefix and reads each entry, mapping it
// to a SourceEntry whose Bundle carries vault: SecretRefs (never raw secret values). An EMPTY namespace (a
// 404 on the metadata LIST, which KV v2 also returns for a prefix holding no keys) is a legitimately-empty
// source → 0 entries, nil error (outcome=ok, coverage honestly empty), NOT a failure. It otherwise fails
// closed — a denied/unreachable list, a read error, or a malformed entry (no user) aborts with an error,
// leaving the prior converged state intact; it never returns a partial set that would orphan real entries.
func (s *Source) Sync(ctx context.Context) ([]credential.SourceEntry, error) {
	// An unconfigured (empty) prefix DISABLES this source. Without a scoped subtree we would LIST the entire
	// KV mount root — which on a shared substrate mixes in other tenants and flat non-bundle secrets (process
	// tokens, signing keys), each of which fails the host-bundle shape (no user) and aborts the whole sync.
	// A source with no prefix is OFF (0 host bundles, outcome=ok, honestly empty), NOT a mount-wide importer:
	// point it at a dedicated host-bundle subtree (e.g. "tg/hosts") to enable it. Fail closed, never fall open.
	if s.prefix == "" {
		return []credential.SourceEntry{}, nil
	}
	keys, err := s.client.ListKV(ctx, s.metaPath(""))
	if err != nil {
		// KV v2 LISTs a path that holds NO keys (an empty or not-yet-populated namespace under an
		// otherwise-valid mount) as a 404 — indistinguishable on the wire from an empty result. That is a
		// legitimately-empty source: report 0 entries so the SyncEngine records outcome=ok with honestly-
		// empty coverage, NOT a failed sync. Every OTHER failure — a denial (401/403), a server error
		// (5xx), a decode error, or an unreachable backend — still fails closed here, leaving the prior
		// converged state intact (INV-05). We never fall open by swallowing a real error.
		if isNotFound(err) {
			return []credential.SourceEntry{}, nil
		}
		return nil, fmt.Errorf("vault: source %q list: %w", s.id, err)
	}
	entries := make([]credential.SourceEntry, 0, len(keys))
	for _, k := range keys {
		if strings.HasSuffix(k, "/") {
			continue // a nested folder; this connector imports one flat level (fail closed, no recursion surprise)
		}
		data, err := s.client.ReadKV(ctx, s.dataPath(k))
		if err != nil {
			return nil, fmt.Errorf("vault: source %q read %q: %w", s.id, k, err)
		}
		entry, err := s.toEntry(k, data)
		if err != nil {
			return nil, err // malformed upstream entry → fail closed, do not inject a blank identity
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// toEntry maps one KV v2 secret's fields to a SourceEntry. The secret-bearing field becomes a vault: ref
// (the value is NOT stored); user/host/port/scheme are non-secret metadata read directly.
func (s *Source) toEntry(name string, data map[string]string) (credential.SourceEntry, error) {
	host := firstNonEmpty(data["host"], name)
	user := data["user"]
	if user == "" {
		return credential.SourceEntry{}, fmt.Errorf("vault: source %q entry %q has no user (fail closed)", s.id, name)
	}
	port := 22
	if p := data["port"]; p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return credential.SourceEntry{}, fmt.Errorf("vault: source %q entry %q invalid port %q: %w", s.id, name, p, err)
		}
		port = n
	}
	connScheme := credential.ConnectionScheme(firstNonEmpty(data["scheme"], string(credential.SchemeSSH)))
	spec := credential.BundleSpec{User: user, Port: port, Scheme: connScheme}
	// The secret field is a REFERENCE back into this same KV entry, resolved at use time (INV-13).
	dataRef := func(field string) config.SecretRef {
		return config.SecretRef(s.scheme + ":" + s.dataPath(name) + "#" + field)
	}
	switch connScheme {
	case credential.SchemeSSH, credential.SchemeNetconf:
		spec.SSHKeyRef = dataRef(orDefault(data["ssh_key_field"], "ssh_key"))
	case credential.SchemeAPI:
		spec.APITokenRef = dataRef(orDefault(data["api_token_field"], "api_token"))
	case credential.SchemeWinRM:
		spec.Become = dataRef(orDefault(data["password_field"], "password"))
	default:
		return credential.SourceEntry{}, fmt.Errorf("vault: source %q entry %q unknown scheme %q", s.id, name, connScheme)
	}
	if bref := data["become_field"]; bref != "" {
		spec.Become = dataRef(bref)
	}
	b, err := credential.NewBundle(spec)
	if err != nil {
		return credential.SourceEntry{}, fmt.Errorf("vault: source %q entry %q bundle: %w", s.id, name, err)
	}
	return credential.SourceEntry{
		NativeID: name,
		Selector: credential.Selector{Kind: credential.KindHost, Pattern: host},
		Bundle:   b,
	}, nil
}

// metaPath / dataPath build the KV v2 API paths (metadata for LIST, data for read) for a leaf under the
// configured mount+prefix.
func (s *Source) metaPath(leaf string) string { return s.kvPath("metadata", leaf) }
func (s *Source) dataPath(leaf string) string { return s.kvPath("data", leaf) }

func (s *Source) kvPath(seg, leaf string) string {
	parts := []string{s.mount, seg}
	if s.prefix != "" {
		parts = append(parts, s.prefix)
	}
	if leaf != "" {
		parts = append(parts, leaf)
	}
	return strings.Join(parts, "/")
}

// compile-time proof the source satisfies the stable credential-source interface.
var _ credential.CredentialSource = (*Source)(nil)

// ---- small helpers ----------------------------------------------------------------------------------

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// vaultErrs extracts OpenBao/Vault's {"errors":[...]} body into a short, secret-free message.
func vaultErrs(body []byte) string {
	var e struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Errors) > 0 {
		return strings.Join(e.Errors, "; ")
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
