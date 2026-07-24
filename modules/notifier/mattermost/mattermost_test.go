package mattermost

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// approvalsChannelID is the opaque 26-char Mattermost channel ID the module MUST post to. A channel *name*
// (e.g. "tg-approvals") in channel_id is rejected 400 by create-post — the regression this suite locks.
const approvalsChannelID = "kwoybt1n1pn5jgh8qs9x4p3qzo"

type recordedReq struct {
	method, path, auth, ctype, body string
}

// fakeDoer records every request and returns a canned response. It lets the oracle drive the module's real
// request-building path against a fake Mattermost server.
type fakeDoer struct {
	reqs   []recordedReq
	status int
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.reqs = append(f.reqs, recordedReq{
		method: req.Method,
		path:   req.URL.Path,
		auth:   req.Header.Get("Authorization"),
		ctype:  req.Header.Get("Content-Type"),
		body:   body,
	})
	st := f.status
	if st == 0 {
		st = 201 // Mattermost create-post returns 201 Created
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_MM_TOKEN", "s3cr3t")
	f := &fakeDoer{}
	m := New("https://mm.example/", config.SecretRef("env:TG_TEST_MM_TOKEN"),
		[]string{"oncall", "lead"},
		map[string]string{"tg-approvals": approvalsChannelID},
		WithHTTPClient(f))
	return m, f
}

// TestNotifyPostsOpaqueChannelIDAndRedacts is the core regression for the fixed bug: create-post must carry
// the opaque channel ID resolved from the routed name, never the channel name slug (which Mattermost 400s),
// and the body must be credential/PII-redacted before it leaves the process (INV-19).
func TestNotifyPostsOpaqueChannelIDAndRedacts(t *testing.T) {
	m, f := newFixture(t)
	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "TG-9#restart",
		Body:       "restart web01; password=hunter2 and token=abcd1234 — contact ops@example.com",
		Approval:   true,
	})
	if err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPost || r.path != "/api/v4/posts" {
		t.Errorf("create-post request = %s %s, want POST /api/v4/posts", r.method, r.path)
	}
	// authenticated with the resolved bearer token (never a literal, INV-13).
	if r.auth != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want Bearer s3cr3t", r.auth)
	}
	if r.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.ctype)
	}
	// REGRESSION: the opaque 26-char channel id is sent in channel_id...
	if !strings.Contains(r.body, `"channel_id":"`+approvalsChannelID+`"`) {
		t.Errorf("create-post must send the opaque channel id, body=%s", r.body)
	}
	// ...and the channel *name* slug must NEVER reach the wire (create-post rejects a name 400).
	if strings.Contains(r.body, "tg-approvals") {
		t.Errorf("create-post must NOT put a channel name in channel_id, body=%s", r.body)
	}
	for _, secret := range []string{"hunter2", "abcd1234", "ops@example.com"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("posted body must redact %q, got %s", secret, r.body)
		}
	}
	if !strings.Contains(r.body, "[REDACTED]") {
		t.Errorf("posted body must contain redaction markers, got %s", r.body)
	}
}

// TestNotifyFailsClosedWithoutDecisionOrChannel: an empty decision id and an unconfigured channel are both
// rejected, and crucially NO request is sent — the module never falls back to posting a bare channel name.
func TestNotifyFailsClosedWithoutDecisionOrChannel(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Notify(context.Background(), notifier.Notice{Body: "x"}); err == nil {
		t.Fatal("a notice with no decision id must be rejected")
	}
	// "ACME-1" -> channel "acme-approvals", which has no configured opaque id.
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "ACME-1", Body: "x"}); err == nil {
		t.Fatal("an unconfigured channel must be rejected, not posted by name")
	}
	if len(f.reqs) != 0 {
		t.Errorf("no create-post may be issued when routing cannot resolve an opaque id, got %d requests", len(f.reqs))
	}
}

// TestNon2xxCreatePostIsError: a non-2xx create-post (the very failure the bug caused for every post) is an
// error, not a silent success.
func TestNon2xxCreatePostIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 400
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "x"}); err == nil {
		t.Fatal("a non-2xx create-post response must be an error")
	}
}

// TestResolveVoteBindsToDecisionFromApprover: an authenticated approver's reply binds to the exact decision
// id it cites (INV-12) and records the sender/intent.
func TestResolveVoteBindsToDecisionFromApprover(t *testing.T) {
	m, _ := newFixture(t)
	raw := []byte(`{"user_name":"oncall","message":"approve TG-9#restart"}`)
	v, err := m.ResolveVote(context.Background(), raw)
	if err != nil {
		t.Fatalf("an approver vote must resolve: %v", err)
	}
	if v.DecisionID != "TG-9#restart" {
		t.Errorf("vote must bind to the exact decision id, got %q", v.DecisionID)
	}
	if v.Sender != "oncall" || !v.Approve {
		t.Errorf("vote sender/intent wrong: %+v", v)
	}
}

// TestResolveVoteRejectsNonApproverAndUnboundReply: a sender outside the approver set is rejected (INV-12),
// and a reply that cites no decision id is rejected — no vote is ever counted for the wrong decision.
func TestResolveVoteRejectsNonApproverAndUnboundReply(t *testing.T) {
	m, _ := newFixture(t)
	if _, err := m.ResolveVote(context.Background(), []byte(`{"user_name":"intruder","message":"approve TG-9#restart"}`)); err == nil {
		t.Fatal("a vote from a non-approver must be rejected")
	}
	if _, err := m.ResolveVote(context.Background(), []byte(`{"user_name":"oncall","message":"approve"}`)); err == nil {
		t.Fatal("a reply citing no decision id must be rejected")
	}
}
