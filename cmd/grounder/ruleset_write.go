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
	"github.com/territory-grounder/grounder/temporal/rulesetwrite"
)

// rulesetWriteBackend implements httpapi.RulesetWriter (spec/015 REQ-1503, TG-104): the grounder never
// writes the active rules-as-data policy itself — the write executes in the WORKER (the governance ledger's
// single writer) via the distinctly-named rulesetwrite.RulesetWriteWorkflow, which VALIDATES the document
// (ParseRuleSet, fail-closed), ledgers it BEFORE the row commits, and persists the active singleton + the
// immutable version archive in one transaction.
type rulesetWriteBackend struct {
	tc client.Client
}

func (b rulesetWriteBackend) WriteRuleset(ctx context.Context, document []byte, expectedVersion, rationale, operator string, adminAuthorized bool) (httpapi.RulesetWriteOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// A SINGLE active ruleset ⇒ one stable workflow id serializes replacements: a completed write may be
		// followed by the next (ALLOW_DUPLICATE), while an IN-FLIGHT duplicate is rejected by Temporal's
		// running-dedup. That serialization is what closes the compare-and-swap read-then-write window in the
		// activity — at most one ruleset write runs at a time.
		ID:                    "tg/rulesetwrite",
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, rulesetwrite.RulesetWriteWorkflow, rulesetwrite.Request{
		Document: document, ExpectedVersion: expectedVersion, Rationale: rationale, Operator: operator, AdminAuthorized: adminAuthorized,
	})
	if err != nil {
		return httpapi.RulesetWriteOutcome{}, err
	}
	var res rulesetwrite.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.RulesetWriteOutcome{}, unwrapRulesetErr(err)
	}
	return httpapi.RulesetWriteOutcome{
		Version: res.Version, RuleCount: res.RuleCount, UpdatedBy: res.UpdatedBy, LedgerSeq: res.LedgerSeq,
	}, nil
}

// unwrapRulesetErr maps a workflow-wrapped refusal back onto the sentinels the handler maps to honest
// statuses (a Temporal ApplicationError carries only the message) — the same longest-message-first
// discipline as the mode/config-write backends. A stale compare-and-swap becomes httpapi's 409 sentinel; a
// malformed ruleset (defense in depth — the surface already validates) surfaces the policy parse error →
// 400.
func unwrapRulesetErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, rulesetwrite.ErrStaleRuleset.Error()) {
		return fmt.Errorf("%w (worker refused)", httpapi.ErrRulesetVersionConflict)
	}
	if strings.Contains(msg, policy.ErrMalformedRule.Error()) {
		return fmt.Errorf("%w (worker refused)", policy.ErrMalformedRule)
	}
	return err
}
