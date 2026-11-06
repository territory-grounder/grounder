package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoSSHClientSetsHostKeyCallbackWithoutItsAlgorithmList is the guard for a production false alarm.
//
// On 2026-08-02 both syslog servers reported a host-key MISMATCH — the alarm that means somebody may be
// impersonating a machine. Neither key had changed. Every SSH client in the tree built its config as
// `HostKeyCallback: hostKeys` and left HostKeyAlgorithms unset, so the client advertised
// golang.org/x/crypto/ssh's default order — ECDSA and RSA AHEAD of Ed25519 — and a stock OpenSSH server
// offering rsa-sha2-512, rsa-sha2-256, ecdsa-sha2-nistp256, ssh-ed25519 negotiated ECDSA. known_hosts
// pinned only ssh-ed25519, so the callback found the host, found no ECDSA key for it, and returned the
// changed-key error.
//
// The two fields are a PAIR: a callback without a matching algorithm list verifies a key the server was
// never going to present. This guard makes setting one without the other impossible to land rather than
// merely discouraged, because the failure mode is expensive in both directions — it silently disabled the
// syslog read path at two sites, and on the actuation lane it would refuse a heal while reporting what
// reads as an impersonation attempt.
//
// KILLING MUTATION: assign HostKeyCallback directly in any SSH client outside core/sshhost. RED.
func TestNoSSHClientSetsHostKeyCallbackWithoutItsAlgorithmList(t *testing.T) {
	var offenders []string
	scanned := 0
	err := filepath.Walk("..", func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return nil // an unreadable tree entry must not silently pass the guard; see the floor below
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// core/sshhost is where the pairing is implemented; it necessarily assigns the field.
		if strings.Contains(filepath.ToSlash(path), "/core/sshhost/") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		scanned++
		if strings.Contains(string(src), "HostKeyCallback") {
			offenders = append(offenders, strings.TrimPrefix(filepath.ToSlash(path), "../"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Vacuity floor: a walk that read almost nothing would certify a tree full of offenders.
	if scanned < 200 {
		t.Fatalf("vacuity floor: only %d Go file(s) scanned — the walk is broken and a pass certifies nothing", scanned)
	}
	for _, o := range offenders {
		t.Errorf("%s sets ssh.ClientConfig.HostKeyCallback directly. Use core/sshhost: New(path) then "+
			"Verifier.Apply(cfg, hostport), which sets HostKeyCallback AND HostKeyAlgorithms together. "+
			"Setting only the callback lets negotiation land on an algorithm the operator never pinned, "+
			"and an unmodified server is then reported as a host-key MISMATCH.", o)
	}
}
