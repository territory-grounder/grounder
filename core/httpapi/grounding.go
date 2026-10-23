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
	// SignalRatio is AvgRealTP / AvgControlTP — >1 means the committed prediction beat the shuffled-graph
	// control. ★ IT USED TO FLOOR THE DENOMINATOR AT 1, WHICH IS AN INTEGER-DOMAIN RULE APPLIED TO A MEAN.
	// core/predict.Ratio() floors a per-prediction COUNT at 1, where 0 and 1 are genuinely adjacent and the
	// floor only avoids a divide-by-zero. These are averages, and they live in (0,1): measured live on
	// 321 scored predictions, AvgRealTP=0.4019 and AvgControlTP=0.2305 — a real signal 1.744x the control —
	// and the floor published 0.4019/1 = 0.40, which the console then captioned "at or below chance".
	// The floor did not guard an edge case; it rewrote every result in the range and INVERTED the verdict on
	// TG's own differentiator. A true zero control is handled by ControlSilent below, not by inventing a 1.
	SignalRatio float64 `json:"signal_ratio"`
	// ControlSilent is true when the shuffled-graph control scored ZERO true-positives across the whole
	// population while real predictions did not. The ratio is then unbounded, and no finite number is honest:
	// the surface must say "the control never hit", never print a manufactured figure.
	ControlSilent bool    `json:"control_silent"`
	Precision     float64 `json:"precision"` // sum(tp)/sum(tp+fp)
	Recall        float64 `json:"recall"`    // sum(tp)/sum(tp+fn)
	// AvgFalsePositives is the mean blast-radius FALSE POSITIVES per scored prediction (sum(fp)/predictions).
	// It is the honest view of over-prediction that Precision cannot express: a correctly-restrained prediction
	// (n_pred=0, fp=0) is a true-negative that leaves precision unchanged but DROPS this rate — so the
	// sibling-gate driving it toward 0 becomes visible, and it self-heals as calibrated predictions accumulate.
	AvgFalsePositives float64 `json:"avg_false_positives"`
	// ★ THE ROLLING VIEW (TG-92). Everything above is ALL-TIME, and an all-time calibration metric cannot
	// report a fix. The TG-61 blast-radius defect left ~26 rows summing tp=1, fp=730 — one leaf guest's
	// local fault predicting ~130 co-hosted siblings — so the all-time SignalRatio keeps describing the
	// CURRENT predictor as badly calibrated until 730+ good predictions outweigh the history. A number
	// that cannot say "fixed" is not a readiness signal.
	//
	// These are the same derivations over the most recent RecentWindow scored predictions. Read
	// RecentSignalRatio for "is the model calibrated NOW"; read SignalRatio for the permanent record. The
	// all-time figures are deliberately unchanged: a calibration fix must not rewrite the audit trail.
	RecentPredictions  int     `json:"recent_predictions"`
	RecentWindow       int     `json:"recent_window"`
	RecentAvgRealTP    float64 `json:"recent_avg_real_tp"`
	RecentAvgControlTP float64 `json:"recent_avg_control_tp"`
	RecentSignalRatio  float64 `json:"recent_signal_ratio"`
	// RecentControlSilent is the rolling twin of ControlSilent: the shuffled control scored zero across the
	// window while real predictions did not, so no finite ratio is honest.
	RecentControlSilent     bool           `json:"recent_control_silent"`
	RecentAvgFalsePositives float64        `json:"recent_avg_false_positives"`
	Bands                   map[string]int `json:"bands"`       // AUTO / AUTO_NOTICE / POLL_PAUSE session counts
	FloorHolds              int            `json:"floor_holds"` // POLL_PAUSE sessions (human required / never-auto floor)
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
