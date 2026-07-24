package ssh

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/config"
)

// TestNativeRunnerLiveDryRun exercises the native crypto/ssh runner against a REAL sshd — the lab dry-run
// that closes "live actuation has never been exercised" before any production canary. It is gated on
// TG_ACTUATION_LAB_HOST so it never runs in CI (no live host there); when set, it connects to a THROWAWAY
// target, verifies its host key against the given known_hosts, authenticates key-only, and runs a benign
// echo through the module's canonical POSIX-quoted path — proving connect + host-key-verify + auth + exec
// all work in-process (no ssh binary), which is the whole reason this runner exists for the distroless
// worker. Mutation is NOT involved: a read-only Module (no gate) just transports the argv.
func TestNativeRunnerLiveDryRun(t *testing.T) {
	host := os.Getenv("TG_ACTUATION_LAB_HOST")
	if host == "" {
		t.Skip("set TG_ACTUATION_LAB_HOST (+ _USER/_KEY/_KNOWN_HOSTS) to run the live native-SSH dry-run")
	}
	m := New(host, os.Getenv("TG_ACTUATION_LAB_USER"),
		NewNativeRunner(os.Getenv("TG_ACTUATION_LAB_KNOWN_HOSTS"), config.SecretRef(os.Getenv("TG_ACTUATION_LAB_KEY"))))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := m.Exec(ctx, []string{"echo", "tg-actuation-canary-ok"}, nil)
	if err != nil {
		t.Fatalf("native runner dry-run failed (connect/host-key/auth/exec): %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "tg-actuation-canary-ok") {
		t.Fatalf("unexpected dry-run result: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Logf("native SSH dry-run OK: in-process connect + host-key-verified + key-authed + ran echo → %q",
		strings.TrimSpace(string(res.Stdout)))

	// A WRONG host key must fail closed: point known_hosts at an empty file ⇒ the runner refuses (never an
	// unverified connection). This proves the host-key gate is live end to end, not just in unit tests.
	empty := t.TempDir() + "/empty_known_hosts"
	_ = os.WriteFile(empty, []byte{}, 0o600)
	m2 := New(host, os.Getenv("TG_ACTUATION_LAB_USER"),
		NewNativeRunner(empty, config.SecretRef(os.Getenv("TG_ACTUATION_LAB_KEY"))))
	if _, err := m2.Exec(ctx, []string{"echo", "should-not-run"}, nil); err == nil {
		t.Fatal("an empty/mismatched known_hosts must refuse the connection (host-key verification is live)")
	}
}
