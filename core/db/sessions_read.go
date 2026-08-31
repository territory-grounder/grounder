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
	// Host is the entity the incident was about. It lives on session_triage and was NOT carried here, so
	// every consumer had to dig it out of SignalsJSON — which never contains it (production: 0 of 50 rows
	// carry a `host` key; the keys are novelty_key, poll_reason, novelty_count, actor_attribution). The
	// knowledge view filtered per-host on that miss and therefore showed ZERO incidents for every host,
	// forever, over a spine holding 3,202 sessions across 78 hosts.
	Host      string
	Verdict   string
	CreatedAt time.Time
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
		       COALESCE(v.verdict::text, ''), r.created_at, COALESCE(t.host, '')
		FROM refs r
		LEFT JOIN LATERAL (
			SELECT * FROM session_risk_audit s
			WHERE s.external_ref = r.external_ref
			ORDER BY created_at DESC LIMIT 1
		) a ON true
		LEFT JOIN LATERAL (
			SELECT band, host FROM session_triage st WHERE st.external_ref = r.external_ref LIMIT 1
		) t ON true
		-- THIS SESSION'S OWN OUTCOME, not the action shape's first-ever one.
		--
		-- This was: LEFT JOIN action_verdict v ON v.action_id = a.action_id. action_id is content-addressed
		-- over the operation alone (INV-07), so it is the same value for every session that ever proposed that
		-- operation, and action_verdict is keyed by it and written first-wins -- one row per SHAPE, for all
		-- time. The join therefore stamped the first execution's verdict onto every later session with the
		-- same shape. Live 2026-07-29, straight off the Command view: three sessions sharing action 47d1d005
		-- all read "match", and three sharing 957f5d4d all read "deviation", on a single row each.
		--
		-- action_execution (migration 0043) is the table that answers "what happened on THIS run?" -- one row
		-- per execution, carrying external_ref. It was built for exactly this and no reader had switched to
		-- it; only an EXISTS-on-action_id in action_manifest_read used it at all. The LATERAL takes this
		-- session's latest execution.
		--
		-- verdict IS NULL means executed-but-unverifiable (TG-182 fail-closed: the post-state could not be
		-- read). That is NOT a clean result and must not read as one, so it surfaces as "unverifiable" rather
		-- than collapsing to the empty string like a session that never executed.
		LEFT JOIN LATERAL (
			SELECT COALESCE(e.verdict::text, 'unverifiable') AS verdict
			FROM action_execution e
			WHERE e.action_id = a.action_id AND e.external_ref = r.external_ref
			ORDER BY e.executed_at DESC LIMIT 1
		) v ON true
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
			&r.Verdict, &r.CreatedAt, &r.Host); err != nil {
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

// Count is the POPULATION the list pages — every session reachable through Recent, not the bounded page
// Recent returns. Surfaces that derive a number from the sessions read need it: the console's Knowledge model
// composes its pages from Recent(limit=50) while the spine holds 1,306 rows, so a badge fed len(page)
// reported the fetch limit rather than the estate. Same defect the alerts badge carried at a constant 50.
func (s *SessionReadStore) Count(ctx context.Context) (int, error) {
	var n int
	// THE SAME POPULATION THE LIST PAGES (TG-249 item 2).
	//
	// This counted session_risk_audit alone while Recent() draws from the UNION of session_risk_audit and
	// session_triage. Measured on the live estate 2026-08-05: the API reported total=2040 against a real
	// population of 3225 — 1,185 sessions, 37%, that a client could page through while the total said they
	// did not exist.
	//
	// That is the same defect class as the !852 badge, which counted a different population than the list
	// beside it in the same response; it just failed in the roomier direction, where a client trusting
	// `total` for pagination stops early and never learns it stopped early.
	//
	// UNION (not UNION ALL) because external_ref is the join key between the two tables and a session
	// present in both must count once — UNION ALL here would over-report by exactly the overlap, trading
	// an undercount for an overcount.
	if err := s.p.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT external_ref FROM session_risk_audit
			UNION
			SELECT external_ref FROM session_triage
		) u`).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: session count: %w", err)
	}
	return n, nil
}
