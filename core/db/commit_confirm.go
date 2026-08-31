package db

// The armed revert's durable state store (spec/029 T-029-2, REQ-2901/REQ-2906). Arm is the ONLY
// insert and happens BEFORE the forward effect executes; every later transition leaves 'armed'
// exactly once (guarded in SQL), so a duplicate or late signal can never resurrect a resolved
// window. The row is the queryable record REQ-2906 demands; the Temporal child workflow holding
// the actual timer is the actor.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The commit_confirm state vocabulary — fixed here in full (T-029-3 writes the last four) so the
// console and queries never chase a moving enum. Mirrors the migration's CHECK constraint.
const (
	CommitConfirmArmed             = "armed"
	CommitConfirmAborted           = "aborted"             // the forward effect provably did not execute
	CommitConfirmElapsedUnconfirmed = "elapsed_unconfirmed" // window elapsed with no confirm capability (pre-T-029-3 interim)
	CommitConfirmConfirmed         = "confirmed"           // T-029-3: mechanical confirm (verdict==match AND verified)
	CommitConfirmHeldUnverifiable  = "held_unverifiable"   // T-029-3: REQ-2902 HOLD+page
	CommitConfirmReverted          = "reverted"            // T-029-3: the inverse fired and verified
	CommitConfirmRevertFailed      = "revert_failed"       // T-029-3: the inverse failed — page + breaker
)

// ErrCommitConfirmResolved reports a transition attempted on a row no longer 'armed' — the
// idempotent duplicate-signal case. Callers treat it as already-done, never as a fresh failure.
var ErrCommitConfirmResolved = errors.New("db: commit_confirm row is already resolved")

// CommitConfirmRow is the durable armed-revert record for one (action, incident) pair.
type CommitConfirmRow struct {
	ActionID         string
	ExternalRef      string
	OpClass          string
	TargetHost       string
	Site             string
	PlanHash         string
	State            string
	WindowSeconds    int64
	ArmedAt          time.Time
	DeadlineAt       time.Time
	ResolvedAt       *time.Time
	ResolutionDetail string
	InverseActionID  string
	// The fired inverse's authorization basis, captured at ARM time (0096, T-029-3): the forward
	// action's live classification band and whether a human vote approved it. The interceptor
	// still judges the inverse fresh — this is the basis the request CARRIES, not a bypass.
	ForwardBand     string
	ForwardApproved bool
	// AlertRule is the incident signature the REQ-2902 hold-watch re-observes the target with.
	AlertRule string
}

// CommitConfirmStore reads and writes commit_confirm rows.
type CommitConfirmStore struct{ p *Pool }

func NewCommitConfirmStore(p *Pool) *CommitConfirmStore { return &CommitConfirmStore{p: p} }

