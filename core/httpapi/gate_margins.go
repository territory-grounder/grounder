package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// The gate-decision BOUNDARY-CASE surface (TG-178): every interceptor gate that passed or refused within ε of
// its numeric threshold — the reviewable queue the ticket asks for, and the skill-store flywheel's preferential
// input. It reads the signed margins recorded on the observe-only gate-verdict trail (interceptor_gate_verdict,
// migration 0076); a gate with no numeric threshold (a binary match/floor) carries no margin and never appears
// here. Read-only, and elevated to AuthTraceRead like the decision-tracer walk it belongs to — the same
// governance data, so the same gate.

// GateBoundaryCase is one gate decision that landed within ε of its threshold, projected NON-SECRET (no argv,
// host, or credential — the trail carries none, INV-13). Margin is signed: negative ⇒ the value sat on the
// refusing side by |Margin|, positive ⇒ it cleared by Margin.
type GateBoundaryCase struct {
	ActionID    string  `json:"action_id"`
	ExternalRef string  `json:"external_ref,omitempty"`
	Ordinal     int     `json:"ordinal"`
	Gate        string  `json:"gate"`
	Verdict     string  `json:"verdict"`
	Reason      string  `json:"reason,omitempty"`
	Margin      float64 `json:"margin"`
}

// GateMarginReader serves the boundary-case set: the gate-verdict rows whose recorded margin is within ε.
type GateMarginReader interface {
	GateVerdictsWithinEpsilon(ctx context.Context, eps float64) ([]trace.GateVerdict, error)
}

// GateBoundaryPage is the read-only within-ε view, echoing the ε it was computed at so the caller reads the
// threshold the set was selected on rather than assuming the default.
type GateBoundaryPage struct {
	Epsilon float64            `json:"epsilon"`
	Cases   []GateBoundaryCase `json:"cases"`
}

const (
	// defaultGateMarginEpsilon is the review band used when the caller names none — a decision within 0.05 of
	// its threshold is close enough to be worth a human's eye without flooding the queue.
	defaultGateMarginEpsilon = 0.05
	// maxGateMarginEpsilon caps the requestable band: a margin is normalised to a [0,1]-scale threshold gap
	// (confidence − min_confidence lives there), so an ε above 1.0 would return everything, which is not a
	// boundary query. Requests above the cap are clamped, not refused.
	maxGateMarginEpsilon = 1.0
)

// gateMarginsHandler serves GET /v1/gates/within-epsilon?eps=E. A nil reader is 503 — the console then holds an
// honest empty state rather than implying there are no boundary cases when the surface is simply not wired.
func (d Deps) gateMarginsHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.GateMargins == nil {
		http.Error(w, "gate margins unavailable", http.StatusServiceUnavailable)
		return
	}
	// The loadable default (TG-178: ε is configuration, not a compiled constant): a configured
	// GateMarginEpsilon in the valid band overrides the compiled 0.05; an unset/out-of-range config keeps the
	// compiled default. An explicit eps= query parameter still wins over this default below.
	eps := defaultGateMarginEpsilon
	if d.GateMarginEpsilon > 0 && d.GateMarginEpsilon <= maxGateMarginEpsilon {
		eps = d.GateMarginEpsilon
	}
	if v := r.URL.Query().Get("eps"); v != "" {
		// A malformed or non-positive ε falls back to the default rather than 400 — the query is a review
		// convenience, not a contract the caller must satisfy exactly; NaN/Inf are rejected the same way.
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 && !math.IsInf(n, 0) {
			eps = n
		}
	}
	if eps > maxGateMarginEpsilon {
		eps = maxGateMarginEpsilon
	}
	rows, err := d.GateMargins.GateVerdictsWithinEpsilon(r.Context(), eps)
	if err != nil {
		http.Error(w, "gate margins unavailable", http.StatusServiceUnavailable)
		return
	}
	cases := make([]GateBoundaryCase, 0, len(rows))
	for _, g := range rows {
		if g.Margin == nil {
			continue // defence-in-depth: the reader already excludes NULL margins; never surface a nil as 0.0
		}
		cases = append(cases, GateBoundaryCase{
			ActionID: g.ActionID, ExternalRef: g.ExternalRef, Ordinal: g.Ordinal,
			Gate: g.Gate, Verdict: g.Verdict, Reason: g.Reason, Margin: *g.Margin,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(GateBoundaryPage{Epsilon: eps, Cases: cases})
}
