package vault

// TG-415 — THE CLIENT'S OWN TRANSPORT MUST BE DECORATABLE, OR OPENBAO TRAFFIC IS UNMETERABLE.
//
// vault.New builds its own http.Transport whenever a CA path or cert auth is configured, because the TLS
// config has to live somewhere. The consequence was that OpenBao calls never touched
// http.DefaultTransport — which is where the TG-160 egress meter installs — so they were not counted, not
// named on first sighting, and not blocked, whatever TG_EGRESS_MODE said.
//
// Measured on the live grounder, 2026-08-07, in the SAME SECOND the boot log resolved four bao: refs:
//
//	tg_egress_enforcing{component="grounder"}        1
//	tg_egress_allowlist_rules{component="grounder"}  15
//	tg_egress_requests_total{component="grounder"}   0
//
// An enforcing meter with a zero numerator reads as a clean estate on every dashboard. That is the defect;
// the missing count is only how it was noticed.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCA emits a throwaway self-signed CA so the custom-transport branch is actually taken. Without a
// readable CA (or cert auth) `custom` stays false, New leaves Transport nil, and this whole file would be
// asserting against the path that was never broken.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tg-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

// THE LOAD-BEARING ORACLE.
//
// KILLING MUTATION: delete the `if cfg.TransportWrap != nil { rt = cfg.TransportWrap(rt) }` branch in
// vault.New. RED — and note it must be THAT branch, not the assignment, because removing the assignment
// entirely would break TLS and fail for an unrelated reason.
func TestTheClientsOwnTransportIsHandedToTheWrap(t *testing.T) {
	ca := writeTestCA(t)

	var wrapped bool
	var sawInner http.RoundTripper
	c, err := New(Config{
		BaseURL:    "https://openbao.example.test:8200",
		Auth:       Token{TokenRef: "env:NOPE"},
		CACertPath: ca,
		TransportWrap: func(next http.RoundTripper) http.RoundTripper {
			wrapped = true
			sawInner = next
			return next
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned no client")
	}
	if !wrapped {
		t.Fatal("TransportWrap was never called. vault.New built its own http.Transport and handed it to " +
			"nobody — which is exactly the state that made every OpenBao call invisible to the egress " +
			"meter, while tg_egress_enforcing read 1.")
	}
	// The wrap must receive the REAL TLS-carrying transport, not nil. A nil inner would mean the meter
	// decorates http.DefaultTransport instead, silently dropping the CA config and the mTLS identity.
	if sawInner == nil {
		t.Error("TransportWrap received a nil transport — the CA/mTLS configuration would be lost, which " +
			"trades a metering gap for an authentication failure")
	}
	if _, ok := sawInner.(*http.Transport); !ok {
		t.Errorf("TransportWrap received %T, want the *http.Transport carrying the TLS config", sawInner)
	}
}

// WITHOUT A CA OR CERT AUTH THERE IS NOTHING TO WRAP, and that must stay true.
//
// On that path New leaves Transport nil, which resolves to http.DefaultTransport at CALL time — already
// the meter. Wrapping there too would double-count every request, so the absence is deliberate rather
// than an oversight, and this pins it.
func TestAPlainClientLeavesTheDefaultTransportAloneSoItIsNotDoubleCounted(t *testing.T) {
	var wrapped bool
	if _, err := New(Config{
		BaseURL:       "https://openbao.example.test:8200",
		Auth:          Token{TokenRef: "env:NOPE"},
		TransportWrap: func(next http.RoundTripper) http.RoundTripper { wrapped = true; return next },
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if wrapped {
		t.Error("a client with no CA and no cert auth wrapped its transport. That path leaves Transport nil " +
			"so it already resolves to the metered http.DefaultTransport; wrapping again counts every " +
			"request twice, and an inflated egress volume is a false exfil signal.")
	}
}

// A NIL WRAP IS THE UNCHANGED BEHAVIOUR. Every pre-existing caller passes none, and none of them may
// change shape because of this field.
func TestANilWrapBuildsTheSameClientAsBefore(t *testing.T) {
	ca := writeTestCA(t)
	c, err := New(Config{BaseURL: "https://openbao.example.test:8200", Auth: Token{TokenRef: "env:NOPE"}, CACertPath: ca})
	if err != nil {
		t.Fatalf("New with no wrap must still build a client: %v", err)
	}
	if c == nil {
		t.Fatal("New returned no client")
	}
}
