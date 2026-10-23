package db

// TG-490: the tracker_entry store — TG's own entry-ticket ledger. The reconciling creator (a
// worker ticker) scans recent alert-sourced incidents that have NO entry row (Unfiled), files a
// ticket per incident through the tracker's EntryCreator capability, and records it here exactly
// once (Ensure — the PK is the idempotency). The recovery-comment pass advances a per-row cursor
// over ingest_transition ids so a recovery is commented once, never re-spammed.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TrackerEntry is one filed entry ticket.
type TrackerEntry struct {
	ExternalRef             string
	IssueID                 string
	Project                 string
	SourceType              string
	CreatedAt               time.Time
	LastCommentTransitionID int64
}

// UnfiledAlert is one alert-sourced incident awaiting an entry ticket — the renderer's complete,
// non-secret input (INV-08: the ticket body is pure data from the ingest record, never a model token).
type UnfiledAlert struct {
	ExternalRef string
	SourceType  string
	AlertRule   string
	Severity    string
	Host        string
	Site        string
	Summary     string
	ReceivedAt  time.Time
}

// RecoveryToComment is one recovery transition newer than its entry's comment cursor.
type RecoveryToComment struct {
	ExternalRef  string
	IssueID      string
	TransitionID int64
	Host         string
	AlertRule    string
	ObservedAt   *time.Time
	ReceivedAt   time.Time
}

// TrackerEntryStore reads and writes tracker_entry rows.
type TrackerEntryStore struct{ p *Pool }

func NewTrackerEntryStore(p *Pool) *TrackerEntryStore { return &TrackerEntryStore{p: p} }

