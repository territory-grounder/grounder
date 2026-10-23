package ssh

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// The native runner must read back EXACTLY the destination + POSIX-quoted command sshArgv builds — the
// round-trip contract that lets the in-process client run the SAME vector the subprocess client would.
func TestParseSSHArgvRoundTripsSSHArgv(t *testing.T) {
	m := New("web01.nl", "svc-agent", &fakeRunner{})
	argv := m.sshArgv([]string{"systemctl", "restart", "nginx"})
	identity, host, remoteCmd, ok := parseSSHArgv(argv)
	if !ok {
		t.Fatalf("canonical sshArgv must parse: %v", argv)
	}
	if identity != "svc-agent" || host != "web01.nl" {
		t.Fatalf("destination mis-parsed: identity=%q host=%q", identity, host)
	}
	if remoteCmd != `'systemctl' 'restart' 'nginx'` {
		t.Fatalf("remote command must be the module's POSIX-quoted word, got %q", remoteCmd)
	}
}

// Defense in depth (security review): a DIRECT call to the public Runner with a non-canonical argv must
// fail closed — the native runner must never dial an attacker-named host or connect with downgraded
// host-key verification just because Run was invoked outside Module.Exec.
func TestParseSSHArgvRejectsMisrouting(t *testing.T) {
	cases := [][]string{
		// short argv naming a different destination (bypasses the canonical prologue entirely)
		{"ssh", "attacker@evil.example", "systemctl restart x"},
		// canonical LENGTH but StrictHostKeyChecking DOWNGRADED to no
		{"ssh", "-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes", "-o", "PasswordAuthentication=no", "svc@web01", "'systemctl' 'restart' 'nginx'"},
		// canonical length but an extra/altered opt shifting the destination
		{"ssh", "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes", "-o", "ProxyJump=evil", "svc@web01", "cmd"},
	}
	for i, argv := range cases {
		if _, _, _, ok := parseSSHArgv(argv); ok {
			t.Errorf("misrouting case %d must fail closed: %v", i, argv)
		}
	}
}

func TestParseSSHArgvRejectsMalformed(t *testing.T) {
	cases := [][]string{
		nil,
		{"ssh"},                             // too short
		{"scp", "-o", "x", "svc@h", "cmd"},  // wrong program
		{"ssh", "-o", "x", "nodest", "cmd"}, // no @ in destination
		{"ssh", "-o", "x", "@host", "cmd"},  // empty identity
		{"ssh", "-o", "x", "svc@", "cmd"},   // empty host
	}
	for i, argv := range cases {
		if _, _, _, ok := parseSSHArgv(argv); ok {
			t.Errorf("case %d must fail closed: %v", i, argv)
		}
	}
}

// The native runner fails closed on a missing known_hosts file (no unverified connection is ever attempted)
// and on a non-canonical argv (no mis-routing) — both refuse BEFORE any dial.
func TestNativeRunnerFailsClosed(t *testing.T) {
	m := New("web01", "svc", &fakeRunner{})
	argv := m.sshArgv([]string{"systemctl", "restart", "nginx"})
	ctx := context.Background()

	if _, err := NewNativeRunner("", config.SecretRef("env:NOPE")).Run(ctx, argv, nil); err == nil {
		t.Fatal("empty known_hosts must fail closed before dialing")
	}
	r := &nativeRunner{knownHosts: "/nonexistent/known_hosts"}
	if _, err := r.Run(ctx, []string{"not-ssh"}, nil); err == nil {
		t.Fatal("a non-canonical argv must fail closed")
	}
}

// An unresolved/empty/invalid actuation key REFERENCE fails closed — never an unauthenticated connection.
func TestParseActuationKeyFailsClosed(t *testing.T) {
	if _, err := parseActuationKey(config.SecretRef("env:TG_TEST_UNSET_ACTUATION_KEY_XYZ")); err == nil {
		t.Fatal("an unresolved key ref must fail closed")
	}
	if _, err := parseActuationKey(config.SecretRef("env:PATH")); err == nil {
		// PATH resolves to a non-key string ⇒ ParsePrivateKey must reject it.
		t.Fatal("a ref that resolves to a non-key must fail closed")
	}
}
