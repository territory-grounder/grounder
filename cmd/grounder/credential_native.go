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
	"github.com/territory-grounder/grounder/temporal/nativerule"
)

// grounderNativeRules serves GET /v1/credentials/native — the operator-authored DB-backed native rule
// rows (TG-109, spec/016 REQ-1610). A package var set once at boot from the pool, per the documented
// positional-rebind hazard on buildPublicAPI's signature (the sync/rollback/axes backends record the same
// decision). nil ⇒ the route 503s.
var grounderNativeRules httpapi.NativeRulesReader

// grounderNativeRuleWrite starts the worker-side native-rule write lane. Same package-var wiring; nil
// (no Temporal client) ⇒ the write routes 503.
var grounderNativeRuleWrite httpapi.NativeRuleWriter

// nativeRulesReadStore adapts the pgx native-rule store to the console read surface. It maps the raw db
// rows to the non-secret console DTOs — formatting timestamps only. The entries carry SecretRef REFERENCE
// strings exactly as stored (INV-13: a reference, never a value), which is why the route sits at the
// elevated trace-read tier (see router.go).
type nativeRulesReadStore struct{ s *db.CredentialNativeRuleStore }

func (r nativeRulesReadStore) NativeRules(ctx context.Context) ([]httpapi.NativeRule, error) {
	rows, err := r.s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]httpapi.NativeRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, httpapi.NativeRule{
			ID: row.ID, Entry: row.Entry, Rationale: row.Rationale, CreatedBy: row.CreatedBy,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// nativeRuleWriteBackend implements httpapi.NativeRuleWriter (TG-109): the grounder never writes the rule
// table itself — the write executes in the WORKER (the governance ledger's single writer) via the
// distinctly-named nativerule.NativeRuleWriteWorkflow, which VALIDATES the entry (ParseRules, exactly one
// rule, fail-closed), ledgers it BEFORE the row commits, and persists it.
type nativeRuleWriteBackend struct {
	tc client.Client
}

func (b nativeRuleWriteBackend) AddNativeRule(ctx context.Context, entry, rationale, operator string, admin bool) (httpapi.NativeRuleWriteOutcome, error) {
	return b.write(ctx, nativerule.Request{
		Verb: "add", Entry: entry, Rationale: rationale, Operator: operator, AdminAuthorized: admin,
	})
}

func (b nativeRuleWriteBackend) DeleteNativeRule(ctx context.Context, id int64, rationale, operator string, admin bool) (httpapi.NativeRuleWriteOutcome, error) {
	return b.write(ctx, nativerule.Request{
		Verb: "delete", RowID: id, Rationale: rationale, Operator: operator, AdminAuthorized: admin,
	})
}

func (b nativeRuleWriteBackend) write(ctx context.Context, req nativerule.Request) (httpapi.NativeRuleWriteOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	run, err := b.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		// ONE rule table ⇒ one stable workflow id serializes writes (the rulesetwrite discipline): a
		// completed write may be followed by the next (ALLOW_DUPLICATE), while an IN-FLIGHT duplicate is
		// rejected by Temporal's running-dedup — at most one native-rule write runs at a time.
		ID:                    "tg/nativerulewrite",
		TaskQueue:             tg.TaskQueueRunner,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, nativerule.NativeRuleWriteWorkflow, req)
	if err != nil {
		return httpapi.NativeRuleWriteOutcome{}, err
	}
	var res nativerule.Result
	if err := run.Get(ctx, &res); err != nil {
		return httpapi.NativeRuleWriteOutcome{}, unwrapNativeRuleErr(err)
	}
	return httpapi.NativeRuleWriteOutcome{ID: res.ID, LedgerSeq: res.LedgerSeq}, nil
}

// unwrapNativeRuleErr maps a workflow-wrapped refusal back onto the sentinels the handler maps to honest
// statuses (a Temporal ApplicationError carries only the message) — the same discipline as the
// mode/config/ruleset backends. A missing row becomes httpapi's 404 sentinel; the worker's non-admin
// re-check becomes the 403 sentinel; a worker-side validation refusal (defense in depth — the surface
// already validates with the same parser) surfaces as the 400 sentinel carrying the validator's text.
func unwrapNativeRuleErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, nativerule.ErrNoSuchRule.Error()) {
		return fmt.Errorf("%w (worker refused)", httpapi.ErrNoSuchNativeRule)
	}
	if strings.Contains(msg, nativerule.ErrNotAdmin.Error()) {
		return fmt.Errorf("%w (worker refused)", httpapi.ErrNativeRuleNotAdmin)
	}
	// "credential: malformed" covers ParseRules' malformed-rule AND malformed-selector spellings;
	// "credential: rule"/"credential: selector" cover its port/bundle and selector-kind refusals.
	for _, refusal := range []string{
		"credential: malformed", "credential: rule", "credential: selector",
		"one row, one rule", "rationale required", "positive row id", nativerule.ErrUnknownVerb.Error(),
	} {
		if strings.Contains(msg, refusal) {
			return fmt.Errorf("%w: %s", httpapi.ErrNativeRuleInvalid, msg)
		}
	}
	return err
}
