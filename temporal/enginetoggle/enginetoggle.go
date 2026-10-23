// Package enginetoggle executes an operator-invoked policy-engine enable/disable in the WORKER — the process
// that owns the live *policy.EngineToggle the AuditedEngine consults (spec/015 REQ-1519). The grounder's
// admin-session surface (POST /v1/policy/engine-toggle) never flips the engine itself: it starts this
// one-activity workflow and waits, exactly as the mode transition / config-write / sealed-secret writes do, so:
//   - the override runs on the SAME *policy.EngineToggle the worker attached to the AuditedEngine (WithToggle)
//     and backed with the durable store — persisting it there BEFORE the in-memory effect means the decision
//     plane observes it (via the worker's refresh Load), instead of a second, split-brain toggle in the
//     grounder whose durable write the worker would only pick up incidentally;
//   - the immutable engine-toggle record is appended by the worker (the governance ledger's single writer),
//     so a concurrent grounder can never fork the hash chain; and
//   - the change is gated EXACTLY as warn.go gates it: the toggle's wired AuthorityChecker (the operator must
//     be flip-authorized) AND the warn-don't-block acknowledgement (a single ack to force the engine ON in a
//     read-only mode; a DISTINCT red double-confirmation to disable it in an actuating mode).
//
// This workflow ENABLES an operator-invoked override; it never auto-toggles anything. Nothing here runs on a
// timer or cron — it fires only when an authenticated admin-session operator posts to the surface. The
// constitutional never-auto floor (INV-09) is unaffected either way: engine-off routes to a human (`approve`),
// never `auto`.
//
// Provenance: [O] INV-19 (single-writer audit), INV-01 (the surface is authenticated) · [R] spec/015 REQ-1519
// (authenticated, authority-checked, warn-don't-block engine toggle) · mirrors temporal/modetransition.
package enginetoggle

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/policy"
)

// ErrRationaleRequired refuses an override with no stated reason — the reason doubles as the audited
// acknowledgement text, and warn.go's ack requires a non-empty text, so a blank reason cannot confirm.
var ErrRationaleRequired = errors.New("enginetoggle: rationale required — every engine toggle states why it exists")

// ErrNoToggle fails closed when the activity has no bound EngineToggle (a worker booted without the toggle
// armed, TG_POLICY_ENGINE_TOGGLE unset). The surface maps this to an honest "not enabled" status.
var ErrNoToggle = errors.New("enginetoggle: no engine toggle bound — the policy-engine toggle is not armed on this deployment")

// Request is the typed override order. Actor + AdminAuthorized are server-DERIVED at the surface from the
// AuthAdminSession principal (never from the request body). The mode the override is evaluated against is the
// LIVE active mode, read server-side — it is never taken from the request.
type Request struct {
	Enable          bool   // enable (true) or disable (false) the policy engine
	Actor           string // the authenticated operator id (the toggle actor recorded in the ledger)
	Reason          string // the mandatory rationale; doubles as the acknowledgement text
	AdminAuthorized bool   // the operator authenticated through the admin tier (LDAP admin group / static admin)
	Acknowledged    bool   // the operator explicitly acknowledged the permissive-posture warning
	DoubleConfirm   bool   // the DISTINCT red double-confirmation required to DISABLE the engine in an actuating mode
}

// Result is the committed override's essentials for the console response.
type Result struct {
	Enabled     bool   // the engine's effective enable state after the override
	Mode        string // the live active mode the override was evaluated against
	WarningCode string // the permissive-posture warning code recorded, if any ("" when none)
	WarningText string // the human-readable warning message, if any
}

// Deps are the worker-side collaborators: the single live EngineToggle the AuditedEngine consults, and the
// live active-mode reader (the override is evaluated against the current mode's default direction).
type Deps struct {
	// Toggle is the SAME *policy.EngineToggle the worker attached to the AuditedEngine (WithToggle) and backed
	// with the durable store. Nil when the toggle is not armed (TG_POLICY_ENGINE_TOGGLE unset) ⇒ ErrNoToggle.
	Toggle *policy.EngineToggle
	// ModeNow reports the live active mode (policyModeCtl.Current) — the override is warn-gated against it.
	ModeNow func() policy.Mode
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ApplyEngineToggleActivity is the single-writer engine toggle. It reads the LIVE active mode, carries the
// trusted admin-group signal into the authority check, and runs policy.EngineToggle.Override — which gates on
// the wired AuthorityChecker AND the warn-don't-block acknowledgement, appends the immutable audit record
// BEFORE the effect, persists the override to the durable store, and only then advances the in-memory state. A
// refusal (unauthorized, unconfirmed) surfaces the typed policy error verbatim (no retry).
func (a *Activities) ApplyEngineToggleActivity(ctx context.Context, req Request) (Result, error) {
	if a.D.Toggle == nil {
		return Result{}, ErrNoToggle
	}
	if strings.TrimSpace(req.Reason) == "" {
		return Result{}, ErrRationaleRequired
	}
	// Carry the trusted admin-group signal into the authority check (the same seam the mode change uses). The
	// surface proved the operator is an admin-session principal before this workflow ever started.
	if req.AdminAuthorized {
		ctx = policy.WithModeChangeAdmin(ctx)
	}
	mode := policy.ModeShadow
	if a.D.ModeNow != nil {
		mode = a.D.ModeNow()
	}
	ack := policy.Warning{Text: req.Reason, Acknowledged: req.Acknowledged, DoubleConfirm: req.DoubleConfirm}
	rec, err := a.D.Toggle.Override(ctx, req.Actor, req.Enable, mode, ack)
	res := Result{Enabled: rec.Enabled, Mode: mode.String(), WarningCode: string(rec.Warning.Code), WarningText: rec.Warning.Message}
	if err != nil {
		// Surface the policy refusal verbatim as a NON-retryable application error (a denied toggle is a
		// DECISION, not a transient) so the grounder maps it to the honest status.
		return res, err
	}
	return res, nil
}

// activityOpts: no retries — a refused toggle (unauthorized, unconfirmed) is a DECISION, not a transient; it
// surfaces verbatim (mirrors temporal/modetransition).
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// EngineToggleWorkflow is the one-activity engine-toggle workflow. Named DISTINCTLY — Temporal registers by
// bare function name, and two packages both exporting `Workflow` collide at RegisterWorkflow (guarded by
// temporal/skilltrial/finalizer_names_test.go, on whose list this workflow now sits).
func EngineToggleWorkflow(ctx workflow.Context, req Request) (Result, error) {
	var res Result
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).ApplyEngineToggleActivity, req).Get(ctx, &res)
	return res, err
}
