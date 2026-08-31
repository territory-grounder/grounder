package servicenow

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

// fakeDoer records every request and returns canned responses: an In Progress (state "2") incident for
// GET, 200 for mutations. It drives the module's REAL request-building path against a fake ServiceNow
// Table API, so the oracle can assert the exact vendor-correct request (verb, path, auth scheme, body)
// without a live instance.
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

// sys_id is ServiceNow's opaque 32-char correlation key; the module keys every verb on it (INV-05).
const testSysID = "1c832d9f4f411200adf9f8e18110c7b1"

// incident.state is NUMERIC in ServiceNow (returned as a string): "1" New, "2" In Progress, "6" Resolved.
const incidentJSON = `{"result":{"sys_id":"1c832d9f4f411200adf9f8e18110c7b1","short_description":"VPN gateway unreachable","state":"2"}}`

const testUsername = "grounder.bot"

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_SN_TOKEN", "s3cr3t") // instance password, supplied only via a secret reference (INV-13).
	f := &fakeDoer{getRet: incidentJSON}
	m := New("https://dev12345.service-now.com/", testUsername, config.SecretRef("env:TG_TEST_SN_TOKEN"), WithHTTPClient(f))
	return m, f
}

// wantBasicAuth is the vendor-correct Authorization header: HTTP Basic base64(username:password). A bare
// "Bearer <token>" (the bug this locks) 401s on every ServiceNow Table API request.
func wantBasicAuth(t *testing.T) string {
	t.Helper()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(testUsername+":s3cr3t"))
}

func TestOpenCorrelatesBySysIDAndUsesBasicAuth(t *testing.T) {
	m, f := newFixture(t)
	iss, err := m.Open(context.Background(), testSysID)
	if err != nil {
		t.Fatalf("Open must succeed: %v", err)
	}
	if iss.ID != testSysID { // the correlation key is the incident sys_id (INV-05)
		t.Errorf("Issue.ID = %q, want %q", iss.ID, testSysID)
	}
	if iss.State != tracker.StateInProgress { // numeric state "2" folds to In Progress
		t.Errorf("Issue.State = %q, want in_progress", iss.State)
	}
	if iss.Title != "VPN gateway unreachable" {
		t.Errorf("Issue.Title = %q", iss.Title)
	}
	if got := f.reqs[0].method; got != http.MethodGet {
		t.Errorf("read verb = %q, want GET", got)
	}
	if got := f.reqs[0].path; got != "/api/now/table/incident/"+testSysID {
		t.Errorf("read path = %q, want /api/now/table/incident/%s", got, testSysID)
	}
	// REGRESSION: ServiceNow Table API auth is HTTP Basic base64(username:password), never a bare Bearer.
	if got := f.reqs[0].auth; got != wantBasicAuth(t) {
		t.Errorf("Authorization = %q, want HTTP Basic base64(username:password) %q", got, wantBasicAuth(t))
	}
	if strings.HasPrefix(f.reqs[0].auth, "Bearer ") {
		t.Errorf("Authorization must not be a bare Bearer token (that 401s on ServiceNow): %q", f.reqs[0].auth)
	}
}

