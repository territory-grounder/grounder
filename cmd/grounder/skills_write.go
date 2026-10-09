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
	"github.com/territory-grounder/grounder/core/skillstore"
	tg "github.com/territory-grounder/grounder/temporal"
	"github.com/territory-grounder/grounder/temporal/skillwrite"
)

// skillsWriteBackend implements httpapi.SkillsWriter: drafts insert directly (validated rows, no ledger
// involvement), transitions execute in the WORKER via the skillwrite workflow — the grounder never
// appends to the hash chain (spec/014 REQ-1311; the ledger is single-writer).
type skillsWriteBackend struct {
	store *db.SkillStore
	tc    client.Client
}

func (b skillsWriteBackend) CreateDraft(ctx context.Context, skillName string, req httpapi.SkillDraftRequest, operator string) (httpapi.SkillVersionView, error) {
	v, err := b.store.CreateVersion(ctx, skillstore.Version{
		SkillName:   skillName,
		Version:     strings.TrimSpace(req.Version),
		Body:        req.Body,
		AppliesWhen: req.AppliesWhen,
		ContentHash: skillstore.ContentHash(req.Body, req.AppliesWhen),
		Author:      "operator:" + operator,
		Source:      "hand",
		Rationale:   "[draft] " + strings.TrimSpace(req.Rationale),
	})
	if err != nil {
		return httpapi.SkillVersionView{}, err
	}
	return httpapi.SkillVersionView{
		ID: v.ID, Version: v.Version, Status: string(v.Status), Body: v.Body,
		ContentHash: v.ContentHash, Author: v.Author, Source: v.Source, Rationale: v.Rationale,
		CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339), StatusAt: v.StatusChangedAt.UTC().Format(time.RFC3339),
	}, nil
}

func (b skillsWriteBackend) Transition(ctx context.Context, versionID int64, to skillstore.Status, rationale, operator string) (httpapi.SkillTransitionOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("tg/skillwrite/%d/%s", versionID, to),
		TaskQueue: tg.TaskQueueRunner,
		// A completed same-id run may repeat (a later retire after a promote reuses ids); an IN-FLIGHT
		// duplicate is a double console click and is rejected by Temporal's default running-dedup.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, skillwrite.TransitionWorkflow, skillwrite.Request{VersionID: versionID, To: to, Rationale: rationale, Operator: operator})
	if err != nil {
		return httpapi.SkillTransitionOutcome{}, err
	}
	var res skillwrite.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.SkillTransitionOutcome{}, unwrapSkillErr(err)
	}
	return httpapi.SkillTransitionOutcome{
		VersionID: res.VersionID, SkillName: res.SkillName, Version: res.Version,
		Status: string(res.Status), LedgerSeq: res.LedgerSeq,
	}, nil
}

// unwrapSkillErr maps a workflow-wrapped state-machine refusal back onto the typed skillstore errors so
// the surface returns the right status code (a Temporal ApplicationError carries only the message).
func unwrapSkillErr(err error) error {
	msg := err.Error()
	// Longest-message-first so no sentinel's text can shadow a more specific one (the review's
	// fragility note); replace with typed unwrapping when the SDK propagates activity error chains.
	for _, known := range []error{
		skillstore.ErrRationaleRequired, skillstore.ErrProductionExists, skillstore.ErrBadPredicate,
		skillstore.ErrBadTransition, skillstore.ErrPinnedSkill, skillstore.ErrBodyBounds, skillstore.ErrNotFound,
	} {
		if strings.Contains(msg, known.Error()) {
			return fmt.Errorf("%w (%s)", known, "worker refused")
		}
	}
	return err
}
