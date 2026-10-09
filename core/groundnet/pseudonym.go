package groundnet

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
)

// Pseudonym is a stable groundnet producer identity: an ed25519 keypair that is a
// PSEUDONYM, never a real-world or estate identity (REQ-2103). Reputation accrues to the
// gnpub: value, not to any estate. It is seed-derived (mirroring core/verdictsig) so an
// operator provisions it as a single secret seed; the verifying half (the public key /
// gnpub value) is not a secret and may live in plain config.
type Pseudonym struct {
	key ed25519.PrivateKey
}

// NewPseudonym derives a producer pseudonym from a 32-byte ed25519 seed (hex-encoded). The
// seed is a secret; the derived gnpub value and public key are not.
func NewPseudonym(seedHex string) (*Pseudonym, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return nil, fmt.Errorf("groundnet: pseudonym seed not hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("groundnet: pseudonym seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Pseudonym{key: ed25519.NewKeyFromSeed(seed)}, nil
}

// PublicKey returns a copy of the raw ed25519 public key used as the COSE kid.
func (p *Pseudonym) PublicKey() []byte {
	return append([]byte(nil), p.key.Public().(ed25519.PublicKey)...)
}

// Issuer returns the gnpub: iss value: the scheme prefix over the hex-encoded public key.
// It is a pseudonym bound to kid and carries no estate identity (REQ-2103).
func (p *Pseudonym) Issuer() string {
	return PseudonymScheme + hex.EncodeToString(p.key.Public().(ed25519.PublicKey))
}
