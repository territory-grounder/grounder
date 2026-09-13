package httpapi

// ORACLES FOR THE DEK REWRAP SURFACE (TG-163). The route re-keys the whole sealed store, so it sits behind
// the same admin-session gate as the secret write next door — and it must never become reachable by a plain
// operator session, because a rewrap is a key-lifecycle operation over every credential in the estate.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type fakeSealRewrapper struct {
	calls     int
	rationale string
	after     string
	limit     int
	operator  string
	err       error
}

func (f *fakeSealRewrapper) RewrapSeals(_ context.Context, rationale, after string, limit int, operator string) (SealRewrapOutcome, error) {
	f.calls++
	f.rationale, f.after, f.limit, f.operator = rationale, after, limit, operator
	if f.err != nil {
		return SealRewrapOutcome{}, f.err
	}
	return SealRewrapOutcome{
		Scanned: 4, Rewrapped: 4, LastName: "z", LedgerSeq: 77,
		Versions: map[string]int{"v2": 4},
		Note:     "4 scanned, 4 rewrapped, 0 already current; key versions now: v2=4",
	}, nil
}

func TestSealRewrapReportsCountsAndVersionsAndNoSecretMaterial(t *testing.T) {
	fr := &fakeSealRewrapper{}
	rt, c := adminSurfaceRig(t, Deps{SealRewrap: fr}, true)
	rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{"rationale":"retiring transit key v1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrap: got %d (%s)", rec.Code, rec.Body.String())
	}
	var out SealRewrapOutcome
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("outcome decode: %v", err)
	}
	if out.Scanned != 4 || out.Rewrapped != 4 || out.LedgerSeq != 77 {
		t.Fatalf("outcome = %+v", out)
	}
	// The version census must reach the operator. It is the ONLY thing that tells them whether the old
	// Transit key version can be retired, and retiring one that is still in use destroys the store.
	if out.Versions["v2"] != 4 {
		t.Fatalf("the response dropped the key-version census (%+v) — without it the operator has no "+
			"basis for the decision this whole route exists to enable", out.Versions)
	}
	if fr.rationale != "retiring transit key v1" || fr.operator != "kyriakos" {
		t.Fatalf("backend call = %+v — the operator identity must be server-derived, never client-supplied", fr)
	}
}

// The cursor and the batch bound must actually reach the backend: an operator resuming an interrupted run
// who is silently restarted from row zero either re-does the whole store or, worse, believes rows were
// covered that were not.
func TestSealRewrapPassesTheResumeCursorAndLimitThrough(t *testing.T) {
	fr := &fakeSealRewrapper{}
	rt, c := adminSurfaceRig(t, Deps{SealRewrap: fr}, true)
	rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{"rationale":"batch two","after":"librenms.token","limit":50}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rewrap: got %d (%s)", rec.Code, rec.Body.String())
	}
	if fr.after != "librenms.token" || fr.limit != 50 {
		t.Fatalf("resume cursor/limit did not reach the backend: after=%q limit=%d", fr.after, fr.limit)
	}
}

func TestSealRewrapValidation(t *testing.T) {
	fr := &fakeSealRewrapper{}
	rt, c := adminSurfaceRig(t, Deps{SealRewrap: fr}, true)
	if rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing rationale: got %d, want 400", rec.Code)
	}
	if rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{"rationale":"r","limit":-1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative limit: got %d, want 400", rec.Code)
	}
	if fr.calls != 0 {
		t.Fatalf("a refused request still reached the backend %d time(s) — validation must happen before "+
			"anything starts re-keying the store", fr.calls)
	}
}

// FAIL CLOSED: no seal backend means 503, never a 200 reporting that nothing needed doing.
func TestSealRewrapNilBackendIs503(t *testing.T) {
	rt, c := adminSurfaceRig(t, Deps{}, true)
	rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{"rationale":"r"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil backend: got %d, want 503", rec.Code)
	}
}

// A plain operator session must NOT be able to re-key every credential in the estate.
//
// KILLING MUTATION (executed 2026-08-05): change the route registration to auth.AuthReadOnly. RED across
// every test in this file — and the observed code is 403 "session principals are read-only", NOT the 200 a
// naive reading would predict. Worth recording: core/auth independently refuses a browser session on a
// read-only route, so the downgrade does not open the store to plain operators; it makes the route
// unreachable from the console entirely. Both the auth tier here and that second line are load-bearing, and
// this test pins the tier so the route cannot drift onto a weaker one and still look wired.
func TestSealRewrapRequiresElevation(t *testing.T) {
	fr := &fakeSealRewrapper{}
	rt, c := adminSurfaceRig(t, Deps{SealRewrap: fr}, false) // logged in, NOT elevated
	rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{"rationale":"r"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a NON-elevated session re-keyed the sealed store: got %d, want 401", rec.Code)
	}
	if fr.calls != 0 {
		t.Fatalf("the backend ran for an unelevated caller")
	}
}

// A failing rewrap must not leak the row detail into the response body (it can be credential-adjacent), and
// must not be mistaken for success.
func TestSealRewrapFailureIs503AndSaysWhereToLook(t *testing.T) {
	fr := &fakeSealRewrapper{err: errStub("rewrap \"librenms.token\": seal: open failed")}
	rt, c := adminSurfaceRig(t, Deps{SealRewrap: fr}, true)
	rec := adminPostJSON(rt, c, "/v1/seal/rewrap", `{"rationale":"r"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed rewrap: got %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "librenms.token") {
		t.Fatalf("the response echoed the failing secret's name: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "grounder log") {
		t.Fatalf("the response does not say where the cause is (%q) — TG-276's lesson was that a vague "+
			"503 with the cause discarded costs days", rec.Body.String())
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
