package hostdiag

// ORACLES FOR THE TEST BUTTON'S BACKEND (TG-265 / TG-271).
//
// Each one re-creates a failure this connector ACTUALLY HAD in production on 2026-08-03 and proves the probe
// names it. A probe that cannot fail on the real failure modes is the disease this repo documents — present,
// green, and useless.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
	"golang.org/x/crypto/ssh"
)

func writeTestKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeKnownHosts(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// a real public-key line so the knownhosts parser accepts the file
func knownHostLine(t *testing.T, host string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func probeAccess(t *testing.T, keyPath string) []Access {
	t.Helper()
	return ParseAccess("dc1|dc1*|root|file:" + keyPath)
}

// The green path — and the summary must carry the ENTRY COUNT, because per-host coverage is the failure an
// operator can only catch by comparing that number against their estate (16-of-38, TG-271).
func TestAHealthyConfigReportsTheEntryCountNotABareOk(t *testing.T) {
	kh := writeKnownHosts(t, knownHostLine(t, "dc1pve01"), knownHostLine(t, "dc1pve02"))
	m := NewModule(probeAccess(t, writeTestKey(t)), kh)
	res, err := m.SelfTest(context.Background(), "")
	if err != nil {
		t.Fatalf("healthy config failed the probe: %v", err)
	}
	if !strings.Contains(res.Summary, "2 host-key entries") {
		t.Fatalf("summary %q does not report the entry count — a bare ok hides the coverage class of failure", res.Summary)
	}
}

// KILLING MUTATION: skip the ParsePrivateKey step (resolve only). RED — the production key sat at 0640 and
// resolution succeeds on it; only a parse distinguishes "the reference points somewhere" from "this key
// can authenticate".
func TestAKeyRefThatResolvesButIsNotAKeyFails(t *testing.T) {
	notAKey := filepath.Join(t.TempDir(), "notakey")
	if err := os.WriteFile(notAKey, []byte("this is not key material"), 0o600); err != nil {
		t.Fatal(err)
	}
	kh := writeKnownHosts(t, knownHostLine(t, "dc1pve01"))
	m := NewModule(probeAccess(t, notAKey), kh)
	if _, err := m.SelfTest(context.Background(), ""); err == nil {
		t.Fatal("a key reference resolving to non-key bytes passed the probe — the agent would fail on every host")
	} else if !strings.Contains(err.Error(), "did not parse") {
		t.Fatalf("failure does not name the parse: %v", err)
	}
}

// KILLING MUTATION: drop the zero-entry floor. RED — an empty-but-parseable known_hosts refuses every read
// on every host, which is byte-for-byte the silent state TG-271 found, at 100% instead of 58%.
func TestAnEmptyKnownHostsIsAFailureNotAGreen(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(empty, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewModule(probeAccess(t, writeTestKey(t)), empty)
	if _, err := m.SelfTest(context.Background(), ""); err == nil {
		t.Fatal("a known_hosts with zero entries passed — every diagnostic read would be refused, silently")
	} else if !strings.Contains(err.Error(), "ZERO entries") {
		t.Fatalf("failure does not name the empty file: %v", err)
	}
}

// KILLING MUTATION: probe only the first row. RED — one row per site; a dead second site is a silent
// partial, the same shape the syslog-ng probe refuses to bless.
func TestEveryRowIsProbedNotJustTheFirst(t *testing.T) {
	good := writeTestKey(t)
	kh := writeKnownHosts(t, knownHostLine(t, "dc1pve01"))
	accs := ParseAccess("dc1|dc1*|root|file:" + good + ";dc2|dc2*|root|file:/does/not/exist")
	if len(accs) != 2 {
		t.Fatalf("fixture expects 2 rows, got %d", len(accs))
	}
	m := NewModule(accs, kh)
	if _, err := m.SelfTest(context.Background(), ""); err == nil {
		t.Fatal("a dead second row passed the probe — that site's diagnostics are gone and nothing says so")
	} else if !strings.Contains(err.Error(), "dc2*") {
		t.Fatalf("failure does not name the dead row: %v", err)
	}
}

// The module must not read the environment: the composition root passes the path it resolved through the
// config chokepoint, and a module-side env read is exactly the TG-260 bypass this change removes.
func TestTheProbeNeverReadsTheEnvironment(t *testing.T) {
	t.Setenv(KnownHostsEnv, writeKnownHosts(t, knownHostLine(t, "planted-by-env")))
	m := NewModule(probeAccess(t, writeTestKey(t)), "") // empty path passed by the root
	if _, err := m.SelfTest(context.Background(), ""); err == nil {
		t.Fatal("with no known_hosts passed in, the probe went green — it read the environment behind the resolver's back")
	}
}

var _ = config.SecretRef("") // keep the import honest if ParseAccess's signature evolves
