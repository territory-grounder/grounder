package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/policy"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/enginetoggle"
)

// engineToggleBackend implements httpapi.EngineToggler (spec/015 REQ-1519): the grounder never toggles the
// policy engine itself — the change executes in the WORKER on the single live EngineToggle via the distinctly-
// named enginetoggle.EngineToggleWorkflow, so the wired AuthorityChecker + the warn-don't-block acknowledgement
// gate it and the immutable record is appended by the worker (the governance ledger's single writer). Mirrors
// modeTransitionBackend.
type engineToggleBackend struct {
	tc client.Client
}

func (b engineToggleBackend) ToggleEngine(ctx context.Context, enable bool, reason, operator string, adminAuthorized, doubleConfirm bool) (httpapi.EngineToggleOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// A SINGLE current override ⇒ one stable workflow id serializes toggles: a completed toggle may be
		// followed by the next (ALLOW_DUPLICATE), while an IN-FLIGHT duplicate (a double console click) is
		// rejected by Temporal's running-dedup.
		ID:                    "tg/enginetoggle",
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, enginetoggle.EngineToggleWorkflow, enginetoggle.Request{
		// operator + adminAuthorized were DERIVED at the surface from the authenticated admin-session principal.
		// The reason is mandatory (enforced at the surface) and doubles as the acknowledgement text, so a
		// non-empty reason IS the single acknowledgement; the red double-confirmation is the operator's explicit
		// second confirmation for disabling the engine in an actuating mode.
		Enable: enable, Actor: operator, Reason: reason, AdminAuthorized: adminAuthorized,
		Acknowledged: true, DoubleConfirm: doubleConfirm,
	})
	if err != nil {
		return httpapi.EngineToggleOutcome{}, err
	}
	var res enginetoggle.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.EngineToggleOutcome{}, unwrapEngineToggleErr(err)
	}
	return httpapi.EngineToggleOutcome{Enabled: res.Enabled, Mode: res.Mode, WarningCode: res.WarningCode, WarningText: res.WarningText}, nil
}

// unwrapEngineToggleErr maps a workflow-wrapped refusal back onto the typed errors so the surface returns the
// honest status (a Temporal ApplicationError carries only the message). Ordered longest-message-first so a
// more specific message is matched before a substring (mirrors unwrapModeErr).
func unwrapEngineToggleErr(err error) error {
	msg := err.Error()
	for _, known := range []error{
		policy.ErrEngineToggleNotConfirmed, policy.ErrUnauthorizedEngineToggle,
		enginetoggle.ErrNoToggle, enginetoggle.ErrRationaleRequired,
	} {
		if strings.Contains(msg, known.Error()) {
			return fmt.Errorf("%w (worker refused)", known)
		}
	}
	return err
}
