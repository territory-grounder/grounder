package githubissues

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/config"
)

type recordedReq struct {
	method, path, auth, accept, contentType string
	body                                    string
}

// fakeDoer records every request and returns canned responses: an open issue for GET, 200 for
// PATCH/POST. It drives the module's REAL request-building path against a fake GitHub REST API, so the
// oracle can assert the exact vendor-correct request (verb, path, Bearer auth, Accept, JSON body) without
// a live API.
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
	f.reqs = append(f.reqs, recordedReq{
		method:      req.Method,
		path:        req.URL.Path,
		auth:        req.Header.Get("Authorization"),
		accept:      req.Header.Get("Accept"),
		contentType: req.Header.Get("Content-Type"),
		body:        body,
	})
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

// An open issue #1347 in the ACTUAL GitHub shape (top-level number/title/state); folds to Open.
const issueJSON = `{"number":1347,"title":"Login returns 500 on SSO callback","state":"open"}`

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_GH_TOKEN", "s3cr3t") // fine-grained PAT, supplied only via a secret reference (INV-13).
	f := &fakeDoer{getRet: issueJSON}
	m := New("https://api.github.com/", "org", "repo", config.SecretRef("env:TG_TEST_GH_TOKEN"), WithHTTPClient(f))
	return m, f
}

// assertGitHubHeaders locks the two protocol-mandated request headers: a Bearer token (never a literal)
// and the GitHub media-type Accept. Every verb must carry them.
func assertGitHubHeaders(t *testing.T, r recordedReq) {
	t.Helper()
	if r.auth != "Bearer s3cr3t" { // the request was authenticated with the resolved bearer token (INV-13).
		t.Errorf("Authorization = %q, want Bearer s3cr3t", r.auth)
	}
	if r.accept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want application/vnd.github+json", r.accept)
	}
}

func TestOpenCorrelatesByIssueIDAndAddressesRepoResource(t *testing.T) {
	m, f := newFixture(t)
	iss, err := m.Open(context.Background(), "1347")
	if err != nil {
		t.Fatalf("Open must succeed: %v", err)
	}
	if iss.ID != "1347" { // the correlation key is the issue number passed in, echoed unchanged (INV-05).
		t.Errorf("Issue.ID = %q, want 1347", iss.ID)
	}
	if iss.State != tracker.StateOpen { // open -> Open
		t.Errorf("Issue.State = %q, want open", iss.State)
	}
	if iss.Title != "Login returns 500 on SSO callback" {
		t.Errorf("Issue.Title = %q", iss.Title)
	}
	r := f.reqs[0]
	// GitHub reads a single issue at GET /repos/{owner}/{repo}/issues/{number}: the right resource, keyed
	// by owner/repo/number, must be addressed.
	if r.method != http.MethodGet || r.path != "/repos/org/repo/issues/1347" {
		t.Errorf("read request = %s %s, want GET /repos/org/repo/issues/1347", r.method, r.path)
	}
	assertGitHubHeaders(t, r)
}

// TestTransitionStateMapsThreeStatesOntoOpenClosed is the heart of the GitHub adapter: GitHub Issues has
// only open|closed, so the three tracker States must fold onto that binary exactly. Resolved closes the
// issue; Open and (the un-representable) InProgress both reopen it. Each transition is a
// PATCH /repos/{owner}/{repo}/issues/{number} with a {"state":...} body — the vendor state-change contract.
func TestTransitionStateMapsThreeStatesOntoOpenClosed(t *testing.T) {
	cases := []struct {
		state     tracker.State
		wantState string
	}{
		{tracker.StateResolved, "closed"}, // resolved -> closed (the only close path)
		{tracker.StateOpen, "open"},       // open -> open
		{tracker.StateInProgress, "open"}, // GitHub has no in-progress: fold to open, never falsely close
	}
	for _, tc := range cases {
		t.Run(string(tc.state), func(t *testing.T) {
			m, f := newFixture(t)
			if err := m.TransitionState(context.Background(), "1347", tc.state); err != nil {
				t.Fatalf("TransitionState must succeed: %v", err)
			}
			r := f.reqs[len(f.reqs)-1]
			// GitHub changes issue state with PATCH (not POST) on the issue resource itself.
			if r.method != http.MethodPatch || r.path != "/repos/org/repo/issues/1347" {
				t.Errorf("transition request = %s %s, want PATCH /repos/org/repo/issues/1347", r.method, r.path)
			}
			if r.body != `{"state":"`+tc.wantState+`"}` {
				t.Errorf("transition body = %s, want exactly {\"state\":%q}", r.body, tc.wantState)
			}
			// A resolved transition must NEVER emit open, and a non-resolved one must NEVER emit closed.
			otherState := "open"
			if tc.wantState == "open" {
				otherState = "closed"
			}
			if strings.Contains(r.body, `"state":"`+otherState+`"`) {
				t.Errorf("%s must not map to state %q: %s", tc.state, otherState, r.body)
			}
			if r.contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", r.contentType)
			}
			assertGitHubHeaders(t, r)
		})
	}
}

func TestCommentPostsTerminalSinkToCommentsCollection(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Comment(context.Background(), "1347", "auto-resolved: service restored"); err != nil {
		t.Fatalf("Comment must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	// GitHub creates a comment with POST /repos/{owner}/{repo}/issues/{number}/comments {"body":...}.
	if r.method != http.MethodPost || r.path != "/repos/org/repo/issues/1347/comments" {
		t.Errorf("comment request = %s %s, want POST /repos/org/repo/issues/1347/comments", r.method, r.path)
	}
	if r.body != `{"body":"auto-resolved: service restored"}` {
		t.Errorf("comment body = %s, want {\"body\":\"auto-resolved: service restored\"}", r.body)
	}
	assertGitHubHeaders(t, r)
}

// TestReadFoldsClosedToResolved locks the GET-side state fold that mirrors the transition mapping: a
// closed GitHub issue reads back as Resolved, so the state round-trips through the tracker enum.
func TestReadFoldsClosedToResolved(t *testing.T) {
	m, f := newFixture(t)
	f.getRet = `{"number":1347,"title":"Found a bug","state":"closed"}`
	iss, err := m.Read(context.Background(), "1347")
	if err != nil {
		t.Fatalf("Read must succeed: %v", err)
	}
	if iss.State != tracker.StateResolved { // closed -> Resolved
		t.Errorf("closed issue folds to %q, want resolved", iss.State)
	}
}

func TestNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 404
	if _, err := m.Read(context.Background(), "999999"); err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
}

func TestEmptyIDRejected(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.Read(context.Background(), ""); err == nil {
		t.Fatal("an empty issue id must be rejected on read")
	}
	if err := m.TransitionState(context.Background(), "", tracker.StateResolved); err == nil {
		t.Fatal("an empty issue id must be rejected on transition")
	}
	if err := m.Comment(context.Background(), "", "x"); err == nil {
		t.Fatal("an empty issue id must be rejected on comment")
	}
}