// ArmCommitConfirm durably records the armed window BEFORE the effect executes (REQ-2901). It is
// idempotent for Temporal activity retries: a conflicting row that is still 'armed' with the SAME
// window is this activity's own earlier attempt and succeeds quietly; a conflicting row in any
// OTHER state is a real refusal (this incident's window already ran to a resolution — re-arming
// over it would erase the record) and errors, which the workflow converts into refuse-forward.
func (s *CommitConfirmStore) ArmCommitConfirm(ctx context.Context, r CommitConfirmRow) error {
	if strings.TrimSpace(r.ActionID) == "" || strings.TrimSpace(r.ExternalRef) == "" {
		return fmt.Errorf("db: arm commit_confirm: empty action_id/external_ref")
	}
	if r.WindowSeconds <= 0 {
		return fmt.Errorf("db: arm commit_confirm %s: non-positive window %d", r.ActionID, r.WindowSeconds)
	}
	// The window rides twice ($8 column bigint, $9 interval seconds): reusing one parameter in both
	// positions makes Postgres deduce two different types for it and refuse (42P08).
	tag, err := s.p.Exec(ctx, `
		INSERT INTO commit_confirm
			(action_id, external_ref, op_class, target_host, site, plan_hash, state, window_seconds, armed_at, deadline_at,
			 forward_band, forward_approved, alert_rule)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now() + make_interval(secs => $9), $10, $11, $12)
		ON CONFLICT (action_id, external_ref) DO NOTHING`,
		r.ActionID, r.ExternalRef, r.OpClass, r.TargetHost, r.Site, r.PlanHash, CommitConfirmArmed, r.WindowSeconds, float64(r.WindowSeconds),
		r.ForwardBand, r.ForwardApproved, r.AlertRule)
	if err != nil {
		return fmt.Errorf("db: arm commit_confirm %s/%s: %w", r.ActionID, r.ExternalRef, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Conflict: read what is there. Our own retry (still armed, same window) is success; anything
	// else refuses — fail closed, the caller withholds the forward effect.
	got, err := s.Get(ctx, r.ActionID, r.ExternalRef)
	if err != nil {
		return fmt.Errorf("db: arm commit_confirm %s/%s: conflict re-read: %w", r.ActionID, r.ExternalRef, err)
	}
	if got.State == CommitConfirmArmed && got.WindowSeconds == r.WindowSeconds {
		return nil
	}
	return fmt.Errorf("db: arm commit_confirm %s/%s: existing row in state %q (window %ds) — refusing to re-arm over a resolved window",
		r.ActionID, r.ExternalRef, got.State, got.WindowSeconds)
}

// Resolve moves an ARMED row to a terminal (or interim-terminal) state, exactly once. A row that
// is no longer armed returns ErrCommitConfirmResolved — the duplicate/late-signal case. The state
// must be one of the non-armed vocabulary; anything else is a programming error and refuses.
func (s *CommitConfirmStore) Resolve(ctx context.Context, actionID, externalRef, state, detail, inverseActionID string) error {
	switch state {
	case CommitConfirmAborted, CommitConfirmElapsedUnconfirmed, CommitConfirmConfirmed,
		CommitConfirmHeldUnverifiable, CommitConfirmReverted, CommitConfirmRevertFailed:
	default:
		return fmt.Errorf("db: resolve commit_confirm %s/%s: %q is not a resolvable state", actionID, externalRef, state)
	}
	// held_unverifiable is the one non-terminal resolution (REQ-2902: the window HOLDS armed and
	// the inverse may still fire on a later observed deviation), so T-029-3's transitions out of
	// it are allowed here alongside transitions out of 'armed'.
	tag, err := s.p.Exec(ctx, `
		UPDATE commit_confirm
		   SET state = $3, resolved_at = now(), resolution_detail = $4, inverse_action_id = $5
		 WHERE action_id = $1 AND external_ref = $2 AND state IN ($6, $7)`,
		actionID, externalRef, state, detail, inverseActionID, CommitConfirmArmed, CommitConfirmHeldUnverifiable)
	if err != nil {
		return fmt.Errorf("db: resolve commit_confirm %s/%s → %s: %w", actionID, externalRef, state, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("resolve commit_confirm %s/%s → %s: %w", actionID, externalRef, state, ErrCommitConfirmResolved)
	}
	return nil
}

// Get loads one commit_confirm row.
func (s *CommitConfirmStore) Get(ctx context.Context, actionID, externalRef string) (CommitConfirmRow, error) {
	var r CommitConfirmRow
	err := s.p.QueryRow(ctx, `
		SELECT action_id, external_ref, op_class, target_host, site, plan_hash, state,
		       window_seconds, armed_at, deadline_at, resolved_at, resolution_detail, inverse_action_id,
		       forward_band, forward_approved, alert_rule
		  FROM commit_confirm
		 WHERE action_id = $1 AND external_ref = $2`, actionID, externalRef).
		Scan(&r.ActionID, &r.ExternalRef, &r.OpClass, &r.TargetHost, &r.Site, &r.PlanHash, &r.State,
			&r.WindowSeconds, &r.ArmedAt, &r.DeadlineAt, &r.ResolvedAt, &r.ResolutionDetail, &r.InverseActionID,
			&r.ForwardBand, &r.ForwardApproved, &r.AlertRule)
	if err != nil {
		return CommitConfirmRow{}, fmt.Errorf("db: get commit_confirm %s/%s: %w", actionID, externalRef, err)
	}
	return r, nil
}

// OverdueArmed lists armed windows whose deadline passed at least slack ago — the ORPHAN SWEEP's
// read (T-029-3; the TG-82 review-#1 obligation). A live, healthy child resolves its window at
// the deadline, so anything armed and slack-past-deadline has LOST its timer (child never
// started, died, or its resolve is stuck): the sweeper re-adopts each by starting the
// deterministic-ID child again, which the WorkflowID dedup makes safe against a live twin.
func (s *CommitConfirmStore) OverdueArmed(ctx context.Context, slack time.Duration, limit int) ([]CommitConfirmRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.p.Query(ctx, `
		SELECT action_id, external_ref, op_class, target_host, site, plan_hash, state,
		       window_seconds, armed_at, deadline_at, resolved_at, resolution_detail, inverse_action_id,
		       forward_band, forward_approved, alert_rule
		  FROM commit_confirm
		 WHERE state = $1 AND deadline_at < now() - make_interval(secs => $2)
		 ORDER BY deadline_at ASC
		 LIMIT $3`, CommitConfirmArmed, slack.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("db: overdue commit_confirm scan: %w", err)
	}
	defer rows.Close()
	var out []CommitConfirmRow
	for rows.Next() {
		var r CommitConfirmRow
		if err := rows.Scan(&r.ActionID, &r.ExternalRef, &r.OpClass, &r.TargetHost, &r.Site, &r.PlanHash, &r.State,
			&r.WindowSeconds, &r.ArmedAt, &r.DeadlineAt, &r.ResolvedAt, &r.ResolutionDetail, &r.InverseActionID,
			&r.ForwardBand, &r.ForwardApproved, &r.AlertRule); err != nil {
			return nil, fmt.Errorf("db: overdue commit_confirm scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestForRef returns the newest commit_confirm window for one incident — the console's
// workflow-timeline chip read (spec/029 T-029-5, REQ-2906: the state SHALL be queryable and
// rendered). found=false when the incident never armed a window (the ordinary non-eligible
// session; the console simply renders no chip).
func (s *CommitConfirmStore) LatestForRef(ctx context.Context, externalRef string) (CommitConfirmRow, bool, error) {
	if strings.TrimSpace(externalRef) == "" {
		return CommitConfirmRow{}, false, fmt.Errorf("db: commit_confirm latest-for-ref requires an external_ref")
	}
	var r CommitConfirmRow
	err := s.p.QueryRow(ctx, `
		SELECT action_id, external_ref, op_class, target_host, site, plan_hash, state,
		       window_seconds, armed_at, deadline_at, resolved_at, resolution_detail, inverse_action_id,
		       forward_band, forward_approved, alert_rule
		  FROM commit_confirm
		 WHERE external_ref = $1
		 ORDER BY armed_at DESC
		 LIMIT 1`, externalRef).
		Scan(&r.ActionID, &r.ExternalRef, &r.OpClass, &r.TargetHost, &r.Site, &r.PlanHash, &r.State,
			&r.WindowSeconds, &r.ArmedAt, &r.DeadlineAt, &r.ResolvedAt, &r.ResolutionDetail, &r.InverseActionID,
			&r.ForwardBand, &r.ForwardApproved, &r.AlertRule)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CommitConfirmRow{}, false, nil
		}
		return CommitConfirmRow{}, false, fmt.Errorf("db: commit_confirm latest-for-ref %s: %w", externalRef, err)
	}
	return r, true, nil
}
