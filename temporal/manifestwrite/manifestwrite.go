// Package manifestwrite executes world-model manifest transitions in the WORKER — the ledger's single
// writer (spec/027 REQ-2703; the same rule spec/014 REQ-1311 imposes on skill versions). The grounder's
// authenticated review surface never appends to the hash chain itself: it starts this workflow and waits
// for the result, so every adopt/reject/retire runs through the one worldmodel.Transition state machine
// with the worker's ledger, and a concurrent grounder can never fork the chain.
//
// WHY THIS MATTERS MORE HERE THAN FOR SKILLS: an adopted entry materializes into the actuation
// allowlist. A second status writer would mean a grant whose ledger entry could be missing or out of
// order — the audit trail would no longer explain why the leaf accepts a target.
package manifestwrite

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/worldmodel"
	"github.com/territory-grounder/grounder/temporal/wfauth"
)

// Request is the typed transition order. Approver is SERVER-DERIVED at the surface (operatorOf on the
// session principal) and never client-supplied — a client that could name its own approver could launder
// a grant through someone else's identity.
type Request struct {
	EntryID   int64
	To        worldmodel.Status
	Rationale string
	Approver  string
	// Auth is the grounder's signed proof over AuthFields (TG-660); Approver is a claim until it verifies.
	Auth wfauth.Envelope
}

// AuthKind / AuthFields: the envelope kind and canonical fields (one function for signer and verifier).
const AuthKind = "manifestwrite"

// AuthFields renders the authorization-relevant fields of the request.
func (r Request) AuthFields() []string {
	return []string{wfauth.Int(r.EntryID), string(r.To), r.Rationale, r.Approver}
}

// Result is the transitioned entry's essentials for the console re-render.
type Result struct {
	EntryID   int64
	Name      string
	Status    worldmodel.Status
	LedgerSeq int64
}

// ErrNotFound is the worker's signal that the id names no reviewable row. It is a DECISION, not a
// transient: the surface maps it to 404 rather than retrying.
var ErrNotFound = errors.New("manifestwrite: unknown manifest entry")

// Loader reads the row the transition applies to. Split from worldmodel.Store because the state machine
// takes an Entry, not an id: the worker loads, then transitions, so the row the ledger describes is the
// row the worker actually read.
type Loader interface {
	EntryByID(ctx context.Context, id int64) (worldmodel.Entry, bool, error)
}

// Deps are the worker-side collaborators.
type Deps struct {
	Loader Loader
	Store  worldmodel.Store
	Ledger worldmodel.Ledger
	// Auth verifies the request's signed envelope (TG-660). nil ⇒ every request is refused (no key).
	Auth wfauth.EnvelopeVerifier
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ledgerRefusal adapts the worldmodel ledger seam to wfauth.Require's best-effort sink (nil-safe).
func (a *Activities) ledgerRefusal(_ context.Context, d audit.GovDecision) error {
	if a.D.Ledger == nil {
		return nil
	}
	_, err := a.D.Ledger.Append(d)
	return err
}

// ManifestTransitionActivity loads the row and runs the single audited state machine. The approver is passed
// through as the state machine's own parameter (it is persisted on the row AND rides the ledger reason) —
// deliberately NOT smuggled into the rationale text the way skillwrite must, because worldmodel.Transition
// takes an approver argument.
func (a *Activities) ManifestTransitionActivity(ctx context.Context, req Request) (Result, error) {
	// THE ENVELOPE FIRST (TG-660): Approver is a claim; the MAC is the proof. Verified before the row is read.
	if err := wfauth.Require(ctx, a.D.Auth, AuthKind, req.Auth, req.Approver, a.ledgerRefusal, req.AuthFields()...); err != nil {
		return Result{}, err
	}
	e, found, err := a.D.Loader.EntryByID(ctx, req.EntryID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, ErrNotFound
	}
	out, err := worldmodel.Transition(ctx, a.D.Store, a.D.Ledger, e, req.To, req.Approver, req.Rationale)
	if err != nil {
		return Result{}, err
	}
	return Result{EntryID: out.ID, Name: out.Name, Status: out.Status, LedgerSeq: out.LedgerSeq}, nil
}

// ManifestTransitionWorkflow is the one-activity transition workflow. Named DISTINCTLY — Temporal
// registers by BARE function name, so a plain `TransitionWorkflow` here would collide with
// skillwrite.TransitionWorkflow and panic the worker at boot (the 2026-07-17 boot-loop). The ACTIVITY is
// renamed for the same reason: skillwrite already registers a bare `TransitionActivity`, and that
// collision the workflow-name guard test does not cover. Both join temporal/skilltrial's names guard.
//
// No retries on the activity: a refused transition (terminal status,
// missing rationale, unknown entity type) is a DECISION, not a transient — it surfaces verbatim rather
// than being re-attempted against a row whose status the first attempt may already have moved.
func ManifestTransitionWorkflow(ctx workflow.Context, req Request) (Result, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	var res Result
	// Temporal dispatches activities by REGISTERED FUNCTION NAME: the zero-Deps receiver here only names
	// the activity; the worker's registered instance (with the real Loader+Store+Ledger) executes it.
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), new(Activities).ManifestTransitionActivity, req).Get(ctx, &res)
	return res, err
}
