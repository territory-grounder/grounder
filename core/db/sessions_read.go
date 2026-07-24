package db

import (
	"context"
	"fmt"
	"time"
)

// SessionReadStore is the pgx-backed READ side of the sessions surface (spec/006 REQ-509): the latest
// GOVERNED SESSION per external_ref — its classification (session_risk_audit) when one exists, else the
// investigation/triage record (session_triage) so a session that reasoned and STOPPED (an agent run that
// proposed no action, hence sealed no classification) still surfaces — joined with the deterministic
// verifier's action_verdict. Read-only by construction — bound parameters ($1), never string-built.
type SessionReadStore struct{ p *Pool }

// NewSessionReadStore returns the Postgres-backed sessions reader.
func NewSessionReadStore(p *Pool) *SessionReadStore { return &SessionReadStore{p: p} }

// SessionRow is one session as the audit spine recorded it (verdict empty when none exists yet).
type SessionRow struct {
	ExternalRef      string
	Band             string
	RiskLevel        string
	ActionID         string
	PlanHash         string
	AutoApproved     bool
	NotifyRequired   bool
	OperatorOverride bool
	SignalsJSON      []byte
	Verdict          string
	CreatedAt        time.Time
}

// Recent returns the newest governed sessions per external_ref, newest first. The ref set is the UNION of
// classified sessions (session_risk_audit) and investigation/triage sessions (session_triage) so an agent
// run that reasoned and STOPPED — leaving triage + agent-cycle rows but no sealed classification — is not
// invisible. Classification-only fields (band, risk, action, flags, signals) come from the latest
// session_risk_audit row when present; a triage-only session carries the triage band and empty
// classification fields (never fabricated). Verdict joins the sealed action, empty when none exists.
func (s *SessionReadStore) Recent(ctx context.Context, limit int) ([]SessionRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.p.Pool.Query(ctx, `
		WITH refs AS (
			SELECT external_ref, MAX(created_at) AS created_at
			FROM (
				SELECT external_ref, created_at FROM session_risk_audit
				UNION ALL
				SELECT external_ref, created_at FROM session_triage
			) u
			GROUP BY external_ref
		)
		SELECT r.external_ref,
		       COALESCE(a.band::text, t.band::text, ''), COALESCE(a.risk_level, ''),
		       COALESCE(a.action_id, ''), COALESCE(a.plan_hash, ''),
		       COALESCE(a.auto_approved, false), COALESCE(a.notify_required, false),
		       COALESCE(a.operator_override, false), a.signals_json,
		       COALESCE(v.verdict::text, ''), r.created_at
		FROM refs r
		LEFT JOIN LATERAL (
			SELECT * FROM session_risk_audit s
			WHERE s.external_ref = r.external_ref
			ORDER BY created_at DESC LIMIT 1
		) a ON true
		LEFT JOIN LATERAL (
			SELECT band FROM session_triage st WHERE st.external_ref = r.external_ref LIMIT 1
		) t ON true
		LEFT JOIN action_verdict v ON v.action_id = a.action_id
		ORDER BY r.created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: sessions read: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		if err := rows.Scan(&r.ExternalRef, &r.Band, &r.RiskLevel, &r.ActionID, &r.PlanHash,
			&r.AutoApproved, &r.NotifyRequired, &r.OperatorOverride, &r.SignalsJSON,
			&r.Verdict, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: sessions scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BandCounts returns the audit spine's band distribution (latest classification per external_ref).
func (s *SessionReadStore) BandCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.p.Pool.Query(ctx, `
		SELECT band::text, count(*)
		FROM (
			SELECT DISTINCT ON (external_ref) band
			FROM session_risk_audit
			ORDER BY external_ref, created_at DESC
		) t GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("db: band counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var band string
		var n int
		if err := rows.Scan(&band, &n); err != nil {
			return nil, fmt.Errorf("db: band counts scan: %w", err)
		}
		out[band] = n
	}
	return out, rows.Err()
}
