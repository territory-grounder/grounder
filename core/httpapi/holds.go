package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/holds"
)

// The open-holds read surface (TG-667, deep-review F5): the incidents TG decided NOT to touch and still
// remembers, and the investigations that failed and were never retried. Until this existed an autonomous
// HOLD was said once (one ledger row, one chat notice) and never shown again; the approvals list, the
// escalation queue and every alert rule excluded it, so an actionable incident TG had declined to heal could
// sit open indefinitely with nothing pointing at it.
//
// A pure read over the hold_decision projection and session_triage. It computes NO band and can release
// NOTHING — there is no operator verb here, because the owner's posture is that TG is the fallback approver
// and prod never votes (TG-558/559); an operator who disagrees with a hold changes the estate or the ruleset,
// not this list. Nil reader = 503 fail-closed, never fabricated rows.

// FailedInvestigationLookback bounds how far back the failed-investigation list reaches.
const FailedInvestigationLookback = 7 * 24 * time.Hour

// HoldsReader is the read seam the handler depends on (a *db.HoldStore in production, a fake in tests).
type HoldsReader interface {
	OpenHolds(ctx context.Context, now time.Time) ([]holds.HoldDecision, error)
	FailedInvestigationsUnretried(ctx context.Context, since time.Time) ([]holds.FailedInvestigation, error)
}

// HoldView is one open hold as the console renders it.
type HoldView struct {
	ExternalRef      string    `json:"external_ref"`
	Host             string    `json:"host"`
	AlertRule        string    `json:"alert_rule"`
	OpClass          string    `json:"op_class,omitempty"`
	ActionID         string    `json:"action_id,omitempty"`
	ReasonKey        string    `json:"reason_key"`
	Reading          string    `json:"reading"`
	Why              string    `json:"why"`
	ActorAttribution string    `json:"actor_attribution,omitempty"`
	DecidedBy        string    `json:"decided_by"`
	DecidedAt        time.Time `json:"decided_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Sticky           bool      `json:"sticky"`
	AgeSeconds       int64     `json:"age_seconds"`
}

// FailedInvestigationView is one unretried failed investigation.
type FailedInvestigationView struct {
	ExternalRef string    `json:"external_ref"`
	Host        string    `json:"host"`
	AlertRule   string    `json:"alert_rule"`
	StopReason  string    `json:"stop_reason"`
	FailedAt    time.Time `json:"failed_at"`
	AgeSeconds  int64     `json:"age_seconds"`
}

// HoldsPage is the read-only holds view: open holds oldest first, then unretried failed investigations.
type HoldsPage struct {
	Holds                []HoldView                `json:"holds"`
	FailedInvestigations []FailedInvestigationView `json:"failed_investigations"`
	FailedLookback       string                    `json:"failed_lookback"`
	AsOf                 time.Time                 `json:"as_of"`
}

// holdsHandler serves GET /v1/holds. Nil reader or a read error = 503; never a fabricated or partial page.
func (d Deps) holdsHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Holds == nil {
		http.Error(w, "holds unavailable", http.StatusServiceUnavailable)
		return
	}
	now := time.Now().UTC()
	open, err := d.Holds.OpenHolds(r.Context(), now)
	if err != nil {
		http.Error(w, "holds unavailable", http.StatusServiceUnavailable)
		return
	}
	failed, err := d.Holds.FailedInvestigationsUnretried(r.Context(), now.Add(-FailedInvestigationLookback))
	if err != nil {
		http.Error(w, "holds unavailable", http.StatusServiceUnavailable)
		return
	}
	page := HoldsPage{Holds: make([]HoldView, 0, len(open)), FailedInvestigations: make([]FailedInvestigationView, 0, len(failed)),
		FailedLookback: FailedInvestigationLookback.String(), AsOf: now}
	for _, h := range open {
		page.Holds = append(page.Holds, HoldView{
			ExternalRef: h.ExternalRef, Host: h.Host, AlertRule: h.AlertRule, OpClass: h.OpClass, ActionID: h.ActionID,
			ReasonKey: h.ReasonKey, Reading: holds.Reading(h.ReasonKey), Why: h.Why, ActorAttribution: h.ActorAttribution,
			DecidedBy: h.DecidedBy, DecidedAt: h.DecidedAt, ExpiresAt: h.ExpiresAt, Sticky: holds.Sticky(h.ReasonKey),
			AgeSeconds: int64(now.Sub(h.DecidedAt).Seconds()),
		})
	}
	for _, f := range failed {
		page.FailedInvestigations = append(page.FailedInvestigations, FailedInvestigationView{
			ExternalRef: f.ExternalRef, Host: f.Host, AlertRule: f.AlertRule, StopReason: f.StopReason, FailedAt: f.FailedAt,
			AgeSeconds: int64(now.Sub(f.FailedAt).Seconds()),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(page)
}
