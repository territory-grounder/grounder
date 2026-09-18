package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/territory-grounder/grounder/core/config"
)

// TG-320 / spec/022 T-022-3 — THE ACTUATION KEY MUST RESOLVE JUST-IN-TIME, ONCE PER DIAL.
//
// TG-320 asks for lease-aware dynamic credentials (OpenBao `database` and `ssh` engines). Measured
// 2026-08-07, that build cannot start: neither engine is mounted (`bao secrets list` returns exactly
// cubbyhole/, identity/, secret/), OpenBao cannot reach Postgres at all (5432 is compose-network-only,
// no published host port), no Postgres role has CREATEROLE for it to mint leases with, and true ssh-otp
// needs `vault-ssh-helper` installed on every target. Four blockers, none of them a code problem.
//
// What IS in the repo's hands is the property every one of those futures depends on: the key reference
// must be resolved INSIDE Run, per dial — never cached on the runner at construction. A lease-aware
// resolver is only meaningful if something asks it again; the moment a signer is hoisted into
// NewNativeRunner, every credential this process uses is pinned for the process's lifetime and no TTL,
// renewal or revocation can ever take effect. Today that property holds by accident of where one line
// sits (native.go:88). This binds it.
//
// The resolve COUNT is the observable proxy for "no key material is retained". A runner that kept a
// signer would not resolve twice, so a count of 2 across 2 Runs is exactly the fact worth pinning — and
// it is the one a hoist mutation destroys.

// jitCountingKeyRef installs a SecretRef scheme that counts resolutions and returns a real, parseable
// ed25519 private key. Named distinctively: this package has other tests and a generic helper name would
// collide.
func jitCountingKeyRef(t *testing.T) (config.SecretRef, *atomic.Int64) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	blk, err := cryptossh.MarshalPrivateKey(priv, "tg315-jit-oracle")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	material := string(pem.EncodeToMemory(blk))

	var calls atomic.Int64
	config.RegisterSchemeResolver("tgjitcount", func(string) (string, error) {
		calls.Add(1)
		return material, nil
	})
	t.Cleanup(func() { config.RegisterSchemeResolver("tgjitcount", nil) })
	return config.SecretRef("tgjitcount:actuation-key"), &calls
}

// jitKnownHosts writes a syntactically valid known_hosts so Run gets PAST host-key setup and reaches the
// key resolution. The dial itself then fails — there is no server — which is fine and expected: this
// oracle is about WHEN the key is resolved, not about completing a session.
func jitKnownHosts(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	sshPub, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	line := cryptossh.MarshalAuthorizedKey(sshPub)
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, append([]byte("127.0.0.1 "), line...), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return p
}

// THE ORACLE. Two Runs must resolve the key TWICE.
func TestTheActuationKeyResolvesOncePerRunAndIsNotCachedOnTheRunner(t *testing.T) {
	ref, calls := jitCountingKeyRef(t)
	r := NewNativeRunner(jitKnownHosts(t), ref)

	// The argv must be the CANONICAL vector parseSSHArgv accepts — "ssh" + sshCanonicalOpts +
	// identity@host + remote command. A shorter one fails closed before the key is ever resolved, which
	// would make this oracle report 0 resolutions and look like the defect it is testing for. (It did,
	// on the first run: the guard caught my synthetic argv, not a violated property.)
	argv := append([]string{"ssh"}, sshCanonicalOpts...)
	argv = append(argv, "root@127.0.0.1", "true")

	// Both Runs fail at the DIAL (nothing is listening). That is expected and irrelevant — the key
	// resolution happens before the dial, and the count is what this asserts.
	for i := 0; i < 2; i++ {
		_, _ = r.Run(context.Background(), argv, nil)
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("the actuation key ref resolved %d time(s) across 2 Runs, want 2.\n"+
			"A count below 2 means a signer is cached on the runner — which pins the credential for the "+
			"whole process lifetime and makes every lease-aware future (TG-320's OpenBao ssh/database "+
			"engines, spec/022 T-022-3's bounded-lifetime lease) structurally impossible: nothing would "+
			"ever ask the resolver again, so no TTL, renewal or revocation could take effect.", got)
	}
}

// The mirror: a construction that never dials must never resolve the key either. Without this, the
// oracle above is satisfied by a runner that resolves eagerly AND per-dial.
func TestConstructingARunnerResolvesNoKeyMaterial(t *testing.T) {
	ref, calls := jitCountingKeyRef(t)
	_ = NewNativeRunner(jitKnownHosts(t), ref)

	if got := calls.Load(); got != 0 {
		t.Errorf("NewNativeRunner resolved the key %d time(s) before any dial — key material must not be "+
			"read, parsed or held at construction (INV-13: resolved at use, never retained)", got)
	}
}
