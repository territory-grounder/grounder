package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/worldmodel"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/manifestwrite"
)

// manifestWriteBackend implements httpapi.ManifestWriter by starting the worker's transition workflow —
// the grounder never appends to the hash chain itself (spec/027 REQ-2703, the skillsWriteBackend
// precedent). Unlike skills there is no CreateDraft counterpart and no route that could want one:
// discovery is the sole author of manifest rows (paradigm rule 9).
type manifestWriteBackend struct{ tc client.Client }

func (b manifestWriteBackend) Transition(ctx context.Context, id int64, to worldmodel.Status, rationale, approver string) (httpapi.ManifestTransitionOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("tg/manifestwrite/%d/%s", id, to),
		TaskQueue: tg.TaskQueueRunner,
		// A completed same-id run may repeat (retire after adopt after a re-drafted row reuses ids); an
		// IN-FLIGHT duplicate is a double console click and is rejected by Temporal's running-dedup.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, manifestwrite.ManifestTransitionWorkflow, manifestwrite.Request{EntryID: id, To: to, Rationale: rationale, Approver: approver})
	if err != nil {
		return httpapi.ManifestTransitionOutcome{}, err
	}
	var res manifestwrite.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.ManifestTransitionOutcome{}, unwrapManifestErr(err)
	}
	return httpapi.ManifestTransitionOutcome{
		ID: res.EntryID, Name: res.Name, Status: string(res.Status), LedgerSeq: res.LedgerSeq,
	}, nil
}

// unwrapManifestErr maps a workflow-wrapped state-machine refusal back onto the typed sentinels the
// surface switches on, so a worker's "no" becomes the right status code instead of a blanket 500 (a
// Temporal ApplicationError carries only the message). Longest-message-first so no sentinel's text can
// shadow a more specific one — the skillwrite review's fragility note applies verbatim here; replace with
// typed unwrapping when the SDK propagates activity error chains.
func unwrapManifestErr(err error) error {
	msg := err.Error()
	for _, known := range []error{
		worldmodel.ErrUnknownEntityType, worldmodel.ErrRationaleRequired, worldmodel.ErrBadTransition,
	} {
		if strings.Contains(msg, known.Error()) {
			return fmt.Errorf("%w (%s)", known, "worker refused")
		}
	}
	// The worker's not-found is the surface's 404 sentinel. Mapped explicitly rather than falling through
	// to 500: an operator acting on a row another operator just retired must be told so, not shown a fault.
	if strings.Contains(msg, manifestwrite.ErrNotFound.Error()) {
		return fmt.Errorf("%w (%s)", httpapi.ErrManifestEntryNotFound, "worker refused")
	}
	return err
}

var _ httpapi.ManifestWriter = manifestWriteBackend{}
