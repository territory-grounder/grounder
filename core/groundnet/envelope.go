package groundnet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/veraison/go-cose"
)

// headerLabelReceipts is the COSE unprotected-header label carrying SCITT Receipt(s)
// (RFC 9943 §7): a Signed Statement plus a Receipt at this label is a Transparent
// Statement. The Receipt body is opaque to the envelope layer — its structure and
// verification are the local transparency log's concern (REQ-2105/2106, T-021-3).
const headerLabelReceipts int64 = 394

// x5chain / x5t COSE header labels. A groundnet statement MUST NOT carry either: they root
// the Issuer in a real-world X.509 identity and defeat pseudonymity (REQ-2103).
const (
	headerLabelX5Chain int64 = 33
	headerLabelX5T     int64 = 34
)

// Payload media types version the body (REQ-2102). The SCITT envelope is stable across
// payload versions; a consumer rejects an unknown payload media type WITHOUT rejecting the
// envelope. These mirror the canonical spec §6 statement-type table.
const (
	WisdomMediaTypeV1       = "application/vnd.groundnet.wisdom+json"
	ConfirmationMediaTypeV1 = "application/vnd.groundnet.confirmation+json"
	DeviationMediaTypeV1    = "application/vnd.groundnet.deviation+json"
	WithdrawalMediaTypeV1   = "application/vnd.groundnet.withdrawal+json"
)

// PseudonymScheme is the mandatory iss prefix: the Issuer is a gnpub: keypair pseudonym,
// never a real-world or estate identity (REQ-2103).
const PseudonymScheme = "gnpub:"

// Envelope-layer errors. Every one is STRUCTURAL: it is decided from the COSE envelope
// alone, independently of the payload body and independently of whether the signature
// verifies (REQ-2100). Signature verification is a separate guard (REQ-2104, signature.go).
var (
	ErrMalformedStatement = errors.New("groundnet: malformed COSE_Sign1 statement")
	ErrMissingClaim       = errors.New("groundnet: statement missing a required protected-header claim")
	ErrIssuerNotPseudonym = errors.New("groundnet: iss is not a gnpub: pseudonym (REQ-2103)")
	ErrIdentityBinding    = errors.New("groundnet: x5t/x5chain identity binding is forbidden (REQ-2103)")
	ErrMissingContentType = errors.New("groundnet: statement carries no content_type (REQ-2102)")
)

// StatementHeader is the typed, de-identified projection of a statement's protected
// header. Every field is generalizable — none carries an estate identifier (REQ-2101).
type StatementHeader struct {
	Issuer      string // iss  — a gnpub: pseudonym bound by KeyID (REQ-2103), NOT an identity
	Subject     string // sub  — content-addressed statement id; dedup + replay key (REQ-2115)
	IssuedAt    int64  // iat  — issuance time (unix seconds); SHOULD be coarsened to the hour
	KeyID       []byte // kid  — the pseudonym public key (REQ-2103)
	ContentType string // content_type — the payload media type (REQ-2102)
}

// Statement is a SCITT Transparent Statement (REQ-2100): a COSE_Sign1 Signed Statement
// whose protected header carries the SCITT/CWT claims (iss, sub, iat, kid, content_type),
// optionally augmented with a Transparency Service Receipt in the unprotected header
// (REQ-2105/2106). It is the STABLE wire envelope a node is born able to parse and validate
// independently of the payload it carries.
type Statement struct {
	msg *cose.Sign1Message
}

// buildProtected assembles the SCITT protected header from a de-identified StatementHeader.
// It is the single assembly point the producer signer (signature.go) shares, so the wire
// header is identical whoever builds it.
func buildProtected(h StatementHeader) (cose.ProtectedHeader, error) {
	if err := validateHeaderFields(h); err != nil {
		return nil, err
	}
	ph := cose.ProtectedHeader{}
	ph.SetAlgorithm(cose.AlgorithmEdDSA)
	ph[cose.HeaderLabelKeyID] = h.KeyID
	ph[cose.HeaderLabelContentType] = h.ContentType
	if _, err := ph.SetCWTClaims(cose.CWTClaims{
		cose.CWTClaimIssuer:   h.Issuer,
		cose.CWTClaimSubject:  h.Subject,
		cose.CWTClaimIssuedAt: h.IssuedAt,
	}); err != nil {
		return nil, fmt.Errorf("groundnet: assembling CWT claims: %w", err)
	}
	return ph, nil
}

// validateHeaderFields checks the required, de-identified header fields are present and
// well-formed before a statement is built (REQ-2100/2103).
func validateHeaderFields(h StatementHeader) error {
	if h.Issuer == "" || h.Subject == "" || len(h.KeyID) == 0 {
		return ErrMissingClaim
	}
	if !strings.HasPrefix(h.Issuer, PseudonymScheme) {
		return ErrIssuerNotPseudonym
	}
	if h.ContentType == "" {
		return ErrMissingContentType
	}
	return nil
}

// ParseStatement decodes a COSE_Sign1 Transparent Statement from CBOR. It validates the
// ENVELOPE structure only — the payload is left opaque and the signature is not verified
// (REQ-2100); ValidateEnvelope and Verify are the explicit next guards.
func ParseStatement(data []byte) (*Statement, error) {
	msg := cose.NewSign1Message()
	if err := msg.UnmarshalCBOR(data); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedStatement, err)
	}
	return &Statement{msg: msg}, nil
}

