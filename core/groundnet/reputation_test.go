package groundnet

import (
	"encoding/json"
	"math"
	"testing"
)

// REQ-2107: idempotent — a confirmer's attestation of a chunk counts at most once.
func TestReputationIdempotent(t *testing.T) {
	r := NewReputation()
	c := ConfirmationV0{Subject: "sha256:chunk1", Result: VerdictClean, VerifierProfile: "mech"}
	r.Observe("gnpub:producer", "gnpub:confirmerA", c)
	r.Observe("gnpub:producer", "gnpub:confirmerA", c) // re-received — must not inflate
	if got := r.Score("gnpub:producer"); got != 1.0 {
		t.Fatalf("idempotent: score = %v, want 1.0", got)
	}
}

// REQ-2107: commutative — order-independent aggregation (CRDT).
func TestReputationCommutative(t *testing.T) {
	obs := []struct{ conf, subj, res string }{
		{"gnpub:A", "sha256:1", VerdictClean},
		{"gnpub:B", "sha256:2", VerdictPartial},
		{"gnpub:C", "sha256:1", VerdictClean},
	}
	score := func(order []int) float64 {
		r := NewReputation()
		for _, i := range order {
			r.Observe("gnpub:producer", obs[i].conf, ConfirmationV0{Subject: obs[i].subj, Result: obs[i].res, VerifierProfile: "m"})
		}
		return r.Score("gnpub:producer")
	}
	if a, b := score([]int{0, 1, 2}), score([]int{2, 0, 1}); math.Abs(a-b) > 1e-9 {
		t.Fatalf("not commutative: %v != %v", a, b)
	}
}

// REQ-2107: weighted by verified-outcome QUALITY, not a flat count; a deviation is negative.
func TestReputationQualityWeighted(t *testing.T) {
	r := NewReputation()
	r.Observe("gnpub:P", "gnpub:A", ConfirmationV0{Subject: "sha256:1", Result: VerdictClean, VerifierProfile: "m"})     // +1.0
	r.Observe("gnpub:P", "gnpub:B", ConfirmationV0{Subject: "sha256:2", Result: VerdictPartial, VerifierProfile: "m"})   // +0.25
	r.Observe("gnpub:P", "gnpub:C", ConfirmationV0{Subject: "sha256:3", Result: VerdictDeviation, VerifierProfile: "m"}) // -1.0
	if got := r.Score("gnpub:P"); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("quality-weighted: score = %v, want 0.25 (1.0 + 0.25 - 1.0)", got)
	}
}

// REQ-2107: NOT weighted by contribution volume — an unconfirmed producer has zero reputation
// regardless of emit volume (Reputation only counts OTHER nodes' confirmations).
func TestReputationNotVolume(t *testing.T) {
	r := NewReputation()
	if got := r.Score("gnpub:P"); got != 0 {
		t.Fatalf("an unconfirmed producer must have zero reputation, got %v", got)
	}
	// one chunk, confirmed clean by three DISTINCT nodes -> 3.0 (from confirmations, not chunk volume).
	for _, conf := range []string{"gnpub:A", "gnpub:B", "gnpub:C"} {
		r.Observe("gnpub:Q", conf, ConfirmationV0{Subject: "sha256:q1", Result: VerdictClean, VerifierProfile: "m"})
	}
	if got := r.Score("gnpub:Q"); got != 3.0 {
		t.Fatalf("three distinct confirmers of one chunk -> 3.0, got %v", got)
	}
}

// A producer cannot inflate its own reputation by self-confirming (REQ-2107: another node's outcome).
func TestReputationRejectsSelfConfirmation(t *testing.T) {
	r := NewReputation()
	r.Observe("gnpub:P", "gnpub:P", ConfirmationV0{Subject: "sha256:1", Result: VerdictClean, VerifierProfile: "m"})
	if got := r.Score("gnpub:P"); got != 0 {
		t.Fatalf("self-confirmation must not accrue reputation, got %v", got)
	}
}

// A confirmation statement round-trips through the envelope + validates; the confirmer is the Issuer,
// and a wisdom-typed statement does not decode as a confirmation.
func TestDecodeConfirmation(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	c := ConfirmationV0{Subject: "sha256:chunk", Result: VerdictClean, VerifierProfile: "mechanical", ObservedAt: 1}
	payload, _ := json.Marshal(c)
	stmt, err := p.Sign(StatementHeader{
		Subject: ComputeSubject(ConfirmationMediaTypeV1, payload), IssuedAt: 1, ContentType: ConfirmationMediaTypeV1,
	}, payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, confirmer, err := DecodeConfirmation(stmt)
	if err != nil {
		t.Fatalf("DecodeConfirmation: %v", err)
	}
	if got.Subject != "sha256:chunk" || got.Result != VerdictClean {
		t.Errorf("payload: %+v", got)
	}
	if confirmer != p.Issuer() {
		t.Errorf("confirmer = %q, want %q", confirmer, p.Issuer())
	}
	if _, _, err := DecodeConfirmation(signedWisdom(t, p)); err == nil {
		t.Error("a wisdom statement must not decode as a confirmation")
	}
}