// Reserve claims an incident for filing BEFORE the tracker create fires (phase 1). true = this
// caller holds the reservation (or an earlier crashed attempt does — either way the incident
// leaves the Unfiled work list and no blind second create can happen). The reservation is the
// structural fix for the create→record crash window: what crashes leaves a VISIBLE reserved row
// for the resolver, never an untracked orphan ticket.
func (s *TrackerEntryStore) Reserve(ctx context.Context, externalRef, project, sourceType string) (bool, error) {
	if strings.TrimSpace(externalRef) == "" || strings.TrimSpace(project) == "" {
		return false, fmt.Errorf("db: tracker_entry reserve: external_ref and project are required")
	}
	tag, err := s.p.Exec(ctx, `
		INSERT INTO tracker_entry (external_ref, issue_id, project, source_type)
		VALUES ($1, '', $2, $3)
		ON CONFLICT (external_ref) DO NOTHING`,
		externalRef, project, sourceType)
	if err != nil {
		return false, fmt.Errorf("db: tracker_entry reserve %s: %w", externalRef, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Complete binds the created ticket to its reservation (phase 3). First completion wins: a row
// already carrying an issue id is never overwritten (the adopt-vs-create race resolves to
// whichever completion landed first, and the loser's ticket id is returned to the caller for the
// duplicate-closing comment path).
func (s *TrackerEntryStore) Complete(ctx context.Context, externalRef, issueID string) (won bool, existing string, err error) {
	if strings.TrimSpace(externalRef) == "" || strings.TrimSpace(issueID) == "" {
		return false, "", fmt.Errorf("db: tracker_entry complete: external_ref and issue_id are required")
	}
	tag, err := s.p.Exec(ctx, `
		UPDATE tracker_entry SET issue_id = $2 WHERE external_ref = $1 AND issue_id = ''`,
		externalRef, issueID)
	if err != nil {
		return false, "", fmt.Errorf("db: tracker_entry complete %s: %w", externalRef, err)
	}
	if tag.RowsAffected() == 1 {
		return true, issueID, nil
	}
	got, found, gerr := s.Get(ctx, externalRef)
	if gerr != nil || !found {
		return false, "", fmt.Errorf("db: tracker_entry complete %s: lost the race but cannot read the winner: %v", externalRef, gerr)
	}
	return false, got.IssueID, nil
}

// StaleReservation is one crash leftover, carrying the reservation's identity AND the incident's
// full render inputs re-joined from the durable ingest record — so a resolver-created ticket is a
// REAL ticket (host, rule, severity, provider summary), never an information-lossy placeholder
// (the round-2 review's finding #3: the data was durable all along; the reservation only needed
// to look it up).
type StaleReservation struct {
	ExternalRef string
	Project     string
	ReservedAt  time.Time
	Alert       UnfiledAlert // zero-valued fields when the ingest row is (abnormally) absent
}

// StaleReserved lists reservations older than age whose create never completed — the resolver's
// work list (search-adopt-or-create). Rare by construction: only a crash inside the filing
// attempt leaves one. LEFT JOIN: a reservation whose ingest row vanished (retention) still
// surfaces, with placeholder render inputs, rather than hiding forever.
func (s *TrackerEntryStore) StaleReserved(ctx context.Context, age time.Duration, limit int) ([]StaleReservation, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.p.Query(ctx, `
		SELECT t.external_ref, t.project, t.created_at,
		       COALESCE(a.source_type,''), COALESCE(a.alert_rule,''), COALESCE(a.severity,''),
		       COALESCE(a.host,''), COALESCE(a.site,''), COALESCE(a.summary,''), COALESCE(a.received_at, t.created_at)
		  FROM tracker_entry t
		  LEFT JOIN ingest_alert a ON a.external_ref = t.external_ref
		 WHERE t.issue_id = '' AND t.created_at < now() - make_interval(secs => $1)
		 ORDER BY t.created_at ASC
		 LIMIT $2`, age.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("db: tracker_entry stale-reserved scan: %w", err)
	}
	defer rows.Close()
	var out []StaleReservation
	for rows.Next() {
		var r StaleReservation
		if err := rows.Scan(&r.ExternalRef, &r.Project, &r.ReservedAt,
			&r.Alert.SourceType, &r.Alert.AlertRule, &r.Alert.Severity,
			&r.Alert.Host, &r.Alert.Site, &r.Alert.Summary, &r.Alert.ReceivedAt); err != nil {
			return nil, fmt.Errorf("db: tracker_entry stale-reserved scan: %w", err)
		}
		r.Alert.ExternalRef = r.ExternalRef
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get loads one entry. found=false when the incident has no filed ticket.
func (s *TrackerEntryStore) Get(ctx context.Context, externalRef string) (TrackerEntry, bool, error) {
	var e TrackerEntry
	err := s.p.QueryRow(ctx, `
		SELECT external_ref, issue_id, project, source_type, created_at, last_comment_transition_id
		  FROM tracker_entry WHERE external_ref = $1`, externalRef).
		Scan(&e.ExternalRef, &e.IssueID, &e.Project, &e.SourceType, &e.CreatedAt, &e.LastCommentTransitionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TrackerEntry{}, false, nil
		}
		return TrackerEntry{}, false, fmt.Errorf("db: tracker_entry get %s: %w", externalRef, err)
	}
	return e, true, nil
}

// Unfiled lists recent alert-sourced incidents with no entry ticket yet — the creator's work
// list. DISTINCT ON keeps one row per incident (a re-fired alert appends more ingest rows; the
// FIRST arrival is the filing's content). The window bound keeps the scan cheap and means TG
// never back-files ancient history when the feature first arms (config-not-code: the arming
// moment is the epoch).
func (s *TrackerEntryStore) Unfiled(ctx context.Context, window time.Duration, limit int) ([]UnfiledAlert, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.p.Query(ctx, `
		SELECT DISTINCT ON (a.external_ref)
		       a.external_ref, a.source_type, a.alert_rule, a.severity, a.host, a.site, a.summary, a.received_at
		  FROM ingest_alert a
		  LEFT JOIN tracker_entry t ON t.external_ref = a.external_ref
		 WHERE t.external_ref IS NULL
		   AND a.received_at > now() - make_interval(secs => $1)
		 ORDER BY a.external_ref, a.received_at ASC
		 LIMIT $2`, window.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("db: tracker_entry unfiled scan: %w", err)
	}
	defer rows.Close()
	var out []UnfiledAlert
	for rows.Next() {
		var u UnfiledAlert
		if err := rows.Scan(&u.ExternalRef, &u.SourceType, &u.AlertRule, &u.Severity, &u.Host, &u.Site, &u.Summary, &u.ReceivedAt); err != nil {
			return nil, fmt.Errorf("db: tracker_entry unfiled scan: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecoveriesToComment lists recovery transitions newer than their entry's comment cursor — the
// recovery-comment pass's work list. Only incidents WITH a filed ticket qualify (no ticket, no
// comment), and the cursor guarantees each transition is commented at most once.
func (s *TrackerEntryStore) RecoveriesToComment(ctx context.Context, limit int) ([]RecoveryToComment, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.p.Query(ctx, `
		SELECT t.external_ref, t.issue_id, tr.id, tr.host, tr.alert_rule, tr.observed_at, tr.received_at
		  FROM tracker_entry t
		  JOIN ingest_transition tr ON tr.external_ref = t.external_ref
		 WHERE t.issue_id <> '' AND tr.kind = 'recovery' AND tr.id > t.last_comment_transition_id
		 ORDER BY tr.id ASC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: tracker_entry recovery scan: %w", err)
	}
	defer rows.Close()
	var out []RecoveryToComment
	for rows.Next() {
		var r RecoveryToComment
		if err := rows.Scan(&r.ExternalRef, &r.IssueID, &r.TransitionID, &r.Host, &r.AlertRule, &r.ObservedAt, &r.ReceivedAt); err != nil {
			return nil, fmt.Errorf("db: tracker_entry recovery scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkCommented advances the comment cursor MONOTONICALLY (a stale/duplicate worker can never
// move it backwards and resurrect an already-commented recovery).
func (s *TrackerEntryStore) MarkCommented(ctx context.Context, externalRef string, transitionID int64) error {
	_, err := s.p.Exec(ctx, `
		UPDATE tracker_entry SET last_comment_transition_id = $2
		 WHERE external_ref = $1 AND last_comment_transition_id < $2`, externalRef, transitionID)
	if err != nil {
		return fmt.Errorf("db: tracker_entry mark-commented %s@%d: %w", externalRef, transitionID, err)
	}
	return nil
}
