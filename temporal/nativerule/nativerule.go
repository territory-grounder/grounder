// Package nativerule executes an operator-invoked write to the DB-backed NATIVE per-target credential
// mapping in the WORKER — the governance ledger's single writer (spec/016 REQ-1610, TG-109). The
// grounder's admin-session surface (POST /v1/credentials/native/rules, DELETE …/rules/{id}) never writes
// the rule table itself: it starts this one-activity workflow and waits, exactly as the ruleset /
// config-write / sealed-secret writes do, so:
//   - an added entry is VALIDATED through core/credential.ParseRules BEFORE anything is persisted
//     (fail-closed): it must parse to EXACTLY ONE rule — "one row, one rule" keeps deletes precise — and
//     a malformed entry is REFUSED with the parser's text, never stored where the sync source would then
//     fail every sync on it;
//   - the immutable governance record is appended by the worker (the ledger's single writer) BEFORE the
//     row commits, so a crash leaves an over-recorded ledger, never an unrecorded rule change. For a
//     delete this ordering means a missing row is discovered AT the store write, after the append — the
//     refusal (ErrNoSuchRule → 404) then trails one ledger entry, which is the over-recorded side the
//     ledger-first discipline chooses on purpose;
//   - the ledger REASON carries the verb, the rule's selector token (or the row id) and the operator's
//     rationale — NEVER the full packed entry, so no SecretRef string is copied into the audit spine
//     (the reference is already a pointer at secret material; the spine needs the decision, not the map).
//
// This workflow ENABLES an operator-invoked write; it never auto-writes anything. Nothing here runs on a
// timer — it fires only when an authenticated, admin-session operator posts to the native-rule surface.
//
// Provenance: [O] INV-13 (references only, and none in the ledger reason), INV-19 (single-writer audit),
// INV-01 (the surface is authenticated) · [R] spec/016 REQ-1610 (native store), TG-109 · mirrors
// temporal/rulesetwrite + temporal/configwrite.
package nativerule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/credential"
)

// ErrNoStore fails closed when the activity has no bound rule store / ledger (a misconfigured worker) —
// refusing an unaudited or unpersistable rule write rather than reporting a phantom success.
var ErrNoStore = errors.New("nativerule: no rule store/ledger bound — refusing an unaudited native-rule write")

// ErrNotAdmin fails closed when the write did not come from an admin-tier operator. The AuthAdminSession
// surface proves this before the workflow starts; re-checked HERE (the authority) so the surface can never
// be the only line — a direct worker call with AdminAuthorized=false is refused.
var ErrNotAdmin = errors.New("nativerule: native credential-rule write requires an admin-tier operator")

// ErrUnknownVerb is the closed-enum refusal: the verb table is {add, delete} and anything else is refused
// before any ledger append or persist — an unrecognized order must not guess at a mutation.
var ErrUnknownVerb = errors.New("nativerule: unknown verb (use add or delete)")

// ErrNoSuchRule reports a delete aimed at a row that does not exist. The surface maps it to 404. It is
// discovered at the store write (after the ledger append — see the package doc's ordering note).
var ErrNoSuchRule = errors.New("nativerule: no such native rule row")

// Request is the typed native-rule write order. Operator + AdminAuthorized are server-DERIVED at the
// surface from the AuthAdminSession principal (never from the request body).
type Request struct {
	Verb            string // "add" | "delete" — a CLOSED enum, anything else refused
	Entry           string // add: the packed ParseRules rule (exactly one)
	RowID           int64  // delete: the credential_native_rule row id
	Rationale       string // why — REQUIRED for add (folded into the ledger reason); delete defaults
	Operator        string // the authenticated operator id (created_by + the ledger actor)
	AdminAuthorized bool   // the operator authenticated through the admin tier (AuthAdminSession)
}

// Result is the committed write's essentials for the console response: the row id the write created (add)
// or removed (delete), the stored entry (add only), and the governance-ledger sequence of the decision.
type Result struct {
	ID        int64
	Entry     string
	LedgerSeq int64
}

// Ledger is the slice of audit.Ledger this write needs — append-only governance decisions (INV-19).
// AppendContext (not Append) so the activity's deadline reaches the durable chain write (mirrors
// temporal/rulesetwrite, TG-277).
type Ledger interface {
	AppendContext(ctx context.Context, d audit.GovDecision) (audit.LedgerEntry, error)
}

// Store is the narrow persist seam over credential_native_rule (db.CredentialNativeRuleStore satisfies
// it) — Insert + Delete only, so the in-memory fake drives the CI oracles unchanged.
type Store interface {
	Insert(ctx context.Context, entry, rationale, createdBy string) (int64, error)
	Delete(ctx context.Context, id int64) (bool, error)
}

