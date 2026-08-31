package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/safety"
)

// ActionExecutionStore is the pgx-backed, APPEND-ONLY writer for action_execution (migration 0043): one row
// per EXECUTION, carrying the fresh verdict computed against that execution's own post-state.
//
// WHY IT EXISTS ALONGSIDE VerdictStore. action_verdict is keyed by action_id and written first-wins, and that
// is deliberate — spec/012 relies on it as "the action shape's FIRST verified outcome" and four readers assume
// exactly one row per id. But action_id is content-addressed over the operation alone, so re-running the same
// operation produces the same id and every execution after the first records nothing. Measured live before
// this store existed: 113 executions collapsed into 28 durable outcomes.
//
// The two answer different questions and both are worth keeping:
//
//	action_verdict    — "for this action SHAPE, what happened the first time we verified it?"
//	action_execution  — "what happened on THIS run?"  The one that supports counting independent heals.
type ActionExecutionStore struct{ p *Pool }

// NewActionExecutionStore returns a Postgres-backed per-execution recorder.
func NewActionExecutionStore(p *Pool) *ActionExecutionStore { return &ActionExecutionStore{p: p} }

// Record appends one execution.
//
// A verified execution carries its fresh verdict. An UNVERIFIABLE one (the post-state could not be read —
// TG-182 fail-closed) carries a NULL verdict and unverifiable=true, so "we executed and could not check" is
// recorded honestly and can never be mistaken later for a clean result. The table's CHECK constraint enforces
// that pairing, so a caller cannot write a contradictory row even by mistake.
// Record appends one row per execution. invertsActionID names the FORWARD action this execution undoes;
// pass "" for a forward action (the overwhelming majority) and it is stored as NULL. A non-empty value marks
// the row an INVERSE — so "did the rollback run, and how did it go?" is a query, not a log-string parse
// (TG-404). It is the LAST parameter deliberately: forward callers append "" and no existing positional
// argument shifts (the positional-rebind hazard this codebase has been bitten by).
func (s *ActionExecutionStore) Record(ctx context.Context, actionID, externalRef, targetHost, site string, v safety.Verdict, verified bool, invertsActionID string) error {
	if verified && !safety.ValidVerdict(v) {
		return ErrInvalidVerdict
	}
	var verdict any // NULL when unverifiable
	if verified {
		verdict = string(v)
	}
	var inverts any // NULL for a forward action; the CHECK constraint rejects a blank-but-present value
	if strings.TrimSpace(invertsActionID) != "" {
		inverts = invertsActionID
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO action_execution (action_id, external_ref, verdict, unverifiable, target_host, site, inverts_action_id, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`,
		actionID, externalRef, verdict, !verified, targetHost, site, inverts)
	if err != nil {
		return fmt.Errorf("db: record execution %s: %w", actionID, err)
	}
	return nil
}

// ForwardExecution is the LATEST recorded execution of an action_id, projected for the manual-rollback endpoint
// (TG-462): it answers "did this action actually run, on what target/site, under which incident, and is it itself
// an inverse?". InvertsActionID is non-empty when the row is ITSELF a rollback (so the endpoint refuses to roll
// back a rollback — no double-undo). Verdict is "" when the run was UNVERIFIABLE (never conflated with a clean).
type ForwardExecution struct {
	ActionID        string
	ExternalRef     string
	TargetHost      string
	Site            string
	Verdict         string
	Unverifiable    bool
	InvertsActionID string // non-empty ⇒ this row is itself an INVERSE (a rollback), not a forward action
	ExecutedAt      time.Time
}

// LatestExecution returns the most recent recorded execution for an action_id (action_id is content-addressed
// over the operation shape, so a repeated remediation has several rows; the newest is the one an operator's
// rollback targets). found=false means the action has NEVER executed — the endpoint maps that to 404, distinct
// from an error. It is a READ ONLY projection; it authorizes nothing.
func (s *ActionExecutionStore) LatestExecution(ctx context.Context, actionID string) (ForwardExecution, bool, error) {
	if strings.TrimSpace(actionID) == "" {
		return ForwardExecution{}, false, fmt.Errorf("db: LatestExecution requires an action_id")
	}
	var e ForwardExecution
	err := s.p.QueryRow(ctx, `
		SELECT action_id, external_ref, COALESCE(target_host,''), COALESCE(site,''),
		       COALESCE(verdict::text,''), unverifiable, COALESCE(inverts_action_id,''), executed_at
		FROM action_execution
		WHERE action_id = $1
		ORDER BY executed_at DESC, id DESC
		LIMIT 1`, actionID).Scan(&e.ActionID, &e.ExternalRef, &e.TargetHost, &e.Site,
		&e.Verdict, &e.Unverifiable, &e.InvertsActionID, &e.ExecutedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ForwardExecution{}, false, nil
		}
		return ForwardExecution{}, false, fmt.Errorf("db: latest execution of %s: %w", actionID, err)
	}
	return e, true, nil
}

// ExecutionFor returns the most recent recorded execution of an action WITHIN ONE incident — the
// commit-confirm consult's terminus read (spec/029 T-029-3, REQ-2902). Both keys are demanded for
// the same TG-142 sibling-collision reason InversesOf documents: action_id is content-addressed
// and recurs across incidents, and the consult must judge THIS incident's run, never a stranger's.
// found=false means this incident never executed the action — the consult's abort arm.
func (s *ActionExecutionStore) ExecutionFor(ctx context.Context, actionID, externalRef string) (ForwardExecution, bool, error) {
	if strings.TrimSpace(actionID) == "" || strings.TrimSpace(externalRef) == "" {
		return ForwardExecution{}, false, fmt.Errorf("db: ExecutionFor requires both action_id and external_ref")
	}
	var e ForwardExecution
	err := s.p.QueryRow(ctx, `
		SELECT action_id, external_ref, COALESCE(target_host,''), COALESCE(site,''),
		       COALESCE(verdict::text,''), unverifiable, COALESCE(inverts_action_id,''), executed_at
		FROM action_execution
		WHERE action_id = $1 AND external_ref = $2
		ORDER BY executed_at DESC, id DESC
		LIMIT 1`, actionID, externalRef).Scan(&e.ActionID, &e.ExternalRef, &e.TargetHost, &e.Site,
		&e.Verdict, &e.Unverifiable, &e.InvertsActionID, &e.ExecutedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ForwardExecution{}, false, nil
		}
		return ForwardExecution{}, false, fmt.Errorf("db: execution of %s under %s: %w", actionID, externalRef, err)
	}
	return e, true, nil
}

// InverseExecution is one recorded run of a rollback: which inverse ran (its own action_id), under which
// incident (external_ref), with what fresh outcome. Verdict is "" when the run was UNVERIFIABLE — the two
// states are never conflated (the table CHECK enforces the pairing).
type InverseExecution struct {
	ActionID     string
	ExternalRef  string
	Verdict      string
	Unverifiable bool
	ExecutedAt   time.Time
}

// InversesOf is THE sanctioned read for "did the rollback of forward action X run, and how did it go?" —
// and it deliberately demands BOTH keys. inverts_action_id is a content-addressed hash shared by every
// session that proposes the identical action (the TG-142 sibling-collision class: `WHERE action_id` alone
// once returned every incident's rows and a per-session surface showed a stranger's outcome), so a
// hash-only lookup is refused here rather than answered wrongly. externalRef scopes the answer to ONE
// incident; rows come back oldest first. Empty result = no inverse has run for that pair (a real,
// recordable fact) — distinct from the error a blank key returns.
func (s *ActionExecutionStore) InversesOf(ctx context.Context, forwardActionID, externalRef string) ([]InverseExecution, error) {
	if strings.TrimSpace(forwardActionID) == "" || strings.TrimSpace(externalRef) == "" {
		return nil, fmt.Errorf("db: InversesOf requires both the forward action_id and the external_ref — a hash-only lookup crosses incidents (TG-142)")
	}
	rows, err := s.p.Query(ctx, `
		SELECT action_id, external_ref, COALESCE(verdict::text, ''), unverifiable, executed_at
		FROM action_execution
		WHERE inverts_action_id = $1 AND external_ref = $2
		ORDER BY executed_at, id`, forwardActionID, externalRef)
	if err != nil {
		return nil, fmt.Errorf("db: inverses of %s: %w", forwardActionID, err)
	}
	defer rows.Close()
	var out []InverseExecution
	for rows.Next() {
		var e InverseExecution
		if err := rows.Scan(&e.ActionID, &e.ExternalRef, &e.Verdict, &e.Unverifiable, &e.ExecutedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
