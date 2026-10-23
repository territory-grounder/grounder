package groundnet

import (
	"encoding/json"
	"fmt"
	"sync"
)

// ConfirmationV0 is the application/vnd.groundnet.confirmation+json payload: a consuming node's signed
// report that it applied a wisdom unit's op-class and its OWN mechanical verifier observed a verdict
// (canonical spec §6). This — not a producer-asserted count — is the evidence reputation aggregates
// (REQ-2107). It is de-identified: subject is a content-address and no field names an estate.
type ConfirmationV0 struct {
	Subject         string `json:"subject"`          // the wisdom unit sub this confirms
	Result          string `json:"result"`           // the confirmer's OWN mechanical verdict (clean/partial/deviation)
	VerifierProfile string `json:"verifier_profile"` // the mechanical verifier profile (INV-5)
	ObservedAt      int64  `json:"observed_at"`
}

// Validate enforces the confirmation content invariants: a subject and a mechanically-verified verdict
// from the closed vocabulary.
func (c *ConfirmationV0) Validate() error {
	if c.Subject == "" {
		return fmt.Errorf("groundnet: confirmation missing subject")
	}
	if c.VerifierProfile == "" {
		return fmt.Errorf("groundnet: confirmation missing verifier_profile (INV-5)")
	}
	switch c.Result {
	case VerdictClean, VerdictPartial, VerdictDeviation:
	default:
		return fmt.Errorf("groundnet: confirmation result %q not in {clean,partial,deviation}", c.Result)
	}
	return nil
}

// DecodeConfirmation reads and validates a confirmation statement's payload (REQ-2107). The CONFIRMER
// is the statement's Issuer (the signing pseudonym); a consumer verifies the COSE signature (REQ-2104)
// before this. An unknown media type is rejected without rejecting the envelope (REQ-2102).
func DecodeConfirmation(s *Statement) (payload ConfirmationV0, confirmer string, err error) {
	h, err := s.Header()
	if err != nil {
		return ConfirmationV0{}, "", err
	}
	if h.ContentType != ConfirmationMediaTypeV1 {
		return ConfirmationV0{}, "", fmt.Errorf("%w: %q", ErrUnknownPayloadType, h.ContentType)
	}
	var c ConfirmationV0
	if err := json.Unmarshal(s.Payload(), &c); err != nil {
		return ConfirmationV0{}, "", fmt.Errorf("groundnet: decoding confirmation payload: %w", err)
	}
	if err := c.Validate(); err != nil {
		return ConfirmationV0{}, "", err
	}
	return c, h.Issuer, nil // the Issuer is the confirmer pseudonym
}

// Reputation is the CRDT-style rollup of verified-outcome confirmations (REQ-2107). It is COMMUTATIVE
// and IDEMPOTENT: a confirmation is counted at most once — keyed by (confirmer, subject) — so the score
// is order-independent and a re-received confirmation cannot inflate it. Influence is weighted by the
// confirmer's VERIFIED-OUTCOME QUALITY, never by how many chunks a producer emitted (not a volume
// count) and never a producer-asserted number. It is never an on-chain vote or token — just a
// commutative, coordinator-free aggregation over signed attestations. Safe for concurrent use.
type Reputation struct {
	mu    sync.Mutex
	seen  map[string]struct{} // confirmer\x00subject -> counted once (idempotent)
	score map[string]float64  // producer pseudonym -> quality-weighted reputation
}

// NewReputation returns an empty rollup.
func NewReputation() *Reputation {
	return &Reputation{seen: make(map[string]struct{}), score: make(map[string]float64)}
}

// Observe folds one verified confirmation into the rollup: the CONFIRMER attests the PRODUCER's chunk
// (subject) verified with a given result on the confirmer's estate. It is idempotent per
// (confirmer, subject) and weighted by the result's quality (REQ-2107). A producer can never confirm
// its OWN chunk — reputation is what OTHER nodes verified. Producer and confirmer are gnpub
// pseudonyms; the caller resolves the producer from the confirmed wisdom and has verified the
// confirmation's signature (REQ-2104).
func (r *Reputation) Observe(producer, confirmer string, c ConfirmationV0) {
	if producer == "" || confirmer == "" || c.Subject == "" {
		return
	}
	if producer == confirmer {
		return // a producer cannot confirm its own chunk — an attestation is ANOTHER node's outcome (REQ-2107)
	}
	key := confirmer + "\x00" + c.Subject
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.seen[key]; seen {
		return // idempotent — a confirmer's attestation of a chunk counts at most once
	}
	r.seen[key] = struct{}{}
	r.score[producer] += confirmationWeight(c.Result)
}

// Score returns a producer pseudonym's current quality-weighted reputation (REQ-2107).
func (r *Reputation) Score(producer string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.score[producer]
}

// confirmationWeight maps a verified verdict to its influence: a clean confirmation is positive
// evidence, a partial is weak positive, a deviation is NEGATIVE (the fix failed on that estate). It is
// a QUALITY weight, never +1-per-confirmation (a count).
func confirmationWeight(result string) float64 {
	switch result {
	case VerdictClean:
		return 1.0
	case VerdictPartial:
		return 0.25
	case VerdictDeviation:
		return -1.0
	default:
		return 0
	}
}
