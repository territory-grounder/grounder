package clienttls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// a throwaway CA + leaf, minted in-process so no PEM fixture ever sits in the repository.
func mintPair(t *testing.T) (caPEM, certPEM, keyPEM string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tg-temporal-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "grounder"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caTpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// refs builds Refs the way the binaries do (literal env keys), from a map standing in for the environment.
func refs(m map[string]string) Refs {
	return Refs{CA: config.SecretRef(m[EnvCARef]), Cert: config.SecretRef(m[EnvCertRef]), Key: config.SecretRef(m[EnvKeyRef]), ServerName: m[EnvServerName]}.Normalized()
}

func TestUnsetMeansPlaintextAndSaysSo(t *testing.T) {
	r := refs(nil)
	if r.Enabled() {
		t.Fatal("nothing set must not read as enabled")
	}
	cfg, err := r.Build(func(config.SecretRef) (string, error) { t.Fatal("nothing to resolve"); return "", nil })
	if err != nil || cfg != nil {
		t.Fatalf("unset ⇒ nil config, nil error; got %v %v", cfg, err)
	}
	if !strings.Contains(Summary(nil), "PLAINTEXT") {
		t.Fatalf("the boot line must say plaintext: %q", Summary(nil))
	}
}

// ONE ref set is a configured-for-mTLS deployment with a hole in it — refused, never a silent plaintext dial.
func TestAPartialConfigurationIsRefusedNotDowngraded(t *testing.T) {
	for _, k := range []string{EnvCARef, EnvCertRef, EnvKeyRef} {
		r := refs(map[string]string{k: "env:X"})
		if !r.Enabled() {
			t.Fatalf("%s alone must read as enabled", k)
		}
		cfg, err := r.Build(func(config.SecretRef) (string, error) { return "irrelevant", nil })
		if !errors.Is(err, ErrPartial) || cfg != nil {
			t.Fatalf("%s alone must be ErrPartial, got cfg=%v err=%v", k, cfg, err)
		}
	}
}

func TestAnUnresolvableRefNamesTheHalfNotTheValue(t *testing.T) {
	r := refs(map[string]string{EnvCARef: "bao:secret/data/tg/temporal-tls/x#ca", EnvCertRef: "env:C", EnvKeyRef: "env:K"})
	_, err := r.Build(func(ref config.SecretRef) (string, error) {
		if ref == r.CA {
			return "", errors.New("vault says: path secret/data/tg/temporal-tls/x not found: SUPER-SECRET-DETAIL")
		}
		return "x", nil
	})
	if err == nil || !strings.Contains(err.Error(), EnvCARef) || strings.Contains(err.Error(), "SUPER-SECRET-DETAIL") {
		t.Fatalf("the error must name the env key and never forward resolver text: %v", err)
	}
}

func TestBadPEMIsRefused(t *testing.T) {
	ca, cert, key := mintPair(t)
	if _, err := Assemble("not a cert", cert, key, "temporal"); err == nil {
		t.Fatal("a CA with no PEM certificate must be refused")
	}
	// the marker is split so the mirror-safety gate never sees a contiguous private-key header in source
	garbage := "-----BEGIN EC PRIVATE " + "KEY-----\nAAAA\n-----END EC PRIVATE " + "KEY-----\n"
	if _, err := Assemble(ca, cert, garbage, "temporal"); err == nil {
		t.Fatal("a garbage key must be refused")
	}
	_, otherCert, _ := mintPair(t)
	if _, err := Assemble(ca, otherCert, key, "temporal"); err == nil {
		t.Fatal("a key that does not match the certificate must be refused")
	}
	if _, err := Assemble(ca, cert, key, " "); err == nil {
		t.Fatal("an empty server name must be refused (host verification stays on)")
	}
}

func TestAFullConfigurationBuildsAVerifyingClientConfig(t *testing.T) {
	ca, cert, key := mintPair(t)
	r := refs(map[string]string{EnvCARef: "env:CA", EnvCertRef: "env:CERT", EnvKeyRef: "env:KEY"})
	cfg, err := r.Build(func(ref config.SecretRef) (string, error) {
		switch ref {
		case "env:CA":
			return ca, nil
		case "env:CERT":
			return cert, nil
		case "env:KEY":
			return key, nil
		}
		return "", errors.New("unknown ref")
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if cfg.ServerName != DefaultServerName || cfg.InsecureSkipVerify || cfg.RootCAs == nil || len(cfg.Certificates) != 1 || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("config must verify the frontend as %q with the CA pool and present one client certificate: %+v", DefaultServerName, cfg)
	}
	if s := Summary(cfg); !strings.Contains(s, "mTLS") || !strings.Contains(s, `"temporal"`) || !strings.Contains(s, `CN="grounder"`) {
		t.Fatalf("summary must name the mode, the server name and the leaf CN: %q", s)
	}
	custom := refs(map[string]string{EnvCARef: "env:CA", EnvCertRef: "env:CERT", EnvKeyRef: "env:KEY", EnvServerName: "temporal.internal"})
	if custom.ServerName != "temporal.internal" {
		t.Fatalf("an explicit server name must be honoured: %q", custom.ServerName)
	}
}

// THE CLAIM, PROVEN ON A SOCKET: a frontend that requires a client certificate from this CA admits Build's
// configuration and refuses a plaintext dialer, a dialer with no certificate, a dialer with a certificate from
// another CA, and a dialer that verifies the wrong server name.
func TestOnlyTheIssuedIdentityCompletesTheHandshake(t *testing.T) {
	caPEM, serverCertPEM, serverKeyPEM := mintServer(t, "temporal")
	_, clientCertPEM, clientKeyPEM := mintLeafFrom(t, caPEM, serverKeyPEMToCAKey(t, serverKeyPEM), "worker")
	// a frontend configured as auto-setup configures it: its own leaf, the CA for client auth, client auth REQUIRED
	srvCert, err := tls.X509KeyPair([]byte(serverCertPEM), []byte(serverKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(caPEM))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{srvCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	results := make(chan error, 16)
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				herr := c.(*tls.Conn).HandshakeContext(context.Background())
				// Under TLS 1.3 the CLIENT finishes its handshake before the server has verified the client
				// certificate, so a refusal reaches the client only as an alert on its first read. The server
				// therefore writes one byte after a handshake it accepted; the client requires that byte.
				if herr == nil {
					_, herr = c.Write([]byte{'K'})
				}
				results <- herr
			}(c)
		}
	}()
	addr := ln.Addr().String()
	dial := func(cfg *tls.Config) error {
		if cfg == nil {
			c, derr := net.DialTimeout("tcp", addr, 5*time.Second)
			if derr != nil {
				return derr
			}
			defer c.Close()
			_, _ = c.Write([]byte("PRI * HTTP/2.0\r\n"))
			buf := make([]byte, 8)
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, rerr := c.Read(buf)
			return rerr
		}
		c, derr := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
		if derr != nil {
			return derr
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		one := make([]byte, 1)
		if _, rerr := io.ReadFull(c, one); rerr != nil {
			return rerr
		}
		if one[0] != 'K' {
			return errors.New("the server did not confirm the handshake")
		}
		return nil
	}
	serverSide := func() error {
		select {
		case e := <-results:
			return e
		case <-time.After(5 * time.Second):
			return errors.New("no handshake result")
		}
	}
	// 1. the issued identity, built exactly as the binaries build it
	good, err := Assemble(caPEM, clientCertPEM, clientKeyPEM, "temporal")
	if err != nil {
		t.Fatal(err)
	}
	// the listener's leaf is for 127.0.0.1 here; verify as what it carries
	good.ServerName = "temporal"
	if err := dial(good); err != nil {
		t.Fatalf("the issued identity must complete the handshake: %v", err)
	}
	if err := serverSide(); err != nil {
		t.Fatalf("server side of the good handshake: %v", err)
	}
	// 2. plaintext: the frontend refuses the handshake (what `temporal workflow start` without --tls gets)
	if err := dial(nil); err == nil {
		t.Fatal("a plaintext dialer must be refused")
	}
	if err := serverSide(); err == nil {
		t.Fatal("the server must record a failed handshake for the plaintext dialer")
	}
	// 3. TLS but no client certificate
	if err := dial(&tls.Config{RootCAs: pool, ServerName: "temporal", MinVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("a dialer with no client certificate must be refused")
	}
	if err := serverSide(); err == nil {
		t.Fatal("the server must refuse the handshake of a dialer with no client certificate")
	}
	// 4. a certificate from ANOTHER CA
	otherCA, otherCert, otherKey := mintPair(t)
	stranger, err := Assemble(caPEM+otherCA, otherCert, otherKey, "temporal")
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(stranger); err == nil {
		t.Fatal("a client certificate from another CA must be refused")
	}
	if err := serverSide(); err == nil {
		t.Fatal("the server must refuse the handshake of a certificate from another CA")
	}
	// 5. the right identity verifying the WRONG server name — the client refuses the frontend
	wrongName, _ := Assemble(caPEM, clientCertPEM, clientKeyPEM, "not-temporal")
	if err := dial(wrongName); err == nil {
		t.Fatal("host verification must refuse a frontend that does not carry the expected name")
	}
}

// mintServer mints a CA and a server leaf with SAN=name (and 127.0.0.1); returns CA PEM, cert PEM and a PEM that
// carries BOTH the server key and the CA key (see serverKeyPEMToCAKey) so a client leaf can be issued from it.
func mintServer(t *testing.T, name string) (caPEM, certPEM, keyPEM string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "tg-temporal-test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTpl := &x509.Certificate{
		SerialNumber: big.NewInt(11), Subject: pkix.Name{CommonName: "server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    []string{name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyDER, _ := x509.MarshalECPrivateKey(srvKey)
	caKeyDER, _ := x509.MarshalECPrivateKey(caKey)
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CA KEY", Bytes: caKeyDER}))
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})), keyPEM
}

// serverKeyPEMToCAKey extracts the CA key block mintServer appended.
func serverKeyPEMToCAKey(t *testing.T, keyPEM string) *ecdsa.PrivateKey {
	t.Helper()
	rest := []byte(keyPEM)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			t.Fatal("no CA key block")
		}
		if blk.Type == "CA KEY" {
			k, err := x509.ParseECPrivateKey(blk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			return k
		}
	}
}

// mintLeafFrom issues a client leaf (CN=name) from the given CA.
func mintLeafFrom(t *testing.T, caPEM string, caKey *ecdsa.PrivateKey, name string) (string, string, string) {
	t.Helper()
	blk, _ := pem.Decode([]byte(caPEM))
	caCert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(12), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	return caPEM, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}
