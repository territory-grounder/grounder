package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/reflect"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// ExperienceStore persists the experience reflector's record (TG-579, migration 0116): one row per session,
// first write wins (ON CONFLICT DO NOTHING — a reconcile retry never rewrites a reflection).
type ExperienceStore struct{ p *Pool }

// NewExperienceStore builds the pgx-backed store.
func NewExperienceStore(p *Pool) *ExperienceStore { return &ExperienceStore{p: p} }

// Write records one experience. Parameter-bound; the caller has already parsed, capped and screened it.
func (s *ExperienceStore) Write(ctx context.Context, e reflect.Experience) error {
	if strings.TrimSpace(e.ExternalRef) == "" {
		return fmt.Errorf("db: experience with empty external_ref refused")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO session_experience (external_ref, host, alert_rule, verdict, cue, lesson, evidence, confidence, moments, model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (external_ref) DO NOTHING`,
		e.ExternalRef, e.Host, e.AlertRule, e.Verdict, e.Cue, e.Lesson, e.Evidence, e.Confidence, e.Moments, e.Model)
	if err != nil {
		return fmt.Errorf("db: write experience %s: %w", e.ExternalRef, err)
	}
	return nil
}

// ExperienceLessonSource adapts the reflector's record to the skill flywheel's LESSON seam (REQ-1312,
// skillstore.NotableIncidentSource): each experience at or above the minimum confidence becomes a lesson for the
// operator-configured target skill + dimension — the Harness² harnessing-level recursion wired into the loop TG
// already has. Joined to the escalation (and caution-comment) feeds by CombineNotableSources when the lesson
// flywheel is armed; dormant otherwise. The lesson text is the reflector's own cue/lesson/evidence — DATA the
// generator incorporates under the offline-admission gate and the live trial, never a direct prompt edit.
type ExperienceLessonSource struct {
	p             *Pool
	skill         string
	dimension     string
	minConfidence string // high | medium | low — "low" admits every entry
	limit         int
}

// NewExperienceLessonSource builds the source. An empty skill or dimension yields nothing (dormant).
func NewExperienceLessonSource(p *Pool, skill, dimension, minConfidence string, limit int) *ExperienceLessonSource {
	if limit <= 0 {
		limit = 20
	}
	mc := strings.ToLower(strings.TrimSpace(minConfidence))
	if mc != "high" && mc != "medium" && mc != "low" {
		mc = "medium"
	}
	return &ExperienceLessonSource{p: p, skill: strings.TrimSpace(skill), dimension: strings.TrimSpace(dimension), minConfidence: mc, limit: limit}
}

// admitted returns the confidence values at or above the minimum.
func (s *ExperienceLessonSource) admitted() []string {
	switch s.minConfidence {
	case "high":
		return []string{"high"}
	case "low":
		return []string{"high", "medium", "low"}
	default:
		return []string{"high", "medium"}
	}
}

// NotableIncidents implements skillstore.NotableIncidentSource over session_experience.
func (s *ExperienceLessonSource) NotableIncidents(ctx context.Context, window time.Duration) ([]skillstore.NotableIncident, error) {
	if s.skill == "" || s.dimension == "" {
		return nil, nil
	}
	// DISTINCT ON (lesson): twenty sessions that reflected to the same lesson are ONE lesson (newest wins),
	// so a recurring failure cannot fill the generator's per-run cap and crowd out the other feeds.
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, cue, lesson, evidence, confidence FROM (
			SELECT DISTINCT ON (lesson) external_ref, cue, lesson, evidence, confidence, created_at
			FROM session_experience
			WHERE created_at > now() - make_interval(secs => $1) AND confidence = ANY($2)
			ORDER BY lesson, created_at DESC
		) d
		ORDER BY created_at DESC
		LIMIT $3`, window.Seconds(), s.admitted(), s.limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: experience lessons: %w", err)
	}
	defer rows.Close()
	var out []skillstore.NotableIncident
	for rows.Next() {
		var ref, cue, lesson, evidence, conf string
		if err := rows.Scan(&ref, &cue, &lesson, &evidence, &conf); err != nil {
			return nil, fmt.Errorf("db: experience lesson scan: %w", err)
		}
		out = append(out, skillstore.NotableIncident{
			ExternalRef:     ref,
			TargetSkill:     s.skill,
			TargetDimension: s.dimension,
			Lesson:          ExperienceLessonText(cue, lesson, evidence, conf),
		})
	}
	return out, rows.Err()
}

// ExperienceLessonText renders one experience as the lesson the generator incorporates.
func ExperienceLessonText(cue, lesson, evidence, confidence string) string {
	return fmt.Sprintf("When %s: %s (evidence from a remediation that did not verify clean: %s; reflector confidence %s)", cue, lesson, evidence, confidence)
}

// compile-time proof the source satisfies the lesson-flywheel seam.
var _ skillstore.NotableIncidentSource = (*ExperienceLessonSource)(nil)
