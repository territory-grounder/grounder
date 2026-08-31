package sshca

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

// --- test doubles -------------------------------------------------------------------------------------

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func genSigner(t *testing.T) cryptossh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	s, err := cryptossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

// fakeBao is a fake OpenBao ssh secret engine: on POST sign/<role> it parses the presented public key, mints
// a DISTINCT cert (incrementing serial) signed by its own in-process CA, and returns it as signed_key. It
// records call count so the oracle can assert an unreachable-role never signs.
type fakeBao struct {
	ca   cryptossh.Signer
	mu   sync.Mutex
	sn   uint64
	seen int
}

func (b *fakeBao) doer() doerFunc {
	return func(r *http.Request) (*http.Response, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.seen++
		var req struct {
			PublicKey       string `json:"public_key"`
			ValidPrincipals string `json:"valid_principals"`
			CertType        string `json:"cert_type"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &req); err != nil {
			return jsonResp(400, `{"errors":["bad body"]}`), nil
		}
		userPub, _, _, _, err := cryptossh.ParseAuthorizedKey([]byte(req.PublicKey))
		if err != nil {
			return jsonResp(400, `{"errors":["bad public_key"]}`), nil
		}
		b.sn++
		cert := &cryptossh.Certificate{
			Key:             userPub,
			Serial:          b.sn,
			CertType:        cryptossh.UserCert,
			KeyId:           "tg-actuate",
			ValidPrincipals: strings.Split(req.ValidPrincipals, ","),
			ValidAfter:      1_000,
			ValidBefore:     1_300,
		}
		if err := cert.SignCert(rand.Reader, b.ca); err != nil {
			return jsonResp(500, `{"errors":["sign failed"]}`), nil
		}
		signed := strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(cert)))
		respBody, _ := json.Marshal(map[string]any{"data": map[string]string{"signed_key": signed}})
		return jsonResp(200, string(respBody)), nil
	}
}

func testEngine(t *testing.T, d Doer) *Engine {
	t.Helper()
	t.Setenv("SSHCA_TEST_TOKEN", "test-root-token")
	e, err := New(Config{
		BaseURL:  "https://bao.example:8200",
		Role:     "tg-actuate",
		TokenRef: config.SecretRef("env:SSHCA_TEST_TOKEN"),
		HTTP:     d,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// --- oracle -------------------------------------------------------------------------------------------

// Sign must mint a DISTINCT, CA-signed certificate that binds the presented public key and the requested
// principal. KILLING MUTATION: return a cached cert / drop the SignCert → serials repeat or the cert fails
// CheckSignature, and this fails.
func TestSignMintsDistinctCASignedCert(t *testing.T) {
	bao := &fakeBao{ca: genSigner(t)}
	eng := testEngine(t, bao.doer())
	user := genSigner(t)

	c1, err := eng.Sign(context.Background(), user.PublicKey(), []string{"tg-actuate"})
	if err != nil {
		t.Fatalf("sign 1: %v", err)
	}
	c2, err := eng.Sign(context.Background(), user.PublicKey(), []string{"tg-actuate"})
	if err != nil {
		t.Fatalf("sign 2: %v", err)
	}
	if c1.Serial == c2.Serial {
		t.Fatalf("two signs returned the same serial %d — not minting per request", c1.Serial)
	}
	// The cert must bind the presented user key.
	if string(c1.Key.Marshal()) != string(user.PublicKey().Marshal()) {
		t.Fatal("the signed cert does not carry the presented public key")
	}
	// The cert must be signed by the CA the fake used — verify the signature, not just its presence.
	checker := &cryptossh.CertChecker{
		// Fixed clock inside the fake's ValidAfter/ValidBefore window [1000,1300] — deterministic, and we are
		// asserting the SIGNATURE + principal binding, not wall-clock validity.
		Clock: func() time.Time { return time.Unix(1_100, 0) },
		IsUserAuthority: func(auth cryptossh.PublicKey) bool {
			return string(auth.Marshal()) == string(bao.ca.PublicKey().Marshal())
		},
	}
	if err := checker.CheckCert("tg-actuate", c1); err != nil {
		t.Fatalf("the returned cert does not verify against the signing CA for its principal: %v", err)
	}
	if got := c1.ValidPrincipals; len(got) != 1 || got[0] != "tg-actuate" {
		t.Fatalf("valid_principals = %v, want [tg-actuate]", got)
	}
}

func TestSignFailsClosed(t *testing.T) {
	user := genSigner(t)
	t.Run("non-2xx is an error, body not echoed", func(t *testing.T) {
		eng := testEngine(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(403, `{"errors":["permission denied: secret-leaked"]}`), nil
		}))
		_, err := eng.Sign(context.Background(), user.PublicKey(), []string{"x"})
		if err == nil {
			t.Fatal("a 403 must fail closed")
		}
		if strings.Contains(err.Error(), "secret-leaked") || strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("error echoed the response body (INV-13 leak): %q", err.Error())
		}
		if !strings.Contains(err.Error(), "403") {
			t.Fatalf("error should name the status, got %q", err.Error())
		}
	})
	t.Run("empty signed_key is an error", func(t *testing.T) {
		eng := testEngine(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, `{"data":{"signed_key":""}}`), nil
		}))
		if _, err := eng.Sign(context.Background(), user.PublicKey(), []string{"x"}); err == nil {
			t.Fatal("an empty signed_key must fail closed")
		}
	})
	t.Run("a bare public key (not a certificate) is refused", func(t *testing.T) {
		bare := strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(user.PublicKey())))
		body, _ := json.Marshal(map[string]any{"data": map[string]string{"signed_key": bare}})
		eng := testEngine(t, doerFunc(func(*http.Request) (*http.Response, error) {
			return jsonResp(200, string(body)), nil
		}))
		if _, err := eng.Sign(context.Background(), user.PublicKey(), []string{"x"}); err == nil {
			t.Fatal("a bare public key where a certificate was expected must be refused, never presented as a credential")
		}
	})
	t.Run("New fails closed on missing role/token/base", func(t *testing.T) {
		if _, err := New(Config{BaseURL: "https://b", TokenRef: "env:X"}); err == nil {
			t.Error("missing role must error")
		}
		if _, err := New(Config{BaseURL: "https://b", Role: "r"}); err == nil {
			t.Error("missing token ref must error")
		}
		if _, err := New(Config{Role: "r", TokenRef: "env:X"}); err == nil {
			t.Error("missing base URL must error")
		}
	})
}
