package predict

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/territory-grounder/grounder/core/manifest"
	"github.com/territory-grounder/grounder/core/proposal"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// Mode is the prediction-gate mode. The zero value is ModeEnforce — the fail-closed default; a
// misconfiguration cannot accidentally leave the gate advisory.
type Mode int

const (
	ModeEnforce      Mode = iota // the approval poll blocks on the committed prediction (default)
	ModeAnalysisOnly             // record prediction + shadow verdict, do NOT block (fail-open advisory)
)

// GatedProposal carries a proposal WITH a committed prediction and a sealed ActionManifest. The `gated`
// field is UNEXPORTED and set only by PredictionGate.Commit, so no external code can forge a gated
// proposal: BuildApprovalPoll default-denies any value whose prediction was not committed by the gate,
// which is the structural closure of H-02 (a poll can never run for an ungated/alternate-grammar
// proposal). [O] INV-06/INV-07.
type GatedProposal struct {
	proposal proposal.Proposal
	record   PredictionRecord
	manifest *manifest.ActionManifest
	gated    bool // set true ONLY by Commit
}

// Proposal returns the underlying parsed proposal.
func (gp GatedProposal) Proposal() proposal.Proposal { return gp.proposal }

// Manifest returns the sealed ActionManifest bound to this gated proposal.
func (gp GatedProposal) Manifest() *manifest.ActionManifest { return gp.manifest }

// Prediction returns the committed machine prediction.
func (gp GatedProposal) Prediction() verify.Prediction { return gp.record.Prediction }

// Gated reports whether a committed prediction produced this proposal (only Commit sets it true).
func (gp GatedProposal) Gated() bool { return gp.gated }

// PredictionGate commits a plan_hash-keyed prediction BEFORE any approval poll and is the ONLY
// constructor of a GatedProposal. It runs in the fail-closed remediation lane.
type PredictionGate struct {
	Store PredictionStore
	Model *InfragraphModel
	Mode  Mode
}

// predictionHash is the content hash of the committed prediction, bound into the ActionManifest so the
// approval and verdict are provably about the same prediction.
func predictionHash(p verify.Prediction) string {
	h := sha256.New()
	h.Write([]byte(p.ActionID))
	h.Write([]byte(p.PlanHash))
	h.Write([]byte(p.TargetHost))
	h.Write([]byte(p.Site))
	hosts := make([]string, 0, len(p.PredictedHosts))
	for host := range p.PredictedHosts {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		h.Write([]byte(host))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Commit computes the machine prediction OUTSIDE the LLM, persists it append-only (BEFORE any approval
// poll — REQ-101), seals the ActionManifest binding the action to its band, plan hash, and prediction
// hash, and returns a GatedProposal. Default-deny: a proposal whose action id cannot be derived errors
// and no gated proposal is produced.
// commonCause is threaded to the model so the common-cause sibling expansion (and its mirrored negative
// control) fire only for availability/connectivity incidents; the caller derives it via SiblingsEligible.
func (g *PredictionGate) Commit(ctx context.Context, p proposal.Proposal, planHash, site string, band safety.Band, commonCause bool) (GatedProposal, error) {
	actionID, err := p.Action.ID()
	if err != nil {
		return GatedProposal{}, fmt.Errorf("predict: derive action id: %w", err)
	}
	pred := g.Model.Predict(actionID, planHash, p.Action.Target, site, commonCause)
	ver, err := schema.Stamp(schema.TableInfragraphPrediction)
	if err != nil {
		return GatedProposal{}, err
	}
	rec := PredictionRecord{
		Prediction:     pred,
		ControlHosts:   g.Model.controlHosts(planHash, p.Action.Target, len(pred.PredictedHosts), commonCause),
		SchemaVersion:  ver,
		PredictionHash: predictionHash(pred),
		ExternalRef:    p.ExternalRef, // the session key (ADR-0010), carried onto the prediction for the calibrator join (spec/020 REQ-2019)
	}
	if err := g.Store.Commit(ctx, rec); err != nil {
		return GatedProposal{}, err
	}
	m, err := manifest.New(p.Action, band, planHash, rec.PredictionHash)
	if err != nil {
		return GatedProposal{}, err
	}
	return GatedProposal{proposal: p, record: rec, manifest: m, gated: true}, nil
}

// ApprovalPoll is the human-circuit-breaker poll. It can be built ONLY from a gated proposal.
type ApprovalPoll struct {
	ActionID string
	PlanHash string
	Choices  []string
	Blocking bool // false only in analysis-only mode (fail-open advisory)
}

// ErrNotGated is returned when an approval poll is requested for a proposal with no committed
// prediction (default-deny). This is the runtime face of the compile-time GatedProposal constraint.
var ErrNotGated = errors.New("predict: approval poll requires a GatedProposal with a committed prediction (default-deny)")

// BuildApprovalPoll builds an approval poll from a GatedProposal. It DENIES by default any proposal
// whose prediction the gate did not commit (gated=false). In analysis-only mode the poll is
// non-blocking (fail-open advisory) but the prediction was still committed. [O] INV-06, REQ-102/REQ-105.
func BuildApprovalPoll(gp GatedProposal, mode Mode) (ApprovalPoll, error) {
	if !gp.gated {
		return ApprovalPoll{}, ErrNotGated
	}
	return ApprovalPoll{
		ActionID: gp.manifest.ActionID,
		PlanHash: gp.record.Prediction.PlanHash,
		Choices:  []string{"approve", "reject"},
		Blocking: mode != ModeAnalysisOnly,
	}, nil
}
