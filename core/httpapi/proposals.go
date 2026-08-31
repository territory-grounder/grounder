package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
)

// The open proposal plane's read surface (spec/026 REQ-2607): the proposals TG made and did NOT carry
// out. Read-only by construction — this file renders records; it exposes no verb, no approval, no
// actuation control of any kind. An empty spine reports an empty list, never fabricated rows (INV-15).
//
// "Shadow" here means NEVER EXECUTED, not one particular outcome string. The rows were previously selected
// by `outcome = 'proposed:shadow'`, which is the narrow branch where TG had no registered op-class at all
// — one row in a store of 3,202 — so this surface rendered a single proposal over 1,484 real ones. See
// db.shadowProposalWhere for the corrected predicate and the production measurement behind it.

// ShadowProposal is one rendered shadow proposal. Every field is persisted, screened data.
type ShadowProposal struct {
	ExternalRef string    `json:"external_ref"`
	Host        string    `json:"host"`
	AlertRule   string    `json:"alert_rule"`
	Op          string    `json:"op"`
	OpClass     string    `json:"op_class"`
	// Band is why this proposal stopped where it did; the console turns it into the shape's honest
	// "why it can't" instead of asserting "no registered op-class" over rows that have one.
	Band        string    `json:"band"`
	Rationale   string    `json:"rationale"`
	UndoSketch  string    `json:"undo_sketch"`
	Confidence  float64   `json:"confidence"`
	Attribution string    `json:"attribution"`
	CreatedAt   time.Time `json:"created_at"`
	// TG-307: the derived diagnosis SIGNAL an operator needs at the moment of proposal review — whether the
	// agent bound a typed claim, whether it cited GROUNDED evidence AGAINST its own root cause (the recorded
	// A2 failure this whole type exists to surface), and how many assertions it could not ground. The SIGNAL
	// ONLY: the claim's text (root cause, mechanism, the screened evidence quotes) is a detail of the walk and
	// stays behind the elevated AuthTraceRead gate on /v1/sessions/{ref}/diagnosis — this lane is AuthReadOnly.
	// The console renders the signal here and deep-links to that gated surface for the evidence itself.
	DiagnosisRecorded     bool `json:"diagnosis_recorded"`
	DiagnosisContradicted bool `json:"diagnosis_contradicted"`
	DiagnosisUncited      int  `json:"diagnosis_uncited"`
}

// ProposalsView is the handler's response envelope: the rows plus the honest total (the console badge
// renders the REAL count, never a made-up one).
type ProposalsView struct {
	Proposals []ShadowProposal `json:"proposals"`
	Total     int              `json:"total"`
	// Counterfactual is the headline an operator can act on: over a window, how many incidents TG saw and
	// for how many it had an answer it was not allowed to apply. Absent (nil) when the store cannot
	// answer — a headline that silently renders 0 of 0 would read as "TG did nothing" rather than
	// "nobody asked", and those are opposite facts.
	Counterfactual *Counterfactual `json:"counterfactual,omitempty"`
}

// Counterfactual is the shadow plane's one legible number. The denominator counts EVERY triage session
// in the window, including the ones TG correctly stood down on: excluding them would inflate the ratio
// into a slogan, and the number is only worth printing if the denominator is real.
// Executed is the subset of Addressed that TG was actually ALLOWED to carry out (mutation is on in this
// deployment). It is reported because "would have addressed N" silently mixes what TG did with what it was
// blocked from doing, and an operator weighing a new capability needs to know which part is which — part
// of the answer is already granted.
type Counterfactual struct {
	WindowDays int `json:"window_days"`
	Incidents  int `json:"incidents"`
	Addressed  int `json:"addressed"`
	Executed   int `json:"executed"`
}

// ProposalsReader lists the newest shadow proposals for the authenticated principal, together with the
// REAL total count of shadow proposals in the store. The total is what the console badge renders — it must
// stay honest past the page limit (a Total of len(page) silently becomes "page size" once the store holds
// more rows than the limit, which is exactly the fabrication INV-15 bans).
// CounterfactualReader is the optional seam behind the headline. Kept separate from ProposalsReader so a
// deployment whose store cannot answer it still serves the list — the headline degrades, the surface does
// not.
type CounterfactualReader interface {
	Counterfactual(ctx context.Context, p auth.Principal, window time.Duration) (incidents, addressed, executed int, err error)
}

type ProposalsReader interface {
	ShadowProposals(ctx context.Context, p auth.Principal, limit int) (rows []ShadowProposal, total int, err error)
}

// proposalsHandler serves GET /v1/proposals. Nil reader = 503 fail-closed (the console's liveGet renders
// the unavailable state honestly rather than an empty-but-plausible list).
func (d Deps) proposalsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Proposals == nil {
		http.Error(w, "proposals unavailable", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, total, err := d.Proposals.ShadowProposals(r.Context(), p, limit)
	if err != nil {
		http.Error(w, "proposals unavailable", http.StatusServiceUnavailable)
		return
	}
	if rows == nil {
		rows = []ShadowProposal{}
	}
	if total < len(rows) {
		total = len(rows) // a reader must never under-count what it just returned
	}
	view := ProposalsView{Proposals: rows, Total: total}
	// The headline is BEST-EFFORT and absent on failure. A shadow-plane number that silently renders
	// "0 of 0" on a store error would read as "TG did nothing this week" — the opposite of the truth, and
	// exactly the kind of confident-but-wrong figure this surface exists to replace.
	if d.Counterfactual != nil {
		if inc, addr, exec, cerr := d.Counterfactual.Counterfactual(r.Context(), p, counterfactualWindow); cerr == nil {
			view.Counterfactual = &Counterfactual{
				WindowDays: int(counterfactualWindow / (24 * time.Hour)),
				Incidents:  inc,
				Addressed:  addr,
				Executed:   exec,
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(view)
}

// counterfactualWindow is the headline's reporting period: "this week". Long enough that a quiet day
// does not read as a dead system, short enough that an operator recognises the incidents being counted.
const counterfactualWindow = 7 * 24 * time.Hour
