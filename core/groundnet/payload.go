package groundnet

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownPayloadType is returned when a consumer does not understand a statement's
// content_type. Per REQ-2102 the consumer rejects the PAYLOAD without rejecting the
// ENVELOPE — the SCITT envelope stays valid, so the standard holds while payloads evolve.
var ErrUnknownPayloadType = errors.New("groundnet: payload media type not understood")

// Verified-outcome vocabulary (canonical spec §4).
const (
	VerifierMechanical = "mechanical" // the only permitted verifier (INV-5)
	VerdictClean       = "clean"
	VerdictPartial     = "partial"
	VerdictDeviation   = "deviation"
)

// WisdomV0 is the application/vnd.groundnet.wisdom+json (wisdom/0) payload: a de-identified,
// generalizable-only remediation-wisdom unit (canonical spec §4). Every field names a KIND,
// never an INSTANCE — INV-2 forbids hostnames, addresses, topology, credentials or secret
// references, raw traces, ticket ids, and organisation names, so the TYPE ITSELF has no slot
// for estate-specific data (REQ-2101).
type WisdomV0 struct {
	AlertClass string          `json:"alert_class"`        // generalised alert category — never a host/addr/rule id
	Diagnosis  string          `json:"diagnosis"`          // generalised, de-identified root-cause finding
	OpClass    string          `json:"op_class"`           // CLASS of remediation op (e.g. restart-service), never a command
	Reversible bool            `json:"reversible"`         // governance property of the op-class
	BlastClass string          `json:"blast_class"`        // governance property of the op-class
	Outcome    WisdomOutcome   `json:"outcome"`            // the VERIFIED result
	Artifact   *WisdomArtifact `json:"artifact,omitempty"` // optional content-addressed graduated-artifact ref
}

// WisdomOutcome is the verified result. Verifier MUST be "mechanical" — an LLM-free,
// deterministic check — and not the reasoning system that proposed the remediation (INV-5).
type WisdomOutcome struct {
	Verifier string `json:"verifier"` // MUST be "mechanical"
	Verdict  string `json:"verdict"`  // ∈ {clean, partial, deviation}
	Method   string `json:"method"`   // the mechanical post-condition check, generalised
}

// WisdomArtifact is a content-addressed reference to a graduated artifact. Ref is a hash
// (e.g. "sha256:..."), NEVER an estate URL — the artifact is fetched out of band.
type WisdomArtifact struct {
	Kind string `json:"kind"` // runbook | skill | rubric
	Ref  string `json:"ref"`  // content-address hash, never a URL
}

// KnownPayloadType reports whether this node understands a payload media type (REQ-2102). A
// future flywheel payload version adds an entry; an already-shipped node returns false for
// the unknown type and rejects it at the payload layer while still validating the envelope.
func KnownPayloadType(mediaType string) bool {
	switch mediaType {
	case WisdomMediaTypeV1, ConfirmationMediaTypeV1, DeviationMediaTypeV1, WithdrawalMediaTypeV1:
		return true
	default:
		return false
	}
}

// DecodeWisdom reads and validates the wisdom/0 payload of a statement. It first checks the
// content_type: an unknown media type is rejected as ErrUnknownPayloadType WITHOUT
// invalidating the envelope (REQ-2102). It then enforces the verified-outcome invariant
// (INV-5). It never verifies the signature — that is the caller's ingest guard (REQ-2104).
func DecodeWisdom(s *Statement) (*WisdomV0, error) {
	h, err := s.Header()
	if err != nil {
		return nil, err
	}
	if h.ContentType != WisdomMediaTypeV1 {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPayloadType, h.ContentType)
	}
	var w WisdomV0
	if err := json.Unmarshal(s.Payload(), &w); err != nil {
		return nil, fmt.Errorf("groundnet: decoding wisdom/0 payload: %w", err)
	}
	if err := w.Validate(); err != nil {
		return nil, err
	}
	return &w, nil
}

// Validate enforces the wisdom/0 content invariants: the generalizable classes are present
// and the outcome is a mechanically verified verdict (INV-5). It cannot see estate-specific
// data because the type carries none (REQ-2101).
func (w *WisdomV0) Validate() error {
	if w.AlertClass == "" || w.OpClass == "" {
		return errors.New("groundnet: wisdom payload missing alert_class or op_class")
	}
	if w.Outcome.Verifier != VerifierMechanical {
		return fmt.Errorf("groundnet: wisdom outcome verifier must be %q (INV-5), got %q", VerifierMechanical, w.Outcome.Verifier)
	}
	switch w.Outcome.Verdict {
	case VerdictClean, VerdictPartial, VerdictDeviation:
	default:
		return fmt.Errorf("groundnet: wisdom outcome verdict %q not in {clean,partial,deviation}", w.Outcome.Verdict)
	}
	return nil
}

// Marshal encodes a wisdom/0 payload to its canonical JSON wire form.
func (w *WisdomV0) Marshal() ([]byte, error) {
	return json.Marshal(w)
}
