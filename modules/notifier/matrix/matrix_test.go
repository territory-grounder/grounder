package matrix

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

type fakeDoer struct {
	paths  []string
	bodies []string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	b := ""
	if req.Body != nil {
		x, _ := io.ReadAll(req.Body)
		b = string(x)
	}
	f.paths = append(f.paths, req.URL.Path)
	f.bodies = append(f.bodies, b)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
}

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_MATRIX_TOKEN", "syt_secret")
	f := &fakeDoer{}
	m := New("https://matrix.example", config.SecretRef("env:TG_TEST_MATRIX_TOKEN"), []string{"@oncall:example", "@lead:example"}, WithHTTPClient(f))
	return m, f
}

func TestNotifyRedactsCredentialsAndRoutesByPrefix(t *testing.T) {
	m, f := newFixture(t)
	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "TG-5#reboot",
		Body:       "reboot web01; password=hunter2 and token=abcd1234 — contact ops@example.com",
		Approval:   true,
	})
	if err != nil {
		t.Fatalf("Notify must succeed: %v", err)
	}
	if !strings.Contains(f.paths[0], "/rooms/%23tg-approvals/") && !strings.Contains(f.paths[0], "/rooms/#tg-approvals/") {
		t.Errorf("must route to the TG project room, path=%q", f.paths[0])
	}
	body := f.bodies[0]
	for _, secret := range []string{"hunter2", "abcd1234", "ops@example.com"} {
		if strings.Contains(body, secret) {
			t.Errorf("posted body must redact %q, got %s", secret, body)
		}
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("posted body must contain redaction markers, got %s", body)
	}
}

// TestNotifyHonoursConfiguredRoom proves the operator's real room id/alias is used (config-not-code): a
// per-route mapping wins; an unmapped route falls back to the default room; and with neither the computed
// #<prefix>-approvals name is kept (backward-compatible). Without this, the computed name is not a valid
// alias (no ":homeserver"), so the poll is undeliverable.
func TestNotifyHonoursConfiguredRoom(t *testing.T) {
	t.Setenv("TG_TEST_MATRIX_TOKEN", "syt_secret")

	f := &fakeDoer{}
	m := New("https://matrix.example", config.SecretRef("env:TG_TEST_MATRIX_TOKEN"), []string{"@a:example"},
		WithHTTPClient(f), WithRooms(map[string]string{"#tg-approvals": "!deploy:example"}), WithDefaultRoom("!fallback:example"))

	// A mapped route uses the configured room id (path-escaped '!' stays '!').
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-5#reboot", Body: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f.paths[0], "/rooms/%21deploy:example/") && !strings.Contains(f.paths[0], "/rooms/!deploy:example/") {
		t.Errorf("mapped route must use the configured room id, path=%q", f.paths[0])
	}

	// An unmapped route falls back to the default room.
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "OPS-1#page", Body: "y"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f.paths[1], "fallback:example") {
		t.Errorf("unmapped route must fall back to the default room, path=%q", f.paths[1])
	}

	// With no config the computed alias is kept (no regression).
	f2 := &fakeDoer{}
	plain := New("https://matrix.example", config.SecretRef("env:TG_TEST_MATRIX_TOKEN"), []string{"@a:example"}, WithHTTPClient(f2))
	if err := plain.Notify(context.Background(), notifier.Notice{DecisionID: "TG-5#reboot", Body: "z"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f2.paths[0], "tg-approvals") {
		t.Errorf("unconfigured module must keep the computed alias, path=%q", f2.paths[0])
	}
}

func TestResolveVoteBindsToDecisionFromApprover(t *testing.T) {
	m, _ := newFixture(t)
	raw := []byte(`{"sender":"@oncall:example","content":{"body":"approve TG-5#reboot"}}`)
	v, err := m.ResolveVote(context.Background(), raw)
	if err != nil {
		t.Fatalf("an approver vote must resolve: %v", err)
	}
	if v.DecisionID != "TG-5#reboot" {
		t.Errorf("vote must bind to the decision id, got %q", v.DecisionID)
	}
	if v.Sender != "@oncall:example" || !v.Approve {
		t.Errorf("vote sender/intent wrong: %+v", v)
	}
}

func TestResolveVoteRejectsNonApproverAndUnboundReply(t *testing.T) {
	m, _ := newFixture(t)
	// sender not in the approver set → rejected (INV-12).
	if _, err := m.ResolveVote(context.Background(), []byte(`{"sender":"@intruder:evil","content":{"body":"approve TG-5"}}`)); err == nil {
		t.Fatal("a vote from a non-approver must be rejected")
	}
	// a reply that cites no decision id → rejected.
	if _, err := m.ResolveVote(context.Background(), []byte(`{"sender":"@oncall:example","content":{"body":"approve"}}`)); err == nil {
		t.Fatal("a reply citing no decision id must be rejected")
	}
}
