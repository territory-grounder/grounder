package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
)

func ogGet(rt *auth.Router, c *http.Cookie, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	rt.Mux().ServeHTTP(rec, req)
	return rec
}

// A valid add flows the VALIDATED name/patterns + rationale and the SERVER-derived operator/admin proof to
// the writer, and returns the worker's outcome.
func TestObjectGroupAddUsesSessionIdentity(t *testing.T) {
	mw := &MemObjectGroupWriter{Outcome: ObjectGroupWriteOutcome{ID: 9, LedgerSeq: 51}}
	rt, c := adminSurfaceRig(t, Deps{ObjectGroupWrite: mw}, true)
	rec := adminPostJSON(rt, c, "/v1/estate/groups/entries",
		`{"name":"webservers","patterns":["dc1demo-web*"," "],"rationale":"cover the web tier"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out ObjectGroupWriteOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.ID != 9 || out.LedgerSeq != 51 {
		t.Fatalf("outcome = %+v", out)
	}
	if mw.LastVerb != "add" || mw.LastName != "webservers" || mw.LastRationale != "cover the web tier" {
		t.Fatalf("writer call = %+v", mw)
	}
	if len(mw.LastPatterns) != 1 || mw.LastPatterns[0] != "dc1demo-web*" {
		t.Fatalf("patterns not trimmed/filtered before forwarding: %v", mw.LastPatterns)
	}
	if mw.LastOperator != "kyriakos" || !mw.LastAdmin {
		t.Fatalf("operator/admin proof must be the SERVER principal's, got %q admin=%v", mw.LastOperator, mw.LastAdmin)
	}
}

// Surface fast-validation refuses BEFORE the worker: missing name / no usable patterns / missing rationale
// each 400, and the writer is never called.
func TestObjectGroupAddSurfaceValidation(t *testing.T) {
	mw := &MemObjectGroupWriter{}
	rt, c := adminSurfaceRig(t, Deps{ObjectGroupWrite: mw}, true)
	for name, body := range map[string]string{
		"missing name":      `{"patterns":["h*"],"rationale":"r"}`,
		"no patterns":       `{"name":"g","patterns":[],"rationale":"r"}`,
		"empty patterns":    `{"name":"g","patterns":["  ",""],"rationale":"r"}`,
		"missing rationale": `{"name":"g","patterns":["h*"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := adminPostJSON(rt, c, "/v1/estate/groups/entries", body); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d, want 400 (%s)", name, rec.Code, rec.Body.String())
			}
		})
	}
	if mw.Calls != 0 {
		t.Fatalf("a surface-refused add must never reach the writer, Calls=%d", mw.Calls)
	}
}

// Delete flows the id + rationale + server identity to the writer.
func TestObjectGroupDeleteUsesSessionIdentity(t *testing.T) {
	mw := &MemObjectGroupWriter{Outcome: ObjectGroupWriteOutcome{ID: 4, LedgerSeq: 60}}
	rt, c := adminSurfaceRig(t, Deps{ObjectGroupWrite: mw}, true)
	rec := adminDeleteJSON(rt, c, "/v1/estate/groups/entries/4", `{"rationale":"no longer used"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d (%s)", rec.Code, rec.Body.String())
	}
	if mw.LastVerb != "delete" || mw.LastID != 4 || mw.LastRationale != "no longer used" || mw.LastOperator != "kyriakos" {
		t.Fatalf("writer delete call = %+v", mw)
	}
	// a missing rationale is refused at the surface.
	if rec := adminDeleteJSON(rt, c, "/v1/estate/groups/entries/4", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing rationale: got %d, want 400", rec.Code)
	}
}

// A worker refusal maps to an honest status: a missing row → 404, a non-admin re-check → 403, a validation
// refusal → 400.
func TestObjectGroupWriteErrMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrNoSuchObjectGroup, http.StatusNotFound},
		{ErrObjectGroupNotAdmin, http.StatusForbidden},
		{ErrObjectGroupInvalid, http.StatusBadRequest},
	}
	for _, tc := range cases {
		mw := &MemObjectGroupWriter{Err: tc.err}
		rt, c := adminSurfaceRig(t, Deps{ObjectGroupWrite: mw}, true)
		rec := adminPostJSON(rt, c, "/v1/estate/groups/entries", `{"name":"g","patterns":["h*"],"rationale":"r"}`)
		if rec.Code != tc.want {
			t.Errorf("worker err %v → got %d, want %d (%s)", tc.err, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// GET renders the reader's object groups.
func TestObjectGroupGet(t *testing.T) {
	mr := &MemObjectGroupsReader{Groups: []ObjectGroup{
		{ID: 1, Name: "edge-firewalls", Patterns: []string{"dc1demo-fw*"}, Precedence: "union", CreatedBy: "kyriakos", CreatedAt: "2026-08-16T00:00:00Z"},
	}}
	rt, c := adminSurfaceRig(t, Deps{ObjectGroupRead: mr}, true)
	rec := ogGet(rt, c, "/v1/estate/groups")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: got %d (%s)", rec.Code, rec.Body.String())
	}
	var page ObjectGroupsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("page decode: %v", err)
	}
	if page.Total != 1 || len(page.Groups) != 1 || page.Groups[0].Name != "edge-firewalls" {
		t.Fatalf("page = %+v", page)
	}
	// nil reader → 503, said plainly.
	rtNil, cNil := adminSurfaceRig(t, Deps{}, true)
	if rec := ogGet(rtNil, cNil, "/v1/estate/groups"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "unavailable") {
		t.Fatalf("nil reader: got %d %q, want 503", rec.Code, rec.Body.String())
	}
}
