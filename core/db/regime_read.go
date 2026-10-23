package db

import (
	"context"
	"fmt"
	"time"
)

// RegimeReadStore is the pgx-backed READ side of the Actuation Regime Engine console surface (spec/017
// T-017-7, REQ-1716). Three read-only projections over the worker-written append-only regime audit tables
// (migration 0020) plus a derived lane-coverage roll-up:
//   - Resolutions:      the recent regime_resolution tail (per lane selection: target → regime → lane).
//   - Actuations:       the recent regime_actuation tail (per job launch: action, lane, template, op-class).
//   - DeferredVerdicts: the recent deferred_verdict tail (per completed deferred verify).
//   - LaneCoverage:     per-lane roll-up of resolved selections + launches, for the coverage view.
//
// Every query is parameterized ($1) — no string-built SQL — and selects ONLY the non-secret columns the 0020
// tables hold (which are secret-free by construction, INV-13): target labels, regime/lane slugs, rule ids,
// op-classes, AWX job ids, verdict/graduation slugs, and the AWX token as a SecretRef REFERENCE (token_ref),
// never a value. No column here can carry key material, an argv/host, or a credential value. It is READ-ONLY:
// the runtime role holds no UPDATE/DELETE on these tables (0020 REVOKE); this store never writes.
//
// At mode Shadow the three tables are EMPTY by design (no resolution/launch/verdict occurs until the flip),
// so every read returns an honest empty slice — never a fabricated row.
type RegimeReadStore struct{ p *Pool }

// NewRegimeReadStore returns the Postgres-backed regime read projections.
func NewRegimeReadStore(p *Pool) *RegimeReadStore { return &RegimeReadStore{p: p} }

// RegimeResolutionRow is one regime_resolution row (non-secret lane-selection metadata).
type RegimeResolutionRow struct {
	Target    string
	Regime    string
	Lane      string
	RuleID    string
	Outcome   string
	CreatedAt time.Time
}

// Resolutions returns the recent regime_resolution tail newest-first, capped at limit.
func (s *RegimeReadStore) Resolutions(ctx context.Context, limit int) ([]RegimeResolutionRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.p.Pool.Query(ctx, `
		SELECT target, regime, lane, rule_id, outcome, created_at
		FROM regime_resolution
		ORDER BY created_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: regime resolutions: %w", err)
	}
	defer rows.Close()
	var out []RegimeResolutionRow
	for rows.Next() {
		var r RegimeResolutionRow
		if err := rows.Scan(&r.Target, &r.Regime, &r.Lane, &r.RuleID, &r.Outcome, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: regime resolution scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegimeActuationRow is one regime_actuation row (non-secret launch metadata; token_ref is a SecretRef
// REFERENCE, never a value).
type RegimeActuationRow struct {
	ActionID      string
	Lane          string
	JobTemplateID string
	OpClass       string
	JobID         string
	TokenRef      string
	CreatedAt     time.Time
}

// Actuations returns the recent regime_actuation tail newest-first, capped at limit.
func (s *RegimeReadStore) Actuations(ctx context.Context, limit int) ([]RegimeActuationRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.p.Pool.Query(ctx, `
		SELECT action_id, lane, job_template_id, op_class, job_id, token_ref, created_at
		FROM regime_actuation
		ORDER BY created_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: regime actuations: %w", err)
	}
	defer rows.Close()
	var out []RegimeActuationRow
	for rows.Next() {
		var r RegimeActuationRow
		if err := rows.Scan(&r.ActionID, &r.Lane, &r.JobTemplateID, &r.OpClass, &r.JobID, &r.TokenRef, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: regime actuation scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegimeDeferredVerdictRow is one deferred_verdict row (non-secret deferred-verify outcome).
type RegimeDeferredVerdictRow struct {
	ActionID   string
	JobID      string
	Status     string
	Verdict    string
	Graduation string
	CreatedAt  time.Time
}

// DeferredVerdicts returns the recent deferred_verdict tail newest-first, capped at limit.
func (s *RegimeReadStore) DeferredVerdicts(ctx context.Context, limit int) ([]RegimeDeferredVerdictRow, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.p.Pool.Query(ctx, `
		SELECT action_id, job_id, status, verdict, graduation, created_at
		FROM deferred_verdict
		ORDER BY created_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: regime deferred verdicts: %w", err)
	}
	defer rows.Close()
	var out []RegimeDeferredVerdictRow
	for rows.Next() {
		var r RegimeDeferredVerdictRow
		if err := rows.Scan(&r.ActionID, &r.JobID, &r.Status, &r.Verdict, &r.Graduation, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: regime deferred verdict scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RegimeLaneCoverageRow is one lane's roll-up: how many resolved lane-selections and job launches it carries.
type RegimeLaneCoverageRow struct {
	Lane        string
	Resolutions int
	Actuations  int
}

// LaneCoverage returns the per-lane roll-up (resolved regime_resolution + regime_actuation counts), ordered
// by lane. Empty at Shadow (no lane has been selected or launched through yet).
func (s *RegimeReadStore) LaneCoverage(ctx context.Context) ([]RegimeLaneCoverageRow, error) {
	byLane := map[string]*RegimeLaneCoverageRow{}
	get := func(lane string) *RegimeLaneCoverageRow {
		if r, ok := byLane[lane]; ok {
			return r
		}
		r := &RegimeLaneCoverageRow{Lane: lane}
		byLane[lane] = r
		return r
	}

	resRows, err := s.p.Pool.Query(ctx, `
		SELECT lane, count(*) FROM regime_resolution WHERE outcome = 'resolved' AND lane <> '' GROUP BY lane`)
	if err != nil {
		return nil, fmt.Errorf("db: regime lane coverage (resolutions): %w", err)
	}
	for resRows.Next() {
		var lane string
		var n int
		if err := resRows.Scan(&lane, &n); err != nil {
			resRows.Close()
			return nil, fmt.Errorf("db: regime lane coverage scan (resolutions): %w", err)
		}
		get(lane).Resolutions = n
	}
	resRows.Close()
	if err := resRows.Err(); err != nil {
		return nil, err
	}

	actRows, err := s.p.Pool.Query(ctx, `
		SELECT lane, count(*) FROM regime_actuation GROUP BY lane`)
	if err != nil {
		return nil, fmt.Errorf("db: regime lane coverage (actuations): %w", err)
	}
	for actRows.Next() {
		var lane string
		var n int
		if err := actRows.Scan(&lane, &n); err != nil {
			actRows.Close()
			return nil, fmt.Errorf("db: regime lane coverage scan (actuations): %w", err)
		}
		get(lane).Actuations = n
	}
	actRows.Close()
	if err := actRows.Err(); err != nil {
		return nil, err
	}

	out := make([]RegimeLaneCoverageRow, 0, len(byLane))
	for _, r := range byLane {
		out = append(out, *r)
	}
	sortLaneCoverage(out)
	return out, nil
}

// sortLaneCoverage orders coverage rows by lane for a deterministic view.
func sortLaneCoverage(rows []RegimeLaneCoverageRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Lane < rows[j-1].Lane; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}
