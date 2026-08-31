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
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// Doer is the minimal HTTP contract (net/http.Client satisfies it; a fake satisfies it in tests).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config configures the OpenBao database-engine lease client.
type Config struct {
	BaseURL  string           // OpenBao base URL, e.g. https://openbao.example:8200
	Mount    string           // database engine mount path; default "database"
	TokenRef config.SecretRef // the bao token REFERENCE (env:/file:), scoped to database/creds/<role> + lease renew/revoke
	CACert   string           // optional path to the substrate's private-CA cert
	HTTP     Doer             // optional transport override (tests)
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
	http        Doer
}

// New builds an Engine. Fails closed on a missing base URL or token reference, or an unreadable CA cert.
func New(cfg Config) (*Engine, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("dyndb: base URL is required")
	}
	if strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, errors.New("dyndb: token reference is required")
	}
	mount := cfg.Mount
	if strings.TrimSpace(mount) == "" {
		mount = "database"
	}
	h := cfg.HTTP
	if h == nil {
		hc := &http.Client{Timeout: 15 * time.Second}
		if cfg.CACert != "" {
			pem, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("dyndb: CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("dyndb: CA cert contains no certificate")
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		h = hc
	}
	return &Engine{
		base:     strings.TrimRight(cfg.BaseURL, "/"),
		mount:    strings.Trim(mount, "/"),
		tokenRef: cfg.TokenRef,
		http:     h,
	}, nil
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

func (e *Engine) call(ctx context.Context, method, path string, body any, out any) error {
	tok, err := e.tokenRef.Resolve()
	if err != nil {
		return fmt.Errorf("dyndb: token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, mErr := json.Marshal(body)
		if mErr != nil {
			return mErr
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		// Status ONLY — never the response body: an OpenBao error can echo request parameters, and the
		// credential path names a role, not a secret, so status is the safe, sufficient diagnostic (INV-13).
		return fmt.Errorf("dyndb: %s %s failed: status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(rb, out)
}
