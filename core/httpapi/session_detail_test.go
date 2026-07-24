package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// withExternalRef injects the chi {external_ref} path param for a direct-handler unit test (the same
// route-context pattern ingest_test.go uses).
func withExternalRef(r *http.Request, ref string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("external_ref", ref)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// fakeDetailReader is the in-memory SessionDetailReader oracle: it serves a fixed trace for a known ref and
// trace.ErrNotFound otherwise — the same repository-interface + fake discipline the other read surfaces use,
// so the handler is testable with no database.
type fakeDetailReader struct {
	byRef map[string]trace.SessionTrace
}

func (f fakeDetailReader) SessionDetail(_ context.Context, _ auth.Principal, ref string) (trace.SessionTrace, error) {
	tr, ok := f.byRef[ref]
	if !ok {
		return trace.SessionTrace{}, trace.ErrNotFound
	}
	return tr, nil
}

func sampleTrace(ref string) trace.SessionTrace {
	return trace.SessionTrace{
		ExternalRef: ref, Host: "librespeed01", AlertRule: "iface_down", Band: "AUTO",
		RiskLevel: "low", ActionID: "act-1", Status: trace.StatusExecuted, Confidence: 0.82, Verdict: "match",
		Steps: []trace.Step{
			{Seq: 0, Kind: trace.StepClassify, Label: "Risk classification", At: time.Unix(100, 0).UTC(), Band: "AUTO"},
			{Seq: 1, Kind: trace.StepPropose, Label: "Proposal", At: time.Unix(110, 0).UTC(), Confidence: 0.82},
			{Seq: 2, Kind: trace.StepPredict, Label: "Consequence prediction", At: time.Unix(120, 0).UTC(), Verdict: "clean"},
			{Seq: 3, Kind: trace.StepVerify, Label: "Mechanical verdict", Verdict: "match"},
		},
	}
}

// The detail endpoint requires authentication (it is registered under AuthReadOnly): an unauthenticated
// request never reaches the handler and is refused.
func TestSessionDetailRequiresAuth(t *testing.T) {
	rt := auth.NewRouter(&auth.Verifier{}) // empty verifier: no credential satisfies AuthReadOnly
	Register(rt, Deps{SessionDetailRead: fakeDetailReader{byRef: map[string]trace.SessionTrace{"ext-1": sampleTrace("ext-1")}}})
	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/sessions/ext-1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauthenticated read must be refused, got 200")
	}
}

// An authenticated principal gets the per-step trace in decision-boundary order, assembled from the spine.
func TestSessionDetailReturnsOrderedSteps(t *testing.T) {
	d := Deps{SessionDetailRead: fakeDetailReader{byRef: map[string]trace.SessionTrace{"ext-1": sampleTrace("ext-1")}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1", nil)
	req = withExternalRef(req, "ext-1")
	rr := httptest.NewRecorder()
	d.sessionDetailHandler(rr, req, auth.Principal{})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var dto SessionDetailDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Ref != "ext-1" || dto.Status != "executed" || dto.Verdict != "match" || dto.Conf != 0.82 {
		t.Fatalf("header wrong: %+v", dto)
	}
	if len(dto.Nodes) != 4 {
		t.Fatalf("want 4 nodes, got %d", len(dto.Nodes))
	}
	wantKinds := []string{"classify", "propose", "predict", "verify"}
	for i, n := range dto.Nodes {
		if n.T != wantKinds[i] {
			t.Fatalf("node %d kind = %q, want %q (decision-boundary order)", i, n.T, wantKinds[i])
		}
	}
	// The verify node reflects the mechanical verdict as "ok"; the classify node is not "pending".
	if dto.Nodes[3].St != "ok" {
		t.Fatalf("verify node state = %q, want ok", dto.Nodes[3].St)
	}
}

// An unknown external_ref maps to 404 (never an empty 200), and a nil reader fails closed to 503.
func TestSessionDetailNotFoundAnd503(t *testing.T) {
	d := Deps{SessionDetailRead: fakeDetailReader{byRef: map[string]trace.SessionTrace{}}}
	req := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/missing", nil), "missing")
	rr := httptest.NewRecorder()
	d.sessionDetailHandler(rr, req, auth.Principal{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown ref status = %d, want 404", rr.Code)
	}

	nild := Deps{}
	req2 := withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1", nil), "ext-1")
	rr2 := httptest.NewRecorder()
	nild.sessionDetailHandler(rr2, req2, auth.Principal{})
	if rr2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil reader status = %d, want 503", rr2.Code)
	}
}
