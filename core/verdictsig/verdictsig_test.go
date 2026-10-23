package verdictsig

import (
	"strings"
	"testing"
)

const testSeed = "9f3c1a2b4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

// TG-81 b3: sign → verify round-trips; ANY tampered tuple field or signature fails. KILLING MUTATION:
// drop any field from canonical() — the matching tamper case verifies where it must fail.
func TestSignVerifyRoundTripAndTamper(t *testing.T) {
	s, err := NewSigner(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(s.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	sig := s.Sign("act-1", "plan#1", "match", "web01", "dc1")
	if !v.Verify("act-1", "plan#1", "match", "web01", "dc1", sig) {
		t.Fatal("a genuine signature must verify")
	}
	tampers := map[string][5]string{
		"action":  {"act-2", "plan#1", "match", "web01", "dc1"},
		"plan":    {"act-1", "plan#2", "match", "web01", "dc1"},
		"verdict": {"act-1", "plan#1", "deviation", "web01", "dc1"},
		"host":    {"act-1", "plan#1", "match", "db02", "dc1"},
		"site":    {"act-1", "plan#1", "match", "web01", "dc2"},
	}
	for name, f := range tampers {
		if v.Verify(f[0], f[1], f[2], f[3], f[4], sig) {
			t.Errorf("tampered %s must not verify — that field is outside the canonical tuple", name)
		}
	}
	if v.Verify("act-1", "plan#1", "match", "web01", "dc1", "not-base64!!") {
		t.Fatal("garbage encoding must be false, not a crash")
	}
	if v.Verify("act-1", "plan#1", "match", "web01", "dc1", strings.Repeat("A", 88)) {
		t.Fatal("a wrong signature must not verify")
	}
}

func TestKeyParsingFailsLoud(t *testing.T) {
	if _, err := NewSigner("abc"); err == nil {
		t.Fatal("a short seed must refuse")
	}
	if _, err := NewSigner("zz" + testSeed[2:]); err == nil {
		t.Fatal("non-hex must refuse")
	}
	if _, err := NewVerifier("beef"); err == nil {
		t.Fatal("a short public key must refuse")
	}
	// A signer built from a DIFFERENT seed must not verify against this public key.
	s1, _ := NewSigner(testSeed)
	s2, _ := NewSigner(strings.Repeat("0", 62) + "aa")
	v, _ := NewVerifier(s1.PublicKeyHex())
	if v.Verify("a", "p", "match", "h", "s", s2.Sign("a", "p", "match", "h", "s")) {
		t.Fatal("a foreign key's signature must not verify")
	}
}
