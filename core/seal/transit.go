// transit.go — DEK wrapping via OpenBao (or Vault) Transit (spec/022 REQ-2201, TG-157/TG-153 Critical#2).
//
// The master-key operation runs INSIDE OpenBao: the worker sends the DEK to /v1/transit/encrypt/<key> and gets
// back an opaque ciphertext, and sends that ciphertext to /v1/transit/decrypt/<key> to recover the DEK. The
// wrapping key never leaves OpenBao, so a worker/host compromise cannot read it — it can, at most, call the
// encrypt/decrypt endpoints while it holds the (read-scoped) token, which is revocable and auditable, unlike a
// leaked 32-byte master. Name-binding is preserved by the value-side AAD (seal.aad), so the DEK-wrap needs no
// convergent context. core must not import modules, so this is a minimal self-contained client (an injectable
// Doer keeps it oracle-testable without a live OpenBao).
package seal

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// Doer is the minimal HTTP contract (net/http.Client satisfies it; a fake satisfies it in tests).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// TransitConfig configures the OpenBao Transit DEK wrapper.
type TransitConfig struct {
	BaseURL  string           // OpenBao base URL, e.g. https://openbao.example:8200
	KeyName  string           // the Transit key that wraps DEKs, e.g. "tg-seal"
	TokenRef config.SecretRef // the bao token REFERENCE (env:/file:), read-scoped to transit encrypt/decrypt on KeyName
	Mount    string           // Transit mount path, default "transit"
	CACert   string           // optional path to the substrate's private-CA cert
	HTTP     Doer             // optional transport override (tests)
}

type transitWrapper struct {
	base, mount, key string
	tokenRef         config.SecretRef
	http             Doer
}

// NewTransitWrapper builds a DEKWrapper that delegates wrap/unwrap to OpenBao Transit. Fails closed on missing
// base URL, key, or token reference, or an unreadable CA.
func NewTransitWrapper(cfg TransitConfig) (DEKWrapper, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("seal: transit base URL is required")
	}
	if strings.TrimSpace(cfg.KeyName) == "" {
		return nil, errors.New("seal: transit key name is required")
	}
	if strings.TrimSpace(string(cfg.TokenRef)) == "" {
		return nil, errors.New("seal: transit token reference is required")
	}
	mount := cfg.Mount
	if mount == "" {
		mount = "transit"
	}
	h := cfg.HTTP
	if h == nil {
		hc := &http.Client{Timeout: 15 * time.Second}
		if cfg.CACert != "" {
			pem, err := os.ReadFile(cfg.CACert)
			if err != nil {
				return nil, fmt.Errorf("seal: transit CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("seal: transit CA cert contains no certificate")
			}
			hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
		h = hc
	}
	return &transitWrapper{
		base:     strings.TrimRight(cfg.BaseURL, "/"),
		mount:    strings.Trim(mount, "/"),
		key:      cfg.KeyName,
		tokenRef: cfg.TokenRef,
		http:     h,
	}, nil
}

// WrapDEK encrypts the DEK with the Transit key. The returned wrapped bytes are Transit's self-describing
// ciphertext ("vault:v1:…"); nonce is nil (Transit carries its own).
func (t *transitWrapper) WrapDEK(_ string, dek []byte) ([]byte, []byte, error) {
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := t.call("encrypt", map[string]string{"plaintext": base64.StdEncoding.EncodeToString(dek)}, &out); err != nil {
		return nil, nil, err
	}
	if out.Data.Ciphertext == "" {
		return nil, nil, errors.New("seal: transit encrypt returned no ciphertext")
	}
	return []byte(out.Data.Ciphertext), nil, nil
}

// UnwrapDEK decrypts the Transit ciphertext back to the DEK. Any failure is ErrOpenFailed (indistinguishable).
func (t *transitWrapper) UnwrapDEK(_ string, wrapped, _ []byte) ([]byte, error) {
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := t.call("decrypt", map[string]string{"ciphertext": string(wrapped)}, &out); err != nil {
		return nil, ErrOpenFailed
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil || len(dek) != KeySize {
		return nil, ErrOpenFailed
	}
	return dek, nil
}

func (t *transitWrapper) call(op string, body map[string]string, out interface{}) error {
	tok, err := t.tokenRef.Resolve()
	if err != nil {
		return fmt.Errorf("seal: transit token: %w", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, t.base+"/v1/"+t.mount+"/"+op+"/"+t.key, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("seal: transit %s failed: status %d", op, resp.StatusCode)
	}
	return json.Unmarshal(rb, out)
}
