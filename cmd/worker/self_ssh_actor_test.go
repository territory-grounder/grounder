package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/modules/actorevidence/journal"
)

// writeKey mints a real OpenSSH ed25519 private key on disk and returns (path, publicKey).
func writeKey(t *testing.T) (string, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	return path, sshPub
}

func envGetter(kv map[string]string) func(k, def string) string {
	return func(k, def string) string {
		if v, ok := kv[k]; ok {
			return v
		}
		return def
	}
}

// ★ THE EDGE THAT JOINS THE LAW TO THE PROOF, AND THE ONE THIS CHANGE LIVES OR DIES ON.
//
// Two independent pieces of code have to agree on one string: the composition root DERIVES TG's own identity
// from its actuation key, and the journal reader PARSES the identity out of what sshd wrote. If those ever
// disagree by a character, TG stops recognising its own heals — every SSH remediation it performs becomes
// `attributed-suspicious`, which is a SECURITY escalation on itself, and suspicion masks every other
// candidate. That is a strictly worse failure than the blindness this change removes.
//
// So the oracle is a ROUND TRIP over a REAL key: derive the actor, synthesise the exact line sshd writes for
// that key, parse it back through the real reader, and require equality. Asserting each side against a
// hand-written literal would let both drift together, which is how this class of defect survives a green
// suite.
func TestTheDerivedSelfIdentityIsEXACTLYWhatSSHDLogs(t *testing.T) {
	path, pub := writeKey(t)
	self := resolveSelfSSHActor(envGetter(map[string]string{
		"TG_ACTUATION_SSH_KEY": "file:" + path,
	}))
	if self == "" {
		t.Fatal("no self identity derived from a valid actuation key")
	}
	if !strings.HasPrefix(self, "root!SHA256:") {
		t.Errorf("self identity %q is not the user!SHA256:fp shape sshd logs", self)
	}

	// The line sshd writes when THIS key authenticates. FingerprintSHA256 is the same function sshd's
	// fingerprint is computed with, so this is the real wire format, not an approximation of it.
	at := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	line := `{"SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd","MESSAGE":"Accepted publickey for root from 10.30.1.9 port 4242 ssh2: ED25519 ` +
		ssh.FingerprintSHA256(pub) + `","__REALTIME_TIMESTAMP":"` + itoaTest(at.UnixMicro()) + `","__CURSOR":"c1"}`

	got := journal.ParseForTest([]byte(line), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(got) != 1 {
		t.Fatalf("the reader produced %d evidence rows for a real sshd line, want 1", len(got))
	}
	if got[0].Actor != self {
		t.Fatalf("SELF-RECOGNITION IS BROKEN: the composition root derives %q from the actuation key, and the "+
			"reader parses %q out of the line sshd writes for that same key. TG would escalate on its own "+
			"heals as attributed-suspicious.", self, got[0].Actor)
	}
}

// A DIFFERENT KEY IS A DIFFERENT ACTOR. If any key resolved to TG's identity, the self-recognition would be a
// blanket amnesty: a stranger's change on the host would be excused as TG's own work.
func TestAnotherKeyIsNotTGsIdentity(t *testing.T) {
	mine, _ := writeKey(t)
	self := resolveSelfSSHActor(envGetter(map[string]string{"TG_ACTUATION_SSH_KEY": "file:" + mine}))

	_, otherPub := writeKey(t)
	at := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	line := `{"SYSLOG_IDENTIFIER":"sshd","_COMM":"sshd","MESSAGE":"Accepted publickey for root from 10.30.9.9 port 1 ssh2: ED25519 ` +
		ssh.FingerprintSHA256(otherPub) + `","__REALTIME_TIMESTAMP":"` + itoaTest(at.UnixMicro()) + `","__CURSOR":"c2"}`

	got := journal.ParseForTest([]byte(line), "guest-a", at.Add(-time.Hour), at.Add(time.Hour), true)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Actor == self {
		t.Fatal("a DIFFERENT key resolved to TG's own identity — self-recognition would excuse a stranger's " +
			"change as TG's own work, which is the failure direction that matters")
	}
}

// IT FAILS CLOSED. An absent, unreadable or unparseable key yields NO identity rather than a guess — a
// fabricated self-identity is an amnesty for whatever happens to match it. The boot-time config-gap report
// then states the domain has none, which is the honest outcome.
func TestAnUnusableKeyYieldsNoIdentityRatherThanAGuess(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "not-a-key")
	if err := os.WriteFile(junk, []byte("this is not a private key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"unset":        {},
		"empty ref":    {"TG_ACTUATION_SSH_KEY": ""},
		"missing file": {"TG_ACTUATION_SSH_KEY": "file:" + filepath.Join(t.TempDir(), "absent")},
		"not a key":    {"TG_ACTUATION_SSH_KEY": "file:" + junk},
	} {
		if got := resolveSelfSSHActor(envGetter(env)); got != "" {
			t.Errorf("%s produced a self identity %q — an unusable credential must yield NO identity, because "+
				"a fabricated one is an amnesty for whatever matches it", name, got)
		}
	}
}

// The login name is operator-declared and must be honoured: an estate whose actuation user is not root would
// otherwise have a self-identity that never matches its own sshd lines.
func TestTheActuationLoginNameIsHonoured(t *testing.T) {
	path, pub := writeKey(t)
	self := resolveSelfSSHActor(envGetter(map[string]string{
		"TG_ACTUATION_SSH_KEY":  "file:" + path,
		"TG_ACTUATION_SSH_USER": "tgactuate",
	}))
	if want := "tgactuate!" + ssh.FingerprintSHA256(pub); self != want {
		t.Errorf("self identity = %q, want %q", self, want)
	}
	// A blank declaration falls back to root rather than producing "!SHA256:…", which would match nothing.
	blank := resolveSelfSSHActor(envGetter(map[string]string{
		"TG_ACTUATION_SSH_KEY":  "file:" + path,
		"TG_ACTUATION_SSH_USER": "   ",
	}))
	if !strings.HasPrefix(blank, "root!") {
		t.Errorf("a blank actuation user produced %q — it must fall back to root, not to an empty login that "+
			"can never match an sshd line", blank)
	}
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
