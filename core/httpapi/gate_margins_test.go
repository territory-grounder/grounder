package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// fakeGateMarginReader is the in-memory GateMarginReader oracle: it returns fixed boundary-case rows and
// records the ε it was queried with, so the handler is testable with no database.
type fakeGateMarginReader struct {
	rows   []trace.GateVerdict
	gotEps float64
	err    error
}

func (f *fakeGateMarginReader) GateVerdictsWithinEpsilon(_ context.Context, eps float64) ([]trace.GateVerdict, error) {
	f.gotEps = eps
	return f.rows, f.err
}

func fptr(v float64) *float64 { return &v }

// A nil reader fail-closes to 503 — the console then holds an honest empty state rather than implying there
// are no boundary cases when the surface is simply not wired.
func TestGateMarginsNilReaderIs503(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon", nil)
	w := httptest.NewRecorder()
	Deps{}.gateMarginsHandler(w, req, auth.Principal{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil GateMargins must be 503, got %d", w.Code)
	}
}

// An authenticated read returns the within-ε boundary cases, projecting the signed margin, and echoes the ε it
// selected on. The explicit eps query is threaded to the reader.
func TestGateMarginsReturnsBoundaryCases(t *testing.T) {
	f := &fakeGateMarginReader{rows: []trace.GateVerdict{
		{ActionID: "act-1", ExternalRef: "ext-1", Ordinal: 7, Gate: "policy", Verdict: "pass", Reason: "auto", Margin: fptr(-0.01)},
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon?eps=0.02", nil)
	w := httptest.NewRecorder()
	Deps{GateMargins: f}.gateMarginsHandler(w, req, auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if f.gotEps != 0.02 {
		t.Fatalf("reader queried with ε=%v, want 0.02 (query param not threaded)", f.gotEps)
	}
	var page GateBoundaryPage
	if err := json.NewDecoder(w.Body).Decode(&page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Epsilon != 0.02 {
		t.Fatalf("page echoed ε=%v, want 0.02", page.Epsilon)
	}
	if len(page.Cases) != 1 || page.Cases[0].Gate != "policy" || page.Cases[0].Margin != -0.01 {
		t.Fatalf("cases = %+v, want one policy case with margin -0.01", page.Cases)
	}
}

// A malformed or non-positive ε falls back to the default band rather than 400 — the query is a review
// convenience, not a contract.
func TestGateMarginsMalformedEpsUsesDefault(t *testing.T) {
	f := &fakeGateMarginReader{}
	for _, q := range []string{"?eps=abc", "?eps=-1", "?eps=0", ""} {
		req := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon"+q, nil)
		w := httptest.NewRecorder()
		Deps{GateMargins: f}.gateMarginsHandler(w, req, auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("eps=%q: got %d, want 200", q, w.Code)
		}
		if f.gotEps != defaultGateMarginEpsilon {
			t.Fatalf("eps=%q: reader queried with %v, want default %v", q, f.gotEps, defaultGateMarginEpsilon)
		}
	}
}

// TG-178: the DEFAULT ε is loadable configuration, not a compiled constant. A configured Deps.GateMarginEpsilon
// in the valid band is used when the caller names no eps=; an explicit eps= query still wins over it; and an
// unset/out-of-range config leaves the compiled 0.05 default. Killing mutation: drop the
// `if d.GateMarginEpsilon > 0 …` block in the handler → the configured default is ignored → RED.
func TestGateMarginsConfiguredEpsilonIsTheLoadableDefault(t *testing.T) {
	// no eps= query ⇒ the configured default is used.
	f := &fakeGateMarginReader{}
	req := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon", nil)
	w := httptest.NewRecorder()
	Deps{GateMargins: f, GateMarginEpsilon: 0.2}.gateMarginsHandler(w, req, auth.Principal{})
	if f.gotEps != 0.2 {
		t.Fatalf("no eps=: reader queried with %v, want the configured default 0.2 (ε is loadable, TG-178)", f.gotEps)
	}

	// an explicit eps= still WINS over the configured default.
	f2 := &fakeGateMarginReader{}
	req2 := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon?eps=0.03", nil)
	w2 := httptest.NewRecorder()
	Deps{GateMargins: f2, GateMarginEpsilon: 0.2}.gateMarginsHandler(w2, req2, auth.Principal{})
	if f2.gotEps != 0.03 {
		t.Fatalf("explicit eps=0.03 with config 0.2: reader queried with %v, want 0.03 (the query wins)", f2.gotEps)
	}

	// an out-of-range configured ε (> max) is ignored, keeping the compiled default.
	f3 := &fakeGateMarginReader{}
	req3 := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon", nil)
	w3 := httptest.NewRecorder()
	Deps{GateMargins: f3, GateMarginEpsilon: 99}.gateMarginsHandler(w3, req3, auth.Principal{})
	if f3.gotEps != defaultGateMarginEpsilon {
		t.Fatalf("out-of-range config: reader queried with %v, want the compiled default %v", f3.gotEps, defaultGateMarginEpsilon)
	}
}

// An ε above the cap is clamped to maxGateMarginEpsilon rather than passed through — an unbounded band is not
// a boundary query. The request is served (clamped), not refused.
func TestGateMarginsEpsilonClampedToMax(t *testing.T) {
	f := &fakeGateMarginReader{}
	req := httptest.NewRequest(http.MethodGet, "/v1/gates/within-epsilon?eps=5", nil)
	w := httptest.NewRecorder()
	Deps{GateMargins: f}.gateMarginsHandler(w, req, auth.Principal{})
	if w.Code != http.StatusOK {
		t.Fatalf("over-cap ε: got %d, want 200", w.Code)
	}
	if f.gotEps != maxGateMarginEpsilon {
		t.Fatalf("ε=5 must clamp to %v, reader queried with %v", maxGateMarginEpsilon, f.gotEps)
	}
}

// The route is registered under the ELEVATED trace-read authority (it is decision-tracer data): an
// unauthenticated request never reaches the handler.
func TestGateMarginsRequiresTraceReadAuth(t *testing.T) {
	rt := auth.NewRouter(&auth.Verifier{}) // empty verifier: nothing satisfies AuthTraceRead
	Register(rt, Deps{GateMargins: &fakeGateMarginReader{}})
	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/gates/within-epsilon")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated within-ε read must be refused, got 200")
	}
	// NOT 404 proves the route is actually REGISTERED (an auth refusal on a registered route, not a
	// missing-route 404) — the "built + tested but never served" defect this codebase guards against.
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("GET /v1/gates/within-epsilon is not registered (404) — the surface is dead")
	}
}
