package db

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// AlertLogStore is the pgx-backed DURABLE alert log (ingest_alert, migration 0033): the alert front door's own
// record of every accepted, normalized envelope, keyed by external_ref. It replaces the bounded in-memory
// MemAlertLog so the /v1/alerts view survives restart AND the decision-tracer can read the ingest boundary for
// any session. Append-only by table grant (the runtime role holds no UPDATE/DELETE, REQ-2016). Bound SELECTs
// only, every parameter bound ($1) — never string-built.
type AlertLogStore struct{ p *Pool }

// NewAlertLogStore returns the Postgres-backed durable alert log.
func NewAlertLogStore(p *Pool) *AlertLogStore { return &AlertLogStore{p: p} }

// compile-time proof it satisfies the read/write seam the ingest handler + alerts view depend on.
var _ httpapi.AlertLog = (*AlertLogStore)(nil)

// Append records one accepted, normalized alert. Best-effort by contract (AlertLog.Append returns no error —
// the ingest path must never block on the log); a write failure is logged, never propagated. INV-15: only an
// already-accepted envelope reaches here.
func (s *AlertLogStore) Append(ctx context.Context, rec httpapi.AlertRecord) {
	labels := rec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	lj, err := json.Marshal(labels)
	if err != nil {
		lj = []byte("{}")
	}
	var observed any // nullable — a zero ObservedAt writes SQL NULL, never a spurious epoch
	if !rec.ObservedAt.IsZero() {
		observed = rec.ObservedAt
	}
	// ON CONFLICT (external_ref) DO NOTHING: a re-delivered webhook for an already-admitted alert is a no-op —
	// the FIRST acceptance is the canonical front-door record. Keeps this append-only table idempotent so a
	// retrying/flapping source never accumulates unremovable duplicate rows (DELETE is revoked). DO NOTHING
	// needs only INSERT, never the revoked UPDATE.
	if _, err := s.p.Exec(ctx, `
		INSERT INTO ingest_alert
		  (external_ref, source_type, source_id, alert_rule, severity, host, site, summary, labels_json, observed_at, received_at, workflow_id, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, 1)
		ON CONFLICT (external_ref) DO NOTHING`,
		rec.ExternalRef, rec.SourceType, rec.SourceID, rec.AlertRule, rec.Severity, rec.Host, rec.Site,
		rec.Summary, string(lj), observed, rec.ReceivedAt, rec.WorkflowID); err != nil {
		log.Printf("db: ingest_alert append %s failed (non-blocking): %v", rec.ExternalRef, err)
	}
}

// Recent returns up to limit accepted alerts, newest first. Authority is enforced upstream at the
// authenticated route; this reads the committed log only.
func (s *AlertLogStore) Recent(ctx context.Context, _ auth.Principal, limit int) ([]httpapi.AlertRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, source_type, source_id, alert_rule, severity, host, site, summary,
		       COALESCE(labels_json, '{}'::jsonb), observed_at, received_at, workflow_id
		FROM ingest_alert
		ORDER BY received_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]httpapi.AlertRecord, 0, limit)
	for rows.Next() {
		rec, err := scanAlertRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// scanAlertRow scans one ingest_alert row into the non-secret AlertRecord projection (shared by Recent and the
// tracer's ByRef read). observed_at is nullable (pgx scans SQL NULL into a nil *time.Time); labels degrade to nil.
func scanAlertRow(row interface{ Scan(...any) error }) (httpapi.AlertRecord, error) {
	var (
		rec        httpapi.AlertRecord
		labelsJSON []byte
		observed   *time.Time
	)
	if err := row.Scan(&rec.ExternalRef, &rec.SourceType, &rec.SourceID, &rec.AlertRule, &rec.Severity,
		&rec.Host, &rec.Site, &rec.Summary, &labelsJSON, &observed, &rec.ReceivedAt, &rec.WorkflowID); err != nil {
		return rec, err
	}
	if observed != nil {
		rec.ObservedAt = *observed
	}
	if len(labelsJSON) > 0 {
		_ = json.Unmarshal(labelsJSON, &rec.Labels) // best-effort; a bad labels blob degrades to nil, not a failure
	}
	return rec, nil
}
