package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/core/trace"
)

// fakeDiagnosisReader serves one recorded claim and trace.ErrDiagnosisNotFound for anything else — the same
// repository-interface + fake discipline every other read surface here uses, so the handler and its route
// tier are testable with no database.
type fakeDiagnosisReader struct{ byRef map[string]trace.SessionDiagnosis }

func (f fakeDiagnosisReader) Diagnosis(_ context.Context, ref string) (trace.SessionDiagnosis, error) {
	d, ok := f.byRef[ref]
	if !ok {
		return trace.SessionDiagnosis{}, trace.ErrDiagnosisNotFound
	}
	return d, nil
}

// contradictedClaim is the recorded A2 case, verbatim in shape: the model proposes a restart while holding a
// GROUNDED observation that the guest was stopped deliberately, plus one supporting assertion whose cited id
// the orchestrator never captured (a fabricated citation, which must stay visible as uncited).
func contradictedClaim(ref string) trace.SessionDiagnosis {
	return trace.SessionDiagnosis{
		ExternalRef: ref,
		RootCause:   "guest 101 is down because its unit failed to start after an unclean shutdown",
		Mechanism:   "systemd gave up after 3 restart attempts inside 60s",
		Supporting: []trace.DiagnosisRef{
			{ID: "incident-history-101", Claim: "two prior unclean shutdowns on this guest", Cited: true},
			{ID: "unit-config-101", Claim: "the unit restarts on boot", Cited: false},
		},
		Contradicting: []trace.DiagnosisRef{
			{ID: "pve-task-history-101", Claim: "root@pam ran vzstop on 101 four minutes before the alert", Cited: true},
		},
		RuledOut: []trace.DiagnosisAlternative{
			{Cause: "host out of memory", Reason: "the node reports 41% in use", ID: "host-metrics", Cited: true},
		},
	}
}

