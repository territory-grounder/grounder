package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// The Actions read surface: every sealed ActionManifest as the governed walk it actually took.
//
// WHY THIS EXISTS. The console's Actions view rendered five INVENTED incidents attached to REAL estate
// hostnames — "Repeated auth failures -> ASA dc1fw01", "NAS volume 88% -> cleanup dc2nas01",
// "Disk pressure eviction cascade dc1k8s-w3" — disclaimed only by a low-contrast corner chip, while 109
// genuine manifests sat unread in action_manifest. Fabricated incidents on real devices are worse than an
// empty panel: an operator who believes one investigates a machine that is fine, and an operator who learns
// the panel invents things stops trusting the ones that do not.

// ActionRibbon is one sealed ActionManifest and the stages it has actually reached. Every field is a
// projection of a stored fact; the five stage booleans are each grounded in a row that exists rather than in
// a status guess.
type ActionRibbon struct {
	ActionID string `json:"action_id"`
	PlanHash string `json:"plan_hash,omitempty"`

	// The sealed manifest (INV-07): what TG bound itself to before it was allowed to act.
	Op         string            `json:"op"`
	OpClass    string            `json:"op_class"`
	Target     string            `json:"target"`
	Reversible bool              `json:"reversible"`
	Params     map[string]string `json:"params,omitempty"`

	// The governed walk. Absence is absence: an empty Verdict means "not yet scored", never "match".
	Band           string `json:"band"`
	Verdict        string `json:"verdict,omitempty"`
	ApprovalChoice string `json:"approval_choice,omitempty"`

	// TG-532 — WHOSE resolution these two labels are. action_id is content-addressed over the operation
	// SHAPE, and the manifest is sealed first-wins, so one row can be the identity of MANY sessions (69
	// shapes on this deployment are shared; the worst by 198 sessions). The labels above are per-SESSION
	// facts on a per-SHAPE row, so publishing them bare invited exactly one misreading: that the session
	// in front of the reader is the one that was approved and verified. These name the owner instead.
	// Empty = the label predates migration 0110 (owner unrecoverable) or is unset.
	ApprovalRef string `json:"approval_ref,omitempty"`
	VerdictRef  string `json:"verdict_ref,omitempty"`
	// SessionsSharing is how many DISTINCT sessions proposed this same shape. 1 (or 0 when unknown) means
	// the labels can only be this session's; >1 means the ribbon is a SHAPE's history, not a session's.
	SessionsSharing int `json:"sessions_sharing,omitempty"`

	// RiskLevel is CATEGORICAL on purpose, and is published as the label it is. core/risk/classifier.go is
	// an ordered ladder with early return — jailbreak, then the non-configurable never-auto floor, then
	// no-prediction/deviation/novelty, and so on — so the band is decided by WHICH rule fired first, not by
	// any accumulated quantity. No score is computed and then bucketed, so none is being discarded here.
	// Rendering it as a decimal would require inventing weights across veto conditions that are not
	// commensurable ("an unattributable actor touched this host" is not N points worse than "this op-class
	// has not graduated"), and a scalar invites the threshold tuning that deny-overrides exists to prevent.
	RiskLevel string `json:"risk_level,omitempty"`

	// Confidence IS a genuine scalar and is published as one: it is a prediction about ONE thing (is this
	// diagnosis right), which is why it is calibratable at all — the Brier/ECE work scores exactly this.
	// HasConfidence distinguishes "no confidence was recorded" from a real 0.0; without it an unrecorded
	// value would render as total no-confidence, which is a statement the data does not make.
	Confidence    float64 `json:"confidence,omitempty"`
	HasConfidence bool    `json:"has_confidence"`

	Classified bool `json:"classified"`
	Predicted  bool `json:"predicted"`
	Approved   bool `json:"approved"`
	Executed   bool `json:"executed"`
	Verified   bool `json:"verified"`

	SealedAt time.Time `json:"sealed_at"`
}

// ActionCounts is the population behind the page — never the page size (the defect that pinned the alerts
// badge at its fetch limit).
type ActionCounts struct {
	Total      int `json:"total"`
	Verified   int `json:"verified"`
	Deviations int `json:"deviations"`
}

// ActionManifestReader serves the sealed-manifest tail and its population.
type ActionManifestReader interface {
	Recent(ctx context.Context, p auth.Principal, limit int) ([]ActionRibbon, error)
	Counts(ctx context.Context, p auth.Principal) (ActionCounts, error)
}

// ActionsPage is the read-only Actions view, newest first.
type ActionsPage struct {
	Actions []ActionRibbon `json:"actions"`
	Counts  ActionCounts   `json:"counts"`
}

// actionsPageLimit bounds a single read.
const actionsPageLimit = 200

// actionsHandler serves GET /v1/actions?limit=N. A nil reader is 503 — the console then holds its empty
// state rather than falling back to the fixtures this endpoint exists to retire (INV-15).
func (d Deps) actionsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Actions == nil {
		http.Error(w, "actions unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > actionsPageLimit {
		limit = actionsPageLimit
	}
	rows, err := d.Actions.Recent(r.Context(), p, limit)
	if err != nil {
		http.Error(w, "actions unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []ActionRibbon{}
	}
	counts, err := d.Actions.Counts(r.Context(), p)
	if err != nil {
		http.Error(w, "actions unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ActionsPage{Actions: rows, Counts: counts})
}
