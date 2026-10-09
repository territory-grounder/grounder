package db

import (
	"context"
	"errors"
	"fmt"
	"github.com/territory-grounder/grounder/core/judge"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// The pgx skillstore.TrialStore over skill_trial / skill_trial_assignment (migration 0009).
// Parameterized SQL only; the one-active partial index and the (ref, trial) UNIQUE carry the
// structural invariants; judged scores join session_judgment-equivalent data from the eval spine
// (grounding read model) — compose-tested (D5).

const trialCols = `id, skill_name, candidate_ids, COALESCE(control_version_id, 0), dimension,
	min_samples_per_arm, min_lift, p_threshold, ends_at, status, note`

// fillingSampleWhere is the ONE definition of "a judgment that fills a trial arm", aliased j:
// scored, stamped with the current rubric, and on the trial's dimension. $1 = rubric version,
// $2 = dimension.
//
// Both the arm reader and the fill-rate projection are assembled from this string so that the
// population the REQ-1309 guard projects against cannot drift from the population the arms actually
// fill from. They HAD drifted: the rate counted DISTINCT external_ref over session_judgment with no
// dimension and no rubric predicate at all, which on the running system read 207.57/day where the
// arms of trial 14 were filling at 0.43/day. A guard written to stop a trial burning its window
// (the predecessor completed zero trials in three months) was projecting against a supply 483x
// larger than the one it was guarding.
const fillingSampleWhere = `j.score > 0 AND j.rubric_version = $1 AND j.dimension = $2`

func scanTrial(row interface{ Scan(...any) error }) (skillstore.Trial, error) {
	var t skillstore.Trial
	err := row.Scan(&t.ID, &t.SkillName, &t.CandidateIDs, &t.ControlVersionID, &t.Dimension,
		&t.MinSamplesPerArm, &t.MinLift, &t.PThreshold, &t.EndsAt, &t.Status, &t.Note)
	return t, err
}

// ActiveTrialFor implements skillstore.TrialStore.
func (s *SkillStore) ActiveTrialFor(ctx context.Context, skillName string) (skillstore.Trial, bool, error) {
	t, err := scanTrial(s.p.QueryRow(ctx,
		`SELECT `+trialCols+` FROM skill_trial WHERE skill_name = $1 AND status = 'active'`, skillName))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return skillstore.Trial{}, false, nil
		}
		return skillstore.Trial{}, false, fmt.Errorf("db: active trial for %s: %w", skillName, err)
	}
	return t, true, nil
}

