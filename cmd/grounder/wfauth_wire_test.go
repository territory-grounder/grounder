package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/temporal/wfauth"
)

// testWfSigner is the signer the served-router / backend oracles use: a fixed 32-byte key, so a backend under
// test can mint the envelope the production request carries (TG-660).
func testWfSigner(t *testing.T) wfauth.EnvelopeSigner {
	t.Helper()
	s, err := wfauth.NewSigner(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("test signer: %v", err)
	}
	return s
}

// With no key configured the grounder signs NOTHING: every write backend refuses locally with the cause, before
// a request the worker would refuse ever leaves the process. KILLING MUTATION (executed 2026-11-07): returning a
// zero wfauth.Envelope with a nil error from signFor turned this RED — "an unsigned request would have been sent".
func TestWorkflowAuthUnsetMeansNoSigningAndALocalRefusal(t *testing.T) {
	t.Setenv("TG_WORKFLOW_AUTH_KEY_REF", "")
	sig := wireWorkflowAuth()
	if _, err := signFor(sig, "modetransition", "Shadow"); !errors.Is(err, errWorkflowAuthUnconfigured) {
		t.Fatalf("an unset key must refuse locally with errWorkflowAuthUnconfigured, got %v (an unsigned request would have been sent)", err)
	}
}

// With a key configured through a reference the grounder signs, and what it signs the worker-side verifier admits.
func TestWorkflowAuthSetSignsVerifiableEnvelopes(t *testing.T) {
	t.Setenv("TG_WORKFLOW_AUTH_KEY_TEST", strings.Repeat("ab", 32))
	t.Setenv("TG_WORKFLOW_AUTH_KEY_REF", "env:TG_WORKFLOW_AUTH_KEY_TEST")
	sig := wireWorkflowAuth()
	env, err := signFor(sig, "modetransition", "Semi-auto", "Shadow", "kyriakos", "resume", "true")
	if err != nil || env.IsZero() {
		t.Fatalf("a configured key must sign: env=%+v err=%v", env, err)
	}
	key, _ := wfauth.ParseKey(strings.Repeat("ab", 32))
	v, verr := wfauth.NewVerifier(key, memNonces{})
	if verr != nil {
		t.Fatalf("verifier: %v", verr)
	}
	if err := v.Verify(t.Context(), "modetransition", env, "Semi-auto", "Shadow", "kyriakos", "resume", "true"); err != nil {
		t.Fatalf("the grounder's envelope must verify under the same key: %v", err)
	}
}
