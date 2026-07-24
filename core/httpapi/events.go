package httpapi

import (
	"encoding/json"

	"fmt"
	"github.com/territory-grounder/grounder/core/auth"
	"net/http"
	"time"
)

// The liveness stream (spec/006 REQ-513): GET /v1/events serves Server-Sent Events carrying the same
// governance posture the /v1/governance surface assembles — an immediate snapshot on connect, then one
// per interval — so the console's "live" indicator reflects an actual stream from the control plane,
// never a client-side simulation (INV-15). Read-only by construction: it emits state, accepts nothing.

// defaultEventsInterval paces posture events when Deps does not override it (oracles use a short one).
const defaultEventsInterval = 5 * time.Second

// eventsHandler streams posture events until the client disconnects. Nil Governance reader or a
// non-flushable writer = 503 fail-closed.
func (d Deps) eventsHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Governance == nil {
		http.Error(w, "events unavailable", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "events unavailable", http.StatusServiceUnavailable)
		return
	}
	interval := d.EventsInterval
	if interval <= 0 {
		interval = defaultEventsInterval
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // the console's nginx must not buffer the stream
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		st, err := d.Governance.Governance(r.Context(), p)
		if err != nil {
			return false // the stream ends; the client reconnects and, if still failing, sees 503 semantics
		}
		if st.Bands == nil {
			st.Bands = map[string]int{}
		}
		b, err := json.Marshal(st)
		if err != nil {
			return false
		}
		fmt.Fprintf(w, "event: posture\ndata: %s\n\n", b)
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}
