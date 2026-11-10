package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/holds"
)

// HoldStore is the pgx-backed projection of every HOLD decision (hold_decision, migration 0127; TG-667). It is
// the cross-process seam between the Runner, which writes a row beside every hold/deny ledger row
// (RecordVoteActivity on tg.runner), the classifier, which asks whether a sticky hold is still open for the
// incident it is banding, and the console/metrics readers in the grounder. INSERT-only: a renewal is a new
// row and "open" is a predicate on expires_at. It holds NO authority — the governance_ledger row is the
// decision; this table only lets a later session REMEMBER it.
type HoldStore struct{ p *Pool }

// NewHoldStore returns a Postgres-backed hold projection.
func NewHoldStore(p *Pool) *HoldStore { return &HoldStore{p: p} }

// Record persists one hold. A hold with no correlation key, no reason, or no expiry is refused (fail closed:
// an unkeyed or unbounded hold could never be honoured correctly or would never be released).
func (s *HoldStore) Record(ctx context.Context, h holds.HoldDecision) error {
	if strings.TrimSpace(h.ExternalRef) == "" || strings.TrimSpace(h.ReasonKey) == "" || h.ExpiresAt.IsZero() {
		return fmt.Errorf("db: hold_decision refused — external_ref, reason_key and expires_at are required (ref=%q reason=%q)", h.ExternalRef, h.ReasonKey)
	}
	decidedAt := h.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO hold_decision
			(external_ref, host, alert_rule, op_class, action_id, reason_key, why, actor_attribution, decided_by, decided_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		h.ExternalRef, h.Host, h.AlertRule, h.OpClass, h.ActionID, h.ReasonKey, h.Why, h.ActorAttribution, h.DecidedBy, decidedAt, h.ExpiresAt)
	if err != nil {
		return fmt.Errorf("db: record hold_decision %s: %w", h.ExternalRef, err)
	}
	return nil
}

const holdColumns = `seq, external_ref, host, alert_rule, op_class, action_id, reason_key, why, actor_attribution, decided_by, decided_at, expires_at`

func scanHold(rows interface{ Scan(...any) error }) (holds.HoldDecision, error) {
	var h holds.HoldDecision
	err := rows.Scan(&h.Seq, &h.ExternalRef, &h.Host, &h.AlertRule, &h.OpClass, &h.ActionID, &h.ReasonKey, &h.Why,
		&h.ActorAttribution, &h.DecidedBy, &h.DecidedAt, &h.ExpiresAt)
	return h, err
}

// OpenFor returns the NEWEST STICKY hold for (host, alertRule) that is still open at now, if any. The sticky
// set is bound as a parameter (holds.StickyReasons) and filtered IN SQL, so a run of newer non-sticky rows can
// never hide an older sticky one whatever their number — a Go-side filter behind a LIMIT would (fresh-eyes
// review, 2026-11-07). The judgement of what is sticky still lives in core/holds; this read only applies it.
func (s *HoldStore) OpenFor(ctx context.Context, host, alertRule string, now time.Time) (holds.HoldDecision, bool, error) {
	if strings.TrimSpace(host) == "" || strings.TrimSpace(alertRule) == "" {
		return holds.HoldDecision{}, false, nil
	}
	rows, err := s.p.Query(ctx, `
		SELECT `+holdColumns+`
		FROM hold_decision
		WHERE host = $1 AND alert_rule = $2 AND expires_at > $3 AND reason_key = ANY($4)
		  AND NOT EXISTS (SELECT 1 FROM control_window cw
		                   WHERE cw.host = hold_decision.host AND hold_decision.decided_at >= cw.starts_at AND hold_decision.decided_at <= cw.ends_at)
		ORDER BY decided_at DESC, seq DESC
		LIMIT 1`, host, alertRule, now, holds.StickyReasons())
	if err != nil {
		return holds.HoldDecision{}, false, fmt.Errorf("db: open hold for %s/%s: %w", host, alertRule, err)
	}
	defer rows.Close()
	if rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return holds.HoldDecision{}, false, err
		}
		return h, true, rows.Err()
	}
	return holds.HoldDecision{}, false, rows.Err()
}

// OpenHolds returns every hold still open at now, oldest first — the console list and the watch gauges.
func (s *HoldStore) OpenHolds(ctx context.Context, now time.Time) ([]holds.HoldDecision, error) {
	rows, err := s.p.Query(ctx, `
		SELECT `+holdColumns+`
		FROM hold_decision
		WHERE expires_at > $1
		ORDER BY decided_at, seq`, now)
	if err != nil {
		return nil, fmt.Errorf("db: list open holds: %w", err)
	}
	defer rows.Close()
	var out []holds.HoldDecision
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HoldRedetections reports, for every hold open at now, when its incident last FIRED AGAIN after the hold was
// decided — a later session_triage row on the same (host, alert_rule) from a DIFFERENT session. Keyed by the
// hold's seq; a hold whose incident has not re-fired is absent from the map. The hold's own session is excluded
// by external_ref, not by time: on the propose path its triage row predates decided_at, but a stand-down before
// propose writes the terminal row after the decision and would otherwise count as its own re-detection
// (fresh-eyes review, 2026-11-07). The hold watch (TG-667) reads this once a minute; the join is covered by
// migration 0128's (host, alert_rule, created_at) index.
func (s *HoldStore) HoldRedetections(ctx context.Context, now time.Time) (map[int64]time.Time, error) {
	rows, err := s.p.Query(ctx, `
		SELECT h.seq, MAX(t.created_at)
		FROM hold_decision h
		JOIN session_triage t
		  ON t.host = h.host AND t.alert_rule = h.alert_rule AND t.created_at > h.decided_at
		 AND t.external_ref <> h.external_ref
		WHERE h.expires_at > $1
		GROUP BY h.seq`, now)
	if err != nil {
		return nil, fmt.Errorf("db: hold redetections: %w", err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var seq int64
		var at time.Time
		if err := rows.Scan(&seq, &at); err != nil {
			return nil, err
		}
		out[seq] = at
	}
	return out, rows.Err()
}

// FailedInvestigationsUnretried returns the sessions since `since` that ended `failed:investigate` and were
// NOT followed by a later non-failed session for the same (host, alert_rule) — the incidents TG tried to look
// at and never got back to. Oldest first. The join reads session_triage, the terminal triage record every
// session writes, so a crashed investigation is never invisible merely because it produced no proposal.
func (s *HoldStore) FailedInvestigationsUnretried(ctx context.Context, since time.Time) ([]holds.FailedInvestigation, error) {
	rows, err := s.p.Query(ctx, `
		SELECT f.external_ref, f.host, f.alert_rule, f.stop_reason, f.created_at
		FROM session_triage f
		WHERE f.outcome = 'failed:investigate' AND f.created_at >= $1
		  AND NOT EXISTS (
		      SELECT 1 FROM session_triage l
		      WHERE l.host = f.host AND l.alert_rule = f.alert_rule
		        AND l.created_at > f.created_at AND l.outcome <> 'failed:investigate')
		ORDER BY f.created_at, f.external_ref`, since)
	if err != nil {
		return nil, fmt.Errorf("db: list unretried failed investigations: %w", err)
	}
	defer rows.Close()
	var out []holds.FailedInvestigation
	for rows.Next() {
		var f holds.FailedInvestigation
		if err := rows.Scan(&f.ExternalRef, &f.Host, &f.AlertRule, &f.StopReason, &f.FailedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
