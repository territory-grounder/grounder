package groundnet

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/veraison/go-cose"
)

// Deterministic 32-byte seeds for test pseudonyms.
const (
	testSeedHex  = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	testSeed2Hex = "a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0"
)

const testIssuedAt int64 = 1724800000

func testPseudonym(t *testing.T, seed string) *Pseudonym {
	t.Helper()
	p, err := NewPseudonym(seed)
	if err != nil {
		t.Fatalf("NewPseudonym: %v", err)
	}
	return p
}

func testWisdom() *WisdomV0 {
	return &WisdomV0{
		AlertClass: "service-down/http",
		Diagnosis:  "process exited; port unbound; no upstream dependency implicated",
		OpClass:    "restart-service",
		Reversible: true,
		BlastClass: "single-host",
		Outcome: WisdomOutcome{
			Verifier: VerifierMechanical,
			Verdict:  VerdictClean,
			Method:   "post-condition re-check: service active, port bound",
		},
		Artifact: &WisdomArtifact{Kind: "runbook", Ref: "sha256:3c96deadbeef"},
	}
}

func signedWisdom(t *testing.T, p *Pseudonym) *Statement {
	t.Helper()
	payload, err := testWisdom().Marshal()
	if err != nil {
		t.Fatalf("Marshal wisdom: %v", err)
	}
	stmt, err := p.Sign(StatementHeader{
		Subject:     ComputeSubject(WisdomMediaTypeV1, payload),
		IssuedAt:    testIssuedAt,
		ContentType: WisdomMediaTypeV1,
	}, payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return stmt
}

// signWithHeader signs a payload under an EXACT header (bypassing Sign's iss/kid forcing),
// so a test can craft a mismatched or identity-bound header. White-box: uses p.key.
func signWithHeader(t *testing.T, p *Pseudonym, h StatementHeader, payload []byte, mutate func(cose.ProtectedHeader)) *Statement {
	t.Helper()
	ph, err := buildProtected(h)
	if err != nil {
		t.Fatalf("buildProtected: %v", err)
	}
	if mutate != nil {
		mutate(ph)
	}
	signer, err := cose.NewSigner(cose.AlgorithmEdDSA, p.key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	raw, err := cose.Sign1(rand.Reader, signer, cose.Headers{Protected: ph}, payload, nil)
	if err != nil {
		t.Fatalf("Sign1: %v", err)
	}
	stmt, err := ParseStatement(raw)
	if err != nil {
		t.Fatalf("ParseStatement: %v", err)
	}
	return stmt
}

// REQ-2100 + REQ-2104: a statement marshals, parses, validates, verifies, and its header
// and payload round-trip through the CBOR wire form.
func TestSignVerifyRoundTrip(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	stmt := signedWisdom(t, p)

	wire, err := stmt.MarshalCBOR()
	if err != nil {
		t.Fatalf("MarshalCBOR: %v", err)
	}
	got, err := ParseStatement(wire)
	if err != nil {
		t.Fatalf("ParseStatement: %v", err)
	}
	if err := got.ValidateEnvelope(); err != nil {
		t.Fatalf("ValidateEnvelope: %v", err)
	}
	if err := Verify(got); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	h, err := got.Header()
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if h.Issuer != p.Issuer() {
		t.Errorf("Issuer = %q, want %q", h.Issuer, p.Issuer())
	}
	if !bytes.Equal(h.KeyID, p.PublicKey()) {
		t.Errorf("KeyID does not round-trip")
	}
	if h.ContentType != WisdomMediaTypeV1 {
		t.Errorf("ContentType = %q, want %q", h.ContentType, WisdomMediaTypeV1)
	}
	if h.IssuedAt != testIssuedAt {
		t.Errorf("IssuedAt = %d, want %d", h.IssuedAt, testIssuedAt)
	}
	if h.Subject == "" {
		t.Errorf("Subject did not round-trip")
	}

	w, err := DecodeWisdom(got)
	if err != nil {
		t.Fatalf("DecodeWisdom: %v", err)
	}
	if w.OpClass != "restart-service" {
		t.Errorf("OpClass = %q, want restart-service", w.OpClass)
	}
}

// REQ-2100: the envelope validates independently of the payload — an opaque, unknown-type
// payload still yields a valid envelope; and REQ-2102: the unknown payload is rejected
// WITHOUT rejecting the envelope.
func TestValidateEnvelopeIndependentOfPayload(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	opaque := []byte{0x00, 0x9f, 0xff, 0x42} // not JSON, not a known media type's body
	stmt, err := p.Sign(StatementHeader{
		Subject:     ComputeSubject("application/vnd.groundnet.future+json", opaque),
		IssuedAt:    testIssuedAt,
		ContentType: "application/vnd.groundnet.future+json",
	}, opaque)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := stmt.ValidateEnvelope(); err != nil {
		t.Fatalf("envelope must validate independently of an unknown payload: %v", err)
	}
	if err := Verify(stmt); err != nil {
		t.Fatalf("signature must verify independently of the payload type: %v", err)
	}
	if _, err := DecodeWisdom(stmt); !errors.Is(err, ErrUnknownPayloadType) {
		t.Fatalf("unknown payload type: got %v, want ErrUnknownPayloadType", err)
	}
}

// REQ-2104: a statement whose payload has been tampered after signing fails verification.
func TestTamperedStatementRefused(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	stmt := signedWisdom(t, p)
	stmt.msg.Payload = append([]byte(nil), stmt.msg.Payload...)
	stmt.msg.Payload[0] ^= 0xFF
	if err := Verify(stmt); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("tampered payload: got %v, want ErrSignatureInvalid", err)
	}
}

// REQ-2103: a statement carrying an x5chain identity binding is refused by the envelope
// validator, independently of the (valid) signature.
func TestX5ChainForbidden(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	payload, _ := testWisdom().Marshal()
	stmt := signWithHeader(t, p, StatementHeader{
		Issuer:      p.Issuer(),
		Subject:     ComputeSubject(WisdomMediaTypeV1, payload),
		KeyID:       p.PublicKey(),
		IssuedAt:    testIssuedAt,
		ContentType: WisdomMediaTypeV1,
	}, payload, func(ph cose.ProtectedHeader) {
		ph[headerLabelX5Chain] = []byte{0x30, 0x82, 0x01} // a would-be cert chain
	})
	if err := stmt.ValidateEnvelope(); !errors.Is(err, ErrIdentityBinding) {
		t.Fatalf("x5chain: got %v, want ErrIdentityBinding", err)
	}
}

// REQ-2103: the iss pseudonym must bind to the kid — a statement claiming a gnpub identity
// it did not sign with is refused, even though the COSE signature itself is valid.
func TestKidBindingMismatchRefused(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	other := testPseudonym(t, testSeed2Hex)
	payload, _ := testWisdom().Marshal()
	// Sign with p's key, but claim other's gnpub as iss and p's key as kid.
	stmt := signWithHeader(t, p, StatementHeader{
		Issuer:      other.Issuer(),
		Subject:     ComputeSubject(WisdomMediaTypeV1, payload),
		KeyID:       p.PublicKey(),
		IssuedAt:    testIssuedAt,
		ContentType: WisdomMediaTypeV1,
	}, payload, nil)
	if err := stmt.ValidateEnvelope(); err != nil {
		t.Fatalf("envelope itself is structurally valid: %v", err)
	}
	if err := Verify(stmt); !errors.Is(err, ErrKeyIDMismatch) {
		t.Fatalf("iss/kid mismatch: got %v, want ErrKeyIDMismatch", err)
	}
}

// REQ-2103: a non-pseudonymous Issuer is rejected at header assembly.
func TestIssuerMustBePseudonym(t *testing.T) {
	err := validateHeaderFields(StatementHeader{
		Issuer:      "acme-corp",
		Subject:     "sha256:abc",
		KeyID:       []byte{1, 2, 3},
		ContentType: WisdomMediaTypeV1,
	})
	if !errors.Is(err, ErrIssuerNotPseudonym) {
		t.Fatalf("non-pseudonym issuer: got %v, want ErrIssuerNotPseudonym", err)
	}
}

// A malformed wire buffer is rejected, not panicked on.
func TestParseMalformed(t *testing.T) {
	if _, err := ParseStatement([]byte{0x00, 0x01, 0x02}); !errors.Is(err, ErrMalformedStatement) {
		t.Fatalf("malformed: got %v, want ErrMalformedStatement", err)
	}
}

// Regression: a Receipt attached AFTER signing must survive the CBOR round-trip. AttachReceipt
// mutates the unprotected header of an already-decoded statement, and go-cose prefers the decoder's
// RawUnprotected over the map on re-encode — AttachReceipt clears RawUnprotected so the Receipt is
// not silently dropped. The whole Emit->Ingest round-trip (T-021-5) depends on this.
func TestReceiptSurvivesRoundTrip(t *testing.T) {
	p := testPseudonym(t, testSeedHex)
	stmt := signedWisdom(t, p)
	receipt := []byte("scitt-receipt-bytes-123")
	stmt.AttachReceipt(receipt)

	wire, err := stmt.MarshalCBOR()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := ParseStatement(wire)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r, ok := got.Receipt()
	if !ok {
		t.Fatal("Receipt did not survive the round-trip")
	}
	if !bytes.Equal(r, receipt) {
		t.Errorf("Receipt = %q, want %q", r, receipt)
	}
	// The signature still verifies — the Receipt rides the UNSIGNED unprotected header.
	if err := Verify(got); err != nil {
		t.Errorf("signature must still verify with a Receipt attached: %v", err)
	}
}