// TG-201 — THE CLAIM READ SITS AT THE ELEVATED TRACE-READ TIER, PINNED BY A TEST.
//
// The body this route serves is the agent's reasoning quoted against screened host output: the same class of
// content as the walk (/v1/sessions/{ref}) and its evidence citation, both of which are AuthTraceRead. Handing
// the claim AuthReadOnly would make it a way around the tracer's own authority — a plain console session could
// read the reasoning the tracer itself refuses it. An authz level with no test is a comment; it survives
// exactly until the next refactor moves the line.
//
// It drives the REAL auth.Router through the REAL httpapi.Register, because a handler-level test cannot see a
// tier at all — the tier lives in the route table, not in the handler.
//
// KILLING MUTATION (executed 2026-08-04): change router.go to auth.AuthReadOnly on this route. RED —
// "a PLAIN read-only operator session was served the recorded claim: got 200, want 403". Restored ⇒ green.
func TestSessionDiagnosisRequiresElevatedTraceReadTier(t *testing.T) {
	const (
		elevated = "/v1/sessions/{external_ref}/diagnosis" // the route under test
		control  = "/v1/sessions"                          // the session INDEX, legitimately AuthReadOnly
	)
	const ref = "ext-1"

	store := &roleGrantingSessionStore{MemSessionStore: auth.NewMemSessionStore()}
	ops := auth.MemOperators{"alice": {Name: "alice", TokenSHA256: sha256.Sum256([]byte("t0ken"))}}
	sa, err := auth.NewSessionAuthenticator([]byte(strings.Repeat("k", 32)), store, ops, time.Hour)
	if err != nil {
		t.Fatalf("session authenticator: %v", err)
	}
	sa.Secure = false // httptest serves plain HTTP; a Secure cookie would never come back
	v := &auth.Verifier{}
	v.EnableBrowserSessions(sa)

	rt := auth.NewRouter(v)
	Register(rt, Deps{
		Sessions:             sa,
		SessionsRead:         spineOf(3),
		SessionDetailRead:    fakeDetailReader{byRef: map[string]trace.SessionTrace{ref: sampleTrace(ref)}},
		SessionDiagnosisRead: fakeDiagnosisReader{byRef: map[string]trace.SessionDiagnosis{ref: contradictedClaim(ref)}},
	})

	// VACUITY FLOOR. Every assertion below reads a STATUS CODE, and a route that does not exist answers 404 to
	// everyone — which is "not asked", not "refused". If this pattern is renamed or removed, say so in those
	// words instead of grading a 404 against a 403 it happens not to equal.
	found := map[string]bool{elevated: false, control: false}
	for _, dr := range rt.DeclaredRoutes() {
		if _, watched := found[dr.Pattern]; watched && dr.Method == http.MethodGet {
			found[dr.Pattern] = true
		}
	}
	for pattern, seen := range found {
		if !seen {
			t.Fatalf("VACUITY FLOOR: no GET %s is registered by httpapi.Register — this test would be asserting "+
				"a tier for a route that does not exist and would pass by matching nothing. Declared routes: %s",
				pattern, declaredPatterns(rt))
		}
	}

	srv := httptest.NewServer(rt.Mux())
	defer srv.Close()

	cookie, _, err := sa.Login(context.Background(), "alice", "t0ken", "192.0.2.1:1234")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	url := srv.URL + "/v1/sessions/" + ref + "/diagnosis"

	// 1. THE REFUSAL. A plain read-only console session must not reach the agent's reasoning.
	code, body := authzGet(t, url, cookie)
	if code != http.StatusForbidden {
		t.Fatalf("a PLAIN read-only operator session was served the recorded claim: got %d, want 403 — the claim "+
			"is a detail OF the walk (AuthTraceRead) and quotes screened host output, so a weaker gate here is a "+
			"way around the tracer's own authority", code)
	}
	// Refused BY THE TRACE-READ GATE specifically: a 403 from some other guard would be the right number for
	// the wrong reason and would keep passing while the tier drifted back down.
	if !strings.Contains(body, "trace-read") {
		t.Fatalf("403 body = %q, want the trace-read refusal", strings.TrimSpace(body))
	}

	// 2. THE CONTROL. The SAME cookie is admitted to the AuthReadOnly session index, so the refusal above is
	// the TIER and not a broken session (which would refuse everything equally).
	if code, _ := authzGet(t, srv.URL+control, cookie); code != http.StatusOK {
		t.Fatalf("the read-only tier was refused at %s (%d) — the session is not valid, so the 403 above proves "+
			"nothing about this route's tier", control, code)
	}

	// 3. THE ADMIT SIDE. Once the session holds admin standing (LDAP tg-admins, what AuthTraceRead recognises)
	// it IS served the claim. Without this the route could be raised to a tier no human can satisfy — "secure"
	// by being dead, which on this surface means the claim is unreadable again and the ticket is undone.
	id, _, _ := strings.Cut(cookie.Value, ".")
	store.grant(id)
	code, body = authzGet(t, url, cookie)
	if code != http.StatusOK {
		t.Fatalf("an ADMIN-ELIGIBLE session was refused the recorded claim: got %d, want 200 — the tier must be "+
			"reachable by the operator who has to check the agent's reasoning", code)
	}
	// The 200 must be the REAL body: a fail-closed 503 or an empty object would also mean "auth let me past"
	// while proving nothing was served.
	var dto SessionDiagnosisDTO
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatalf("elevated 200 body did not decode as SessionDiagnosisDTO (%v): %q", err, strings.TrimSpace(body))
	}
	if !dto.Contradicted || len(dto.Contradicting) != 1 {
		t.Fatalf("elevated read served contradicted=%v with %d contradicting refs, want true/1 — the one fact "+
			"this surface exists to carry did not reach the caller", dto.Contradicted, len(dto.Contradicting))
	}

	// 4. FAIL-CLOSED. No credential at all is 401 before the handler (INV-01) — never 200, never 404.
	if code, _ := authzGet(t, url, nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET %s: got %d, want 401", url, code)
	}
}

