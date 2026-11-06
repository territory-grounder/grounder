package main

import (
	"context"
	"errors"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/manifest"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/runner"
)

// rollbackExecReader / rollbackManifestGetter are the narrow read seams the backend needs — interface-typed so
// the fail-closed refusals (unknown / already-inverted / irreversible), which never reach Temporal, are
// oracle-testable with in-memory fakes. The concrete db stores satisfy them.
type rollbackExecReader interface {
	LatestExecution(ctx context.Context, actionID string) (db.ForwardExecution, bool, error)
	InversesOf(ctx context.Context, forwardActionID, externalRef string) ([]db.InverseExecution, error)
}
type rollbackManifestGetter interface {
	Get(ctx context.Context, actionID string) (*manifest.ActionManifest, bool, error)
}

// rollbackWorkflowStarter is the narrow Temporal seam the backend needs — interface-typed (client.Client
// satisfies it) so the pre-check refusals AND the InversesOf idempotency layer can be proven in an oracle with a
// fake starter, INDEPENDENT of Temporal's own REJECT_DUPLICATE dedup.
type rollbackWorkflowStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// grounderRollback is the process-wide manual-rollback backend, set once at boot when a Temporal client is
// wired (mirroring the ingest-counter package-var pattern so buildPublicAPI's already-longest signature does not
// grow another positional parameter — the documented positional-rebind hazard). nil ⇒ POST
// /v1/actions/{action_id}/rollback fails closed to 503, exactly like every other worker-backed write surface.
var grounderRollback httpapi.Rollbacker

// rollbackBackend implements httpapi.Rollbacker (TG-462): the grounder never seals or actuates the inverse
// itself — it does a fast, read-only pre-check (so an operator learns immediately that a target is unknown/
// never-executed → 404, not cleanly reversible → 400, or already rolled back → 409) and then starts the
// governed runner.RollbackWorkflow in the WORKER, which seals the content-hashed inverse, binds the forward
// execution record as evidence, classifies it POLL_PAUSE, and drives it through the interceptor with
// InvertsActionID set (inert under Shadow). The idempotency is doubly enforced: the InversesOf read here AND a
// REJECT_DUPLICATE workflow id keyed by the forward action, so a concurrent second request cannot start a
// second undo.
type rollbackBackend struct {
	tc        rollbackWorkflowStarter
	execs     rollbackExecReader
	manifests rollbackManifestGetter
}

func (b rollbackBackend) StartRollback(ctx context.Context, forwardActionID, operator string) (httpapi.RollbackRequested, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	forwardActionID = strings.TrimSpace(forwardActionID)
	// The deterministic external_ref the INVERSE execution row is written under (buildRollbackRequest sets
	// req.ExternalRef = in.RollbackExternalRef, and the interceptor records the inverse execution under it). It is
	// the SINGLE source of truth used for BOTH the InversesOf idempotency read below and in.RollbackExternalRef,
	// so the two can never disagree — the mismatch that silently disabled this check.
	rollbackRef := "rollback:" + forwardActionID

	// 1) The action must have actually EXECUTED — only a previously-executed forward action can be rolled back.
	//    NOTE: this gates on "an execution ROW exists" (executed), NOT on a verified-success verdict — a forward
	//    that ran but deviated is still rollback-able; the interceptor re-evaluates the inverse on its own merits.
	fe, found, err := b.execs.LatestExecution(ctx, forwardActionID)
	if err != nil {
		return httpapi.RollbackRequested{}, err
	}
	if !found {
		return httpapi.RollbackRequested{}, httpapi.ErrRollbackUnknownAction
	}
	// 2) IDEMPOTENCY (no double-undo). Refuse if the action is ITSELF a rollback (an inverse can't be inverted),
	//    or if an inverse of it has already run. The InversesOf key MUST be the rollback external_ref the inverse
	//    row is written with (rollbackRef), NOT the forward incident's ref — keying on the forward ref never
	//    matches and silently disables this check.
	if strings.TrimSpace(fe.InvertsActionID) != "" {
		return httpapi.RollbackRequested{}, httpapi.ErrRollbackAlreadyInverted
	}
	inverses, err := b.execs.InversesOf(ctx, forwardActionID, rollbackRef)
	if err != nil {
		return httpapi.RollbackRequested{}, err
	}
	if len(inverses) > 0 {
		return httpapi.RollbackRequested{}, httpapi.ErrRollbackAlreadyInverted
	}
	// 3) Load the sealed forward manifest for the authoritative op-class / op / params / reversibility the
	//    inverse derives from (never the request body).
	m, ok, err := b.manifests.Get(ctx, forwardActionID)
	if err != nil {
		return httpapi.RollbackRequested{}, err
	}
	if !ok || m == nil {
		return httpapi.RollbackRequested{}, httpapi.ErrRollbackUnknownAction
	}
	target := strings.TrimSpace(fe.TargetHost)
	if target == "" {
		target = m.Action.Target
	}
	in := runner.RollbackInput{
		ForwardActionID:     forwardActionID,
		ForwardOpClass:      m.Action.OpClass,
		ForwardOp:           m.Action.Op,
		ForwardTarget:       target,
		ForwardParams:       m.Action.Params,
		ForwardReversible:   m.Action.Reversible,
		ForwardSite:         fe.Site,
		ForwardExternalRef:  fe.ExternalRef,
		RollbackExternalRef: rollbackRef,
		Operator:            operator,
	}
	// 4) REVERSIBILITY pre-check (fast 400) — the SAME rollbackArgvFor gate the seal activity re-runs as the
	//    authority. Returns the inverse's deterministic content-addressed id for the response.
	inverseID, verr := runner.ValidateRollback(in)
	if verr != nil {
		return httpapi.RollbackRequested{}, httpapi.ErrRollbackIrreversible
	}
	// 5) Start the governed workflow. The workflow id is keyed by the forward action (via the rollback
	//    external_ref) with REJECT_DUPLICATE, so the existing POST /v1/vote path signals it by external_ref AND
	//    a concurrent duplicate rollback of the same forward action is rejected by Temporal (no double-undo).
	wfID := tg.WorkflowID(in.RollbackExternalRef)
	_, serr := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    wfID,
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, runner.RollbackWorkflow, in)
	if serr != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(serr, &already) {
			// A rollback of this forward action is already in flight (or already ran) — no double-undo.
			return httpapi.RollbackRequested{}, httpapi.ErrRollbackAlreadyInverted
		}
		return httpapi.RollbackRequested{}, serr
	}
	return httpapi.RollbackRequested{
		ForwardActionID: forwardActionID,
		InverseActionID: inverseID,
		WorkflowID:      wfID,
		Band:            "POLL_PAUSE",
		Status:          "pending-approval",
	}, nil
}
