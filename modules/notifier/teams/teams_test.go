package teams

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

type recordedReq struct {
	method, path, auth string
	body               string
}

// fakeDoer records every request (method, wire path, auth header, body) and returns a canned Bot Framework
// send-activity response. It drives the module's REAL request-building path against a fake bot service, so
// the oracle asserts the exact vendor-correct request without a live API.
type fakeDoer struct {
	reqs   []recordedReq
	status int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	// EscapedPath() is the on-the-wire path, so a mandatory {conversationId} segment and its percent-escaping
	// are both observable exactly as a real Teams service would receive them.
	f.reqs = append(f.reqs, recordedReq{method: req.Method, path: req.URL.EscapedPath(), auth: req.Header.Get("Authorization"), body: body})
	st := f.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(`{"id":"1616000000002"}`)), Header: make(http.Header)}, nil
}

const testConversationID = "19:abc123def456@thread.tacv2"

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_TEAMS_TOKEN", "bf_secret")
	f := &fakeDoer{}
	m := New("https://teams.example", testConversationID, config.SecretRef("env:TG_TEST_TEAMS_TOKEN"),
		[]string{"29:abc", "29:lead"}, WithHTTPClient(f))
	return m, f
}

// TestNotifyPostsToMandatoryConversationActivitiesRoute is the regression for the reported HIGH bug: the
// Bot Framework send-activity route is POST /v3/conversations/{conversationId}/activities and the
// {conversationId} segment is MANDATORY. The buggy path "/v3/conversations/activities" 404s and delivers
// nothing. This locks the vendor-correct verb, path, auth scheme, redaction, and decision-id round-trip.
func TestNotifyPostsToMandatoryConversationActivitiesRoute(t *testing.T) {
	m, f := newFixture(t)
	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "TG-9#restart",
		Body:       "restart web01; password=hunter2 and token=abcd1234 — page ops@example.com",
		Approval:   true,
	})
	if err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("Notify must issue exactly one request, got %d", len(f.reqs))
	}
	r := f.reqs[0]

	// Verb + the exact vendor-correct route WITH the mandatory conversation id segment.
	want := "/v3/conversations/" + url.PathEscape(testConversationID) + "/activities"
	if r.method != http.MethodPost || r.path != want {
		t.Errorf("request = %s %s, want POST %s", r.method, r.path, want)
	}
	// The specific bug: the conversation-id segment must NOT be dropped.
	if r.path == "/v3/conversations/activities" {
		t.Error("path must not drop the mandatory {conversationId} segment (this is the 404 bug)")
	}
	if !strings.Contains(r.path, testConversationID) {
		t.Errorf("path must carry the destination conversation id, got %q", r.path)
	}

	// Authenticated with the token resolved from its secret reference (INV-13) — Bearer, never a literal.
	if r.auth != "Bearer bf_secret" {
		t.Errorf("Authorization = %q, want Bearer bf_secret", r.auth)
	}

	// Redaction (INV-19): no secret/PII reaches the channel; a redaction marker is present.
	for _, secret := range []string{"hunter2", "abcd1234", "ops@example.com"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("posted body must redact %q, got %s", secret, r.body)
		}
	}
	if !strings.Contains(r.body, "REDACTED") {
		t.Errorf("posted body must contain a redaction marker, got %s", r.body)
	}

	// Decision id round-trips (INV-12): it must reach the channel so a human's reply can bind the vote back.
	if !strings.Contains(r.body, "TG-9#restart") {
		t.Errorf("posted body must carry the decision id so the returned vote binds back, got %s", r.body)
	}
}

// TestNotifyEscapesConversationIDInPath proves the conversation id is url.PathEscape'd, so a reserved
// character in the id cannot be reparsed as URL structure (mirrors matrix escaping its room id).
func TestNotifyEscapesConversationIDInPath(t *testing.T) {
	t.Setenv("TG_TEST_TEAMS_TOKEN", "bf_secret")
	f := &fakeDoer{}
	// A '#' would otherwise be read as a URL fragment delimiter and truncate the path.
	m := New("https://teams.example", "19:conv#frag@thread.tacv2", config.SecretRef("env:TG_TEST_TEAMS_TOKEN"),
		[]string{"29:abc"}, WithHTTPClient(f))
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-1", Body: "ping"}); err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	p := f.reqs[0].path
	if !strings.Contains(p, "%23") {
		t.Errorf("conversation id must be percent-escaped in the path, got %q", p)
	}
	if strings.Contains(p, "conv#frag") {
		t.Errorf("a reserved char must not survive unescaped in the path, got %q", p)
	}
	if !strings.HasSuffix(p, "/activities") {
		t.Errorf("path must still terminate in /activities, got %q", p)
	}
}

// TestNotifyRejectsEmptyDecisionID: a notice with no decision id is rejected before any request is issued.
func TestNotifyRejectsEmptyDecisionID(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Notify(context.Background(), notifier.Notice{Body: "orphan"}); err == nil {
		t.Fatal("a notice with no decision id must be rejected")
	}
	if len(f.reqs) != 0 {
		t.Errorf("no request must be sent for a decision-id-less notice, got %d", len(f.reqs))
	}
}

// TestNotifyNon2xxIsError: a non-2xx from the bot service (e.g. the 404 the buggy route caused) is an error.
func TestNotifyNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 404
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "hi"}); err == nil {
		t.Fatal("a non-2xx bot-service response must surface as an error")
	}
}

// TestResolveVoteBindsToDecisionFromApprover: an approver's reply resolves to a Vote bound to the exact
// decision id it cites and the authenticated sender (INV-12).
func TestResolveVoteBindsToDecisionFromApprover(t *testing.T) {
	m, _ := newFixture(t)
	raw := []byte(`{"from":{"id":"29:abc"},"text":"approve TG-9#restart"}`)
	v, err := m.ResolveVote(context.Background(), raw)
	if err != nil {
		t.Fatalf("an approver vote must resolve: %v", err)
	}
	if v.DecisionID != "TG-9#restart" {
		t.Errorf("vote must bind to the cited decision id, got %q", v.DecisionID)
	}
	if v.Sender != "29:abc" || !v.Approve {
		t.Errorf("vote sender/intent wrong: %+v", v)
	}
}

// TestResolveVoteRejectsNonApproverAndUnboundReply: a vote from outside the approver set, or a reply that
// cites no decision id, is rejected (sender authentication + INV-12 binding).
func TestResolveVoteRejectsNonApproverAndUnboundReply(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.ResolveVote(context.Background(), []byte(`{"from":{"id":"29:intruder"},"text":"approve TG-9#restart"}`)); err == nil {
		t.Fatal("a vote from a non-approver must be rejected")
	}
	if _, err := m.ResolveVote(context.Background(), []byte(`{"from":{"id":"29:abc"},"text":"approve"}`)); err == nil {
		t.Fatal("a reply citing no decision id must be rejected")
	}
}
