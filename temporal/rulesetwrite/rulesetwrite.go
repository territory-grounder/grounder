// Package rulesetwrite executes an operator-invoked replacement of the ACTIVE rules-as-data policy
// document in the WORKER — the governance ledger's single writer (spec/015 REQ-1503, TG-104). The
// grounder's admin-session surface (POST /v1/policy/ruleset) never writes the ruleset itself: it starts
// this one-activity workflow and waits, exactly as the mode-transition / config-write / sealed-secret
// writes do, so:
//   - the submitted document is VALIDATED through core/policy.ParseRuleSet BEFORE anything is persisted
//     (fail-closed, INV-09) — a malformed ruleset is REFUSED, never stored as the active policy, so the
//     engine keeps its prior known-good rules rather than falling open (a bad ruleset governs actuation);
//   - the immutable governance record is appended by the worker (the ledger's single writer) BEFORE the
//     row commits, so a concurrent grounder can never fork the hash chain and a crash leaves an
//     over-recorded ledger, never an unrecorded ruleset change; and
//   - the persist upserts the singleton active document AND archives the immutable versioned copy in one
//     transaction (PolicyRulesetStore.Save), so a past decision's bundle_version still resolves the exact
//     document that produced it.
//
// This workflow ENABLES an operator-invoked replacement; it never auto-writes anything. Nothing here runs
// on a timer — it fires only when an authenticated, admin-session operator posts to /v1/policy/ruleset.
//
// Provenance: [O] INV-09 (fail closed on a malformed ruleset), INV-19 (single-writer audit), INV-01 (the
// surface is authenticated) · [R] spec/015 REQ-1503 (rules-as-data, validated policy document) · mirrors
// temporal/modetransition + temporal/configwrite.
package rulesetwrite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/policy"
)

// ErrNoStore fails closed when the activity has no bound ruleset store / ledger (a misconfigured worker) —
// refusing an unaudited or unpersistable ruleset write rather than reporting a phantom success.
var ErrNoStore = errors.New("rulesetwrite: no ruleset store/ledger bound — refusing an unaudited ruleset write")

// ErrNotAdmin fails closed when the write did not come from an admin-tier operator. The AuthAdminSession
// surface proves this before the workflow starts; re-checked HERE (the authority) so the surface can never
// be the only line — a direct worker call with AdminAuthorized=false is refused.
var ErrNotAdmin = errors.New("rulesetwrite: ruleset write requires an admin-tier operator")

// ErrStaleRuleset is the optimistic-concurrency refusal: the operator pinned ExpectedVersion (the bundle
// version they read) and it no longer matches the ACTIVE ruleset — someone else changed it. The write is
// refused (audited nothing, persisted nothing) so a stale console view can never silently overwrite a
// newer ruleset. It maps to a 409 at the surface (mirrors policy.ErrStaleMode on the mode surface).
var ErrStaleRuleset = errors.New("rulesetwrite: expected_version no longer matches the active ruleset — re-read and retry")

// Request is the typed ruleset-replacement order. Operator + AdminAuthorized are server-DERIVED at the
// surface from the AuthAdminSession principal (never from the request body). ExpectedVersion is an OPTIONAL
// compare-and-swap guard: when set, the write is refused unless it is still the active ruleset's bundle
// version (the content fingerprint) — the ruleset analogue of the mode transition's ExpectedFrom.
type Request struct {
	Document        []byte // the new rules-as-data JSON document; validated via ParseRuleSet (fail closed)
	ExpectedVersion string // OPTIONAL bundle_version the operator expects active; "" ⇒ last-writer-wins
	Rationale       string // OPTIONAL operator rationale, folded into the ledger reason
	Operator        string // the authenticated operator id (updated_by + the ledger actor)
	AdminAuthorized bool   // the operator authenticated through the admin tier (AuthAdminSession)
}

