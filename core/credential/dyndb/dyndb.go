// Package dyndb leases short-lived Postgres credentials from OpenBao's `database` secret engine, so TG's own
// database roles (migration/runtime/triage/actuate) can be MINTED per-lease and expire on their own instead
// of living as long-lived static passwords in the process environment — the longest-lived, highest-value
// static secrets TG holds (TG-422, the self-contained first slice of TG-320).
//
// Layering: core/ must not import modules/, so this is a minimal self-contained OpenBao client (the
// seal/transit.go pattern) with an INJECTABLE Doer, which makes the issue/renew/revoke lifecycle
// oracle-testable without a live substrate. The composition root (cmd/worker) builds an Engine and injects
// the `dyn:` scheme resolver into core/config (the delivery.go inject pattern), so nothing here reaches a
// binary until the flag is set.
//
// FAIL-CLOSED throughout (INV-13, spec/022 REQ-2204): any transport, status, or parse failure returns an
// error and NEVER an empty, stale, or static credential. The whole point of a dynamic credential is defeated
// by a silent fallback to a fixed one, so there is no fallback path in this package.
//
// THE ENGINE'S OWN IDENTITY (TG-565). Two modes, one per deployment:
//   - token mode (TokenRef): a STORED periodic bao token, kept alive by renew-self while TG runs. Its failure
//     shape is the 2026-09-18 incident: the stack was down 57h, nothing renewed the token, it expired, and on
//     restart every mint 403'd — grounder and worker refused to boot until a human re-minted it by hand.
//   - cert mode (CertRole): the engine LOGS IN with the host's machine-identity certificate
//     (auth/<mount>/login, the spec/024 "identity the host IS" pattern). A token that died — outage, expiry,
//     revocation — is replaced by logging in again, so no outage of any length can strand the boot.
//
// A token change in either mode strands every lease minted under the OLD token (OpenBao revokes a token's
// leases with it), so the engine reports each change through its token-change hook and the Provider rotates
// every lease at once instead of waiting for each one's renew point.
package dyndb

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// Doer is the minimal HTTP contract (net/http.Client satisfies it; a fake satisfies it in tests).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config configures the OpenBao database-engine lease client. Exactly one identity is used: CertRole set ⇒
// cert mode (CertPath+KeyPath required; TokenRef, if also set, is ignored); otherwise token mode (TokenRef
// required).
type Config struct {
	BaseURL  string           // OpenBao base URL, e.g. https://openbao.example:8200
	Mount    string           // database engine mount path; default "database"
	TokenRef config.SecretRef // token mode: the bao token REFERENCE, scoped to database/creds/<role> + lease renew/revoke
	CACert   string           // optional path to the substrate's private-CA cert
	HTTP     Doer             // optional transport override (tests)

	// Cert mode (TG-565): log in as OpenBao cert role CertRole with the client certificate CertPath/KeyPath.
	// The role must carry token_policies=tg-dyndb-postgres and a token_period (see deploy/openbao/README.md).
	CertRole  string
	CertPath  string // PEM client certificate (the host's machine identity)
	KeyPath   string // PEM private key
	CertMount string // cert auth mount; default "cert"
}

// Lease is one minted credential with its lifecycle handle. Duration is the TTL the engine granted; LeaseID
// is the handle Renew/Revoke act on.
type Lease struct {
	Username  string
	Password  string
	LeaseID   string
	Duration  time.Duration
	Renewable bool
}

// Engine is a lease client for one OpenBao database secret engine.
type Engine struct {
	base, mount string
	tokenRef    config.SecretRef
	certRole    string
	certMount   string
	http        Doer

	loginMu  sync.Mutex // serializes cert logins, so concurrent 403s re-login ONCE, not once per caller
	mu       sync.Mutex
	token    string // cert mode: the logged-in token; token mode: the last resolved token (change detection)
	changes  int    // how many times the token in use has CHANGED after the first one
	onChange func()
}

