package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/screen"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// CautionCommentStore is the TG-52 caution feed for the skill-store LESSON flywheel — the generation-side
// consumer of the Reflexion signal. Where the escalation source (NotableIncidentStore) learns from incidents
// the agent HANDED OFF, this learns from the failing sessions the caution lane captures, identified robustly by
// the JUDGE's own verdict: a session scored AT OR BELOW maxScore on the target dimension. It carries the
// judge's OWN verbal comment into the lesson, so the generator is told WHAT went wrong — not merely that a
// rolling mean regressed (the numeric signal the eval-failure half already reads).
//
// Read-only, generate-only; the lane never actuates. The lesson is a DETERMINISTIC TEMPLATE over the
// incident's grammar-validated fields (alert_rule / host) plus the judge comment — the ONE free-text field,
// which is input-SCREENED (neutralize-and-flag, never trusted) before it enters the generation trigger
// (INV-08). Dormant unless BOTH skill and dimension are configured (the composition root only constructs it
// then), exactly like NotableIncidentStore.
type CautionCommentStore struct {
	p         *Pool
	skill     string  // the production skill caution comments improve
	dimension string  // the judge dimension a caution candidate trials on (and the dimension whose low scores select the sessions)
	maxScore  float64 // a judged score AT OR BELOW this is a "failing" session (a clear low, not a soft miss)
	limit     int     // max sessions per run (bounds the generation burst; the creation half caps again)
}

// NewCautionCommentStore builds the caution feed. skill and dimension are operator config; an empty skill or
// dimension yields a dormant source (belt-and-suspenders — the wiring should not construct it then). maxScore
// <= 0 defaults to 2.0 (a 1–5 rubric: at or below 2 is a clear failure on the dimension, not a near miss).
func NewCautionCommentStore(p *Pool, skill, dimension string, maxScore float64, limit int) *CautionCommentStore {
	if limit <= 0 {
		limit = 20
	}
	if maxScore <= 0 {
		maxScore = 2.0
	}
	return &CautionCommentStore{p: p, skill: strings.TrimSpace(skill), dimension: strings.TrimSpace(dimension), maxScore: maxScore, limit: limit}
}

// NotableIncidents returns recent sessions the judge scored AT OR BELOW maxScore on the target dimension —
// the failing sessions — each distilled into a lesson enriched with the judge's verbal comment. Newest first,
// bounded. `j.score > 0` excludes the absent/NA sentinel (a 0 is "not judged on this dimension", not a
// failure). Read-only; an absent judge table is no failures, not an error.
func (s *CautionCommentStore) NotableIncidents(ctx context.Context, window time.Duration) ([]skillstore.NotableIncident, error) {
	if s.skill == "" || s.dimension == "" {
		return nil, nil // unconfigured ⇒ dormant
	}
	rows, err := s.p.Query(ctx, `
		SELECT j.external_ref, coalesce(t.alert_rule, ''), coalesce(t.host, ''), coalesce(j.comment, '')
		FROM session_judgment j
		JOIN session_triage t ON t.external_ref = j.external_ref
		WHERE j.dimension = $1 AND j.score > 0 AND j.score <= $2
		  AND j.external_ref <> '' AND t.created_at > now() - make_interval(secs => $3)
		ORDER BY t.created_at DESC
		LIMIT $4`, s.dimension, s.maxScore, window.Seconds(), s.limit)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: caution comments: %w", err)
	}
	defer rows.Close()
	var out []skillstore.NotableIncident
	for rows.Next() {
		var ref, rule, host, comment string
		if err := rows.Scan(&ref, &rule, &host, &comment); err != nil {
			return nil, fmt.Errorf("db: caution comment scan: %w", err)
		}
		out = append(out, skillstore.NotableIncident{
			ExternalRef:     ref,
			TargetSkill:     s.skill,
			TargetDimension: s.dimension,
			Lesson:          cautionLessonFromComment(rule, host, comment),
		})
	}
	return out, rows.Err()
}

// cautionLessonFromComment templates the lesson from the incident's grammar-validated fields plus the judge's
// verbal comment. The comment is the ONE untrusted free-text field (it can echo alert prose), so it is
// input-SCREENED before it enters the generation trigger (INV-08: neutralize-and-flag, never trust). An empty
// comment still yields a lesson from the signature alone — the low score itself is the signal.
func cautionLessonFromComment(alertRule, host, judgeComment string) string {
	rule := strings.TrimSpace(alertRule)
	if rule == "" {
		rule = "this alert class"
	}
	where := ""
	if h := strings.TrimSpace(host); h != "" {
		where = " on host " + h
	}
	base := fmt.Sprintf("A triage session for %q%s scored LOW on this dimension — the skill's handling did not hold up under judgement.", rule, where)
	if c := strings.TrimSpace(judgeComment); c != "" {
		scrubbed, _ := screen.Scrub(c)
		if sc := strings.TrimSpace(scrubbed); sc != "" {
			base += fmt.Sprintf(" The judge's assessment: %q.", sc)
		}
	}
	base += fmt.Sprintf(" Add the diagnostic checks or the guidance that would raise the skill's handling of %q, so a recurrence scores well.", rule)
	return base
}

// compile-time proof the store satisfies the lesson-flywheel source seam.
var _ skillstore.NotableIncidentSource = (*CautionCommentStore)(nil)
