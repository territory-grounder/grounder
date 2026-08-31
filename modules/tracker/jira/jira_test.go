package jira

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/config"
)

type recordedReq struct {
	method, path, auth string
	body               string
}

// fakeDoer records every request and returns canned responses: an In Progress issue for GET, 200 for
// POST. It drives the module's REAL request-building path against a fake Jira Cloud, so the oracle can
// assert the exact vendor-correct request (verb, path, auth scheme, body) without a live API.
type fakeDoer struct {
	reqs   []recordedReq
	getRet string
	status int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.reqs = append(f.reqs, recordedReq{method: req.Method, path: req.URL.Path, auth: req.Header.Get("Authorization"), body: body})
	st := f.status
	if st == 0 {
		st = 200
	}
	resp := ""
	if req.Method == http.MethodGet {
		resp = f.getRet
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

const issueJSON = `{"key":"PROJ-123","fields":{"summary":"Login returns 500 on SSO callback","status":{"name":"In Progress"}}}`

const testEmail = "grounder-bot@example.com"

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_JIRA_TOKEN", "s3cr3t") // API token, supplied only via a secret reference (INV-13).
	f := &fakeDoer{getRet: issueJSON}
	m := New("https://acme.atlassian.net/", testEmail, config.SecretRef("env:TG_TEST_JIRA_TOKEN"), WithHTTPClient(f))
	return m, f
}

// wantBasicAuth is the vendor-correct Authorization header: HTTP Basic base64(email:api_token). A bare
// "Bearer <token>" (the bug this locks) 401s on every Jira Cloud request.
func wantBasicAuth(t *testing.T) string {
	t.Helper()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(testEmail+":s3cr3t"))
}

func TestOpenCorrelatesByIssueIDAndUsesBasicAuth(t *testing.T) {
	m, f := newFixture(t)
	iss, err := m.Open(context.Background(), "PROJ-123")
	if err != nil {
		t.Fatalf("Open must succeed: %v", err)
	}
	if iss.ID != "PROJ-123" { // the correlation key is the issue key (INV-05)
		t.Errorf("Issue.ID = %q, want PROJ-123", iss.ID)
	}
	if iss.State != tracker.StateInProgress {
		t.Errorf("Issue.State = %q, want in_progress", iss.State)
	}
	if iss.Title != "Login returns 500 on SSO callback" {
		t.Errorf("Issue.Title = %q", iss.Title)
	}
	if got := f.reqs[0].path; got != "/rest/api/2/issue/PROJ-123" {
		t.Errorf("read path = %q, want /rest/api/2/issue/PROJ-123", got)
	}
	// REGRESSION: Jira Cloud API-token auth is HTTP Basic base64(email:api_token), never a bare Bearer.
	if got := f.reqs[0].auth; got != wantBasicAuth(t) {
		t.Errorf("Authorization = %q, want HTTP Basic base64(email:token) %q", got, wantBasicAuth(t))
	}
	if strings.HasPrefix(f.reqs[0].auth, "Bearer ") {
		t.Errorf("Authorization must not be a bare Bearer token (that 401s on Jira Cloud): %q", f.reqs[0].auth)
	}
}

func TestTransitionStatePostsTransitionID(t *testing.T) {
	m, f := newFixture(t)
	if err := m.TransitionState(context.Background(), "PROJ-123", tracker.StateResolved); err != nil {
		t.Fatalf("TransitionState must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	// Jira moves state via the transition-id flow: POST /issue/{key}/transitions {"transition":{"id":"31"}}.
	if r.method != http.MethodPost || r.path != "/rest/api/2/issue/PROJ-123/transitions" {
		t.Errorf("transition request = %s %s, want POST /rest/api/2/issue/PROJ-123/transitions", r.method, r.path)
	}
	if !strings.Contains(r.body, `"transition"`) || !strings.Contains(r.body, `"id":"`+transitionResolved+`"`) {
		t.Errorf("transition body must carry transition id %q: %s", transitionResolved, r.body)
	}
	if r.auth != wantBasicAuth(t) {
		t.Errorf("transition must use Basic auth, got %q", r.auth)
	}
}

// TestTransitionStateHonoursConfiguredScheme proves the deployment's own workflow transition ids are
// POSTed (config-not-code) — not the reference defaults — and that an empty override for one state keeps
// that state's default. A real Jira workflow rarely uses 21/31/11, so a hardcoded id would 404 every
// close-out; this locks the operator-declared scheme onto the wire.
func TestTransitionStateHonoursConfiguredScheme(t *testing.T) {
	t.Setenv("TG_TEST_JIRA_TOKEN", "s3cr3t")
	f := &fakeDoer{getRet: issueJSON}
	// Deployment scheme: custom resolved + in-progress ids; open left empty ⇒ keeps the reference default.
	m := New("https://acme.atlassian.net/", testEmail, config.SecretRef("env:TG_TEST_JIRA_TOKEN"),
		WithHTTPClient(f), WithTransitions("501", "911", ""))

	if err := m.TransitionState(context.Background(), "PROJ-123", tracker.StateResolved); err != nil {
		t.Fatalf("TransitionState must succeed: %v", err)
	}
	if body := f.reqs[len(f.reqs)-1].body; !strings.Contains(body, `"id":"911"`) {
		t.Errorf("configured resolved transition id 911 must be POSTed, got: %s", body)
	}

	if err := m.TransitionState(context.Background(), "PROJ-123", tracker.StateInProgress); err != nil {
		t.Fatalf("TransitionState must succeed: %v", err)
	}
	if body := f.reqs[len(f.reqs)-1].body; !strings.Contains(body, `"id":"501"`) {
		t.Errorf("configured in-progress transition id 501 must be POSTed, got: %s", body)
	}

	// Open was not overridden ⇒ the reference default id still applies.
	if err := m.TransitionState(context.Background(), "PROJ-123", tracker.StateOpen); err != nil {
		t.Fatalf("TransitionState must succeed: %v", err)
	}
	if body := f.reqs[len(f.reqs)-1].body; !strings.Contains(body, `"id":"`+transitionOpen+`"`) {
		t.Errorf("un-overridden open state must keep the reference default %q, got: %s", transitionOpen, body)
	}
}

func TestCommentPostsTerminalSink(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Comment(context.Background(), "PROJ-123", "auto-resolved: service restored"); err != nil {
		t.Fatalf("Comment must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPost || r.path != "/rest/api/2/issue/PROJ-123/comment" {
		t.Errorf("comment request = %s %s, want POST /rest/api/2/issue/PROJ-123/comment", r.method, r.path)
	}
	if !strings.Contains(r.body, `"body":"auto-resolved: service restored"`) {
		t.Errorf("comment body missing text: %s", r.body)
	}
	if r.auth != wantBasicAuth(t) {
		t.Errorf("comment must use Basic auth, got %q", r.auth)
	}
}

func TestNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 401 // the exact failure the auth bug produced on the live API.
	if _, err := m.Read(context.Background(), "PROJ-404"); err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
}

func TestEmptyIDRejected(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.Read(context.Background(), ""); err == nil {
		t.Fatal("an empty issue id must be rejected")
	}
	if err := m.TransitionState(context.Background(), "", tracker.StateResolved); err == nil {
		t.Fatal("an empty issue id must be rejected on transition")
	}
	if err := m.Comment(context.Background(), "", "x"); err == nil {
		t.Fatal("an empty issue id must be rejected on comment")
	}
}
