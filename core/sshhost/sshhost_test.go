package sshhost

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// knownHostsWith writes a known_hosts file pinning exactly the given keys for host.
func knownHostsWith(t *testing.T, host string, keys ...ssh.PublicKey) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "known_hosts")
	var buf []byte
	for _, k := range keys {
		buf = append(buf, []byte(host+" "+k.Type()+" "+base64Of(k)+"\n")...)
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func base64Of(k ssh.PublicKey) string {
	line := ssh.MarshalAuthorizedKey(k) // "type base64\n"
	s := string(line)
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return trimNL(s[i+1:])
		}
	}
	return ""
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func ed25519Key(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ecdsaKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// THE PRODUCTION FAILURE, as an oracle.
//
// known_hosts pins ONLY ssh-ed25519 for the host. Go's default algorithm order puts ECDSA and RSA ahead of
// Ed25519, so a client that leaves HostKeyAlgorithms unset negotiates ECDSA against any stock OpenSSH
// server and is told the host identification has CHANGED — for a server whose key never changed.
//
// KILLING MUTATION: make Algorithms return nil. RED — the advertised set no longer constrains negotiation
// to what is pinned, which is precisely the shipped bug.
func TestOnlyPinnedAlgorithmsAreAdvertised(t *testing.T) {
	host := "syslog01.example"
	pinned := ed25519Key(t)
	v, err := New(knownHostsWith(t, host, pinned))
	if err != nil {
		t.Fatal(err)
	}
	got := v.Algorithms(host + ":22")
	if len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("advertised %v, want exactly [%s] — anything else lets negotiation land on an algorithm "+
			"the operator never pinned, which reports an unmodified server as a host-key MISMATCH",
			got, ssh.KeyAlgoED25519)
	}
	// And the callback itself must still be the real verifier: a DIFFERENT ed25519 key is refused.
	if err := v.Callback(host+":22", probeAddr, ed25519Key(t)); err == nil {
		t.Fatal("a key that is not the pinned one was accepted — constraining the algorithm list must not " +
			"weaken verification")
	}
	// The pinned key itself verifies.
	if err := v.Callback(host+":22", probeAddr, pinned); err != nil {
		t.Fatalf("the pinned key was refused: %v", err)
	}
}

// A host pinned with several algorithms advertises all of them, so a server that has since dropped one can
// still be reached on another.
func TestEveryPinnedAlgorithmIsOffered(t *testing.T) {
	host := "multi.example"
	v, err := New(knownHostsWith(t, host, ed25519Key(t), ecdsaKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	got := v.Algorithms(host + ":22")
	if len(got) != 2 {
		t.Fatalf("advertised %v, want both pinned algorithms", got)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	if !seen[ssh.KeyAlgoED25519] || !seen[ssh.KeyAlgoECDSA256] {
		t.Errorf("advertised %v, want ed25519 and ecdsa-p256", got)
	}
}

// An UNKNOWN host advertises nothing, so the connection is refused by the callback with the honest
// "unknown host" error rather than by an invented algorithm list.
func TestAnUnknownHostAdvertisesNothing(t *testing.T) {
	v, err := New(knownHostsWith(t, "known.example", ed25519Key(t)))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Algorithms("stranger.example:22"); got != nil {
		t.Errorf("advertised %v for a host that is not pinned at all", got)
	}
	if err := v.Callback("stranger.example:22", probeAddr, ed25519Key(t)); err == nil {
		t.Error("an unpinned host was accepted")
	}
}

// Apply sets BOTH fields. A config carrying a callback but no algorithm list is the shipped bug.
//
// KILLING MUTATION: drop the HostKeyAlgorithms assignment from Apply. RED.
func TestApplySetsBothFieldsTogether(t *testing.T) {
	host := "pair.example"
	v, err := New(knownHostsWith(t, host, ed25519Key(t)))
	if err != nil {
		t.Fatal(err)
	}
	var cfg ssh.ClientConfig
	v.Apply(&cfg, host+":22")
	if cfg.HostKeyCallback == nil {
		t.Error("Apply left HostKeyCallback unset — the connection would be unverified")
	}
	if len(cfg.HostKeyAlgorithms) == 0 {
		t.Error("Apply left HostKeyAlgorithms unset — negotiation can land on an unpinned algorithm and " +
			"report an unmodified server as a MISMATCH")
	}
}

// An empty path is refused rather than defaulted: connecting unverified is never the safe fallback.
func TestNoKnownHostsFileIsRefused(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("an empty known_hosts path was accepted")
	}
}
