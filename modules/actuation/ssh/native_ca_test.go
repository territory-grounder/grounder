package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

func caTestKeyRef(t *testing.T) config.SecretRef {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	blk, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	t.Setenv("TG_TEST_ACTUATION_KEY", string(pem.EncodeToMemory(blk)))
	return config.SecretRef("env:TG_TEST_ACTUATION_KEY")
}

func caTestKnownHosts(t *testing.T, host string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen host key: %v", err)
	}
	sshPub, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("host pubkey: %v", err)
	}
	line := host + " " + string(cryptossh.MarshalAuthorizedKey(sshPub))
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return p
}

// TG-423: a certSign FAILURE must fail the actuation CLOSED — the runner must NEVER fall back to the
// long-lived bare key, and must not even dial (the whole point of the short-lived cert is defeated by a
// silent bare-key fallback). KILLING MUTATION: change the certSign-error branch to `authSigner = signer`
// (bare-key fallback) → the runner proceeds to dial and this test fails on `dialed`.
func TestNativeRunnerCertSignFailsClosed(t *testing.T) {
	keyRef := caTestKeyRef(t)
	kh := caTestKnownHosts(t, "web01")
	m := New("web01", "svc", &fakeRunner{})
	argv := m.sshArgv([]string{"systemctl", "restart", "nginx"})

	dialed := false
	r := &nativeRunner{
		knownHosts:     kh,
		keyRef:         keyRef,
		connectTimeout: defaultConnectTimeout,
		dial: func(context.Context, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("dial must not be reached")
		},
		certSign: func(_ context.Context, _ cryptossh.PublicKey, principal string) (*cryptossh.Certificate, error) {
			if principal != "svc" {
				t.Errorf("certSign got principal %q, want the actuation identity svc", principal)
			}
			return nil, errors.New("bao unreachable")
		},
	}
	_, err := r.Run(context.Background(), argv, nil)
	if err == nil {
		t.Fatal("a certSign failure must fail the actuation closed")
	}
	if dialed {
		t.Fatal("the runner DIALED after a certSign failure — it must fail closed BEFORE dialing, never fall back to the bare key")
	}
	if !strings.Contains(err.Error(), "ssh-CA certificate signing failed") {
		t.Fatalf("error should name the ssh-CA signing failure, got %q", err.Error())
	}
}

// A nil certSign (the un-armed default) reaches the bare-key dial exactly as before — proving the flag-off
// path is unchanged: same runner, no cert logic, the dial IS attempted with the bare key.
func TestNativeRunnerNilCertSignUsesBareKeyPath(t *testing.T) {
	keyRef := caTestKeyRef(t)
	kh := caTestKnownHosts(t, "web01")
	m := New("web01", "svc", &fakeRunner{})
	argv := m.sshArgv([]string{"systemctl", "restart", "nginx"})

	dialed := false
	r := &nativeRunner{
		knownHosts:     kh,
		keyRef:         keyRef,
		connectTimeout: defaultConnectTimeout,
		certSign:       nil, // un-armed
		dial: func(context.Context, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("stop here — we only assert the bare-key path reaches dial")
		},
	}
	_, _ = r.Run(context.Background(), argv, nil)
	if !dialed {
		t.Fatal("with no certSign the runner must take the bare-key path and dial, exactly as before TG-423")
	}
}
