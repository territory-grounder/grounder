package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/skillstore"
)

type fakeSkillsWriter struct {
	drafted   string
	transTo   skillstore.Status
	transID   int64
	transWhy  string
	transOper string
	failWith  error
}

func (f *fakeSkillsWriter) CreateDraft(_ context.Context, name string, req SkillDraftRequest, operator string) (SkillVersionView, error) {
	if f.failWith != nil {
		return SkillVersionView{}, f.failWith
	}
	f.drafted = name
	f.transOper = operator
	return SkillVersionView{ID: 9, Version: req.Version, Status: "draft"}, nil
}

func (f *fakeSkillsWriter) Transition(_ context.Context, id int64, to skillstore.Status, why, operator string) (SkillTransitionOutcome, error) {
	if f.failWith != nil {
		return SkillTransitionOutcome{}, f.failWith
	}
	f.transID, f.transTo, f.transWhy, f.transOper = id, to, why, operator
	return SkillTransitionOutcome{VersionID: id, Status: string(to), LedgerSeq: 77}, nil
}

func postJSON(path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// A write without a rationale is refused at the surface (REQ-1301) — before any backend call.
func TestSkillWriteRequiresRationale(t *testing.T) {
	fw := &fakeSkillsWriter{}
	mux := chiWriteRouterForTest(Deps{SkillsWrite: fw})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/v1/skills/triage-protocol/versions", `{"version":"2.0.0","body":"b"}`))
	if w.Code != http.StatusBadRequest || fw.drafted != "" {
		t.Fatalf("draft without rationale must 400 before the backend, got %d drafted=%q", w.Code, fw.drafted)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/v1/skills/versions/9/promote", `{}`))
	if w.Code != http.StatusBadRequest || fw.transID != 0 {
		t.Fatalf("transition without rationale must 400, got %d", w.Code)
	}
}

// The verb table is closed: an unknown verb 404s, a known one maps to exactly its status; the operator
// identity comes from the PRINCIPAL, never the body.
func TestSkillTransitionVerbTableAndIdentity(t *testing.T) {
	fw := &fakeSkillsWriter{}
	mux := chiWriteRouterForTest(Deps{SkillsWrite: fw})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/v1/skills/versions/9/destroy", `{"rationale":"r"}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown verb must 404, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, postJSON("/v1/skills/versions/9/promote", `{"rationale":"welch passed","operator":"forged-identity"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("promote must succeed, got %d %s", w.Code, w.Body.String())
	}
	if fw.transTo != skillstore.StatusProduction || fw.transID != 9 {
		t.Fatalf("verb must map to production for id 9, got %v id=%d", fw.transTo, fw.transID)
	}
	if fw.transOper != "acc-operator" {
		t.Fatalf("operator must be principal-derived, got %q", fw.transOper)
	}
}

// State-machine refusals map to honest statuses: pinned=422, bad transition=409, not found=404.
func TestSkillWriteErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{skillstore.ErrPinnedSkill, http.StatusUnprocessableEntity},
		{skillstore.ErrBadTransition, http.StatusConflict},
		{skillstore.ErrNotFound, http.StatusNotFound},
		{skillstore.ErrBodyBounds, http.StatusBadRequest},
	}
	for _, c := range cases {
		mux := chiWriteRouterForTest(Deps{SkillsWrite: &fakeSkillsWriter{failWith: c.err}})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, postJSON("/v1/skills/versions/9/retire", `{"rationale":"r"}`))
		if w.Code != c.want {
			t.Errorf("%v must map to %d, got %d", c.err, c.want, w.Code)
		}
	}
}

// chiWriteRouterForTest mounts the write routes with a pass-through operator principal (dispatch +
// handler logic; the machine-principal refusal is proven by the acceptance oracle on the real router).
func chiWriteRouterForTest(d Deps) http.Handler {
	mux := newTestChiMux()
	pass := func(h func(Deps, http.ResponseWriter, *http.Request, auth.Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h(d, w, r, auth.Principal{SourceID: "operator:acc-operator"})
		}
	}
	mux.Handle("/v1/skills/{name}/versions", pass(Deps.skillDraftHandler))
	mux.Handle("/v1/skills/versions/{id}/{verb}", pass(Deps.skillTransitionHandler))
	return mux
}