// Result is the committed replacement's essentials for the console response. Version is the content
// fingerprint (bundle_version) now active — the same value a subsequent write passes as ExpectedVersion.
type Result struct {
	Version   string // the bundle_version (content fingerprint) now active
	RuleCount int    // the number of rules in the newly active document
	UpdatedBy string // the operator recorded as updated_by
	LedgerSeq int64  // the governance-ledger sequence of this write's audit record
}

// Ledger is the slice of audit.Ledger this write needs — append-only governance decisions (INV-19).
// AppendContext (not Append) so the activity's deadline reaches the durable chain write (mirrors
// temporal/configwrite, TG-277).
type Ledger interface {
	AppendContext(ctx context.Context, d audit.GovDecision) (audit.LedgerEntry, error)
}

// Deps are the worker-side collaborators: the single active-ruleset store (Load for the CAS read, Save to
// validate+persist+archive) and the governance ledger's single-writer append.
type Deps struct {
	// Store is the durable active-ruleset store (db.PolicyRulesetStore) — policy.RulesetStore is the exact
	// narrow seam (Load + Save), so the in-memory fake drives the CI oracles unchanged.
	Store policy.RulesetStore
	// Ledger is the SAME chain-continued governance ledger the config/opclass writes append to.
	Ledger Ledger
	// OnParsed, when set, is called with the freshly-PARSED new ruleset after validation succeeds. It is
	// OBSERVE-ONLY — it must not error, block, or mutate — and runs before the persist. The composition root
	// wires it to the TG-437 Matrix-approver namespace cross-check so a write that re-strands the Matrix
	// approver is surfaced at write time, not silently until the next boot re-runs the boot check. Nil = no-op.
	OnParsed func(policy.RuleSet)
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ApplyRulesetWriteActivity is the single-writer ruleset replacement. In order: (1) VALIDATE the document
// via ParseRuleSet — the load-bearing safety check; a malformed document is REFUSED here with the parse
// error and NOTHING is appended or persisted (the prior active ruleset stands, fail closed). (2) If the
// operator pinned ExpectedVersion, compare-and-swap on the active ruleset's bundle version and refuse a
// stale expectation (audited nothing, persisted nothing). (3) LEDGER the change BEFORE the row commits
// (ledger-first, mirroring configwrite/opclassratify). (4) PERSIST via Save, which re-validates, upserts
// the singleton active document, and archives the immutable versioned copy in one transaction. A refusal
// surfaces its typed error verbatim (no retry) so the grounder maps it to an honest status.
func (a *Activities) ApplyRulesetWriteActivity(ctx context.Context, req Request) (Result, error) {
	if a.D.Store == nil || a.D.Ledger == nil {
		return Result{}, ErrNoStore
	}
	// The admin tier is the authority for a ruleset write. The surface proved it (AuthAdminSession);
	// re-checked here so a worker-direct call can never bypass it (fail closed).
	if !req.AdminAuthorized {
		return Result{}, ErrNotAdmin
	}
	// 1. VALIDATE (fail closed): a malformed ruleset is refused BEFORE any ledger append or persist — a bad
	//    ruleset governs actuation, so it must never become the active policy, not even transiently.
	rs, err := policy.ParseRuleSet(req.Document)
	if err != nil {
		return Result{}, err // ErrMalformedRule — no ledger entry, no persist; the prior ruleset stands.
	}
	newVersion := policy.BundleVersion(rs) // the content fingerprint (REQ-1522) of the document being written.

	// 2. OPTIMISTIC CONCURRENCY (compare-and-swap on the content fingerprint). The active singleton
	//    (policy_ruleset, migration 0019) is last-writer-wins with no monotonic version column — only
	//    updated_at and the derived bundle_version. When the operator pins ExpectedVersion, refuse (no
	//    ledger, no persist) unless it still matches the ACTIVE ruleset's fingerprint, so a stale console
	//    view can never silently clobber a newer ruleset. Concurrent writes are serialized by the stable
	//    workflow id (see the grounder backend), which closes the read-then-write window.
	if strings.TrimSpace(req.ExpectedVersion) != "" {
		current, cerr := a.activeVersion(ctx)
		if cerr != nil {
			return Result{}, cerr
		}
		if strings.TrimSpace(req.ExpectedVersion) != current {
			return Result{}, fmt.Errorf("%w (expected %q, active %q)",
				ErrStaleRuleset, strings.TrimSpace(req.ExpectedVersion), orAbsent(current))
		}
	}

	// TG-437: observe the parsed ruleset for a re-stranded Matrix approver (namespace cross-check) — now that
	// validation and the optimistic-concurrency check have passed, so this write WILL proceed. Observe-only:
	// it logs (sharing the boot check's logic), never blocks the write, so a ruleset write that strands the
	// Matrix approver surfaces immediately instead of silently until the next boot.
	if a.D.OnParsed != nil {
		a.D.OnParsed(rs)
	}

	// 3. LEDGER (ledger-first, like configwrite/opclassratify): the governance record is appended BEFORE the
	//    row commits, so a crash leaves an over-recorded ledger, never an unrecorded ruleset change. The
	//    ActionID binds the decision to the exact content (INV-07) so the audit joins the archived version.
	entry, err := a.D.Ledger.AppendContext(ctx, audit.GovDecision{
		Decision: "policy:ruleset-write",
		Reason:   rulesetReason(req),
		ActionID: "ruleset:" + newVersion,
		Withheld: false,
	})
	if err != nil {
		return Result{}, fmt.Errorf("rulesetwrite: ledger append: %w", err)
	}

	// 4. PERSIST: Save re-validates via ParseRuleSet (defense in depth), upserts the active singleton, AND
	//    archives the immutable versioned copy (policy_ruleset + policy_ruleset_version) in ONE transaction.
	saved, err := a.D.Store.Save(ctx, req.Document, req.Operator)
	if err != nil {
		return Result{}, fmt.Errorf("rulesetwrite: persist: %w", err)
	}
	return Result{
		Version:   policy.BundleVersion(saved),
		RuleCount: len(saved.Rules),
		UpdatedBy: req.Operator,
		LedgerSeq: entry.Seq,
	}, nil
}

// activeVersion resolves the ACTIVE ruleset's bundle version for the compare-and-swap. An absent ruleset
// (a fresh deployment before the first write) resolves to "" — an operator who pinned any ExpectedVersion
// against an absent ruleset is stale by definition. A genuine read error is surfaced, never swallowed.
func (a *Activities) activeVersion(ctx context.Context) (string, error) {
	cur, _, err := a.D.Store.Load(ctx)
	switch {
	case err == nil:
		return policy.BundleVersion(cur), nil
	case errors.Is(err, policy.ErrRulesetAbsent):
		return "", nil
	default:
		return "", fmt.Errorf("rulesetwrite: load active ruleset for compare-and-swap: %w", err)
	}
}

// rulesetReason builds the ledger reason: the operator's rationale (or a default) tagged with the actor,
// matching the "[by <operator>]" convention the config/secret writes record.
func rulesetReason(req Request) string {
	r := strings.TrimSpace(req.Rationale)
	if r == "" {
		r = "active policy ruleset replaced"
	}
	return r + " [by " + req.Operator + "]"
}

// orAbsent renders an empty active version as "(absent)" in the stale-CAS error, so the refusal names the
// real state rather than an empty string.
func orAbsent(v string) string {
	if v == "" {
		return "(absent)"
	}
	return v
}

// activityOpts: no retries — a refused write (malformed ruleset, stale expected_version, non-admin) is a
// DECISION, not a transient; it surfaces verbatim (mirrors temporal/modetransition + temporal/configwrite).
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// RulesetWriteWorkflow is the one-activity ruleset-replacement workflow. Named DISTINCTLY — Temporal
// registers by bare function name, and two packages both exporting `Workflow` collide at RegisterWorkflow
// (the 2026-07-17 worker boot-loop; guarded by temporal/skilltrial/finalizer_names_test.go, on whose list
// this workflow now sits).
func RulesetWriteWorkflow(ctx workflow.Context, req Request) (Result, error) {
	var res Result
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).ApplyRulesetWriteActivity, req).Get(ctx, &res)
	return res, err
}
