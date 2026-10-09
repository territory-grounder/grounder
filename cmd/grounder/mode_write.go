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
	"github.com/territory-grounder/grounder/temporal/modetransition"
)

// modeTransitionBackend implements httpapi.ModeTransitioner (spec/015 REQ-1502): the grounder never flips
// the autonomy mode itself — the LAST gate before the mutation flip executes in the WORKER on the single
// chokepoint-bound ModeController via the distinctly-named modetransition.ModeTransitionWorkflow (the wired
// AuthorityChecker + green preflight gate the flip, and both outcomes are audited to the hash chain by the
// single-writer worker).
type modeTransitionBackend struct {
	tc client.Client
}

func (b modeTransitionBackend) TransitionMode(ctx context.Context, to, expectedFrom, reason, operator string, adminAuthorized bool) (httpapi.ModeTransitionOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// A SINGLE active mode ⇒ one stable workflow id serializes flips: a completed flip may be followed by
		// the next (ALLOW_DUPLICATE), while an IN-FLIGHT duplicate (a double console click) is rejected by
		// Temporal's running-dedup.
		ID:                    "tg/modetransition",
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, modetransition.ModeTransitionWorkflow, modetransition.Request{
		To: to, ExpectedFrom: expectedFrom, Actor: operator, Reason: reason, AdminAuthorized: adminAuthorized,
	})
	if err != nil {
		return httpapi.ModeTransitionOutcome{}, err
	}
	var res modetransition.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.ModeTransitionOutcome{}, unwrapModeErr(err)
	}
	return httpapi.ModeTransitionOutcome{Mode: res.Mode, From: res.From, To: res.To}, nil
}

// unwrapModeErr maps a workflow-wrapped refusal back onto the typed policy errors so the surface returns the
// honest status (a Temporal ApplicationError carries only the message) — the same longest-message-first
// discipline as the config-write backend. Ordered so a more specific message is matched before a substring.
func unwrapModeErr(err error) error {
	msg := err.Error()
	for _, known := range []error{
		policy.ErrUnauthorizedModeChange, policy.ErrEmptyModeChangeActor, policy.ErrPreflightNotGreen,
		policy.ErrStaleMode, policy.ErrUnknownMode,
	} {
		if strings.Contains(msg, known.Error()) {
			return fmt.Errorf("%w (worker refused)", known)
		}
	}
	return err
}
