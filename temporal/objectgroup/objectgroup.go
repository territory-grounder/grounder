// Package objectgroup executes an operator-invoked write to the DB-backed OBJECT GROUP model in the WORKER —
// the governance ledger's single writer (spec/016, TG-481). The grounder's admin-session surface
// (POST /v1/estate/groups, DELETE …/groups/{id}) never writes estate_object_group itself: it starts this
// one-activity workflow and waits, exactly as the native-rule / ruleset / config writes do, so:
//   - an added group is VALIDATED (non-empty name, at least one non-empty host-glob pattern, a recognized
//     precedence) BEFORE anything is persisted (fail-closed);
//   - the immutable governance record is appended by the worker (the ledger's single writer) BEFORE the row
//     commits, so a crash leaves an over-recorded ledger, never an unrecorded group change. A delete's
//     missing row is discovered AT the store write, after the append — the over-recorded side the
//     ledger-first discipline chooses on purpose;
//   - the ledger REASON carries the verb, the group NAME (or the row id) and the operator's rationale. Object
//     groups hold NO secret material (names + host-glob patterns only), so the whole decision is non-secret.
//
// This workflow ENABLES an operator-invoked write; it never auto-writes anything. Nothing here runs on a
// timer — it fires only when an authenticated, admin-session operator posts to the object-group surface.
//
// Provenance: [O] INV-19 (single-writer audit), INV-01 (the surface is authenticated) · [R] spec/016,
// TG-481 · mirrors temporal/nativerule.
package objectgroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/territory-grounder/grounder/core/audit"
)

// ErrNoStore fails closed when the activity has no bound store / ledger (a misconfigured worker) — refusing
// an unaudited or unpersistable object-group write rather than reporting a phantom success.
var ErrNoStore = errors.New("objectgroup: no store/ledger bound — refusing an unaudited object-group write")

// ErrNotAdmin fails closed when the write did not come from an admin-tier operator. The AuthAdminSession
// surface proves this before the workflow starts; re-checked HERE (the authority) so the surface can never be
// the only line — a direct worker call with AdminAuthorized=false is refused.
var ErrNotAdmin = errors.New("objectgroup: object-group write requires an admin-tier operator")

// ErrUnknownVerb is the closed-enum refusal: the verb table is {add, delete} and anything else is refused
// before any ledger append or persist.
var ErrUnknownVerb = errors.New("objectgroup: unknown verb (use add or delete)")

// ErrNoSuchGroup reports a delete aimed at a row that does not exist. The surface maps it to 404. It is
// discovered at the store write (after the ledger append — see the package doc's ordering note).
var ErrNoSuchGroup = errors.New("objectgroup: no such object-group row")

// ErrInvalid is the fail-closed validation refusal (empty name, no patterns, bad precedence, missing
// rationale). The surface already validates; re-checked here (defense in depth), refused with the reason.
var ErrInvalid = errors.New("objectgroup: invalid object-group")

// validPrecedence is the closed set of merge semantics. 'union' today (a hand-authored group ADDS to
// inventory-derived membership, never masks it — the TG-481 ratified semantics). Widen with a migration.
func validPrecedence(p string) bool { return p == "union" }

// Request is the typed object-group write order. Operator + AdminAuthorized are server-DERIVED at the surface
// from the AuthAdminSession principal (never from the request body).
type Request struct {
	Verb            string   // "add" | "delete" — a CLOSED enum, anything else refused
	Name            string   // add: the group name (referenced by KindGroup selectors)
	Patterns        []string // add: the host-glob patterns defining membership (at least one, each non-empty)
	Precedence      string   // add: the merge precedence ('union'); empty defaults to 'union'
	RowID           int64    // delete: the estate_object_group row id
	Rationale       string   // why — REQUIRED for add (folded into the ledger reason); delete defaults
	Operator        string   // the authenticated operator id (created_by + the ledger actor)
	AdminAuthorized bool     // the operator authenticated through the admin tier (AuthAdminSession)
}

// Result is the committed write's essentials for the console response: the row id created (add) or removed
// (delete) and the governance-ledger sequence of the decision.
type Result struct {
	ID        int64
	LedgerSeq int64
}

// Ledger is the slice of audit.Ledger this write needs — append-only governance decisions (INV-19).
// AppendContext (not Append) so the activity's deadline reaches the durable chain write (mirrors nativerule).
type Ledger interface {
	AppendContext(ctx context.Context, d audit.GovDecision) (audit.LedgerEntry, error)
}

// Store is the narrow persist seam over estate_object_group (db.EstateObjectGroupStore satisfies it) —
// Insert + Delete only, so the in-memory fake drives the CI oracles unchanged.
type Store interface {
	Insert(ctx context.Context, name string, patterns []string, precedence, createdBy string) (int64, error)
	Delete(ctx context.Context, id int64) (bool, error)
}

