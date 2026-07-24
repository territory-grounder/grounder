package proposal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/territory-grounder/grounder/core/manifest"
)

// proposalJSON is the strict wire shape a model tool-call must match. It is the ONLY accepted grammar.
// json unmarshalling with DisallowUnknownFields rejects any extra key, so a second grammar smuggled as
// additional fields does not parse.
type proposalJSON struct {
	ExternalRef    string            `json:"external_ref"`
	Target         string            `json:"target"`
	OpClass        string            `json:"op_class"`
	Op             string            `json:"op"`
	Params         map[string]string `json:"params"`
	Reversible     bool              `json:"reversible"`
	ApprovalChoice string            `json:"approval_choice"`
	Confidence     float64           `json:"confidence"`
	Rationale      string            `json:"rationale"`
	EvidenceIDs    []string          `json:"evidence_ids"`
}

// Parse errors. Every one routes the caller to POLL_PAUSE (fail closed). They are sentinel-wrapped so
// a caller can branch on the class without string matching.
var (
	// ErrUnparseable: the response is not a single schema-valid JSON tool-call (markdown, a second
	// grammar, unknown fields, or trailing data).
	ErrUnparseable = errors.New("proposal: response is not a schema-valid tool-call (no second grammar is accepted)")
	// ErrIncompleteProposal: a required field is missing.
	ErrIncompleteProposal = errors.New("proposal: tool-call missing a required field (external_ref, target, op_class, op)")
	// ErrConfidenceRange: confidence is outside [0,1].
	ErrConfidenceRange = errors.New("proposal: confidence outside [0,1]")
)

// ParseProposal is the SOLE entry point that turns a model tool-call response into a typed Proposal.
// It fails closed on ANY input that is not a single, complete, schema-valid tool-call — there is no
// markdown/sentinel fallback path, so a second grammar cannot exist:
//   - DisallowUnknownFields rejects extra keys (a smuggled second grammar).
//   - The trailing-data check rejects a second JSON object appended after the first.
//   - Markdown containing "[AUTO-RESOLVE]" or "Which plan? - Plan A:" is not valid JSON and is rejected.
//
// [O] INV-06, H-02, spec/002 REQ-102.
func ParseProposal(resp []byte) (Proposal, error) {
	dec := json.NewDecoder(bytes.NewReader(resp))
	dec.DisallowUnknownFields()

	var pj proposalJSON
	if err := dec.Decode(&pj); err != nil {
		return Proposal{}, fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
	// Reject anything after the first JSON value — a second appended object is a second grammar.
	if dec.More() {
		return Proposal{}, fmt.Errorf("%w: trailing data after the tool-call", ErrUnparseable)
	}
	if pj.ExternalRef == "" || pj.Target == "" || pj.OpClass == "" || pj.Op == "" {
		return Proposal{}, ErrIncompleteProposal
	}
	if pj.Confidence < 0 || pj.Confidence > 1 {
		return Proposal{}, ErrConfidenceRange
	}
	return Proposal{
		ExternalRef: pj.ExternalRef,
		Action: manifest.Action{
			Target:     pj.Target,
			OpClass:    pj.OpClass,
			Op:         pj.Op,
			Params:     pj.Params,
			Reversible: pj.Reversible,
		},
		ApprovalChoice: pj.ApprovalChoice,
		Confidence:     pj.Confidence,
		Rationale:      pj.Rationale,
		EvidenceIDs:    pj.EvidenceIDs,
	}, nil
}
