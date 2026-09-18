package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/httpapi"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/worldmodel"
	"github.com/territory-grounder/grounder/temporal/manifestwrite"
)

// recordingWriter stands in for the Temporal-backed backend so the COMPOSITION is exercised without a
// worker: what matters here is that an operator's click reaches a writer at all.
type recordingWriter struct {
	gotID       int64
	gotTo       worldmodel.Status
	gotApprover string
	gotRational string
	err         error
}

func (w *recordingWriter) Transition(_ context.Context, id int64, to worldmodel.Status, rationale, approver string) (httpapi.ManifestTransitionOutcome, error) {
	w.gotID, w.gotTo, w.gotRational, w.gotApprover = id, to, rationale, approver
	if w.err != nil {
		return httpapi.ManifestTransitionOutcome{}, w.err
	}
	return httpapi.ManifestTransitionOutcome{ID: id, Name: "mealie.service", Status: string(to), LedgerSeq: 41}, nil
}

// operatorSession builds a REAL browser session on a real Verifier — the credential the write lane
// demands — so the oracle drives the same auth path an operator's browser does.
func operatorSession(t *testing.T, name string) (*auth.Verifier, *auth.SessionAuthenticator, *http.Cookie) {
	t.Helper()
	v, err := auth.NewVerifier(fixedSource{secret: []byte("unused-here")}, freshNonces{}, time.Minute)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	const token = "operator-token-for-the-oracle"
	sa, err := auth.NewSessionAuthenticator([]byte("0123456789abcdef0123456789abcdef"),
		auth.NewMemSessionStore(),
		auth.MemOperators{name: {Name: name, TokenSHA256: sha256.Sum256([]byte(token))}},
		time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	v.EnableBrowserSessions(sa)
	cookie, _, err := sa.Login(context.Background(), name, token, "192.0.2.9:5555")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return v, sa, cookie
}

func adoptRequest(t *testing.T, cookie *http.Cookie, id, verb, rationale string) *http.Request {
	t.Helper()
	body := strings.NewReader(`{"rationale":` + strconvQuote(rationale) + `}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/manifest/entries/"+id+"/"+verb, body)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	return r
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestServedGrounderExecutesAnOperatorsAdoption is the write lane's ALIVENESS oracle, and the reason it
// exists is that the write lane is where the Stage-1 defect would hurt most: the console renders adopt
// controls, the operator clicks, and a dead seam answers 503 forever — which the fail-closed design makes
// look deliberate. So this drives a real session cookie through the exact router main builds and asserts
// the writer actually ran, with the SERVER-derived approver, not one the client could name.
func TestServedGrounderExecutesAnOperatorsAdoption(t *testing.T) {
	v, sa, cookie := operatorSession(t, "zoe")
	rec := &recordingWriter{}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, sa, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, manifestReadStore{s: oneDraft()}, rec, nil, nil, nil, nil, nil, nil, 0, nil, nil)

	w := httptest.NewRecorder()
	api.Mux().ServeHTTP(w, adoptRequest(t, cookie, "7", "adopt", "mealie is ours; TG may restart it"))

	if w.Code != http.StatusOK {
		t.Fatalf("the served adopt lane answered %d, not 200 — a 503 here is the dead-seam defect: an operator "+
			"clicks Adopt forever and nothing is ever granted; body: %s", w.Code, w.Body.String())
	}
	if rec.gotID != 7 || rec.gotTo != worldmodel.StatusApproved {
		t.Fatalf("the writer did not receive the operator's decision: id=%d to=%q", rec.gotID, rec.gotTo)
	}
	if rec.gotApprover != "zoe" {
		t.Errorf("the approver must be SERVER-derived from the session, got %q", rec.gotApprover)
	}
	if !strings.Contains(rec.gotRational, "mealie is ours") {
		t.Errorf("the rationale must reach the ledger writer intact, got %q", rec.gotRational)
	}
}

// TestServedGrounderRefusesAnUnauthenticatedAdoption proves the lane is not merely alive but GATED: the
// same request without a session is refused before any writer runs. An alive-but-open write lane would be
// a worse defect than a dead one — it grants actuation targets to anyone who can reach the port.
func TestServedGrounderRefusesAnUnauthenticatedAdoption(t *testing.T) {
	v, sa, _ := operatorSession(t, "zoe")
	rec := &recordingWriter{}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, sa, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, manifestReadStore{s: oneDraft()}, rec, nil, nil, nil, nil, nil, nil, 0, nil, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/manifest/entries/7/adopt",
		strings.NewReader(`{"rationale":"no session"}`))
	r.Header.Set("Content-Type", "application/json")
	api.Mux().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated adopt must be 401, got %d", w.Code)
	}
	if rec.gotID != 0 {
		t.Fatal("the writer ran for an unauthenticated caller — the grant lane is open")
	}
}

// TestServedGrounderRefusesAMachinePrincipalsAdoption pins the OTHER half of the gate: an HMAC caller is
// a first-class principal on every read surface, and must still have NO grant lane. Adoption widens what
// the leaf will actuate, so it is a human decision by construction — a machine that could adopt could
// grow its own permissions, which is the whole failure mode this plane exists to prevent.
func TestServedGrounderRefusesAMachinePrincipalsAdoption(t *testing.T) {
	v, sa, _ := operatorSession(t, "zoe")
	rec := &recordingWriter{}
	api := buildPublicAPI(v, safety.NewReadOnlyChokepoint(), nil, nil, nil, nil, sa, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, 0, nil, nil, nil, nil, nil, nil, nil, nil, manifestReadStore{s: oneDraft()}, rec, nil, nil, nil, nil, nil, nil, 0, nil, nil)

	// A FULLY VALID machine request — same credential that reads /v1/manifest happily, correct signature
	// over a well-formed body. Nothing but the principal class may stand between it and the writer;
	// an empty body would have been refused for the wrong reason and proved nothing.
	body := `{"rationale":"machine adopting itself a target"}`
	r := signedPost(t, "/v1/manifest/entries/7/adopt", []byte("unused-here"), body)
	w := httptest.NewRecorder()
	api.Mux().ServeHTTP(w, r)

	if w.Code == http.StatusOK || rec.gotID != 0 {
		t.Fatalf("a MACHINE principal adopted an entry (status %d, writer saw id=%d) — the grant lane must be "+
			"session-only; a machine that can adopt can widen its own actuation allowlist", w.Code, rec.gotID)
	}
}

// TestUnwrapManifestErrMapsTheWorkersRefusalsBackToTypedSentinels pins the error translation the surface's
// status codes depend on. A Temporal ApplicationError carries only a message, so without this the worker's
// "that row is gone" and "that transition is illegal" both surface as a 500 fault — telling an operator
// the system broke when in fact the system correctly said no.
func TestUnwrapManifestErrMapsTheWorkersRefusalsBackToTypedSentinels(t *testing.T) {
	for _, tc := range []struct{ in, want error }{
		{errors.New("workflow execution error: " + worldmodel.ErrBadTransition.Error()), worldmodel.ErrBadTransition},
		{errors.New("activity error: " + worldmodel.ErrRationaleRequired.Error()), worldmodel.ErrRationaleRequired},
		{errors.New("activity error: " + worldmodel.ErrUnknownEntityType.Error()), worldmodel.ErrUnknownEntityType},
		{errors.New("activity error: " + manifestwrite.ErrNotFound.Error()), httpapi.ErrManifestEntryNotFound},
	} {
		if got := unwrapManifestErr(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("unwrap(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// An unrecognised failure must NOT be laundered into a decision — a genuine fault stays a fault.
	boom := errors.New("connection refused")
	if got := unwrapManifestErr(boom); !errors.Is(got, boom) {
		t.Errorf("an unrecognised error must pass through untouched, got %v", got)
	}
}
