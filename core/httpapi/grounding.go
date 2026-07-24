package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
)

// The grounding scorecard (spec/006 REQ-517): the aggregate evidence that TG's core differentiator
// actually works, computed from the REAL prediction/verdict/audit tables — not asserted. It publishes
// the mechanical verifier's match/partial/deviation distribution, the falsifiability signal (did the
// committed predictions beat the degree-preserving shuffled-graph control), blast-radius
// precision/recall, and the autonomy-band distribution (how the never-auto floor shaped outcomes).
// Every number is a live aggregate; an empty spine reports zeros, never fabricated rates (INV-15).

// GroundingScorecard is the read-only evidence view the console renders.
type GroundingScorecard struct {
	Verdicts     map[string]int `json:"verdicts"` // match / partial / deviation counts
	VerdictTotal int            `json:"verdict_total"`
	MatchRate    float64        `json:"match_rate"`     // match / verdict_total (0 when none)
	Predictions  int            `json:"predictions"`    // scored predictions (verify-time scores present)
	AvgRealTP    float64        `json:"avg_real_tp"`    // avg real true-positive cascade hits
	AvgControlTP float64        `json:"avg_control_tp"` // avg shuffled-graph control true-positives
	SignalRatio  float64        `json:"signal_ratio"`   // AvgRealTP / max(AvgControlTP, epsilon) — >1 beats chance
	Precision    float64        `json:"precision"`      // sum(tp)/sum(tp+fp)
	Recall       float64        `json:"recall"`         // sum(tp)/sum(tp+fn)
	// AvgFalsePositives is the mean blast-radius FALSE POSITIVES per scored prediction (sum(fp)/predictions).
	// It is the honest view of over-prediction that Precision cannot express: a correctly-restrained prediction
	// (n_pred=0, fp=0) is a true-negative that leaves precision unchanged but DROPS this rate — so the
	// sibling-gate driving it toward 0 becomes visible, and it self-heals as calibrated predictions accumulate.
	AvgFalsePositives float64 `json:"avg_false_positives"`
	Bands        map[string]int `json:"bands"`          // AUTO / AUTO_NOTICE / POLL_PAUSE session counts
	FloorHolds   int            `json:"floor_holds"`    // POLL_PAUSE sessions (human required / never-auto floor)
}

// GroundingReader assembles the live scorecard for the authenticated principal.
type GroundingReader interface {
	Grounding(ctx context.Context, p auth.Principal) (GroundingScorecard, error)
}

// groundingHandler serves GET /v1/grounding. Nil reader = 503 fail-closed.
func (d Deps) groundingHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Grounding == nil {
		http.Error(w, "grounding unavailable", http.StatusServiceUnavailable)
		return
	}
	sc, err := d.Grounding.Grounding(r.Context(), p)
	if err != nil {
		http.Error(w, "grounding unavailable", http.StatusServiceUnavailable)
		return
	}
	if sc.Verdicts == nil {
		sc.Verdicts = map[string]int{}
	}
	if sc.Bands == nil {
		sc.Bands = map[string]int{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sc)
}
