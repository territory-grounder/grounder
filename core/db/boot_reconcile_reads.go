package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/audit"
)

// The reads the boot reconciliation (cmd/worker/boot_reconcile.go, TG-674) needs and nothing else exposed:
// the ledger's LAST authority-governing row, the stored ladder rungs, and the exactly-once credits behind
// them. All bound parameters; all read-only.

// LatestDecisionMatching returns the newest governance_ledger row whose decision matches ANY of the LIKE
// patterns — e.g. "mode-transition:%", "safety:halt" — or found=false on an empty match. The ledger is the
// authority the stored posture rows are reconciled AGAINST, so this reads the ledger directly, never a
// projection of it.
func (s *LedgerStore) LatestDecisionMatching(ctx context.Context, patterns []string) (audit.LedgerEntry, bool, error) {
	if len(patterns) == 0 {
		return audit.LedgerEntry{}, false, nil
	}
	var e audit.LedgerEntry
	err := s.p.QueryRow(ctx, `
		SELECT seq, decision, reason, action_id, withheld, prev_hash, hash, created_at
		FROM governance_ledger
		WHERE decision LIKE ANY($1)
		ORDER BY seq DESC
		LIMIT 1`, patterns).Scan(&e.Seq, &e.Decision, &e.Reason, &e.ActionID, &e.Withheld, &e.PrevHash, &e.Hash, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return audit.LedgerEntry{}, false, nil
		}
		return audit.LedgerEntry{}, false, fmt.Errorf("db: ledger latest decision: %w", err)
	}
	return e, true, nil
}

// GraduationRung is one stored policy_graduation row as the boot reconciliation reads it.
type GraduationRung struct {
	OpClass        string
	Level          string
	CleanRunCount  int
	NoticeRunCount int
	UpdatedAt      time.Time
}

// Rungs returns every stored ladder rung (the policy_graduation table), op-class order.
func (s *PolicyGraduationStore) Rungs(ctx context.Context) ([]GraduationRung, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT op_class, level, clean_run_count, notice_run_count, updated_at
		FROM policy_graduation ORDER BY op_class`)
	if err != nil {
		return nil, fmt.Errorf("db: policy_graduation rungs: %w", err)
	}
	defer rows.Close()
	var out []GraduationRung
	for rows.Next() {
		var r GraduationRung
		if err := rows.Scan(&r.OpClass, &r.Level, &r.CleanRunCount, &r.NoticeRunCount, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifiedCleanCredits counts the exactly-once graduation credits recorded as verified_clean per op-class — the
// evidence a rung is supposed to rest on (spec/028 REQ-2804).
func (s *PolicyGraduationStore) VerifiedCleanCredits(ctx context.Context) (map[string]int, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT op_class, count(*) FROM graduation_credit
		WHERE outcome = 'verified_clean' GROUP BY op_class`)
	if err != nil {
		return nil, fmt.Errorf("db: graduation credits: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		out[c] = n
	}
	return out, rows.Err()
}
