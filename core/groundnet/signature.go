package groundnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/veraison/go-cose"
)

var (
	// ErrSignatureInvalid is returned when a statement's COSE_Sign1 signature does not
	// verify against the key bound in its kid (REQ-2104).
	ErrSignatureInvalid = errors.New("groundnet: COSE_Sign1 signature does not verify (REQ-2104)")
	// ErrKeyIDMismatch is returned when the iss gnpub pseudonym does not bind to the kid,
	// i.e. the Issuer claims a key it did not sign with (REQ-2103).
	ErrKeyIDMismatch = errors.New("groundnet: iss pseudonym does not match kid (REQ-2103)")
)

// Sign produces a signed SCITT Signed Statement. It assembles the protected header from the
// de-identified StatementHeader — forcing iss and kid to THIS pseudonym so a producer cannot
// sign under another's identity — and signs the COSE_Sign1 with the pseudonym's ed25519 key
// (REQ-2104). The result is a bare Signed Statement; a Receipt is attached after
// Transparency-Service registration (REQ-2105).
func (p *Pseudonym) Sign(h StatementHeader, payload []byte) (*Statement, error) {
	h.Issuer = p.Issuer()
	h.KeyID = p.PublicKey()
	ph, err := buildProtected(h)
	if err != nil {
		return nil, err
	}
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, p.key)
	if err != nil {
		return nil, fmt.Errorf("groundnet: cose signer: %w", err)
	}
	raw, err := cose.Sign1(rand.Reader, signer, cose.Headers{Protected: ph}, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("groundnet: signing statement: %w", err)
	}
	return ParseStatement(raw)
}

// Verify checks a statement's COSE_Sign1 signature against the pseudonym public key bound in
// its own kid, and confirms the iss gnpub value binds to that key (REQ-2103/2104). A consumer
// runs Verify BEFORE ingest: a statement that does not verify never reaches the local
// re-graduation path. Verify is envelope-and-signature only — passing it grants NO standing;
// standing is earned solely by local re-graduation (REQ-2110).
func Verify(s *Statement) error {
	if err := s.ValidateEnvelope(); err != nil {
		return err
	}
	h, err := s.Header()
	if err != nil {
		return err
	}
	// The kid IS the verifying key; the iss must bind to it — no external identity (REQ-2103).
	if len(h.KeyID) != ed25519.PublicKeySize {
		return ErrKeyIDMismatch
	}
	if h.Issuer != PseudonymScheme+hex.EncodeToString(h.KeyID) {
		return ErrKeyIDMismatch
	}
	verifier, err := cose.NewVerifier(cose.AlgorithmEdDSA, ed25519.PublicKey(h.KeyID))
	if err != nil {
		return fmt.Errorf("groundnet: cose verifier: %w", err)
	}
	if err := s.msg.Verify(nil, verifier); err != nil {
		return fmt.Errorf("%w: %v", ErrSignatureInvalid, err)
	}
	return nil
}
