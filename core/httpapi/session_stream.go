package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// The per-session step channel (spec/020 REQ-2013/REQ-2010): GET /v1/sessions/{external_ref}/stream serves
// Server-Sent Events carrying the session's decision walk AS ITS DURABLE ROWS COMMIT — the walk the detail
// endpoint assembles, but streamed so a queued or live-running session ANIMATES from REAL boundary events
// rather than a client-side simulation clock. On connect and on each interval it reassembles the walk from the
// durable spine and, WHENEVER THE WALK CHANGED, emits the FULL current walk as a `snapshot` event; it closes
// with a `done` event once the session is terminal (executed/stopped), the duration cap is reached, or the
// client disconnects.
//
// Why a full snapshot, not an incremental tail: the assembler (core/trace.Assemble) orders steps by BOUNDARY
// TYPE (classify → agent-cycles → credentials → propose → predict → policy → gates → verify) and ENRICHES a
// step IN PLACE as later rows land (a predict step's verdict fills in when the falsify window scores it; a
// lower-ordered boundary that commits late shifts positions). An index/count cursor would duplicate, skip, or
// never re-emit those; re-sending the whole walk and letting the client re-render is correct by construction.
//
// It is OBSERVE-ONLY like the detail endpoint: it runs only after core/auth authenticated the caller (the same
// elevated AuthTraceRead), reaches no actuator, and every field it projects is a committed spine value carrying
// no secret (references and schemes only, INV-13). Nil reader or a non-flushable writer fails closed to 503; an
// unknown external_ref is 404 before the stream opens.

// maxSessionStreamDuration bounds a single stream's server work. Unlike the always-on posture stream
// (events.go), a PER-SESSION channel should end — but a proposal that is denied/expired/POLL_PAUSE-stuck stays
// non-terminal (StatusProposed) indefinitely, so without a cap an idle browser tab would re-assemble the walk
// every interval forever. On the cap we send `done` and close; the client may reconnect if it still cares.
const maxSessionStreamDuration = 10 * time.Minute

func (d Deps) sessionStreamHandler(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if d.SessionDetailRead == nil {
		http.Error(w, "session stream unavailable", http.StatusServiceUnavailable)
		return
	}
	ref := chi.URLParam(r, "external_ref")
	if ref == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "session stream unavailable", http.StatusServiceUnavailable)
		return
	}
	interval := d.EventsInterval
	if interval <= 0 {
		interval = defaultEventsInterval
	}

	// First fetch BEFORE opening the stream, so an unknown session is a clean 404 (not an empty 200 event-stream).
	// Authority is resolved inside SessionDetail against the principal (INV-12).
	tr, err := d.SessionDetailRead.SessionDetail(r.Context(), p, ref)
	if err != nil {
		if errors.Is(err, trace.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "session stream unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // the console's nginx must not buffer the stream
	w.WriteHeader(http.StatusOK)

	var last []byte
	// emit sends the full current walk as a `snapshot` IFF it changed since the last send (bytes-equal on the
	// projected DTO), and reports whether the session is terminal. A changed walk = a step appeared OR an
	// existing step was enriched in place — both reach the client as the latest snapshot it re-renders.
	emit := func(t trace.SessionTrace) (terminal bool) {
		if b, mErr := json.Marshal(ProjectSessionDetail(t)); mErr == nil && !bytes.Equal(b, last) {
			last = b
			fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b)
			fl.Flush()
		}
		return t.Status == trace.StatusExecuted || t.Status == trace.StatusStopped
	}
	done := func(status trace.Status) {
		fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", string(status))
		fl.Flush()
	}

	if emit(tr) {
		done(tr.Status)
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	deadline := time.After(maxSessionStreamDuration)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			done(tr.Status) // bounded: a non-terminal (denied/POLL_PAUSE-stuck) session is not streamed forever
			return
		case <-tick.C:
			t, terr := d.SessionDetailRead.SessionDetail(r.Context(), p, ref)
			if terr != nil {
				return // the stream ends; the client reconnects (and gets 404/503 semantics if it now fails)
			}
			tr = t
			if emit(t) {
				done(t.Status)
				return
			}
		}
	}
}
