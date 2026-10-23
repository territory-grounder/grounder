package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/skillstore"
)

// NotableIncidentStore is the pgx-backed resolved-incident source for the skill-store LESSON flywheel (spec/014
// REQ-1312). It reads ESCALATED triage incidents — the ones the agent could NOT resolve autonomously and handed
// to a human, the clearest signal the triage skill was MISSING a procedure — and distills each into a lesson
// for a FIXED, operator-configured target skill + judge dimension. Read-only; the distillation is a
// deterministic TEMPLATE over the incident's own fields (external_ref / host / alert_rule), never model output
// (INV-08). An empty skill or dimension makes it a no-op source — the composition root constructs it ONLY when
// both are configured, so the lesson flywheel stays OFF by default.
type NotableIncidentStore struct {
	p         *Pool
	skill     string // the production skill lessons improve
	dimension string // the judge dimension a lesson candidate trials on
	limit     int    // max incidents per run (bounds the generation burst; the creation half caps again)
}

// NewNotableIncidentStore builds the lesson source. skill and dimension are operator config; an empty skill or
// dimension yields a source that returns nothing (belt-and-suspenders — the wiring should not construct it then).
func NewNotableIncidentStore(p *Pool, skill, dimension string, limit int) *NotableIncidentStore {
	if limit <= 0 {
		limit = 20
	}
	return &NotableIncidentStore{p: p, skill: strings.TrimSpace(skill), dimension: strings.TrimSpace(dimension), limit: limit}
}

// NotableIncidents returns recent ESCALATED incidents (outcome LIKE 'escalated%') within the window, each
// distilled into a lesson for the configured skill+dimension. Newest first, bounded. Read-only.
func (s *NotableIncidentStore) NotableIncidents(ctx context.Context, window time.Duration) ([]skillstore.NotableIncident, error) {
	if s.skill == "" || s.dimension == "" {
		return nil, nil // unconfigured ⇒ no lessons (dormant)
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, coalesce(alert_rule, ''), coalesce(host, '')
		FROM session_triage
		WHERE outcome LIKE 'escalated%' AND external_ref <> '' AND created_at > now() - make_interval(secs => $1)
		ORDER BY created_at DESC
		LIMIT $2`, window.Seconds(), s.limit)
	if err != nil {
		return nil, fmt.Errorf("db: notable incidents: %w", err)
	}
	defer rows.Close()
	var out []skillstore.NotableIncident
	for rows.Next() {
		var ref, rule, host string
		if err := rows.Scan(&ref, &rule, &host); err != nil {
			return nil, fmt.Errorf("db: notable incident scan: %w", err)
		}
		out = append(out, skillstore.NotableIncident{
			ExternalRef:     ref,
			TargetSkill:     s.skill,
			TargetDimension: s.dimension,
			Lesson:          lessonFromEscalation(rule, host),
		})
	}
	return out, rows.Err()
}

// lessonFromEscalation templates the missing-procedure text from the incident's own fields — DATA, not model
// output. An escalated incident is one the agent could not resolve autonomously, so the lesson asks the skill
// to acquire the procedure for that alert class.
func lessonFromEscalation(alertRule, host string) string {
	rule := strings.TrimSpace(alertRule)
	if rule == "" {
		rule = "this alert class"
	}
	where := ""
	if h := strings.TrimSpace(host); h != "" {
		where = " on host " + h
	}
	return fmt.Sprintf("A triage incident for %q%s ESCALATED to a human because the skill had no autonomous procedure to resolve it. Add the diagnostic checks and the safe remediation step that resolve %q, so a recurrence is handled without escalation.", rule, where, rule)
}

// compile-time proof the store satisfies the lesson-flywheel source seam.
var _ skillstore.NotableIncidentSource = (*NotableIncidentStore)(nil)
