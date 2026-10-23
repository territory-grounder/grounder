package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/axis"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/httpapi"
)

// axesWindow is the scoreboard's trailing window — the axisscore CLI's default, so the console and the
// CLI read the same period by default.
const axesWindow = 168 * time.Hour

// axesCacheTTL bounds how often the aggregate SQL re-runs: the scoreboard is a trailing-week rollup whose
// truth changes on the minutes scale, and every console login adopts the endpoint once — a short TTL keeps
// a burst of logins from re-running the heaviest read path in the tree while staying fresh enough that an
// operator watching a drill sees it move.
const axesCacheTTL = 60 * time.Second

// grounderAxes is the process-wide axis-scoreboard backend, set once at boot when a DB pool exists — a
// package var rather than a buildPublicAPI parameter, per the documented positional-rebind hazard on that
// signature. nil ⇒ the route 503s.
var grounderAxes httpapi.AxesReader

// axesReadStore computes the benchmark-axis scorecard for GET /v1/axes (TG-480) with the SAME reads and
// the SAME non-fatal G5/G6 discipline as cmd/axisscore: a falsifiability or loop-bypass read failure logs
// and serves the other axes rather than blanking the scoreboard (an operator taught that the tool refuses
// when one axis is unavailable learns to skip the tool).
type axesReadStore struct {
	store *db.AxisReadStore

	mu       sync.Mutex
	cached   []byte
	cachedAt time.Time
}

// Axes implements httpapi.AxesReader: the serialized axis.Scorecard (see the seam's cycle note).
func (a *axesReadStore) Axes(ctx context.Context) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached != nil && time.Since(a.cachedAt) < axesCacheTTL {
		return a.cached, nil
	}
	since := time.Now().Add(-axesWindow)
	agg, err := a.store.Aggregate(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("axes aggregate: %w", err)
	}
	sc := axis.Score(agg, axesWindow)
	if fals, ferr := a.store.Falsifiability(ctx, since); ferr != nil {
		log.Printf("axes surface: falsifiability axis unavailable: %v (other axes still served)", ferr)
	} else {
		sc.Falsifiability = fals
	}
	if lb, lberr := a.store.LoopBypass(ctx, since); lberr != nil {
		log.Printf("axes surface: loop-bypass axis unavailable: %v (other axes still served)", lberr)
	} else {
		sc.LoopBypass = lb
	}
	body, err := json.Marshal(sc)
	if err != nil {
		return nil, fmt.Errorf("axes marshal: %w", err)
	}
	a.cached, a.cachedAt = body, time.Now()
	return body, nil
}
