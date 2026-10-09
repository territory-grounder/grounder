package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// The sessions read surface (spec/006 REQ-509): the console's list of triage sessions served from the
// AUDIT SPINE — session_risk_audit joined with the mechanical verdict — never from anything the model
// asserted about itself. A session summary is the classifier's committed record (band, risk level,
// content-hashed action) plus the deterministic verifier's verdict when one exists; a session with no
// verdict reports none (the console renders "pending", never a fabricated outcome — INV-15).

// sessionsPageLimit bounds a single read; the console pages the recent tail.
const sessionsPageLimit = 200

// SessionSummary is one triage session as the audit spine recorded it.
type SessionSummary struct {
	ExternalRef      string            `json:"external_ref"`
	// Host is the entity the incident was about. Absent from this DTO until 2026-08-01, which left the
	// knowledge view filtering per host on a field it could never see — every host page read "no incidents
	// recorded against this entity" while the spine held 3,202 sessions across 78 hosts.
	Host             string            `json:"host"`
	Band             string            `json:"band"`
	RiskLevel        string            `json:"risk_level"`
	ActionID         string            `json:"action_id"`
	PlanHash         string            `json:"plan_hash,omitempty"`
	AutoApproved     bool              `json:"auto_approved"`
	NotifyRequired   bool              `json:"notify_required"`
	OperatorOverride bool              `json:"operator_override"`
	Signals          map[string]string `json:"signals,omitempty"`
	// Verdict is the deterministic verifier's outcome for the bound action: match | partial |
	// deviation, or empty when no verdict exists yet. Never authored here (INV-10).
	Verdict      string    `json:"verdict,omitempty"`
	ClassifiedAt time.Time `json:"classified_at"`
}

// SessionsReader returns the most recent session summaries the principal is authorized to see.
// Authority is resolved inside the implementation against the principal (INV-12).
type SessionsReader interface {
	RecentSessions(ctx context.Context, p auth.Principal, limit int) ([]SessionSummary, error)
	// SessionCount is the POPULATION behind the bounded page. Without it a surface fed len(page) reports
	// its own fetch limit: the Knowledge model composes its pages from this read at limit=50 while the
	// spine holds 1,306 sessions, so its badge counted the page, not the estate.
	SessionCount(ctx context.Context, p auth.Principal) (int, error)
}

// SessionsPage is the read-only sessions view the console renders, newest first.
type SessionsPage struct {
	Sessions []SessionSummary `json:"sessions"`
	// Total is every session on the spine, not the number returned. A surface that must say "showing N of
	// M" — or that derives a count from this read — needs the M, and inferring it from len(Sessions)
	// reports the fetch limit. Same defect the alerts badge carried at a constant 50 (!665).
	Total int `json:"total"`
}

// sessionsHandler serves GET /v1/sessions?limit=N. It runs only after core/auth authenticated the
// caller; a nil reader (no durable spine wired) fails closed to 503 rather than fabricating rows.
func (d Deps) sessionsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.SessionsRead == nil {
		http.Error(w, "sessions unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > sessionsPageLimit {
		limit = sessionsPageLimit
	}
	rows, err := d.SessionsRead.RecentSessions(r.Context(), p, limit)
	if err != nil {
		http.Error(w, "sessions unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []SessionSummary{} // an empty spine is an empty list, not null
	}
	w.Header().Set("Content-Type", "application/json")
	// Fail closed on the count as well: serving a zero would tell a surface the spine is empty when the
	// truth is that it could not be counted, and the Knowledge model would compose from "no incidents".
	total, err := d.SessionsRead.SessionCount(r.Context(), p)
	if err != nil {
		http.Error(w, "sessions unavailable", http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(SessionsPage{Sessions: rows, Total: total})
}