// ActiveTrials implements skillstore.TrialStore.
func (s *SkillStore) ActiveTrials(ctx context.Context) ([]skillstore.Trial, error) {
	rows, err := s.p.Query(ctx, `SELECT `+trialCols+` FROM skill_trial WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("db: active trials: %w", err)
	}
	defer rows.Close()
	var out []skillstore.Trial
	for rows.Next() {
		t, err := scanTrial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Assign implements the read-before-hash idempotency structurally: INSERT ... ON CONFLICT DO NOTHING,
// then ALWAYS read the stored row back — the row wins over any recomputation (REQ-1306).
func (s *SkillStore) Assign(ctx context.Context, externalRef string, trialID int64, variant int) (int, error) {
	if _, err := s.p.Exec(ctx, `
		INSERT INTO skill_trial_assignment (external_ref, trial_id, variant_idx)
		VALUES ($1, $2, $3) ON CONFLICT (external_ref, trial_id) DO NOTHING`,
		externalRef, trialID, variant); err != nil {
		return 0, fmt.Errorf("db: assign %s to trial %d: %w", externalRef, trialID, err)
	}
	var stored int
	if err := s.p.QueryRow(ctx, `
		SELECT variant_idx FROM skill_trial_assignment WHERE external_ref = $1 AND trial_id = $2`,
		externalRef, trialID).Scan(&stored); err != nil {
		return 0, fmt.Errorf("db: read assignment %s/%d: %w", externalRef, trialID, err)
	}
	return stored, nil
}

// ArmScores joins assignments against the judged-session scores for the trial's dimension. The judge
// spine writes session scores into grounding's session_judgment-equivalent store; until task #26 lands
// the durable judge writer, this reads the session_scores table if present and returns empty maps
// otherwise (an empty arm keeps the trial active — honest, never fabricated).
func (s *SkillStore) ArmScores(ctx context.Context, trialID int64) (map[int][]float64, error) {
	return s.armScoresForDim(ctx, trialID, "")
}

// SafetyArmScores implements the asymmetric guard's read (REQ-1308).
func (s *SkillStore) SafetyArmScores(ctx context.Context, trialID int64) (map[int][]float64, error) {
	return s.armScoresForDim(ctx, trialID, "appropriate_band")
}

// ProposalArmRates implements the anti-abstention guard's read (Phase C3): per-arm 1/0 proposal
// indicators from session_triage, so FinalizeTrials can refuse a candidate that "wins" its judged
// dimension by standing down more than the concurrent control.
func (s *SkillStore) ProposalArmRates(ctx context.Context, trialID int64) (map[int][]float64, error) {
	rows, err := s.p.Query(ctx, `
		SELECT a.variant_idx, CASE WHEN st.proposed THEN 1.0 ELSE 0.0 END
		FROM skill_trial_assignment a
		JOIN session_triage st ON st.external_ref = a.external_ref
		WHERE a.trial_id = $1`, trialID)
	if err != nil {
		if isUndefinedTable(err) {
			return map[int][]float64{}, nil
		}
		return nil, fmt.Errorf("db: proposal arm rates for trial %d: %w", trialID, err)
	}
	defer rows.Close()
	out := map[int][]float64{}
	for rows.Next() {
		var variant int
		var v float64
		if err := rows.Scan(&variant, &v); err != nil {
			return nil, err
		}
		out[variant] = append(out[variant], v)
	}
	return out, rows.Err()
}

func (s *SkillStore) armScoresForDim(ctx context.Context, trialID int64, dimOverride string) (map[int][]float64, error) {
	// The dimension resolves in Go rather than a COALESCE against the joined trial row so that the
	// filling-sample predicate can be one literal string shared with ArmFillRate. An unknown trial is
	// an empty result, never a fabricated one.
	dim := dimOverride
	if dim == "" {
		if err := s.p.QueryRow(ctx, `SELECT dimension FROM skill_trial WHERE id = $1`, trialID).Scan(&dim); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return map[int][]float64{}, nil
			}
			if isUndefinedTable(err) {
				return map[int][]float64{}, nil
			}
			return nil, fmt.Errorf("db: dimension for trial %d: %w", trialID, err)
		}
	}
	// ONE RUBRIC PER POOL (TG-194): arms race concurrently, so a mid-trial rubric bump would score
	// early sessions under one wording and late ones under another — inside a single comparison.
	rows, err := s.p.Query(ctx, `
		SELECT a.variant_idx, j.score
		FROM skill_trial_assignment a
		JOIN session_judgment j ON j.external_ref = a.external_ref
		WHERE `+fillingSampleWhere+` AND a.trial_id = $3`,
		judge.RubricVersion(), dim, trialID)
	if err != nil {
		// The judge table not existing yet is an EMPTY result, not an error: trials stay honestly
		// active until task #26's durable judge writer lands.
		if isUndefinedTable(err) {
			return map[int][]float64{}, nil
		}
		return nil, fmt.Errorf("db: arm scores for trial %d: %w", trialID, err)
	}
	defer rows.Close()
	out := map[int][]float64{}
	for rows.Next() {
		var variant int
		var score float64
		if err := rows.Scan(&variant, &score); err != nil {
			return nil, err
		}
		out[variant] = append(out[variant], score)
	}
	return out, rows.Err()
}

// FinalizeTrial implements skillstore.TrialStore (append to the note log, set winner columns).
func (s *SkillStore) FinalizeTrial(ctx context.Context, trialID int64, status string, winnerVersionID int64, winnerMean, winnerP float64, note string) error {
	var winner any
	if winnerVersionID > 0 {
		winner = winnerVersionID
	}
	tag, err := s.p.Exec(ctx, `
		UPDATE skill_trial SET status = $2, winner_version_id = $3, winner_mean = NULLIF($4, 0),
			winner_p = NULLIF($5, 0), note = note || E'\n' || $6, finalized_at = now()
		WHERE id = $1 AND status = 'active'`,
		trialID, status, winner, winnerMean, winnerP, note)
	if err != nil {
		return fmt.Errorf("db: finalize trial %d: %w", trialID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: finalize trial %d: not active", trialID)
	}
	return nil
}

// ArmFillRate counts, per day over the trailing window, the sessions that produced a judgment able to
// FILL an arm on this dimension (REQ-1309's start-refusal input, and REQ-1313's projection input).
// Built from fillingSampleWhere, so it counts exactly what ArmScores counts. Absent judge table ⇒
// 0 ⇒ every start refuses (honest: no measurable supply).
func (s *SkillStore) ArmFillRate(ctx context.Context, dimension string, window time.Duration) (float64, error) {
	var n int64
	err := s.p.QueryRow(ctx, `
		SELECT COUNT(DISTINCT j.external_ref) FROM session_judgment j
		WHERE `+fillingSampleWhere+` AND j.judged_at > now() - $3::interval`,
		judge.RubricVersion(), dimension, fmt.Sprintf("%d seconds", int(window.Seconds()))).Scan(&n)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("db: arm fill rate for %s: %w", dimension, err)
	}
	return float64(n) / (window.Hours() / 24), nil
}

// CreateTrial inserts the active row (the one-active partial index refuses a second).
func (s *SkillStore) CreateTrial(ctx context.Context, t skillstore.Trial) (skillstore.Trial, error) {
	sv, err := schema.Stamp(schema.TableSkillTrial)
	if err != nil {
		return skillstore.Trial{}, err
	}
	var control any
	if t.ControlVersionID > 0 {
		control = t.ControlVersionID
	}
	err = s.p.QueryRow(ctx, `
		INSERT INTO skill_trial (skill_name, candidate_ids, control_version_id, dimension,
			min_samples_per_arm, min_lift, p_threshold, ends_at, status, note, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'active', $9, $10) RETURNING id`,
		t.SkillName, t.CandidateIDs, control, t.Dimension, t.MinSamplesPerArm, t.MinLift,
		t.PThreshold, t.EndsAt, t.Note, int(sv)).Scan(&t.ID)
	if err != nil {
		return skillstore.Trial{}, fmt.Errorf("db: create trial for %s: %w", t.SkillName, err)
	}
	t.Status = "active"
	return t, nil
}

// IsPinned implements skillstore.TrialStore (TG-218). A skill row that does not exist is not pinned:
// StartTrial's other gates and the FK on skill_trial reject an unknown skill on their own, and
// answering "pinned" for a missing row would refuse every trial the moment the join went wrong.
func (s *SkillStore) IsPinned(ctx context.Context, skillName string) (bool, error) {
	var pinned bool
	err := s.p.QueryRow(ctx, `SELECT pinned FROM skill WHERE name = $1`, skillName).Scan(&pinned)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: pin state for %s: %w", skillName, err)
	}
	return pinned, nil
}

// isUndefinedTable reports the postgres undefined_table error (42P01) — the judge spine's table may
// not exist until task #26 lands.
func isUndefinedTable(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "42P01"
}
