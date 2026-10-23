// Package modetransition executes an operator-invoked autonomy-mode transition in the WORKER — the
// process that owns the single live ModeController the actuation chokepoint consults (spec/015 REQ-1502).
// The grounder's admin-session surface (POST /v1/mode) never flips the mode itself: it starts this
// one-activity workflow and waits, exactly as the config-write / sealed-secret writes do, so:
//   - the transition runs on the SAME *policy.ModeController the chokepoint is bound to (BindMode) — the
//     one source of "may actuate?" — instead of a second, split-brain controller in the grounder whose
//     writes the worker's cached mode would never observe;
//   - the immutable mode-transition record is appended by the worker (the governance ledger's single
//     writer), so a concurrent grounder can never fork the hash chain; and
//   - the transition is gated EXACTLY as mode.go gates it: the controller's wired AuthorityChecker (the
//     operator must be flip-authorized) AND, for any escalation into an actuating mode, the green preflight.
//
// This workflow ENABLES an operator-invoked transition; it never auto-transitions anything. Nothing here
// runs on a timer or a cron — it fires only when an authenticated, admin-session operator posts to
// /v1/mode. The default/absent/corrupt mode stays Shadow (REQ-1519); mutation stays OFF until the owner
// actually flips.
//
// Provenance: [O] INV-19 (single-writer audit), INV-01 (the surface is authenticated) · [R] spec/015
// REQ-1502 (authenticated, authority-checked, audited mode change) · mirrors temporal/configwrite.
package modetransition

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/policy"
)

// ErrRationaleRequired refuses a transition with no stated reason — enforced at the surface AND here (the
// authority), like every governed write lane.
var ErrRationaleRequired = errors.New("modetransition: rationale required — every mode change states why it exists")

// ErrNoController fails closed when the activity has no bound ModeController (a misconfigured worker).
var ErrNoController = errors.New("modetransition: no mode controller bound — refusing an unaudited transition")

// Request is the typed transition order. Actor + AdminAuthorized are server-DERIVED at the surface from the
// AuthAdminSession principal (never from the request body). ExpectedFrom is an OPTIONAL compare-and-swap
// guard: when set, the transition is refused (audited) unless it is still the active mode.
type Request struct {
	To              string // canonical target mode name ("Shadow" | "HITL" | "Semi-auto" | "Full-auto")
	ExpectedFrom    string // OPTIONAL canonical current-mode the operator expects; "" ⇒ use the live active mode
	Actor           string // the authenticated operator id (the mode-change actor recorded in the ledger)
	Reason          string // the mandatory rationale
	AdminAuthorized bool   // the operator authenticated through the admin tier (LDAP admin group / static admin)
}

// Result is the committed transition's essentials for the console response.
type Result struct {
	Mode string // the active mode AFTER the transition (unchanged on a refusal)
	From string // the mode the transition moved from
	To   string // the requested target mode
}

// Deps are the worker-side collaborators: the single live, chokepoint-bound mode controller.
type Deps struct {
	// Controller is the SAME *policy.ModeController the worker bound into the actuation chokepoint. The
	// transition executes on it so the chokepoint observes the new mode live.
	Controller *policy.ModeController
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ApplyModeTransitionActivity is the single-writer mode transition. It re-derives the active mode as the
// compare-and-swap `from` (or verifies the operator's ExpectedFrom), carries the admin-group signal into
// the authority check, and runs the transition through policy.AuditedModeTransition — which gates on the
// wired AuthorityChecker + the green preflight and audits BOTH outcomes (a success via the controller's own
// mode-transition record, a refusal via a mode-transition-refused record). A refused transition surfaces
// the typed policy error verbatim (no retry) so the surface can map it to an honest status.
func (a *Activities) ApplyModeTransitionActivity(ctx context.Context, req Request) (Result, error) {
	if a.D.Controller == nil {
		return Result{}, ErrNoController
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Result{}, ErrRationaleRequired
	}
	to, err := policy.ParseMode(req.To)
	if err != nil {
		return Result{}, err // unknown target ⇒ fail closed (ErrUnknownMode)
	}
	// Carry the trusted admin-group signal into the authority check (the "LDAP admin group" branch). The
	// surface proved the operator is an admin-session principal before this workflow ever started.
	if req.AdminAuthorized {
		ctx = policy.WithModeChangeAdmin(ctx)
	}
	// The compare-and-swap `from`: the live active mode, unless the operator pinned an ExpectedFrom (then the
	// controller's own CAS refuses — and audits — a stale expectation).
	from := a.D.Controller.Current()
	if strings.TrimSpace(req.ExpectedFrom) != "" {
		ef, perr := policy.ParseMode(req.ExpectedFrom)
		if perr != nil {
			return Result{}, perr
		}
		from = ef
	}
	newMode, terr := policy.AuditedModeTransition(ctx, a.D.Controller, from, to, req.Actor, req.Reason)
	res := Result{Mode: newMode.String(), From: from.String(), To: to.String()}
	if terr != nil {
		// Surface the policy refusal verbatim as a NON-retryable application error (a denied flip is a
		// DECISION, not a transient) so the grounder maps it to the honest status.
		return res, terr
	}
	return res, nil
}

// activityOpts: no retries — a refused transition (unauthorized, red preflight, stale from, unknown mode)
// is a DECISION, not a transient; it surfaces verbatim (mirrors temporal/configwrite).
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// ModeTransitionWorkflow is the one-activity mode-transition workflow. Named DISTINCTLY — Temporal
// registers by bare function name, and two packages both exporting `Workflow` collide at RegisterWorkflow
// (guarded by temporal/skilltrial/finalizer_names_test.go, on whose list this workflow now sits).
func ModeTransitionWorkflow(ctx workflow.Context, req Request) (Result, error) {
	var res Result
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).ApplyModeTransitionActivity, req).Get(ctx, &res)
	return res, err
}