// Deps are the worker-side collaborators: the rule store and the governance ledger's single-writer append.
type Deps struct {
	// Store is the durable native-rule store (db.CredentialNativeRuleStore).
	Store Store
	// Ledger is the SAME chain-continued governance ledger the ruleset/config writes append to.
	Ledger Ledger
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ApplyNativeRuleWriteActivity is the single-writer native-rule write. In order: (1) fail-closed deps +
// admin re-check. (2) VERB VALIDATION over the closed enum. (3) For add: the entry must ParseRules to
// EXACTLY ONE rule (the parser's refusal surfaces verbatim; 0 or >1 rules refused — "one row, one rule"
// keeps deletes precise) and the rationale is required; for delete: RowID must be positive. NOTHING is
// appended or persisted by a refusal here (the prior rule set stands, fail closed). (4) LEDGER the change
// BEFORE the row commits (ledger-first, mirroring rulesetwrite/configwrite) — reason carries verb +
// selector token/row id + rationale, never the packed entry. (5) PERSIST via the store; a delete that
// finds no row surfaces ErrNoSuchRule. A refusal surfaces its typed error verbatim (no retry) so the
// grounder maps it to an honest status.
func (a *Activities) ApplyNativeRuleWriteActivity(ctx context.Context, req Request) (Result, error) {
	if a.D.Store == nil || a.D.Ledger == nil {
		return Result{}, ErrNoStore
	}
	// The admin tier is the authority for a native-rule write. The surface proved it (AuthAdminSession);
	// re-checked here so a worker-direct call can never bypass it (fail closed).
	if !req.AdminAuthorized {
		return Result{}, ErrNotAdmin
	}

	switch req.Verb {
	case "add":
		return a.applyAdd(ctx, req)
	case "delete":
		return a.applyDelete(ctx, req)
	default:
		return Result{}, fmt.Errorf("%w (got %q)", ErrUnknownVerb, req.Verb)
	}
}

// applyAdd validates (fail closed), ledgers, then inserts one rule row.
func (a *Activities) applyAdd(ctx context.Context, req Request) (Result, error) {
	// 1. VALIDATE (fail closed): the entry must parse to EXACTLY ONE rule BEFORE any ledger append or
	//    persist. A stored row the sync source cannot parse would fail EVERY subsequent sync (the source
	//    fails closed on any bad row), so a malformed entry must never land.
	entry := strings.TrimSpace(req.Entry)
	rules, err := credential.ParseRules(entry)
	if err != nil {
		return Result{}, err // the parser's text, verbatim — no ledger entry, no persist.
	}
	if len(rules) != 1 {
		return Result{}, fmt.Errorf("nativerule: entry packs %d rules — one row, one rule (split into separate adds)", len(rules))
	}
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		return Result{}, errors.New("nativerule: rationale required — every native rule states why it exists")
	}

	// 2. LEDGER (ledger-first): appended BEFORE the row commits, so a crash leaves an over-recorded
	//    ledger, never an unrecorded rule change. The reason carries the SELECTOR TOKEN only (rules[0].ID
	//    is ParseRules' kind:pattern token) — never the packed entry, so no SecretRef string enters the
	//    audit spine.
	entryRec, err := a.D.Ledger.AppendContext(ctx, audit.GovDecision{
		Decision: "credential:native-rule-write",
		Reason:   fmt.Sprintf("add %s: %s [by %s]", rules[0].ID, rationale, req.Operator),
		ActionID: "native-rule:add:" + rules[0].ID,
		Withheld: false,
	})
	if err != nil {
		return Result{}, fmt.Errorf("nativerule: ledger append: %w", err)
	}

	// 3. PERSIST.
	id, err := a.D.Store.Insert(ctx, entry, rationale, req.Operator)
	if err != nil {
		return Result{}, fmt.Errorf("nativerule: persist: %w", err)
	}
	return Result{ID: id, Entry: entry, LedgerSeq: entryRec.Seq}, nil
}

// applyDelete validates (fail closed), ledgers, then deletes one rule row; a missing row surfaces
// ErrNoSuchRule (after the append — the over-recorded side, per the package doc).
func (a *Activities) applyDelete(ctx context.Context, req Request) (Result, error) {
	if req.RowID <= 0 {
		return Result{}, errors.New("nativerule: delete requires a positive row id")
	}
	entryRec, err := a.D.Ledger.AppendContext(ctx, audit.GovDecision{
		Decision: "credential:native-rule-write",
		Reason:   fmt.Sprintf("delete row-%d: %s [by %s]", req.RowID, deleteReason(req.Rationale), req.Operator),
		ActionID: fmt.Sprintf("native-rule:delete:row-%d", req.RowID),
		Withheld: false,
	})
	if err != nil {
		return Result{}, fmt.Errorf("nativerule: ledger append: %w", err)
	}
	ok, err := a.D.Store.Delete(ctx, req.RowID)
	if err != nil {
		return Result{}, fmt.Errorf("nativerule: persist: %w", err)
	}
	if !ok {
		return Result{}, fmt.Errorf("%w (id %d)", ErrNoSuchRule, req.RowID)
	}
	return Result{ID: req.RowID, LedgerSeq: entryRec.Seq}, nil
}

// deleteReason defaults an absent delete rationale, matching the rulesetwrite convention (the surface
// requires one; a worker-direct caller still produces an honest reason).
func deleteReason(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return "native rule removed"
	}
	return r
}

// activityOpts: no retries — a refused write (malformed entry, unknown verb, missing row, non-admin) is a
// DECISION, not a transient; it surfaces verbatim (mirrors temporal/rulesetwrite).
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// NativeRuleWriteWorkflow is the one-activity native-rule write workflow. Named DISTINCTLY — Temporal
// registers by bare function name, and two packages both exporting `Workflow` collide at RegisterWorkflow
// (the 2026-07-17 worker boot-loop; guarded by temporal/skilltrial/finalizer_names_test.go, on whose list
// this workflow now sits).
func NativeRuleWriteWorkflow(ctx workflow.Context, req Request) (Result, error) {
	var res Result
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).ApplyNativeRuleWriteActivity, req).Get(ctx, &res)
	return res, err
}
