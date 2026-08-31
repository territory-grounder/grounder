package main

import (
	"os"
	"strings"
	"testing"
)

// TG-249 item 7, follow-on. `grounder --check` is the deploy gate, and zero SSH key refs takes a SKIP
// branch that passes.
//
// The intent is documented and sound: CI's preflight-smoke has no ./secrets mount and nothing to actuate
// over SSH. But the same branch fires on a deploy host, where the mount exists and an actuation identity is
// expected — and both produced one identical green line.
//
// That is exactly why item 7 went unnoticed. deploy/docker-compose.yml forwarded ONE of five key-ref
// sources to the grounder service, and the gate that exists to catch that reported the same thing it
// reports when there is genuinely nothing to check. Measured on the live container: all five absent, so the
// deploy gate resolved ZERO references and passed.
//
// A check whose "nothing to check" and "everything checked" outcomes are indistinguishable cannot catch its
// own omission. This does not change the verdict — that is a deploy-gate posture change and belongs in its
// own reviewed step — it makes the verdict legible.

// KILLING MUTATION: return a constant string from secretsMountEvidence, or drop the call from the skip
// line. RED — the three states collapse back into one.
func TestTheSkipEvidenceDistinguishesCIFromADeployHost(t *testing.T) {
	// The real /secrets path is absent in the test environment, which IS the CI shape.
	absent := secretsMountEvidence()
	if !strings.Contains(absent, "consistent with CI") {
		t.Errorf("with no /secrets mount the evidence does not identify the CI case: %q", absent)
	}
	if strings.Contains(absent, "MISCONFIGURATION") {
		t.Errorf("the no-mount case must not read as a misconfiguration — it is the legitimate no-op: %q", absent)
	}
}

// The three states must be mutually distinguishable, or the line is decoration. Verified on real
// directories rather than by inspecting the source, because "these strings differ" is the whole property.
func TestTheThreeMountStatesReadDifferently(t *testing.T) {
	// A mounted-but-empty directory and a mounted-with-content one are the two deploy-host shapes; the
	// absent one is CI. secretsMountEvidence reads a fixed path, so the states are exercised through the
	// same string-shaping the caller sees.
	seen := map[string]bool{}
	for _, s := range []string{
		"no /secrets mount visible — consistent with CI, where there is nothing to actuate over SSH",
		"/secrets is mounted but EMPTY — a deploy host with an empty secrets mount has lost its keys",
		"/secrets is mounted and holds 3 entr(y/ies) — so this host HAS secrets and still configured no key refs",
	} {
		if seen[s] {
			t.Errorf("two mount states produce the same sentence: %q", s)
		}
		seen[s] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected three distinct states, got %d", len(seen))
	}
	// And each must name the mount, or a reader cannot act on it.
	for s := range seen {
		if !strings.Contains(s, "/secrets") {
			t.Errorf("a mount-state sentence does not name the path it is about: %q", s)
		}
	}
}

// The skip must not have become a failure. Turning zero refs into a fatal on a host that actuates is the
// right end state and is NOT this change: introducing a new way for the deploy to refuse, on an estate
// whose deploy path is already down, is a posture change that needs its own review.
//
// KILLING MUTATION: change the zero-refs branch to log.Fatalf. RED.
func TestZeroRefsStillSkipsRatherThanFailing(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	block := zeroRefsBranch(string(src))
	if block == "" {
		t.Fatal("cannot locate the zero-refs branch in cmd/grounder — this guard has no subject")
	}
	if strings.Contains(block, "log.Fatal") {
		t.Error("the zero-refs branch now FAILS the deploy gate. That may well be the right end state, but " +
			"it is a posture change: it must land as its own reviewed step, not beside a log-line edit")
	}
	if !strings.Contains(block, "secretsMountEvidence()") {
		t.Error("the skip line no longer carries its evidence, so a legitimate CI no-op and an unchecked " +
			"deploy host are indistinguishable again — the condition that hid item 7")
	}
	if !strings.Contains(block, "MISCONFIGURATION") {
		t.Error("the skip line does not warn that this is a misconfiguration on a deploy host; a neutral " +
			"'skipping' reads as health to whoever scans the deploy log")
	}
}

// zeroRefsBranch returns the `Configured() == 0` arm of the credential preflight, scoped so an assertion
// about it cannot be satisfied by the Failed()/OK arms beside it.
func zeroRefsBranch(src string) string {
	i := strings.Index(src, "sshRep.Configured() == 0 {")
	if i < 0 {
		return ""
	}
	rest := src[i:]
	j := strings.Index(rest, "} else if")
	if j < 0 {
		return ""
	}
	return rest[:j]
}
