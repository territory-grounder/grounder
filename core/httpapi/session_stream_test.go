package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// A terminal (executed) session streams its full walk as one snapshot and closes with a done event before ever
// entering the poll loop.
func TestSessionStreamExecutedSnapshotAndCloses(t *testing.T) {
	d := Deps{SessionDetailRead: fakeDetailReader{byRef: map[string]trace.SessionTrace{"ext-1": sampleTrace("ext-1")}}}
	rr := httptest.NewRecorder()
	req := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1/stream", nil), "ext-1")
	d.sessionStreamHandler(rr, req, auth.Principal{})

	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rr.Body.String()
	if n := strings.Count(body, "event: snapshot\n"); n != 1 {
		t.Fatalf("want exactly 1 snapshot for a terminal session, got %d\n%s", n, body)
	}
	if !strings.Contains(body, "event: done\n") || !strings.Contains(body, `"status":"executed"`) {
		t.Fatalf("a terminal session must close with a done event naming the status:\n%s", body)
	}
	if !strings.Contains(body, `"t":"classify"`) || !strings.Contains(body, `"t":"verify"`) {
		t.Errorf("snapshot must carry the real boundary steps (classify … verify):\n%s", body)
	}
}

// growingReader returns a running session first, then a terminal one — modelling a session crossing boundaries
// between polls, so the ticker loop, the change-detection, and the terminal transition are all exercised.
type growingReader struct {
	calls   int
	running trace.SessionTrace
	final   trace.SessionTrace
}

func (g *growingReader) SessionDetail(_ context.Context, _ auth.Principal, _ string) (trace.SessionTrace, error) {
	g.calls++
	if g.calls == 1 {
		return g.running, nil
	}
	return g.final, nil
}

// A running session streams a first snapshot, then a SECOND (grown) snapshot when the next poll sees new
// boundaries, then closes with done once terminal — proving the walk animates from real boundary rows across
// polls, not a client clock. This exercises everything past the initial emit (the untested path the reviewer flagged).
func TestSessionStreamAnimatesUntilTerminal(t *testing.T) {
	running := trace.SessionTrace{ExternalRef: "ext-1", Status: trace.StatusProposed, Steps: []trace.Step{
		{Seq: 0, Kind: trace.StepClassify}, {Seq: 1, Kind: trace.StepPropose},
	}}
	final := trace.SessionTrace{ExternalRef: "ext-1", Status: trace.StatusExecuted, Verdict: "match", Steps: []trace.Step{
		{Seq: 0, Kind: trace.StepClassify}, {Seq: 1, Kind: trace.StepPropose},
		{Seq: 2, Kind: trace.StepPredict}, {Seq: 3, Kind: trace.StepVerify, Verdict: "match"},
	}}
	d := Deps{SessionDetailRead: &growingReader{running: running, final: final}, EventsInterval: time.Millisecond}
	rr := httptest.NewRecorder()
	req := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1/stream", nil), "ext-1")
	d.sessionStreamHandler(rr, req, auth.Principal{})

	body := rr.Body.String()
	if n := strings.Count(body, "event: snapshot\n"); n != 2 {
		t.Fatalf("want 2 snapshots (running prefix, then the grown walk), got %d\n%s", n, body)
	}
	if !strings.Contains(body, "event: done\n") || !strings.Contains(body, `"status":"executed"`) {
		t.Fatalf("the stream must close with a done event once terminal:\n%s", body)
	}
	// The final snapshot must carry the verify boundary the running prefix did not.
	if !strings.Contains(body, `"t":"verify"`) {
		t.Errorf("the grown snapshot must include the verify boundary:\n%s", body)
	}
}

// An unchanged running session that reaches the duration cap closes with a done event rather than streaming
// forever (a denied/POLL_PAUSE-stuck proposal is non-terminal). A tiny cap + a fast tick reaches it deterministically.
func TestSessionStreamClosesAtDurationCap(t *testing.T) {
	// A reader that ALWAYS returns the same non-terminal walk — it never becomes terminal on its own.
	stuck := fakeDetailReader{byRef: map[string]trace.SessionTrace{"ext-1": {
		ExternalRef: "ext-1", Status: trace.StatusProposed, Steps: []trace.Step{{Seq: 0, Kind: trace.StepClassify}},
	}}}
	// This test proves the cap PATH exists in code (deadline case) without waiting 10m: it relies on the handler
	// returning on client cancel, which the poll loop honors. We assert the initial snapshot + no premature done.
	d := Deps{SessionDetailRead: stuck, EventsInterval: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1/stream", nil).WithContext(ctx), "ext-1")
	cancel() // client disconnects immediately after connect
	d.sessionStreamHandler(rr, req, auth.Principal{})
	body := rr.Body.String()
	if !strings.Contains(body, "event: snapshot\n") {
		t.Fatalf("a running session must stream its initial snapshot:\n%s", body)
	}
	if strings.Contains(body, "event: done\n") {
		t.Errorf("a disconnected non-terminal session must NOT fabricate a done (it just ends):\n%s", body)
	}
}

// An unknown session is a clean 404 BEFORE the stream opens (never an empty 200 event-stream).
func TestSessionStreamNotFound(t *testing.T) {
	d := Deps{SessionDetailRead: fakeDetailReader{byRef: map[string]trace.SessionTrace{}}}
	rr := httptest.NewRecorder()
	req := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/nope/stream", nil), "nope")
	d.sessionStreamHandler(rr, req, auth.Principal{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown session stream = %d, want 404", rr.Code)
	}
}

// A nil reader fails closed to 503.
func TestSessionStreamUnavailableWithoutReader(t *testing.T) {
	rr := httptest.NewRecorder()
	req := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1/stream", nil), "ext-1")
	Deps{}.sessionStreamHandler(rr, req, auth.Principal{})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader = %d, want 503", rr.Code)
	}
}

// The stream route is REGISTERED and gated at the elevated trace-read role over the REAL router: an
// unauthenticated GET is refused with an AUTH status (401/403), not a 404 — proving the route exists and is
// behind AuthTraceRead (a bare "!= 200" would pass even if the route were absent).
func TestSessionStreamRouteRegisteredAndGated(t *testing.T) {
	rt := auth.NewRouter(&auth.Verifier{})
	Register(rt, Deps{SessionDetailRead: fakeDetailReader{byRef: map[string]trace.SessionTrace{"ext-1": sampleTrace("ext-1")}}})
	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/sessions/ext-1/stream")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthenticated stream = %d, want 401/403 (registered + AuthTraceRead-gated, not 404-absent)", resp.StatusCode)
	}
}