// New builds an Engine. Fails closed on a missing base URL, a missing identity (no token reference and no
// cert role), an incomplete cert identity, or an unreadable CA cert / client key pair.
func New(cfg Config) (*Engine, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("dyndb: base URL is required")
	}
	certRole := strings.TrimSpace(cfg.CertRole)
	if certRole == "" && strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, errors.New("dyndb: an engine identity is required — a cert role (TG_DYNDB_CERT_ROLE) or a token reference")
	}
	if certRole != "" && !validRole.MatchString(certRole) {
		return nil, fmt.Errorf("dyndb: cert role has invalid characters (allowed: A-Za-z0-9_-); %d chars", len(certRole))
	}
	if certRole != "" && (strings.TrimSpace(cfg.CertPath) == "" || strings.TrimSpace(cfg.KeyPath) == "") {
		return nil, errors.New("dyndb: cert role set but the client certificate/key paths are empty (TG_OPENBAO_CERT / TG_OPENBAO_KEY)")
	}
	mount := cfg.Mount
	if strings.TrimSpace(mount) == "" {
		mount = "database"
	}
	certMount := cfg.CertMount
	if strings.TrimSpace(certMount) == "" {
		certMount = "cert"
	}
	h := cfg.HTTP
	if h == nil {
		hc := &http.Client{Timeout: 15 * time.Second}
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		custom := false
		if cfg.CACert != "" {
			pem, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("dyndb: CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("dyndb: CA cert contains no certificate")
			}
			tlsCfg.RootCAs = pool
			custom = true
		}
		if certRole != "" {
			// Presented on EVERY handshake, not only the login: OpenBao's cert auth binds a token to the
			// certificate that logged it in (disable_binding=false), so renew-self must carry it too.
			crt, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
			if err != nil {
				return nil, fmt.Errorf("dyndb: client cert/key: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{crt}
			custom = true
		}
		if custom {
			hc.Transport = &http.Transport{TLSClientConfig: tlsCfg}
		}
		h = hc
	}
	e := &Engine{
		base:      strings.TrimRight(cfg.BaseURL, "/"),
		mount:     strings.Trim(mount, "/"),
		certRole:  certRole,
		certMount: strings.Trim(certMount, "/"),
		http:      h,
	}
	if certRole == "" {
		e.tokenRef = cfg.TokenRef
	}
	return e, nil
}

// CertRole is the OpenBao cert role the engine logs in as, or "" in token mode.
func (e *Engine) CertRole() string { return e.certRole }

// SetOnTokenChange registers the hook fired (outside every lock) each time the token the engine uses changes
// after the first one — a cert re-login, or a different value behind the token reference. Leases minted
// under the old token died with it, so the Provider wires this to rotate every lease at once. Last writer wins.
//
// Token mode NOTE (new with TG-565): a re-minted value behind TokenRef now also rotates every lease at once.
// That is deliberate — the old token's leases die when IT expires, and nothing renews the old token any more —
// but it means a torn read of a token FILE (a non-atomic rewrite) looks like a change and triggers one spurious
// rotation round (fail-closed: a mint under a garbage token 403s and each Manager keeps its lease). Write
// token files atomically (rename into place).
func (e *Engine) SetOnTokenChange(cb func()) {
	e.mu.Lock()
	e.onChange = cb
	e.mu.Unlock()
}

// TokenChanges reports how many times the engine's token has changed after the first one.
func (e *Engine) TokenChanges() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.changes
}