// KILLING MUTATION (executed): drop the Contradicting lane from sessionDiagnosisDTO (or filter it to cited-only
// refs, the plausible "show grounded evidence" variant). RED — "the projection dropped the CONTRADICTING
// evidence": the console then renders a claim that looks unopposed while the store holds an observation
// against it, which is the A2 failure with a nicer type on top.
func TestTheProjectionCarriesTheContradictionAndTheUncitedAssertion(t *testing.T) {
	dto := ProjectSessionDiagnosis(contradictedClaim("ext-1"))

	if len(dto.Contradicting) != 1 || !strings.Contains(dto.Contradicting[0].Claim, "vzstop") {
		t.Fatalf("the projection dropped the CONTRADICTING evidence (%+v) — an operator reading this claim would "+
			"never learn the agent held a grounded observation against its own root cause", dto.Contradicting)
	}
	if !dto.Contradicted {
		t.Fatal("contradicted=false on a claim carrying a CITED contradicting ref — the console renders its " +
			"marker from this field, so the contradiction becomes something the operator must notice by reading")
	}
	// The ungrounded supporting assertion must survive the projection AND stay marked. Filtering it out is the
	// plausible-looking mistake: it hides that the model asserted something it could not ground, which is the
	// most interesting row on the screen.
	if len(dto.Supporting) != 2 {
		t.Fatalf("supporting refs = %d, want 2 — an uncited assertion was dropped rather than marked", len(dto.Supporting))
	}
	if dto.Supporting[1].Cited {
		t.Fatal("an assertion whose id the orchestrator never captured was projected as CITED — that is a " +
			"fabricated citation promoted to evidence, exactly what INV-11 exists to prevent")
	}
	if dto.Uncited != 1 {
		t.Fatalf("uncited=%d, want 1 — the console's \"N uncited assertions\" chip is served from here", dto.Uncited)
	}
}

// A session that recorded NO claim is 404, never a 200 carrying an empty claim: "the agent recorded no typed
// claim" and "the agent asserted nothing" are different facts about the estate, and the console renders them
// differently on purpose.
//
// KILLING MUTATION (executed): return the zero-value record instead of mapping ErrDiagnosisNotFound to 404.
// RED — the handler answers 200 with an empty claim, which reads on screen as an agent that concluded nothing.
func TestASessionWithNoRecordedClaimIs404NotAnEmptyClaim(t *testing.T) {
	d := Deps{
		SessionDetailRead:    fakeDetailReader{byRef: map[string]trace.SessionTrace{"ext-1": sampleTrace("ext-1")}},
		SessionDiagnosisRead: fakeDiagnosisReader{byRef: map[string]trace.SessionDiagnosis{}},
	}
	rr := httptest.NewRecorder()
	d.sessionDiagnosisHandler(rr, withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1/diagnosis", nil), "ext-1"), auth.Principal{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("a session with no recorded claim answered %d (body %q), want 404 — a 200 here puts an empty "+
			"claim on the console, which reads as \"the agent asserted nothing\"", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
}

// A nil reader fails closed to 503 on THIS route only — the correct degradation for a deployment that runs the
// tracer without the claim store (every session recorded before migration 0056 is exactly that).
func TestClaimReadFailsClosedWithoutAReader(t *testing.T) {
	rr := httptest.NewRecorder()
	Deps{}.sessionDiagnosisHandler(rr, withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/ext-1/diagnosis", nil), "ext-1"), auth.Principal{})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil claim reader = %d, want 503", rr.Code)
	}
}

// An unknown/unauthorized session is 404 BEFORE the claim store is touched. The store is keyed by
// external_ref and knows nothing about principals, so reading it first would serve any session's claim to
// anyone permitted to read ANY session.
func TestAnUnknownSessionNeverReachesTheClaimStore(t *testing.T) {
	reached := false
	d := Deps{
		SessionDetailRead:    fakeDetailReader{byRef: map[string]trace.SessionTrace{}},
		SessionDiagnosisRead: spyDiagnosisReader{onRead: func() { reached = true }},
	}
	rr := httptest.NewRecorder()
	d.sessionDiagnosisHandler(rr, withExternalRef(httptest.NewRequest(http.MethodGet, "/v1/sessions/nope/diagnosis", nil), "nope"), auth.Principal{})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown session claim = %d, want 404", rr.Code)
	}
	if reached {
		t.Fatal("the claim store was read for a session the authority-bearing reader refused — the walk's " +
			"authorization is bypassed, so any readable session id would expose any other session's claim")
	}
}

type spyDiagnosisReader struct{ onRead func() }

func (s spyDiagnosisReader) Diagnosis(context.Context, string) (trace.SessionDiagnosis, error) {
	s.onRead()
	return trace.SessionDiagnosis{}, nil
}
