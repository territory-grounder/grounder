package httpapi

import (
	"context"
	"net/http"

	"github.com/territory-grounder/grounder/core/auth"
)

// The benchmark-axis scoreboard read surface (TG-480, epic TG-187): the SAME axis.Scorecard the axisscore
// CLI prints and the -json artifact carries, served authenticated so the operator console can render the
// A1–A8 + G5/G6 aggregates without a CLI or Prometheus. Read-only, computed off the durable tables; the
// coverage gaps ride along (axes_not_live_measurable) so an axis whose enabling event never occurred
// renders "unmeasured", never a fabricated 0 — the same honesty the CLI enforces.
//
// The seam carries the scorecard ALREADY SERIALIZED. Deliberate: core/axis imports core/db, and core/db
// imports THIS package (the ingest TransitionRecorder seam), so httpapi typing axis.Scorecard directly
// would be an import cycle. The grounder-side reader marshals the one authoritative struct; this layer
// passes its bytes through untouched — one authority, no second shape to drift (INV-15's spirit).

// AxesReader computes the live axis scorecard and returns it serialized (the axis.Scorecard JSON, exactly
// what `axisscore -json` emits). nil ⇒ the route 503s.
type AxesReader interface {
	Axes(ctx context.Context) ([]byte, error)
}

// axesHandler is GET /v1/axes.
func (d Deps) axesHandler(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	if d.Axes == nil {
		http.Error(w, "axis scoreboard not deployed", http.StatusServiceUnavailable)
		return
	}
	sc, err := d.Axes.Axes(r.Context())
	if err != nil {
		// The aggregate could not be computed — say so rather than serving a half-scorecard that reads as
		// a measurement. (G5/G6 unavailability is already non-fatal INSIDE the reader, mirroring the CLI.)
		http.Error(w, "axis aggregate unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(sc)
}

// MemAxesReader is the in-memory AxesReader twin for the CI oracles.
type MemAxesReader struct {
	Body []byte
	Err  error
}

// Axes implements AxesReader.
func (m *MemAxesReader) Axes(context.Context) ([]byte, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Body, nil
}
