// Package manifest defines the immutable, content-hashed ActionManifest — the binding spine of
// Territory Grounder.
//
// Provenance: [O] INV-07/INV-10 (one content-hashed action id threaded end to end; prediction &
// verification bound to the SAME action), H-03 (the predecessor's committed prediction was NOT
// bound to the executed action), P1-6.
//
// The load-bearing lesson: "a prediction exists" must never be mistaken for "the prediction is for
// the thing being executed." Identity — not existence — is what the gate protects. Any change to the
// Action yields a new action_id that invalidates all prior authorization and re-enters the gate.
//
// In Phase 0/1 the manifest is BUILT and THREADED (so the binding exists and is inspectable in the
// console) but no execution stage consumes it — that is Phase 2.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/territory-grounder/grounder/core/safety"
)

// Action is the normalized, canonical description of a proposed operation. Its fields are the only
// inputs to the content hash; changing any of them changes the action_id.
type Action struct {
	Target     string            `json:"target"`   // validated hostname / resource id
	OpClass    string            `json:"op_class"` // e.g. "restart-service", "kubectl-get" (matched vs never-auto floor)
	Op         string            `json:"op"`
	Params     map[string]string `json:"params"` // json.Marshal sorts map keys → deterministic
	Reversible bool              `json:"reversible"`
}

// canonicalJSON returns a deterministic byte encoding of the Action. json.Marshal is deterministic
// for structs (field order) and maps (sorted keys); a stricter canonical form (RFC 8785) can replace
// this later without changing the interface.
func canonicalJSON(a Action) ([]byte, error) { return json.Marshal(a) }

