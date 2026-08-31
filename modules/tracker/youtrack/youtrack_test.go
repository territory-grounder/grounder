package youtrack

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
	method, path, auth string
	body               string
}

// fakeDoer records every request and returns canned responses: a State=In Progress issue for GET, 200 for
// POST. It lets the oracle drive the module's real request-building path against a fake YouTrack.
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

const issueJSON = `{"idReadable":"TG-5","summary":"Device Down: sw-core-01","customFields":[{"name":"State","value":{"name":"In Progress"}}]}`

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_YT_TOKEN", "s3cr3t")
	f := &fakeDoer{getRet: issueJSON}
	m := New("https://yt.example/", config.SecretRef("env:TG_TEST_YT_TOKEN"), WithHTTPClient(f))
	return m, f
}

func TestOpenCorrelatesByIssueID(t *testing.T) {
	m, f := newFixture(t)
	iss, err := m.Open(context.Background(), "TG-5")
	if err != nil {
		t.Fatalf("Open must succeed: %v", err)
	}
	if iss.ID != "TG-5" { // the correlation key is the issue id (INV-05)
		t.Errorf("Issue.ID = %q, want TG-5", iss.ID)
	}
	if iss.State != tracker.StateInProgress {
		t.Errorf("Issue.State = %q, want in_progress", iss.State)
	}
	if iss.Title != "Device Down: sw-core-01" {
		t.Errorf("Issue.Title = %q", iss.Title)
	}
	// the request was authenticated with the resolved bearer token (never a literal).
	if got := f.reqs[0].auth; got != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want Bearer s3cr3t", got)
	}
}

func TestTransitionStatePostsStateChange(t *testing.T) {
	m, f := newFixture(t)
	if err := m.TransitionState(context.Background(), "TG-5", tracker.StateResolved); err != nil {
		t.Fatalf("TransitionState must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPost || r.path != "/api/issues/TG-5" {
		t.Errorf("transition request = %s %s, want POST /api/issues/TG-5", r.method, r.path)
	}
	if !strings.Contains(r.body, `"name":"Resolved"`) || !strings.Contains(r.body, "StateIssueCustomField") {
		t.Errorf("transition body did not set State=Resolved: %s", r.body)
	}
}

// TestConfiguredStateNamesAndField proves a project's own State bundle values + field name are honored
// (config-not-code): the default bundle has no "Resolved" value (its terminal is "Fixed"), so the write path
// must set the deployment's actual value name on its actual field. Without this, every close-out POSTs a
// value that doesn't exist and no-ops.
func TestConfiguredStateNamesAndField(t *testing.T) {
	t.Setenv("TG_TEST_YT_TOKEN", "s3cr3t")
	// A default-bundle project: resolved value is "Fixed", the state field renamed to "Stage".
	f := &fakeDoer{getRet: `{"idReadable":"TG-5","summary":"x","customFields":[{"name":"Stage","value":{"name":"Fixed"}}]}`}
	m := New("https://yt.example/", config.SecretRef("env:TG_TEST_YT_TOKEN"), WithHTTPClient(f),
		WithStateNames("", "Fixed", ""), WithStateFieldName("Stage"))

	// Write: Resolved sets the configured value "Fixed" on the configured field "Stage".
	if err := m.TransitionState(context.Background(), "TG-5", tracker.StateResolved); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	body := f.reqs[len(f.reqs)-1].body
	if !strings.Contains(body, `"name":"Fixed"`) || !strings.Contains(body, `"name":"Stage"`) {
		t.Errorf("transition must set value=Fixed on field=Stage, got: %s", body)
	}

	// Read: the configured field "Stage" holding "Fixed" folds back to Resolved.
	iss, err := m.Read(context.Background(), "TG-5")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if iss.State != tracker.StateResolved {
		t.Errorf("Fixed on the configured field must read back as resolved, got %q", iss.State)
	}
}

// A dismissed anchor issue (Cancelled / Duplicate) is a TERMINAL/closed state: it must read back as Resolved,
// not Open. Reading it as Open made the live dedup gate treat a genuine re-fire as joining a still-open issue
// and silently suppress it, where the predecessor escalates a fresh incident. Done already worked; this closes
// the asymmetry for the dismissed terminal states.
func TestDismissedTerminalStatesReadAsResolved(t *testing.T) {
	t.Setenv("TG_TEST_YT_TOKEN", "s3cr3t")
	for _, name := range []string{"Cancelled", "Duplicate"} {
		f := &fakeDoer{getRet: `{"idReadable":"INFRA-42","summary":"x","customFields":[{"name":"State","value":{"name":"` + name + `"}}]}`}
		m := New("https://yt.example/", config.SecretRef("env:TG_TEST_YT_TOKEN"), WithHTTPClient(f))
		iss, err := m.Read(context.Background(), "INFRA-42")
		if err != nil {
			t.Fatalf("Read(%s): %v", name, err)
		}
		if iss.State != tracker.StateResolved {
			t.Errorf("a %q anchor is terminal/closed and must read as resolved (so a re-fire escalates), got %q", name, iss.State)
		}
	}
}

func TestCommentPostsTerminalSink(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Comment(context.Background(), "TG-5", "auto-resolved: service restored"); err != nil {
		t.Fatalf("Comment must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPost || r.path != "/api/issues/TG-5/comments" {
		t.Errorf("comment request = %s %s, want POST /api/issues/TG-5/comments", r.method, r.path)
	}
	if !strings.Contains(r.body, "auto-resolved: service restored") {
		t.Errorf("comment body missing text: %s", r.body)
	}
}

func TestNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 404
	if _, err := m.Read(context.Background(), "TG-404"); err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
}

func TestEmptyIDRejected(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.Read(context.Background(), ""); err == nil {
		t.Fatal("an empty issue id must be rejected")
	}
}
