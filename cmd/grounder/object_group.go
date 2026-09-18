package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/objectgroup"
)

// grounderObjectGroups serves GET /v1/estate/groups — the operator-authored object groups (TG-481). A package
// var set once at boot from the pool (the same positional-rebind discipline as the native-rule backend).
// nil ⇒ the route 503s.
var grounderObjectGroups httpapi.ObjectGroupsReader

// grounderObjectGroupWrite starts the worker-side object-group write lane. nil (no Temporal client) ⇒ 503.
var grounderObjectGroupWrite httpapi.ObjectGroupWriter

// objectGroupsReadStore adapts the pgx object-group store to the console read surface, formatting timestamps.
type objectGroupsReadStore struct{ s *db.EstateObjectGroupStore }

func (r objectGroupsReadStore) ObjectGroups(ctx context.Context) ([]httpapi.ObjectGroup, error) {
	rows, err := r.s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.ObjectGroup, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.ObjectGroup{
			ID: row.ID, Name: row.Name, Patterns: row.Patterns, Precedence: row.Precedence,
			CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// objectGroupWriteBackend implements httpapi.ObjectGroupWriter (TG-481): the grounder never writes
// estate_object_group itself — the write executes in the WORKER (the governance ledger's single writer) via
// the distinctly-named objectgroup.ObjectGroupWriteWorkflow, which VALIDATES the payload, ledgers it BEFORE
// the row commits, and persists it.
type objectGroupWriteBackend struct {
	tc client.Client
}

func (b objectGroupWriteBackend) AddObjectGroup(ctx context.Context, name string, patterns []string, precedence, rationale, operator string, admin bool) (httpapi.ObjectGroupWriteOutcome, error) {
	return b.write(ctx, objectgroup.Request{
		Verb: "add", Name: name, Patterns: patterns, Precedence: precedence, Rationale: rationale, Operator: operator, AdminAuthorized: admin,
	})
}

func (b objectGroupWriteBackend) DeleteObjectGroup(ctx context.Context, id int64, rationale, operator string, admin bool) (httpapi.ObjectGroupWriteOutcome, error) {
	return b.write(ctx, objectgroup.Request{
		Verb: "delete", RowID: id, Rationale: rationale, Operator: operator, AdminAuthorized: admin,
	})
}

func (b objectGroupWriteBackend) write(ctx context.Context, req objectgroup.Request) (httpapi.ObjectGroupWriteOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// ONE object-group table ⇒ one stable workflow id serializes writes (the rulesetwrite discipline): a
		// completed write may be followed by the next (ALLOW_DUPLICATE), while an IN-FLIGHT duplicate is
		// rejected by Temporal's running-dedup — at most one object-group write runs at a time.
		ID:                    "tg/objectgroupwrite",
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, objectgroup.ObjectGroupWriteWorkflow, req)
	if err != nil {
		return httpapi.ObjectGroupWriteOutcome{}, err
	}
	var res objectgroup.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.ObjectGroupWriteOutcome{}, unwrapObjectGroupErr(err)
	}
	return httpapi.ObjectGroupWriteOutcome{ID: res.ID, LedgerSeq: res.LedgerSeq}, nil
}

// unwrapObjectGroupErr maps a workflow-wrapped refusal (a Temporal ApplicationError carries only the message)
// back onto the httpapi sentinels the handler maps to honest statuses — the native-rule discipline.
func unwrapObjectGroupErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, objectgroup.ErrNoSuchGroup.Error()) {
		return fmt.Errorf("%w (worker refused)", httpapi.ErrNoSuchObjectGroup)
	}
	if strings.Contains(msg, objectgroup.ErrNotAdmin.Error()) {
		return fmt.Errorf("%w (worker refused)", httpapi.ErrObjectGroupNotAdmin)
	}
	for _, refusal := range []string{objectgroup.ErrInvalid.Error(), objectgroup.ErrUnknownVerb.Error()} {
		if strings.Contains(msg, refusal) {
			return fmt.Errorf("%w: %s", httpapi.ErrObjectGroupInvalid, msg)
		}
	}
	return err
}
