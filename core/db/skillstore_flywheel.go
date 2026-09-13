package db

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/territory-grounder/grounder/core/judge"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pgx reads/writes the creation-half flywheel cron drives (spec/014 REQ-1314 + REQ-1307): the
// production set, the per-version rolling judged means (the regressed-dimension signal), the
// open-candidate dedup and admitted-candidate reads, and the offline-eval writer. Parameterized SQL
// only; the LIKE patterns are BOUND VALUES built in Go (never SQL assembled from strings). A version's
// composing sessions are matched by the `#<id>:store` anchor its skill_load provenance carries
// (name@version#id:store[:arm]) — the '#' before and ':store' after fence off prefix/suffix collisions
// so version 12 never matches version 2. Integration-tested under compose (CI has no Postgres, D5).

// SetOfflineEval persists the offline admission run onto the version row (REQ-1307: stored pass or fail
// — an admission refusal is as visible as an admission). Satisfies skillstore.OfflineEvalWriter.
func (s *SkillStore) SetOfflineEval(ctx context.Context, versionID int64, eval json.RawMessage) error {
	tag, err := s.p.Exec(ctx, `UPDATE skill_version SET eval_offline = $2 WHERE id = $1`, versionID, []byte(eval))
	if err != nil {
		return fmt.Errorf("db: set offline eval for version %d: %w", versionID, err)
	}
	if tag.RowsAffected() == 0 {
		return skillstore.ErrNotFound
	}
	return nil
}

// ProductionVersions lists the current production version of every skill (FlywheelStore).
func (s *SkillStore) ProductionVersions(ctx context.Context) ([]skillstore.Version, error) {
	rows, err := s.p.Query(ctx, `SELECT `+versionCols+` FROM skill_version v WHERE v.status = 'production' ORDER BY v.id`)
	if err != nil {
		return nil, fmt.Errorf("db: production versions: %w", err)
	}
	defer rows.Close()
	var out []skillstore.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// OpenCandidates counts open (draft|trial) flywheel candidates parented on a production version
// (FlywheelStore) — the generator's dedup.
func (s *SkillStore) OpenCandidates(ctx context.Context, parentVersionID int64) (int, error) {
	var n int
	err := s.p.QueryRow(ctx, `
		SELECT COUNT(*) FROM skill_version
		WHERE parent_version_id = $1 AND author = $2 AND status IN ('draft', 'trial')`,
		parentVersionID, skillstore.AuthorFlywheel).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: open candidates for parent %d: %w", parentVersionID, err)
	}
	return n, nil
}

// FlywheelDrafts lists the open flywheel DRAFT versions awaiting the offline gate (FlywheelStore),
// NEVER-SCORED first (eval_offline IS NULL), then oldest id. A refused candidate stays a draft with its
// eval_offline stored, so ordering by id alone would let the oldest refused drafts recur every run and
// consume the bounded MaxAdmitPerRun slots forever — STARVING candidates that have never been scored. This
// ordering guarantees every candidate is offline-evaluated at least once before any is re-scored (TG-63 follow-up).
func (s *SkillStore) FlywheelDrafts(ctx context.Context) ([]skillstore.Version, error) {
	rows, err := s.p.Query(ctx, `
		SELECT `+versionCols+` FROM skill_version v
		WHERE v.status = 'draft' AND v.author = $1
		ORDER BY (v.eval_offline IS NOT NULL), v.id`, skillstore.AuthorFlywheel)
	if err != nil {
		return nil, fmt.Errorf("db: flywheel drafts: %w", err)
	}
	defer rows.Close()
	var out []skillstore.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AdmittedCandidates lists the flywheel trial-status versions parented on a production version
// (FlywheelStore) — offline-passed, awaiting an active trial.
func (s *SkillStore) AdmittedCandidates(ctx context.Context, parentVersionID int64) ([]skillstore.Version, error) {
	rows, err := s.p.Query(ctx, `
		SELECT `+versionCols+` FROM skill_version v
		WHERE v.parent_version_id = $1 AND v.author = $2 AND v.status = 'trial' ORDER BY v.id`,
		parentVersionID, skillstore.AuthorFlywheel)
	if err != nil {
		return nil, fmt.Errorf("db: admitted candidates for parent %d: %w", parentVersionID, err)
	}
	defer rows.Close()
	var out []skillstore.Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DimensionMeans returns the rolling judged mean + sample count per dimension over the sessions that
// composed a production version within the trailing window (REQ-1314) — the generator's regressed-
// dimension signal (skillstore.MeansReader). Absent judge tables ⇒ no stats (honest: no measurable
// traffic, so nothing regressed).
//
// GROUP BY means a dimension with ZERO judged samples in the window has NO row here — it does not mean
// the dimension is healthy. The consumer (skillstore.FloorAbsentDimensions, applied in the creation
// half's generate) re-represents every canonical judge dimension, flooring the absent ones, so a
// proposal collapse's falsifiable_prediction ranks as the WORST regression instead of vanishing from the
// improvement loop's view (the flywheel's denominator blindness — the same class the eval plane closed
// with its AbstentionFloor). Kept out of the SQL deliberately: the canonical dimension list is
// core/judge.Dimensions, and a second SQL-side copy could drift from it.
func (s *SkillStore) DimensionMeans(ctx context.Context, versionID int64, window time.Duration) ([]skillstore.DimensionStat, error) {
	// The bound LIKE value matching this version's composing sessions (never SQL assembled from strings).
	loadPattern := fmt.Sprintf("%%#%d:store%%", versionID)
	rows, err := s.p.Query(ctx, `
		SELECT j.dimension, avg(j.score), count(*)
		FROM session_judgment j
		JOIN session_triage t ON t.external_ref = j.external_ref
		WHERE t.created_at > now() - $2::interval AND j.score > 0
		  -- ONE RUBRIC PER POOL (TG-194): a rubric edit must never read as a skill regression — the
		  -- Regressed trigger fires generation off this mean, so mixing versions could spend a whole
		  -- improvement cycle chasing a wording change. Filter, not refuse: refusing would stall the
		  -- flywheel after every bump; filtering just waits for current-rubric samples to accrue.
		  AND j.rubric_version = $3
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(t.skill_loads) e WHERE e LIKE $1)
		  -- falsifiable_prediction is N/A for a grounded stand-down (t.proposed=false): no action ⇒ no
		  -- prediction to falsify. Excluding it here is the SQL-side of judge.PredictionApplicable (TG-61
		  -- seq C) — so the generator's regressed-dimension trigger no longer floors globally on stand-downs.
		  AND NOT (j.dimension = 'falsifiable_prediction' AND t.proposed = false)
		GROUP BY j.dimension`,
		loadPattern, fmt.Sprintf("%d seconds", int(window.Seconds())), judge.RubricVersion())
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: dimension means for version %d: %w", versionID, err)
	}
	defer rows.Close()
	var out []skillstore.DimensionStat
	for rows.Next() {
		var d skillstore.DimensionStat
		if err := rows.Scan(&d.Dimension, &d.Mean, &d.Samples); err != nil {
			return nil, fmt.Errorf("db: scan dimension mean: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
