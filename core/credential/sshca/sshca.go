// Package sshca mints short-lived SSH user CERTIFICATES from OpenBao's `ssh` secret engine (CA/signed-cert
// mode), so TG's actuation + diagnostic SSH lanes can present a per-session certificate signed by an OpenBao
// CA instead of a static private key that lives on disk indefinitely (TG-423, the SSH slice of TG-320).
//
// CA mode, not OTP: the target host trusts the OpenBao CA via sshd's `TrustedUserCAKeys`, so nothing
// privileged runs on the target and the exposure window becomes the cert TTL. A worker compromise then
// harvests a certificate that expires on its own, not a key valid until a human rotates it.
//
// Layering: core/ must not import modules/, so this is a minimal self-contained OpenBao client (the
// seal/transit.go + dyndb pattern) with an INJECTABLE Doer, which makes signing oracle-testable without a
// live substrate — a test signs the returned cert with its own in-process CA. The composition root (cmd/
// worker) builds an Engine and passes its Sign method to the native SSH runner as a cert-signer hook, so
// nothing here reaches a binary until the flag is set.
//
// FAIL-CLOSED throughout (INV-13): any transport, status, or parse failure returns an error and NEVER an
// empty, stale, or unsigned credential. The static-key path is the reversible fallback and lives in the
// composition root; this package has no fallback of its own — a signing failure refuses the actuation.
package sshca

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
	"strings"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

// Doer is the minimal HTTP contract (net/http.Client satisfies it; a fake satisfies it in tests).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Config configures the OpenBao ssh-engine certificate signer.
type Config struct {
	BaseURL  string           // OpenBao base URL, e.g. https://openbao.example:8200
	Mount    string           // ssh engine mount path; default "ssh-client-signer"
	Role     string           // the sign role (allowed_users/extensions/ttl policy live on the role)
	TTL      time.Duration    // requested cert TTL; the role's max_ttl caps it. Zero ⇒ omit (role default)
	TokenRef config.SecretRef // the bao token REFERENCE (env:/file:), scoped to <mount>/sign/<role>
	CACert   string           // optional path to the substrate's private-CA cert
	HTTP     Doer             // optional transport override (tests)
}

// Engine signs SSH public keys into short-lived user certificates via one OpenBao ssh secret engine.
type Engine struct {
	base, mount, role string
	ttl               time.Duration
	tokenRef          config.SecretRef
	http              Doer
}

// New builds an Engine. Fails closed on a missing base URL, role, or token reference, or an unreadable CA.
func New(cfg Config) (*Engine, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("sshca: base URL is required")
	}
	if strings.TrimSpace(cfg.Role) == "" {
		return nil, errors.New("sshca: sign role is required")
	}
	if strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, errors.New("sshca: token reference is required")
	}
	mount := cfg.Mount
	if strings.TrimSpace(mount) == "" {
		mount = "ssh-client-signer"
	}
	h := cfg.HTTP
	if h == nil {
		hc := &http.Client{Timeout: 15 * time.Second}
		if cfg.CACert != "" {
			pem, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("sshca: CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("sshca: CA cert contains no certificate")
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		h = hc
	}
	return &Engine{
		base:     strings.TrimRight(cfg.BaseURL, "/"),
		mount:    strings.Trim(mount, "/"),
		role:     cfg.Role,
		ttl:      cfg.TTL,
		tokenRef: cfg.TokenRef,
		http:     h,
	}, nil
}

// Sign mints a short-lived user certificate for pub, valid for the given principals (the SSH login the cert
// authorizes — e.g. the actuation identity): POST /v1/<mount>/sign/<role>. Each call returns a DISTINCT
// certificate (fresh serial + validity window — OpenBao mints, it does not cache), which the oracle pins.
// Fails closed on any transport/status/parse error, an empty signed_key, or a response that is not a cert.
func (e *Engine) Sign(ctx context.Context, pub cryptossh.PublicKey, principals []string) (*cryptossh.Certificate, error) {
	if pub == nil {
		return nil, errors.New("sshca: nil public key")
	}
	body := map[string]any{
		"public_key": string(cryptossh.MarshalAuthorizedKey(pub)),
		"cert_type":  "user",
	}
	if len(principals) > 0 {
		body["valid_principals"] = strings.Join(principals, ",")
	}
	if e.ttl > 0 {
		body["ttl"] = fmt.Sprintf("%ds", int(e.ttl.Seconds()))
	}
	var out struct {
		Data struct {
			SignedKey string `json:"signed_key"`
		} `json:"data"`
	}
	if err := e.call(ctx, "/v1/"+e.mount+"/sign/"+e.role, body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Data.SignedKey) == "" {
		return nil, errors.New("sshca: engine returned an empty signed_key (fail closed)")
	}
	parsed, _, _, _, err := cryptossh.ParseAuthorizedKey([]byte(out.Data.SignedKey))
	if err != nil {
		return nil, fmt.Errorf("sshca: signed_key did not parse (fail closed): %w", err)
	}
	cert, ok := parsed.(*cryptossh.Certificate)
	if !ok {
		// A bare public key where a certificate was expected — never present it as an actuation credential.
		return nil, errors.New("sshca: engine returned a non-certificate public key (fail closed)")
	}
	return cert, nil
}

// SignOne adapts Sign to the single-principal cert-signer shape the SSH actuation runner expects
// (modules/actuation/ssh.CertSigner). Passing this method value lets the composition root wire the signer
// without naming the crypto/ssh types itself.
func (e *Engine) SignOne(ctx context.Context, pub cryptossh.PublicKey, principal string) (*cryptossh.Certificate, error) {
	return e.Sign(ctx, pub, []string{principal})
}

func (e *Engine) call(ctx context.Context, path string, body any, out any) error {
	tok, err := e.tokenRef.Resolve()
	if err != nil {
		return fmt.Errorf("sshca: token: %w", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		// Status + path ONLY — never the response body. The path names a mount/role (config identifiers,
		// not secrets); an OpenBao error body can echo request params, so it stays out of the error (INV-13).
		return fmt.Errorf("sshca: POST %s failed: status %d", path, resp.StatusCode)
	}
	return json.Unmarshal(rb, out)
}