// MarshalCBOR encodes the statement to its COSE_Sign1 wire form.
func (s *Statement) MarshalCBOR() ([]byte, error) {
	return s.msg.MarshalCBOR()
}

// Header returns the typed, de-identified projection of the protected header (REQ-2101).
func (s *Statement) Header() (StatementHeader, error) {
	ph := s.msg.Headers.Protected
	if ph == nil {
		return StatementHeader{}, ErrMalformedStatement
	}
	var h StatementHeader
	if ct, ok := ph[cose.HeaderLabelContentType]; ok {
		h.ContentType, _ = ct.(string)
	}
	if kid, ok := ph[cose.HeaderLabelKeyID]; ok {
		h.KeyID, _ = kid.([]byte)
	}
	claims, err := cwtClaims(ph)
	if err != nil {
		return h, err
	}
	h.Issuer, _ = claims[cose.CWTClaimIssuer].(string)
	h.Subject, _ = claims[cose.CWTClaimSubject].(string)
	h.IssuedAt = asInt64(claims[cose.CWTClaimIssuedAt])
	return h, nil
}

// Payload returns the opaque, media-type-versioned body (REQ-2102). The envelope layer
// does not interpret it.
func (s *Statement) Payload() []byte { return s.msg.Payload }

// Receipt returns the SCITT Transparency Service Receipt from the unprotected header, if
// one has been attached (REQ-2105). ok is false for a bare Signed Statement.
func (s *Statement) Receipt() (receipt []byte, ok bool) {
	if s.msg.Headers.Unprotected == nil {
		return nil, false
	}
	v, present := s.msg.Headers.Unprotected[headerLabelReceipts]
	if !present {
		return nil, false
	}
	b, ok := v.([]byte)
	return b, ok
}

// AttachReceipt records a Transparency Service Receipt in the UNPROTECTED header (label
// 394), forming a Transparent Statement. The unprotected header is not covered by the COSE
// signature, so attaching a Receipt after signing does not invalidate it — which is exactly
// the SCITT registration order: sign, register, receive Receipt, attach (REQ-2105).
func (s *Statement) AttachReceipt(receipt []byte) {
	if s.msg.Headers.Unprotected == nil {
		s.msg.Headers.Unprotected = cose.UnprotectedHeader{}
	}
	s.msg.Headers.Unprotected[headerLabelReceipts] = receipt
	// A statement returned from signing has been round-tripped through the decoder, so its
	// RawUnprotected holds the (empty) unprotected header verbatim. go-cose's MarshalCBOR prefers
	// RawUnprotected over the Unprotected map when it is set — which would silently DROP the Receipt
	// we just added on re-encode. Clear it so the map (with the Receipt) is the encoding source.
	s.msg.Headers.RawUnprotected = nil
}

// ValidateEnvelope checks that the statement is a well-formed groundnet SCITT envelope,
// INDEPENDENTLY of the payload body and of signature verification (REQ-2100):
//   - the required protected-header claims (iss, sub, kid, content_type) are present;
//   - the Issuer is a gnpub: pseudonym (REQ-2103);
//   - no x5t / x5chain identity binding is present (REQ-2103).
//
// It does NOT read the payload and does NOT verify the signature; those are the payload
// registry (REQ-2102) and the ingest signature guard (REQ-2104) respectively.
func (s *Statement) ValidateEnvelope() error {
	ph := s.msg.Headers.Protected
	if ph == nil {
		return ErrMalformedStatement
	}
	if _, hasX5C := ph[headerLabelX5Chain]; hasX5C {
		return ErrIdentityBinding
	}
	if _, hasX5T := ph[headerLabelX5T]; hasX5T {
		return ErrIdentityBinding
	}
	h, err := s.Header()
	if err != nil {
		return err
	}
	return validateHeaderFields(h)
}

// ComputeSubject derives the content-addressed sub claim: a hash over the de-identified
// statement content (its media type and payload). Two statements with identical content
// share a sub, which the contract reads as amendment/supersession (canonical spec §10) and
// which the replay guard keys on (REQ-2115). It hashes no estate-specific data because the
// payload is generalizable-only (REQ-2101).
func ComputeSubject(contentType string, payload []byte) string {
	sum := sha256.New()
	sum.Write([]byte(contentType))
	sum.Write([]byte{0})
	sum.Write(payload)
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

// cwtClaims extracts the CWT_Claims map (label 15) from a protected header, tolerating both
// the typed form (as built) and the generic CBOR map form (as decoded).
func cwtClaims(ph cose.ProtectedHeader) (cose.CWTClaims, error) {
	v, ok := ph[cose.HeaderLabelCWTClaims]
	if !ok {
		return nil, ErrMissingClaim
	}
	switch c := v.(type) {
	case cose.CWTClaims:
		return c, nil
	case map[any]any:
		return cose.CWTClaims(c), nil
	default:
		return nil, ErrMalformedStatement
	}
}

// asInt64 coerces a CBOR-decoded integer claim (which may arrive as int64/uint64/int) to
// int64.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}
