// Package wfauthtest is TEST SUPPORT for the signed authorization envelope (temporal/wfauth): an in-memory
// nonce store, a signer/verifier pair over a fixed key, and a permissive verifier for oracles whose subject is
// something other than authorization. It is imported only by _test.go files — never by a binary — so a
// production composition root cannot reach AllowAll by accident, and the deadcode gate (rooted at the
// binaries) never sees this package at all.
package wfauthtest

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/temporal/wfauth"
)

// Key is the fixed 32-byte test key both halves of Pair use.
var Key = bytes.Repeat([]byte{0x7e}, 32)

// MemNonces is an in-memory wfauth.NonceStore: the first Claim of a nonce is fresh, every later one a replay.
type MemNonces struct {
	mu   sync.Mutex
	seen map[string]bool
}

// Claim implements wfauth.NonceStore.
func (m *MemNonces) Claim(_ context.Context, nonce, _ string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	if m.seen[nonce] {
		return false, nil
	}
	m.seen[nonce] = true
	return true, nil
}

// Pair returns a real signer and a real verifier over Key, sharing a fresh MemNonces — what an oracle uses to
// prove that a request the grounder would sign is admitted and that a tampered/unsigned one is refused.
func Pair(t testing.TB) (*wfauth.Signer, *wfauth.Verifier) {
	t.Helper()
	s, err := wfauth.NewSigner(Key)
	if err != nil {
		t.Fatalf("wfauthtest: signer: %v", err)
	}
	v, err := wfauth.NewVerifier(Key, &MemNonces{})
	if err != nil {
		t.Fatalf("wfauthtest: verifier: %v", err)
	}
	return s, v
}

// Sign mints an envelope over (kind, fields) with the Pair key, for tests that build requests by hand.
func Sign(t testing.TB, kind string, fields ...string) wfauth.Envelope {
	t.Helper()
	s, _ := Pair(t)
	e, err := s.Sign(kind, fields...)
	if err != nil {
		t.Fatalf("wfauthtest: sign %s: %v", kind, err)
	}
	return e
}

// AllowAll is a wfauth.EnvelopeVerifier that admits everything. For oracles whose subject is NOT authorization
// (a ledger-ordering proof, a latency bound, a state-machine refusal): they keep driving the real activity with
// the envelope check out of the way. It must never be wired into a composition root — see the package comment.
type AllowAll struct{}

// Verify implements wfauth.EnvelopeVerifier and admits unconditionally.
func (AllowAll) Verify(context.Context, string, wfauth.Envelope, ...string) error { return nil }
