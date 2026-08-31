package httpapi

import (
	"context"
	"encoding/json"
	"net"
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
	// DeliveryPeer / DeliveryHost record HOW this alert reached TG (TG-372): the transport-level remote
	// address, and the Host header the caller addressed TG by. EMPTY IS MEANINGFUL — an in-process intake
	// (the pve-liveness poller) uses this same constructor and has no request, so '' reads as "did not
	// arrive over HTTP" rather than as unknown.
	//
	// Evidence, never identity. Nothing authenticates or routes on either, and DeliveryHost is
	// caller-controlled text — recorded because what a caller CLAIMS to have addressed is diagnostic, and
	// bounded at the database because caller-controlled text without a bound is a storage lever.
	DeliveryPeer string `json:"delivery_peer,omitempty"`
	DeliveryHost string `json:"delivery_host,omitempty"`
	// SubjectIP is the incident's subject when the source identified it by ADDRESS rather than by name
	// (IncidentEnvelope.IP). Empty when the subject was named, or not identified at all.
	//
	// Distinct from DeliveryPeer above, and the two are easy to confuse: DeliveryPeer is the hop that handed
	// TG the request, SubjectIP is what the alert is ABOUT. An alert about 10.0.2.193 can arrive from the
	// console proxy, and both facts are worth keeping.
	SubjectIP string `json:"subject_ip,omitempty"`
}

// WithDelivery stamps how the record arrived. It is a separate step from RecordFromEnvelope on purpose: the
// constructor is shared with an IN-PROCESS intake that has no http.Request, and threading a nil request
// through it would make "" ambiguous between "not over HTTP" and "the caller forgot".
//
// maxDeliveryHost mirrors the column CHECK. Truncating here rather than letting the INSERT fail keeps a
// hostile Host header from costing an accepted alert its record — the alert matters more than the label.
func (r AlertRecord) WithDelivery(peer, host string) AlertRecord {
	const maxDeliveryPeer, maxDeliveryHost = 100, 253
	if len(peer) > maxDeliveryPeer {
		peer = peer[:maxDeliveryPeer]
	}
	if len(host) > maxDeliveryHost {
		host = host[:maxDeliveryHost]
	}
	r.DeliveryPeer, r.DeliveryHost = peer, host
	return r
}

// AlertCounts reports the POPULATION behind a page, so a surface can never present its page size as a
// count. The console's alerts badge read "50" for every estate volume because the only number it had was
// len(page) and the page was fetched with limit=50; the live store held 1,553 accepted alerts and 549 in
// the last 24h. A badge pinned to the fetch limit tells an operator nothing about how much is happening.
type AlertCounts struct {
	Total   int `json:"total"`    // every accepted alert this store holds
	Last24h int `json:"last_24h"` // accepted within the last 24 hours
}

// AlertLog records accepted envelopes and serves the recent tail. Append never blocks ingest on a
// full log (bounded, oldest evicted); Recent returns newest first. Counts reports the population the
// page was drawn from — it is a separate read because Recent is bounded by construction.
type AlertLog interface {
	Append(ctx context.Context, rec AlertRecord)
	Recent(ctx context.Context, p auth.Principal, limit int) ([]AlertRecord, error)
	Counts(ctx context.Context, p auth.Principal) (AlertCounts, error)
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

// Counts reports this bounded log's population. Total is what the log actually HOLDS, not what the estate
// has emitted: MemAlertLog evicts beyond capacity, so it is honest about being a since-boot window and must
// never be read as an all-time total.
func (l *MemAlertLog) Counts(_ context.Context, _ auth.Principal) (AlertCounts, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cut := time.Now().Add(-24 * time.Hour)
	c := AlertCounts{Total: len(l.rows)}
	for _, r := range l.rows {
		if r.ReceivedAt.After(cut) {
			c.Last24h++
		}
	}
	return c, nil
}

// RecordFromEnvelope maps a normalized envelope to its log record — a projection, never an inference.
//
// EXPORTED so that a caller which mints a triage WITHOUT passing through the HTTP front door records the
// acceptance in exactly the same shape. The pve-liveness poller is such a caller: it detects a stopped guest
// by polling Proxmox and starts the workflow directly, so before this it never produced an ingest_alert row
// — and A1 (detection recall) correlates injected faults against ingest_alert. TG's FASTEST detector
// (~39s versus the ~6-11min LibreNMS push) therefore scored ZERO on the metric it exists to raise.
//
// Two constructors for one record would drift, silently, in whichever direction the reader is not looking —
// the same failure this file's own detectRuleMatch comment records. One function, both callers.
func RecordFromEnvelope(sourceType string, env ingest.IncidentEnvelope, workflowID string) AlertRecord {
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
		// env.IP had NO consumer before TG-373: four ingest modules populate it, core/ingest validates it
		// into the envelope, and it was dropped here. 40 of the 48 host-less Alertmanager incidents carry
		// their only identifier in this field.
		SubjectIP: ipString(env.IP),
	}
}

// ipString renders an envelope IP for storage, and returns "" for the nil (no address supplied) case rather
// than net.IP(nil).String()'s "<nil>" — which would store the four characters "<nil>" in every row whose
// subject was named, and pass a NOT NULL check while meaning the opposite of what it says.
func ipString(ip net.IP) string {
	if len(ip) == 0 {
		return ""
	}
	return ip.String()
}

// alertsPageLimit bounds a single read; the console pages the recent tail.
const alertsPageLimit = 200

// AlertsPage is the read-only alerts view the console renders, newest first. Counts carries the
// population the page was drawn from so a surface never has to infer volume from len(Alerts) — which is
// the fetch limit, not a measurement.
type AlertsPage struct {
	Alerts []AlertRecord `json:"alerts"`
	Counts AlertCounts   `json:"counts"`
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
	// Fail closed on the counts too. Serving the page with a zeroed Counts would hand the console a
	// confident "0 alerts" for a store that simply could not be counted — the console renders the badge
	// from this, so a silent zero is worse than an unavailable panel.
	counts, err := d.Alerts.Counts(r.Context(), p)
	if err != nil {
		http.Error(w, "alerts unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(AlertsPage{Alerts: rows, Counts: counts})
}