// Deps are the worker-side collaborators: the object-group store and the governance ledger's single-writer append.
type Deps struct {
	Store  Store
	Ledger Ledger
}

// Activities carries Deps for Temporal registration.
type Activities struct{ D Deps }

// ApplyObjectGroupWriteActivity is the single-writer object-group write. In order: (1) fail-closed deps +
// admin re-check. (2) VERB VALIDATION over the closed enum. (3) validate the payload (fail closed — nothing
// appended or persisted by a refusal). (4) LEDGER the change BEFORE the row commits (ledger-first) — the
// reason carries verb + name/row id + rationale (all non-secret). (5) PERSIST; a delete that finds no row
// surfaces ErrNoSuchGroup. A refusal surfaces its typed error verbatim (no retry).
func (a *Activities) ApplyObjectGroupWriteActivity(ctx context.Context, req Request) (Result, error) {
	if a.D.Store == nil || a.D.Ledger == nil {
		return Result{}, ErrNoStore
	}
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

// applyAdd validates (fail closed), ledgers, then inserts one object-group row.
func (a *Activities) applyAdd(ctx context.Context, req Request) (Result, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Result{}, fmt.Errorf("%w: name required", ErrInvalid)
	}
	patterns := make([]string, 0, len(req.Patterns))
	for _, p := range req.Patterns {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) == 0 {
		return Result{}, fmt.Errorf("%w: at least one non-empty host-glob pattern required", ErrInvalid)
	}
	precedence := strings.TrimSpace(req.Precedence)
	if precedence == "" {
		precedence = "union"
	}
	if !validPrecedence(precedence) {
		return Result{}, fmt.Errorf("%w: precedence %q not recognized (use 'union')", ErrInvalid, precedence)
	}
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		return Result{}, fmt.Errorf("%w: rationale required — every object group states why it exists", ErrInvalid)
	}

	// LEDGER-first: appended BEFORE the row commits. Non-secret throughout (group name + pattern count).
	entryRec, err := a.D.Ledger.AppendContext(ctx, audit.GovDecision{
		Decision: "credential:object-group-write",
		Reason:   fmt.Sprintf("add group %q (%d pattern(s)): %s [by %s]", name, len(patterns), rationale, req.Operator),
		ActionID: "object-group:add:" + name,
		Withheld: false,
	})
	if err != nil {
		return Result{}, fmt.Errorf("objectgroup: ledger append: %w", err)
	}

	id, err := a.D.Store.Insert(ctx, name, patterns, precedence, req.Operator)
	if err != nil {
		return Result{}, fmt.Errorf("objectgroup: persist: %w", err)
	}
	return Result{ID: id, LedgerSeq: entryRec.Seq}, nil
}

// applyDelete validates (fail closed), ledgers, then deletes one row; a missing row surfaces ErrNoSuchGroup
// (after the append — the over-recorded side, per the package doc).
func (a *Activities) applyDelete(ctx context.Context, req Request) (Result, error) {
	if req.RowID <= 0 {
		return Result{}, fmt.Errorf("%w: delete requires a positive row id", ErrInvalid)
	}
	entryRec, err := a.D.Ledger.AppendContext(ctx, audit.GovDecision{
		Decision: "credential:object-group-write",
		Reason:   fmt.Sprintf("delete row-%d: %s [by %s]", req.RowID, deleteReason(req.Rationale), req.Operator),
		ActionID: fmt.Sprintf("object-group:delete:row-%d", req.RowID),
		Withheld: false,
	})
	if err != nil {
		return Result{}, fmt.Errorf("objectgroup: ledger append: %w", err)
	}
	ok, err := a.D.Store.Delete(ctx, req.RowID)
	if err != nil {
		return Result{}, fmt.Errorf("objectgroup: persist: %w", err)
	}
	if !ok {
		return Result{}, fmt.Errorf("%w (id %d)", ErrNoSuchGroup, req.RowID)
	}
	return Result{ID: req.RowID, LedgerSeq: entryRec.Seq}, nil
}

func deleteReason(r string) string {
	if r = strings.TrimSpace(r); r != "" {
		return r
	}
	return "object group removed"
}

// activityOpts: no retries — a refused write (invalid payload, unknown verb, missing row, non-admin) is a
// DECISION, not a transient; it surfaces verbatim (mirrors temporal/nativerule).
func activityOpts() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// ObjectGroupWriteWorkflow is the one-activity object-group write workflow. Named DISTINCTLY — Temporal
// registers by bare function name, and two packages both exporting `Workflow` collide at RegisterWorkflow
// (guarded by temporal/skilltrial/finalizer_names_test.go, on whose list this workflow now sits).
func ObjectGroupWriteWorkflow(ctx workflow.Context, req Request) (Result, error) {
	var res Result
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, activityOpts()),
		new(Activities).ApplyObjectGroupWriteActivity, req).Get(ctx, &res)
	return res, err
}