// TestTransitionStatePatchesNumericState locks the vendor protocol for a state change: a PATCH to the
// incident record whose body sets the NUMERIC state code (not a label), authenticated with HTTP Basic.
func TestTransitionStatePatchesNumericState(t *testing.T) {
	m, f := newFixture(t)
	if err := m.TransitionState(context.Background(), testSysID, tracker.StateResolved); err != nil {
		t.Fatalf("TransitionState must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPatch || r.path != "/api/now/table/incident/"+testSysID {
		t.Errorf("transition request = %s %s, want PATCH /api/now/table/incident/%s", r.method, r.path, testSysID)
	}
	// Resolved maps to the numeric code "6" — carried as a JSON string, ServiceNow's on-wire shape.
	if !strings.Contains(r.body, `"state":"6"`) {
		t.Errorf("transition body must set numeric state \"6\" (Resolved): %s", r.body)
	}
	if r.auth != wantBasicAuth(t) {
		t.Errorf("transition must use Basic auth, got %q", r.auth)
	}
}

// TestConfiguredStateCodesRoundTrip proves a customized instance's incident.state codes are honored on BOTH
// the write and read paths (config-not-code): the deployment's resolved code is PATCHed, its in-progress
// code is read back, and an un-overridden state keeps the ITSM default. Without this, a customized instance
// PATCHes a code its state model lacks and every close-out silently fails.
func TestConfiguredStateCodesRoundTrip(t *testing.T) {
	t.Setenv("TG_TEST_SN_TOKEN", "s3cr3t")
	f := &fakeDoer{getRet: incidentJSON}
	// Custom scheme: resolved=3, in-progress=18; open left empty ⇒ keeps the default "1".
	m := New("https://dev12345.service-now.com/", testUsername, config.SecretRef("env:TG_TEST_SN_TOKEN"),
		WithHTTPClient(f), WithStates("18", "3", ""))

	// Write: Resolved PATCHes the configured code "3".
	if err := m.TransitionState(context.Background(), testSysID, tracker.StateResolved); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	if !strings.Contains(f.reqs[len(f.reqs)-1].body, `"state":"3"`) {
		t.Errorf("configured resolved code 3 must be PATCHed, got: %s", f.reqs[len(f.reqs)-1].body)
	}
	// Write: Open keeps the default code "1".
	if err := m.TransitionState(context.Background(), testSysID, tracker.StateOpen); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	if !strings.Contains(f.reqs[len(f.reqs)-1].body, `"state":"1"`) {
		t.Errorf("un-overridden open must keep default code 1, got: %s", f.reqs[len(f.reqs)-1].body)
	}
	// Read: the configured in-progress code "18" folds back to In Progress.
	f.getRet = `{"result":{"short_description":"x","state":"18"}}`
	iss, err := m.Read(context.Background(), testSysID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if iss.State != tracker.StateInProgress {
		t.Errorf("configured in-progress code 18 must read back as in_progress, got %q", iss.State)
	}
}

// TestStateNumericCodeMapping asserts the tracker State -> ServiceNow numeric-code mapping end to end,
// observed on the wire in the PATCH body, for every state the session lifecycle transitions through:
// New "1", In Progress "2", Resolved "6".
func TestStateNumericCodeMapping(t *testing.T) {
	cases := []struct {
		state tracker.State
		code  string
	}{
		{tracker.StateOpen, "1"},
		{tracker.StateInProgress, "2"},
		{tracker.StateResolved, "6"},
	}
	for _, c := range cases {
		m, f := newFixture(t)
		if err := m.TransitionState(context.Background(), testSysID, c.state); err != nil {
			t.Fatalf("TransitionState(%s) must succeed: %v", c.state, err)
		}
		r := f.reqs[len(f.reqs)-1]
		if want := `"state":"` + c.code + `"`; !strings.Contains(r.body, want) {
			t.Errorf("state %s -> body %s, want it to contain %s", c.state, r.body, want)
		}
	}
}

// TestReadFoldsNumericStateToEnum asserts the inverse mapping: the NUMERIC code on the incident record
// read back folds onto the tracker-agnostic enum, with an unknown code folding to the least-progressed
// state (Open) rather than silently claiming resolution.
func TestReadFoldsNumericStateToEnum(t *testing.T) {
	cases := []struct {
		code string
		want tracker.State
	}{
		{"1", tracker.StateOpen},
		{"2", tracker.StateInProgress},
		{"6", tracker.StateResolved},
		{"7", tracker.StateResolved}, // Closed folds to Resolved
		{"99", tracker.StateOpen},    // unrecognized -> least-progressed
		{"", tracker.StateOpen},
	}
	for _, c := range cases {
		m, f := newFixture(t)
		f.getRet = `{"result":{"short_description":"x","state":"` + c.code + `"}}`
		iss, err := m.Read(context.Background(), testSysID)
		if err != nil {
			t.Fatalf("Read(state=%q) must succeed: %v", c.code, err)
		}
		if iss.State != c.want {
			t.Errorf("numeric state %q -> %q, want %q", c.code, iss.State, c.want)
		}
	}
}

// TestCommentPatchesWorkNotes locks the audit-sink protocol: a comment is appended to the incident's
// work_notes via a PATCH to the record, authenticated with HTTP Basic.
func TestCommentPatchesWorkNotes(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Comment(context.Background(), testSysID, "auto-resolved: service restored"); err != nil {
		t.Fatalf("Comment must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPatch || r.path != "/api/now/table/incident/"+testSysID {
		t.Errorf("comment request = %s %s, want PATCH /api/now/table/incident/%s", r.method, r.path, testSysID)
	}
	if !strings.Contains(r.body, `"work_notes":"auto-resolved: service restored"`) {
		t.Errorf("comment must append to work_notes: %s", r.body)
	}
	if r.auth != wantBasicAuth(t) {
		t.Errorf("comment must use Basic auth, got %q", r.auth)
	}
}

func TestNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 401 // the exact failure the bare-Bearer auth bug produced on the live instance.
	if _, err := m.Read(context.Background(), testSysID); err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
}

func TestEmptyIDRejected(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.Read(context.Background(), ""); err == nil {
		t.Fatal("an empty issue id must be rejected on read")
	}
	if _, err := m.Open(context.Background(), ""); err == nil {
		t.Fatal("an empty issue id must be rejected on open")
	}
	if err := m.TransitionState(context.Background(), "", tracker.StateResolved); err == nil {
		t.Fatal("an empty issue id must be rejected on transition")
	}
	if err := m.Comment(context.Background(), "", "x"); err == nil {
		t.Fatal("an empty issue id must be rejected on comment")
	}
}
