package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/ingest"
)

// The alerts read surface (spec/006 REQ-510): what the alert front door ACTUALLY accepted — each
// normalized envelope the ingest handler admitted, recorded at the moment of acceptance. It is the
// ingest tier's own record (grammar-validated envelopes), never a re-statement by any other component
// (INV-15); a rejected payload is never logged as an alert because it never became an envelope.

// AlertRecord is one accepted, normalized alert as the front door admitted it.
type AlertRecord struct {
	ExternalRef string            `json:"external_ref"`
	SourceType  string            `json:"source_type"`
	SourceID    string            `json:"source_id"`
	AlertRule   string            `json:"alert_rule"`
	Severity    string            `json:"severity"`
	Host        string            `json:"host,omitempty"`
	Site        string            `json:"site,omitempty"`
	Summary     string            `json:"summary,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	ObservedAt  time.Time         `json:"observed_at"`
	ReceivedAt  time.Time         `json:"received_at"`
	WorkflowID  string            `json:"workflow_id,omitempty"` // the triage session minted for it, if any
}

// AlertLog records accepted envelopes and serves the recent tail. Append never blocks ingest on a
// full log (bounded, oldest evicted); Recent returns newest first.
type AlertLog interface {
	Append(ctx context.Context, rec AlertRecord)
	Recent(ctx context.Context, p auth.Principal, limit int) ([]AlertRecord, error)
}

// MemAlertLog is the bounded in-memory alert log: the CI oracle fake AND the Phase-1 store (the
// recent ingest window since boot — the console labels it exactly that; durability arrives with the
// pgx twin when the alert table lands).
type MemAlertLog struct {
	mu   sync.RWMutex
	cap  int
	rows []AlertRecord
}

// NewMemAlertLog builds a bounded log (capacity clamped to at least 1).
func NewMemAlertLog(capacity int) *MemAlertLog {
	if capacity < 1 {
		capacity = 1
	}
	return &MemAlertLog{cap: capacity}
}

// Append records an accepted alert, evicting the oldest beyond capacity.
func (l *MemAlertLog) Append(_ context.Context, rec AlertRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, rec)
	if len(l.rows) > l.cap {
		l.rows = l.rows[len(l.rows)-l.cap:]
	}
}

// Recent returns up to limit records, newest first.
func (l *MemAlertLog) Recent(_ context.Context, _ auth.Principal, limit int) ([]AlertRecord, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := len(l.rows)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]AlertRecord, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, l.rows[i])
	}
	return out, nil
}

// recordFromEnvelope maps a normalized envelope to its log record — a projection, never an inference.
func recordFromEnvelope(sourceType string, env ingest.IncidentEnvelope, workflowID string) AlertRecord {
	return AlertRecord{
		ExternalRef: env.ExternalRef,
		SourceType:  sourceType,
		SourceID:    env.SourceID,
		AlertRule:   env.AlertRule,
		Severity:    env.Severity.String(),
		Host:        env.Host,
		Site:        env.Site,
		Summary:     env.Summary,
		Labels:      env.Labels,
		ObservedAt:  env.ObservedAt,
		ReceivedAt:  env.ReceivedAt,
		WorkflowID:  workflowID,
	}
}

// alertsPageLimit bounds a single read; the console pages the recent tail.
const alertsPageLimit = 200

// AlertsPage is the read-only alerts view the console renders, newest first.
type AlertsPage struct {
	Alerts []AlertRecord `json:"alerts"`
}

// alertsHandler serves GET /v1/alerts?limit=N. Nil log = 503 fail-closed, never fabricated rows.
func (d Deps) alertsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Alerts == nil {
		http.Error(w, "alerts unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > alertsPageLimit {
		limit = alertsPageLimit
	}
	rows, err := d.Alerts.Recent(r.Context(), p, limit)
	if err != nil {
		http.Error(w, "alerts unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []AlertRecord{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(AlertsPage{Alerts: rows})
}
