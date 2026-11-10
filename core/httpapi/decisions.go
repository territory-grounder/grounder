package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// The pending-decisions read surface (spec/006 REQ-519): the POLL_PAUSE decisions currently awaiting a
// human vote, so the console can LIST them and an operator can act (the vote itself is REQ-518/INV-12).
//
// It is a pure read over the projection the Runner writes when it enters POLL_PAUSE. It computes NO
// band/verdict/floor and can release NOTHING — releasing an action goes through POST /v1/vote → the
// waiting workflow, which accepts a vote only when action_id names its sealed action (INV-12), so a stale
// projection row is harmless. caller_can_act is SERVER-computed: only an authenticated operator session
// can reach the vote lane, so a machine principal sees the queue read-only (never a fabricated grant,
// INV-15).

// PlanView carries the candidate approaches the agent proposed for this decision.
type PlanView struct {
	Approaches []string `json:"approaches"`
}

// PendingDecisionView is one open decision as the console renders it. decision_id equals external_ref (the
// correlation key the vote binds); action_id is the sealed action the poll presented.
type PendingDecisionView struct {
	DecisionID   string    `json:"decision_id"`
	ExternalRef  string    `json:"external_ref"`
	ActionID     string    `json:"action_id"`
	Band         string    `json:"band"`
	Plan         PlanView  `json:"plan"`
	Prediction   string    `json:"prediction"`
	Reversible   bool      `json:"reversible"`
	Site         string    `json:"site,omitempty"`
	OpenedAt     time.Time `json:"opened_at"`
	CallerCanAct bool      `json:"caller_can_act"`
}

// DecisionsPage is the read-only approvals view the console renders, oldest first.
type DecisionsPage struct {
	Decisions []PendingDecisionView `json:"decisions"`
}

// decisionsHandler serves GET /v1/decisions. Nil reader = 503 fail-closed, never fabricated rows.
func (d Deps) decisionsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.PendingDecisions == nil {
		http.Error(w, "pending decisions unavailable", http.StatusServiceUnavailable)
		return
	}
	rows, err := d.PendingDecisions.OpenDecisions(r.Context())
	if err != nil {
		http.Error(w, "pending decisions unavailable", http.StatusServiceUnavailable)
		return
	}
	// Only an authenticated operator session can reach the vote lane (POST /v1/vote is AuthSession). A
	// machine principal sees the queue read-only — the control is never offered where it cannot be used.
	canAct := strings.HasPrefix(p.SourceID, "operator:")
	views := make([]PendingDecisionView, 0, len(rows))
	for _, row := range rows {
		app := row.Approaches
		if app == nil {
			app = []string{}
		}
		views = append(views, PendingDecisionView{
			DecisionID:   row.ExternalRef,
			ExternalRef:  row.ExternalRef,
			ActionID:     row.ActionID,
			Band:         row.Band,
			Plan:         PlanView{Approaches: app},
			Prediction:   row.Prediction,
			Reversible:   row.Reversible,
			Site:         row.Site,
			OpenedAt:     row.OpenedAt,
			CallerCanAct: canAct,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(DecisionsPage{Decisions: views})
}
