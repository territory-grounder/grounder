package main

import (
	"strings"
	"testing"
)

// TG-423: the SSH CA/signed-cert wiring must stay present at the worker composition root. When TG_SSHCA_ADDR
// is set, actuations present a short-lived OpenBao-signed certificate instead of the static key; a silent
// regression to "never wired" would leave the engine built-but-unreachable (present-not-reaching) and every
// actuation back on the long-lived key. A misconfigured enabled engine must fail the BOOT closed, never fall
// back to the static key. This source guard pins all three, in the guest_liveness_wire_test.go house pattern
// (workerMainSource strips comment lines + a vacuity floor), anchoring on CODE not the comments.
//
// KILLING MUTATION: delete the `sshca.NewNativeRunnerWithCASigner(` call (or the boot-refusal) → this test
// fails naming the gap. Restore → green.
func TestSSHCASchemeIsWiredFailClosed(t *testing.T) {
	src := workerMainSource(t)
	if !strings.Contains(src, "sshca.New(") {
		t.Error("the ssh-CA engine is not constructed at the composition root — a set TG_SSHCA_ADDR would do " +
			"nothing and actuations stay on the static key (TG-423)")
	}
	if !strings.Contains(src, "TG_SSHCA_ADDR") {
		t.Error("the ssh-CA enable flag (TG_SSHCA_ADDR) is gone from main() — the lane can no longer be armed (TG-423)")
	}
	if !strings.Contains(src, "NewNativeRunnerWithCASigner(") {
		t.Error("the actuation runner is not wired to the ssh-CA signer — an armed engine would never actually " +
			"present a certificate, so the feature is built-but-unreachable (TG-423)")
	}
	if !strings.Contains(src, "refusing to boot rather than fall back to the static actuation key") {
		t.Error("the fail-closed boot refusal for a bad ssh-CA config is missing — a misconfigured enabled " +
			"engine must not silently downgrade actuations to the static key (TG-423)")
	}
}
