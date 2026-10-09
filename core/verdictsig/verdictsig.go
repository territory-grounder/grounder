// Package verdictsig signs and verifies action-verdict rows (TG-81 borrow 3; clean-room from the
// h-network RFC-ASA signed gate↔judge verdict envelope, attribution: SOURCE-BENCHMARK-CATALOG —
// Ed25519 chosen over their HMAC: verification needs only the PUBLIC key, so the triage plane can
// check a verdict without holding the key that could mint one, matching TG's plane split).
//
// WHAT A SIGNATURE PROVES, AND WHAT STAYS AUTHORITATIVE. The deterministic verifier remains the sole
// verdict AUTHORITY (INV-10); a signature adds only provenance: a row whose signature verifies was
// written through the interceptor's VerdictSink by a process holding the signing seed — not INSERTed
// around the API by a compromised triage-plane writer fabricating "match" history to graduate an
// op-class. Consumers treat a BAD signature as an ABSENT verdict (evidence removed, review raised) —
// the safe direction — and accept unsigned rows as pre-feature history.
//
// The canonical message is a versioned, newline-joined tuple of exactly the columns the verdict row
// binds (action_id, plan_hash, verdict, target_host, site); none of them may carry a newline by
// construction (content-addressed ids, enum, hostnames).
package verdictsig

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// canonical renders the versioned signing message. Bump the prefix on ANY change to the tuple.
func canonical(actionID, planHash, verdict, targetHost, site string) []byte {
	return []byte("tg-action-verdict/1\n" + actionID + "\n" + planHash + "\n" + verdict + "\n" + targetHost + "\n" + site)
}

// Signer holds the Ed25519 private key derived from a 32-byte seed.
type Signer struct{ key ed25519.PrivateKey }

// NewSigner derives the signing key from a 32-byte seed given as 64 hex characters (the shape a
// SecretRef resolve returns). Whitespace is trimmed; anything else is refused loudly — a mis-pasted
// seed must fail the boot line that wires it, never sign with garbage.
func NewSigner(seedHex string) (*Signer, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil {
		return nil, fmt.Errorf("verdictsig: seed is not hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("verdictsig: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Signer{key: ed25519.NewKeyFromSeed(seed)}, nil
}

// Sign returns the base64 signature over the canonical verdict tuple.
func (s *Signer) Sign(actionID, planHash, verdict, targetHost, site string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, canonical(actionID, planHash, verdict, targetHost, site)))
}

// PublicKeyHex renders the verifying half — NOT a secret; it may live in plain env/config.
func (s *Signer) PublicKeyHex() string {
	return hex.EncodeToString(s.key.Public().(ed25519.PublicKey))
}

// Verifier holds only the public key — the triage plane's half.
type Verifier struct{ pub ed25519.PublicKey }

// NewVerifier parses the 64-hex-character public key.
func NewVerifier(pubHex string) (*Verifier, error) {
	b, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil {
		return nil, fmt.Errorf("verdictsig: public key is not hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("verdictsig: public key must be %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return &Verifier{pub: ed25519.PublicKey(b)}, nil
}

// Verify reports whether sig (base64) is a valid signature over the canonical tuple. A malformed
// encoding is simply false — the caller's treat-as-absent path needs no second error lane.
func (v *Verifier) Verify(actionID, planHash, verdict, targetHost, site, sig string) bool {
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(v.pub, canonical(actionID, planHash, verdict, targetHost, site), raw)
}
