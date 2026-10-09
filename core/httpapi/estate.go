package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
)

// The estate read surface (spec/006 REQ-516): the causal estate graph the worker builds and the
// prediction gate reasons over, PUBLISHED for the operator console's Estate view. The grounder does
// not build the graph — the worker does, then persists a snapshot; this surface serves the latest one
// verbatim. It reports available=false when no snapshot exists yet (the console renders "no snapshot
// published", never a fabricated topology — INV-15).

// EstateNode is one entity in the graph (identity = type + name).
type EstateNode struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// EstateEdge is one directed dependency: From depends-on To, with the winning confidence and provenance.
type EstateEdge struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Rel        string  `json:"rel"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
}

// EstateSnapshot is the read-only estate view the console renders.
type EstateSnapshot struct {
	Available   bool         `json:"available"`
	CapturedAt  string       `json:"captured_at,omitempty"`
	NodeCount   int          `json:"node_count"`
	EdgeCount   int          `json:"edge_count"`
	SourceCount int          `json:"source_count"`
	Nodes       []EstateNode `json:"nodes"`
	Edges       []EstateEdge `json:"edges"`
}

// EstateReader returns the latest published estate snapshot for the authenticated principal.
type EstateReader interface {
	LatestEstate(ctx context.Context, p auth.Principal) (EstateSnapshot, error)
}

// estateHandler serves GET /v1/estate. Nil reader = 503; no snapshot yet = 200 with available=false.
func (d Deps) estateHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Estate == nil {
		http.Error(w, "estate unavailable", http.StatusServiceUnavailable)
		return
	}
	snap, err := d.Estate.LatestEstate(r.Context(), p)
	if err != nil {
		http.Error(w, "estate unavailable", http.StatusServiceUnavailable)
		return
	}
	if snap.Nodes == nil {
		snap.Nodes = []EstateNode{}
	}
	if snap.Edges == nil {
		snap.Edges = []EstateEdge{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}
