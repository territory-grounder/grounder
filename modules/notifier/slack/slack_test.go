package slack

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

type recordedReq struct {
	method, path, auth, ctype string
	body                      string
}

// fakeDoer records every request and returns a canned Slack Web API response. By default it returns the
// success envelope ({"ok":true}); tests set status/resp to drive the failure paths. It lets the oracle
// drive the module's real request-building path against a fake Slack.
type fakeDoer struct {
	reqs   []recordedReq
	status int
	resp   string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
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
		st = 200
	}
	r := f.resp
	if r == "" {
		r = `{"ok":true}`
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(r)), Header: make(http.Header)}, nil
}

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_SLACK_TOKEN", "xoxb-secret")
	f := &fakeDoer{}
	m := New("https://slack.example/", config.SecretRef("env:TG_TEST_SLACK_TOKEN"), []string{"U123", "U456"}, WithHTTPClient(f))
	return m, f
}

func TestNotifyPostsRedactedMessageAndAuthenticates(t *testing.T) {
	m, f := newFixture(t)
	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "TG-9#restart",
		Body:       "restart web01; password=hunter2 and token=abcd1234 — contact ops@example.com",
		Approval:   true,
	})
	if err != nil {
		t.Fatalf("Notify must succeed on ok:true: %v", err)
	}
	r := f.reqs[0]
	// vendor-correct request: POST to the Web API chat.postMessage method.
	if r.method != http.MethodPost || r.path != "/api/chat.postMessage" {
		t.Errorf("request = %s %s, want POST /api/chat.postMessage", r.method, r.path)
	}
	// authenticated with the resolved bot token as a Bearer credential (never a literal, INV-13).
	if r.auth != "Bearer xoxb-secret" {
		t.Errorf("Authorization = %q, want Bearer xoxb-secret", r.auth)
	}
	if !strings.HasPrefix(r.ctype, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", r.ctype)
	}
	// routed to the decision's project channel.
	if !strings.Contains(r.body, `"channel":"#tg-approvals"`) {
		t.Errorf("must route to the TG project channel, body=%q", r.body)
	}
	// the posted body is credential- and PII-redacted before it leaves the process (INV-19).
	for _, secret := range []string{"hunter2", "abcd1234", "ops@example.com"} {
		if strings.Contains(r.body, secret) {
			t.Errorf("posted body must redact %q, got %s", secret, r.body)
		}
	}
	if !strings.Contains(r.body, "[REDACTED]") {
		t.Errorf("posted body must contain redaction markers, got %s", r.body)
	}
}

// TestNotifyHonoursConfiguredChannel proves the operator's real channel is posted to (config-not-code): a
// per-route mapping wins; an unmapped route falls back to the default channel; and with neither the computed
// #<prefix>-approvals name is kept (backward-compatible). Without this, posting the computed name returns
// channel_not_found and the approval poll never reaches humans.
func TestNotifyHonoursConfiguredChannel(t *testing.T) {
	t.Setenv("TG_TEST_SLACK_TOKEN", "xoxb-secret")

	// A per-route mapping: #tg-approvals -> the deployment's real channel id.
	f := &fakeDoer{}
	m := New("https://slack.example/", config.SecretRef("env:TG_TEST_SLACK_TOKEN"), []string{"U1"},
		WithHTTPClient(f), WithChannels(map[string]string{"#tg-approvals": "C0DEPLOY"}), WithDefaultChannel("C0FALLBACK"))
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f.reqs[0].body, `"channel":"C0DEPLOY"`) {
		t.Errorf("mapped route must post to the configured channel id, body=%q", f.reqs[0].body)
	}

	// An unmapped route (different project prefix) falls back to the default channel.
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "OPS-1#page", Body: "y"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f.reqs[1].body, `"channel":"C0FALLBACK"`) {
		t.Errorf("unmapped route must fall back to the default channel, body=%q", f.reqs[1].body)
	}

	// With no config at all, the computed name is kept (no regression).
	f2 := &fakeDoer{}
	plain := New("https://slack.example/", config.SecretRef("env:TG_TEST_SLACK_TOKEN"), []string{"U1"}, WithHTTPClient(f2))
	if err := plain.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "z"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f2.reqs[0].body, `"channel":"#tg-approvals"`) {
		t.Errorf("unconfigured module must keep the computed name, body=%q", f2.reqs[0].body)
	}
}

// Regression for the fail-open approval bug: Slack signals app-level failure with HTTP 200 +
// {"ok":false,"error":"..."}. Notify MUST surface that as an error — a dropped poll must never be reported
// as delivered on the approval path.
func TestNotifyFailsWhenSlackReturnsOKFalse(t *testing.T) {
	m, f := newFixture(t)
	f.status = 200
	f.resp = `{"ok":false,"error":"not_in_channel"}`
	err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "restart web01"})
	if err == nil {
		t.Fatal("Notify must error on ok:false (a dropped poll must not be reported as delivered)")
	}
	if !strings.Contains(err.Error(), "not_in_channel") {
		t.Errorf("error must carry Slack's failure code, got %v", err)
	}
}

func TestNotifyNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 500
	f.resp = `{"ok":false,"error":"internal_error"}`
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "x"}); err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
}

func TestNotifyEmptyDecisionRejected(t *testing.T) {
	m, _ := newFixture(t)
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "", Body: "x"}); err == nil {
		t.Fatal("a notice with no decision id must be rejected")
	}
}

func TestResolveVoteBindsToDecisionFromApprover(t *testing.T) {
	m, _ := newFixture(t)
	raw := []byte(`{"user":"U123","text":"approve TG-9#restart"}`)
	v, err := m.ResolveVote(context.Background(), raw)
	if err != nil {
		t.Fatalf("an approver vote must resolve: %v", err)
	}
	if v.DecisionID != "TG-9#restart" { // the vote binds to exactly the decision it answers (INV-12)
		t.Errorf("vote must bind to the decision id, got %q", v.DecisionID)
	}
	if v.Sender != "U123" || !v.Approve {
		t.Errorf("vote sender/intent wrong: %+v", v)
	}
}

func TestResolveVoteRejectsNonApproverAndUnboundReply(t *testing.T) {
	m, _ := newFixture(t)
	// sender not in the approver set → rejected (INV-12).
	if _, err := m.ResolveVote(context.Background(), []byte(`{"user":"intruder","text":"approve TG-9#restart"}`)); err == nil {
		t.Fatal("a vote from a non-approver must be rejected")
	}
	// a reply that cites no decision id → rejected.
	if _, err := m.ResolveVote(context.Background(), []byte(`{"user":"U123","text":"approve"}`)); err == nil {
		t.Fatal("a reply citing no decision id must be rejected")
	}
}