// ID computes action_id = SHA-256(canonicalJSON(Action)) — computed once and threaded unchanged
// through predict → approve → execute → verify. [O] INV-07.
func (a Action) ID() (string, error) {
	b, err := canonicalJSON(a)
	if err != nil {
		return "", fmt.Errorf("manifest: canonicalize action: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Provenance is the context an action was born from — the trigger and the inputs that produced it. It is
// bound into the manifest so the whole chain (why → what → predicted → approved → executed → verified) is
// one reconstructable record (the external audit's ActionManifest/ExecutionRecord recommendation, and INV-14
// reconstructability). None of it participates in the action_id — identity is the operation alone — but it
// is frozen once the first lifecycle stage is recorded.
type Provenance struct {
	IncidentRef       string `json:"incident_ref,omitempty"`        // the normalized trigger (external_ref) this action answers
	ContextSnapshotID string `json:"context_snapshot_id,omitempty"` // the RAG/estate context the plan was grounded on
	ModelRef          string `json:"model_ref,omitempty"`           // the model that produced the proposal
	PromptHash        string `json:"prompt_hash,omitempty"`         // content hash of the compiled prompt
}

// ActionManifest binds one immutable action to its lifecycle records. It is sealed at creation and
// persisted append-only; execute/verify are stubbed under mutation_enabled=false in Phase 0/1. The Stages
// slice is the append-only, action-id-bound lifecycle chain — the single immutable record the whole system
// replays, evaluates and audits against.
type ActionManifest struct {
	ActionID       string          `json:"action_id"`
	Action         Action          `json:"action"`
	Band           safety.Band     `json:"band"`
	PlanHash       string          `json:"plan_hash"`
	PredictionHash string          `json:"prediction_hash"`
	Provenance     Provenance      `json:"provenance"`
	Stages         []Stage         `json:"stages"` // append-only lifecycle chain, every stage bound to ActionID
	ApprovalChoice string          `json:"approval_choice"`
	ToolCalls      []string        `json:"tool_calls"`
	Verdict        *safety.Verdict `json:"verdict,omitempty"` // written only by the verifier, in Phase 2

	sealed bool
}

// StageName is a lifecycle stage of the bound action.
type StageName string

const (
	StagePredicted StageName = "predicted" // the machine prediction was committed (the authorizing artifact)
	StageApproved  StageName = "approved"  // an authorized approver selected an option
	StageExecuted  StageName = "executed"  // the actuation ran (Phase 2 only; mutation OFF in Phase 0/1)
	StageVerified  StageName = "verified"  // the mechanical verdict was written
)

// stageOrder is the monotonic lifecycle order; a stage may only be recorded after an earlier one.
var stageOrder = map[StageName]int{StagePredicted: 0, StageApproved: 1, StageExecuted: 2, StageVerified: 3}

// Stage is one immutable, action-id-bound entry in the lifecycle chain. PayloadHash is the SHA-256 of the
// stage's canonical payload (the committed prediction, the approval choice, the ACTUAL tool calls, the
// observed postconditions/verdict) so the chain records not just that a stage happened but the exact content
// it happened with — and ActionID ties every stage to the ONE authorized action.
type Stage struct {
	Stage       StageName `json:"stage"`
	ActionID    string    `json:"action_id"`
	PayloadHash string    `json:"payload_hash"`
	Seq         int       `json:"seq"`
}

// New seals a manifest around an action, computing and binding its action_id. The manifest is
// immutable after this call; mutating the action requires a NEW manifest with a NEW id.
func New(a Action, band safety.Band, planHash, predictionHash string) (*ActionManifest, error) {
	id, err := a.ID()
	if err != nil {
		return nil, err
	}
	return &ActionManifest{
		ActionID:       id,
		Action:         a,
		Band:           band,
		PlanHash:       planHash,
		PredictionHash: predictionHash,
		sealed:         true,
	}, nil
}

// Rehydrate reconstructs a SEALED manifest from its persisted fields and RE-ASSERTS its content hash: the
// stored action_id must equal the id re-derived from the stored action, or the row is rejected as tampered.
// This is the read-back path a durable store (core/db) must use — the unexported `sealed` flag cannot be set
// by a cross-package struct literal, so a hand-built manifest would fail Assert (`!sealed`) on every row and
// the INV-07 re-assertion would never actually run. Rehydrate performs that check itself and returns a sealed
// manifest ready for a further Assert against an expected id.
func Rehydrate(actionID string, a Action, band safety.Band, planHash, predictionHash string) (*ActionManifest, error) {
	derived, err := a.ID()
	if err != nil {
		return nil, err
	}
	if derived != actionID {
		return nil, fmt.Errorf("manifest: persisted action tampered — stored id %s != derived id %s", short(actionID), short(derived))
	}
	return &ActionManifest{
		ActionID:       actionID,
		Action:         a,
		Band:           band,
		PlanHash:       planHash,
		PredictionHash: predictionHash,
		sealed:         true,
	}, nil
}

// Assert re-derives the action_id from the bound action and checks it equals the sealed id AND the
// caller-provided expected id. Every Temporal activity calls this on receipt; a mismatch is a
// fail-closed hard error — the action being executed is not the action that was authorized. [O] INV-07.
func (m *ActionManifest) Assert(expectedActionID string) error {
	if !m.sealed {
		return fmt.Errorf("manifest: unsealed manifest may not be asserted")
	}
	derived, err := m.Action.ID()
	if err != nil {
		return err
	}
	if derived != m.ActionID {
		return fmt.Errorf("manifest: action tampered — bound id %s != derived id %s", short(m.ActionID), short(derived))
	}
	if expectedActionID != "" && expectedActionID != m.ActionID {
		return fmt.Errorf("manifest: action_id mismatch — expected %s got %s (authorized a different action)", short(expectedActionID), short(m.ActionID))
	}
	return nil
}

// WithProvenance binds the context this action was born from. It may be called only before the first
// lifecycle stage is recorded — provenance is part of the immutable record, not something rewritten after
// execution begins. Returns the manifest for chaining.
func (m *ActionManifest) WithProvenance(p Provenance) *ActionManifest {
	if len(m.Stages) == 0 {
		m.Provenance = p
	}
	return m
}

// payloadHash returns SHA-256(canonical JSON(payload)) — the content fingerprint of a stage payload. A nil
// payload hashes the empty object so a stage can be recorded without content.
func payloadHash(payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("manifest: hash stage payload: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Record appends a lifecycle stage to the chain, bound to THIS manifest's action_id and fingerprinting the
// stage's payload (the committed prediction, the approval choice, the actual tool calls, the observed
// postconditions/verdict). Stages are append-only and must advance in lifecycle order — a stage that would
// go backwards, or a record against an unsealed manifest, is a fail-closed error. Every stage carries the
// action_id, so the chain is self-evidently for ONE authorized action (this is the structural fix for the
// predecessor's "committed prediction not bound to the executed action" defect).
func (m *ActionManifest) Record(stage StageName, payload any) error {
	if !m.sealed {
		return fmt.Errorf("manifest: cannot record a stage on an unsealed manifest")
	}
	if _, ok := stageOrder[stage]; !ok {
		return fmt.Errorf("manifest: unknown stage %q", stage)
	}
	if n := len(m.Stages); n > 0 && stageOrder[stage] <= stageOrder[m.Stages[n-1].Stage] {
		return fmt.Errorf("manifest: stage %q may not follow %q (chain is append-only, in lifecycle order)", stage, m.Stages[n-1].Stage)
	}
	ph, err := payloadHash(payload)
	if err != nil {
		return err
	}
	m.Stages = append(m.Stages, Stage{Stage: stage, ActionID: m.ActionID, PayloadHash: ph, Seq: len(m.Stages)})
	return nil
}

// VerifyChain re-derives the action_id (proving the action is untampered) and asserts EVERY recorded stage
// binds that same action_id in strict lifecycle order. A stage bound to a different action_id — a prediction,
// approval or verdict for some OTHER action being passed off as this one's — is a fail-closed error. This is
// the whole-chain analogue of Assert: the audit's "one immutable typed chain from evidence to verdict".
func (m *ActionManifest) VerifyChain() error {
	if err := m.Assert(""); err != nil {
		return err
	}
	last := -1
	for i, s := range m.Stages {
		if s.ActionID != m.ActionID {
			return fmt.Errorf("manifest: stage %d (%s) is bound to a DIFFERENT action %s, not %s — chain broken", i, s.Stage, short(s.ActionID), short(m.ActionID))
		}
		o, ok := stageOrder[s.Stage]
		if !ok {
			return fmt.Errorf("manifest: stage %d has unknown name %q", i, s.Stage)
		}
		if o <= last {
			return fmt.Errorf("manifest: stage %d (%s) out of lifecycle order", i, s.Stage)
		}
		last = o
	}
	return nil
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