// validRole bounds a database role name to characters safe to interpolate into the OpenBao request path
// (OpenBao role names are this shape anyway). It is defence-in-depth: a `dyn:` reference's role is
// operator-controlled config, but a malformed one carrying `/` or `..` must be rejected in CODE, not left to
// the server's ACL to refuse — the path is built by concatenation, so the guard belongs at the boundary.
var validRole = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Issue mints a fresh credential for the named database role: GET /v1/<mount>/creds/<role>. Each successful
// call returns a DISTINCT username/password — the engine mints, it does not cache — which is the property the
// oracle pins (a cached or static value is the failure this whole package exists to end). Fails closed on any
// transport/status/parse error or an empty credential.
func (e *Engine) Issue(ctx context.Context, role string) (*Lease, error) {
	if strings.TrimSpace(role) == "" {
		return nil, errors.New("dyndb: empty database role")
	}
	if !validRole.MatchString(role) {
		// Report the SHAPE only, never the role text: a misconfigured `dyn:<secret>` would otherwise leak the
		// secret into this error and any log that records it (INV-13). A valid role is not secret-shaped.
		return nil, fmt.Errorf("dyndb: database role has invalid characters (allowed: A-Za-z0-9_-); %d chars", len(role))
	}
	var out struct {
		LeaseID       string `json:"lease_id"`
		LeaseDuration int    `json:"lease_duration"`
		Renewable     bool   `json:"renewable"`
		Data          struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"data"`
	}
	if err := e.call(ctx, http.MethodGet, "/v1/"+e.mount+"/creds/"+role, nil, &out); err != nil {
		return nil, err
	}
	if out.Data.Username == "" || out.Data.Password == "" {
		return nil, errors.New("dyndb: engine returned an empty credential (fail closed)")
	}
	return &Lease{
		Username:  out.Data.Username,
		Password:  out.Data.Password,
		LeaseID:   out.LeaseID,
		Duration:  time.Duration(out.LeaseDuration) * time.Second,
		Renewable: out.Renewable,
	}, nil
}

// Renew extends a lease in place: PUT /v1/sys/leases/renew {lease_id, increment}. It returns the NEW TTL the
// engine granted (which can be shorter than requested — the engine caps at the role's max_ttl). Fails closed;
// the caller must treat an error as "this lease is not extended" and re-issue or fail, never keep using it
// past its old TTL.
func (e *Engine) Renew(ctx context.Context, leaseID string, increment time.Duration) (time.Duration, error) {
	if strings.TrimSpace(leaseID) == "" {
		return 0, errors.New("dyndb: empty lease id")
	}
	var out struct {
		LeaseDuration int `json:"lease_duration"`
	}
	body := map[string]any{"lease_id": leaseID, "increment": int(increment.Seconds())}
	if err := e.call(ctx, http.MethodPut, "/v1/sys/leases/renew", body, &out); err != nil {
		return 0, err
	}
	return time.Duration(out.LeaseDuration) * time.Second, nil
}

// RenewSelf extends the engine's OWN bao token: PUT /v1/auth/token/renew-self (TG-422 slice 2). The scoped
// token is periodic; nothing else renews it, so without this it quietly ages out and every mint after that
// fails. NOTE the policy implication: tg-dyndb-postgres.hcl denies auth/* wholesale, and in OpenBao a deny
// beats the default policy's renew-self allow — the HCL carves out exactly auth/token/renew-self for this
// call. Fails closed; the maintainer logs and retries on its next tick.
func (e *Engine) RenewSelf(ctx context.Context) error {
	return e.call(ctx, http.MethodPut, "/v1/auth/token/renew-self", nil, nil)
}

// TokenSelf is the subset of auth/token/lookup-self this package acts on: whether the engine's OWN token is
// renewable and PERIODIC (a non-zero period), plus its remaining ttl. It exists to catch the TG-545 outage
// SHAPE at boot: a token provisioned WITHOUT `-period` ages out at max_ttl no matter how often renew-self
// runs, and a non-renewable token cannot be extended at all — either way renew-self cannot save it, the
// lease eventually runs out, and the whole plane fails closed (the ~3.5h triage outage, 2026-08-26). The
// renew loop and the HCL carve-out are correct; the drift is in PROVISIONING, which is silent until weeks
// later. LookupSelf turns that into a loud boot signal.
type TokenSelf struct {
	Renewable bool
	Period    time.Duration // 0 ⇒ NOT periodic — ages out at max_ttl, renew-self cannot prevent it
	TTL       time.Duration
}

// LookupSelf reads the engine's OWN token metadata: GET /v1/auth/token/lookup-self. Unlike renew-self it
// needs no HCL carve-out (lookup-self is a default-token-policy allow). Fails closed on any transport/parse
// error. Used only by the boot self-check — it never gates a mint.
func (e *Engine) LookupSelf(ctx context.Context) (TokenSelf, error) {
	var out struct {
		Data struct {
			Renewable bool `json:"renewable"`
			Period    int  `json:"period"`
			TTL       int  `json:"ttl"`
		} `json:"data"`
	}
	if err := e.call(ctx, http.MethodGet, "/v1/auth/token/lookup-self", nil, &out); err != nil {
		return TokenSelf{}, err
	}
	return TokenSelf{
		Renewable: out.Data.Renewable,
		Period:    time.Duration(out.Data.Period) * time.Second,
		TTL:       time.Duration(out.Data.TTL) * time.Second,
	}, nil
}

// Revoke terminates a lease NOW: PUT /v1/sys/leases/revoke {lease_id}. After a successful revoke the credential
// is dead in the engine (bao lease list no longer shows it, and Postgres drops the role), which is how the
// oracle proves shutdown actually withdrew the credential rather than merely stopping renewing it. Fails closed.
func (e *Engine) Revoke(ctx context.Context, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return errors.New("dyndb: empty lease id")
	}
	return e.call(ctx, http.MethodPut, "/v1/sys/leases/revoke", map[string]any{"lease_id": leaseID}, nil)
}

// StatusError is a non-2xx answer from OpenBao. Its text is status ONLY — never the response body: an OpenBao
// error can echo request parameters (INV-13).
type StatusError struct {
	Method, Path string
	Code         int
}

func (s *StatusError) Error() string {
	return fmt.Sprintf("dyndb: %s %s failed: status %d", s.Method, s.Path, s.Code)
}

// LoginError is a cert login OpenBao answered with a non-2xx status. A 4xx is a REFUSAL — the role is missing,
// the certificate's CN is not allowed, the certificate expired or chains to the wrong CA — which no amount of
// retrying fixes; a 5xx is OpenBao itself failing (sealed, standby trouble) and does clear on its own.
type LoginError struct {
	Role string
	Code int
}

func (l *LoginError) Error() string {
	return fmt.Sprintf("dyndb: cert login (role %q) failed: status %d", l.Role, l.Code)
}

// IsLoginRefused reports whether err is (or wraps) a cert login OpenBao REFUSED (4xx) — a misconfiguration
// that will not heal by retrying, as opposed to an outage that will.
func IsLoginRefused(err error) bool {
	var le *LoginError
	return errors.As(err, &le) && le.Code/100 == 4
}

// IsForbidden reports whether err is (or wraps) an OpenBao 403 — for the engine's own endpoints, a dead or
// revoked token.
func IsForbidden(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden
}

func (e *Engine) call(ctx context.Context, method, path string, body any, out any) error {
	tok, err := e.currentToken(ctx)
	if err != nil {
		return err
	}
	code, rb, err := e.do(ctx, method, path, body, tok)
	if err != nil {
		return err
	}
	if code == http.StatusForbidden && e.certRole != "" && e.tokenDead(ctx, tok) {
		// Cert mode, and the token itself is dead (an outage longer than its period, a revocation) — not a
		// policy refusal of this one path. Log in again ONCE and retry; a second 403 fails closed below. Token
		// mode has nothing to re-login with — it re-resolves the reference on every call.
		if tok, err = e.certToken(ctx, tok); err != nil {
			return fmt.Errorf("dyndb: %s %s: re-login after 403: %w", method, path, err)
		}
		if code, rb, err = e.do(ctx, method, path, body, tok); err != nil {
			return err
		}
	}
	if code/100 != 2 {
		return &StatusError{Method: method, Path: path, Code: code}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rb, out)
}

func (e *Engine) do(ctx context.Context, method, path string, body any, tok string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, mErr := json.Marshal(body)
		if mErr != nil {
			return 0, nil, mErr
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if tok != "" {
		req.Header.Set("X-Vault-Token", tok)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, rb, nil
}

// tokenDead reports whether tok itself is dead — lookup-self answers 403 — as opposed to alive but refused
// for one path. Only a DEAD token justifies a re-login: re-logging in on a policy refusal would mint a new
// token and rotate every lease for nothing, on every refused call. Anything inconclusive (transport error,
// 5xx) is not treated as dead; the caller's 403 stands and fails closed.
func (e *Engine) tokenDead(ctx context.Context, tok string) bool {
	code, _, err := e.do(ctx, http.MethodGet, "/v1/auth/token/lookup-self", nil, tok)
	return err == nil && code == http.StatusForbidden
}

// currentToken returns the token to present: token mode re-resolves the reference (so a re-minted value is
// picked up without a restart); cert mode returns the logged-in token, logging in first if there is none.
func (e *Engine) currentToken(ctx context.Context) (string, error) {
	if e.certRole == "" {
		tok, err := e.tokenRef.Resolve()
		if err != nil {
			return "", fmt.Errorf("dyndb: token: %w", err)
		}
		e.noteToken(tok)
		return tok, nil
	}
	e.mu.Lock()
	tok := e.token
	e.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	return e.certToken(ctx, "")
}

// certToken logs in unless another caller already replaced `stale` (the token that just failed, or "" for
// "none yet") — so N callers hitting the same dead token produce ONE login, not N.
func (e *Engine) certToken(ctx context.Context, stale string) (string, error) {
	e.loginMu.Lock()
	defer e.loginMu.Unlock()
	e.mu.Lock()
	cur := e.token
	e.mu.Unlock()
	if cur != "" && cur != stale {
		return cur, nil
	}
	tok, err := e.login(ctx)
	if err != nil {
		return "", err
	}
	e.noteToken(tok)
	return tok, nil
}

// login authenticates the host certificate: POST /v1/auth/<certMount>/login {"name": <role>}. The role is
// always named — an empty name matches EVERY cert role, and the engine must land on the dyndb-scoped one.
func (e *Engine) login(ctx context.Context) (string, error) {
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	code, rb, err := e.do(ctx, http.MethodPost, "/v1/auth/"+e.certMount+"/login", map[string]string{"name": e.certRole}, "")
	if err != nil {
		return "", fmt.Errorf("dyndb: cert login (role %q): %w", e.certRole, err)
	}
	if code/100 != 2 {
		return "", &LoginError{Role: e.certRole, Code: code}
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("dyndb: cert login (role %q): %w", e.certRole, err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("dyndb: cert login (role %q) returned no token (fail closed)", e.certRole)
	}
	return out.Auth.ClientToken, nil
}

// noteToken records the token in use and fires the change hook when it differs from a previous one.
func (e *Engine) noteToken(tok string) {
	e.mu.Lock()
	prev := e.token
	if prev == tok {
		e.mu.Unlock()
		return
	}
	e.token = tok
	var hook func()
	if prev != "" {
		e.changes++
		hook = e.onChange
	}
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
}
