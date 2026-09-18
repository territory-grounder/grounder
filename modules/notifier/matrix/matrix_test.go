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

// A POLL_PAUSE notice must go out as a REAL MSC3381 poll, not prose.
//
// Until 2026-08-02 Notice.Approval was accepted and ignored: every notice was one flat m.room.message,
// and the only way to answer was to type a command whose syntax nothing disclosed. An approver who
// replied "yes, go ahead" cast no vote, and the decision timed out to DENY.
//
// KILLING MUTATION: drop the `n.Approval && len(n.Choices) > 0` branch from Notify. RED.
func TestApprovalNoticeSendsAnMSC3381Poll(t *testing.T) {
	f := &fakeDoer{}
	m := New("https://matrix.example", config.SecretRef("env:MATRIX_TEST_TOKEN"),
		[]string{"@oncall:example"}, WithHTTPClient(f), WithDefaultRoom("!room:example"))
	t.Setenv("MATRIX_TEST_TOKEN", "tok")

	err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "INC-123", Body: "restart web01?", Approval: true,
		Choices: []notifier.Choice{
			{ID: notifier.ChoiceID("plan", "INC-123"), Label: "Approve — restart guest", Approve: true},
			{ID: notifier.ChoiceID("deny", "INC-123"), Label: "Deny"},
		},
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(f.paths) != 1 {
		t.Fatalf("want 1 request, got %d", len(f.paths))
	}
	if !strings.Contains(f.paths[0], "org.matrix.msc3381.poll.start") {
		t.Fatalf("an approval notice was NOT sent as a poll — path %s", f.paths[0])
	}
	// Idempotent send: a retry must not open a second poll for one decision.
	if !strings.Contains(f.paths[0], "INC-123") {
		t.Errorf("the send carries no per-decision transaction id: %s", f.paths[0])
	}
	for _, want := range []string{
		"org.matrix.msc3381.poll.disclosed", // the room sees the tally — an audit surface, not a DM
		"org.matrix.msc1767.text",           // MSC1767 extensible text on question and answers
		"plan|INC-123", "deny|INC-123",      // every answer id carries its own decision binding
		"max_selections",
	} {
		if !strings.Contains(f.bodies[0], want) {
			t.Errorf("poll body missing %q: %s", want, f.bodies[0])
		}
	}
	// A client that cannot render polls must still see the decision.
	if !strings.Contains(f.bodies[0], "restart web01?") {
		t.Error("the poll carries no plain-text fallback body")
	}
}

// A PAGE is not a poll. Escalations must stay plain messages or they become unanswerable ballots.
func TestPageStaysAPlainMessage(t *testing.T) {
	f := &fakeDoer{}
	m := New("https://matrix.example", config.SecretRef("env:MATRIX_TEST_TOKEN"),
		[]string{"@oncall:example"}, WithHTTPClient(f), WithDefaultRoom("!room:example"))
	t.Setenv("MATRIX_TEST_TOKEN", "tok")

	if err := m.Notify(context.Background(), notifier.Notice{
		DecisionID: "INC-9", Body: "escalation", Approval: false,
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if strings.Contains(f.paths[0], "poll") {
		t.Fatalf("a page was sent as a poll: %s", f.paths[0])
	}
}

// THE BINDING PROPERTY. A poll response carries only the chosen answer's id — no text, no decision
// reference. Encoding the decision in the id is what makes INV-12 structural instead of dependent on a
// server-side event->decision map that can drift.
//
// KILLING MUTATION: in ResolveVote, ignore the poll-response branch and fall through to parseVote. RED.
func TestPollResponseBindsToItsDecisionFromTheAnswerID(t *testing.T) {
	m := New("https://matrix.example", config.SecretRef("env:X"), []string{"@oncall:example"})
	for _, tc := range []struct{ name, raw string }{
		{"unstable", `{"sender":"@oncall:example","content":{"org.matrix.msc3381.poll.response":{"answers":["plan|INC-77"]}}}`},
		{"stabilized", `{"sender":"@oncall:example","content":{"m.poll.response":{"answers":["plan|INC-77"]}}}`},
	} {
		v, err := m.ResolveVote(context.Background(), []byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if v.DecisionID != "INC-77" || !v.Approve {
			t.Fatalf("%s: vote did not bind to its decision: %+v", tc.name, v)
		}
		if v.Choice != "plan|INC-77" {
			t.Errorf("%s: the selected option was not recorded: %q", tc.name, v.Choice)
		}
	}
}

// "Investigate" is hesitation, not consent. Folding it into approval would turn a human's request for
// more information into authorization to act.
func TestInvestigateIsNotConsent(t *testing.T) {
	m := New("https://matrix.example", config.SecretRef("env:X"), []string{"@oncall:example"})
	v, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@oncall:example","content":{"m.poll.response":{"answers":["investigate|INC-5"]}}}`))
	if err != nil {
		t.Fatalf("ResolveVote: %v", err)
	}
	if v.Approve {
		t.Fatal("'investigate' was counted as APPROVAL — a request for more information became consent to act")
	}
	if v.DecisionID != "INC-5" {
		t.Fatalf("bound to the wrong decision: %+v", v)
	}
}

// An answer id with no embedded decision must be REFUSED, never guessed at.
func TestUnboundPollAnswerIsRefused(t *testing.T) {
	m := New("https://matrix.example", config.SecretRef("env:X"), []string{"@oncall:example"})
	if _, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@oncall:example","content":{"m.poll.response":{"answers":["approve"]}}}`)); err == nil {
		t.Fatal("an unbound answer id resolved to a vote — it would be counted against an unknown decision")
	}
	// max_selections is 1, so a multi-answer response is malformed. Picking one would invent intent.
	if _, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@oncall:example","content":{"m.poll.response":{"answers":["plan|A","deny|A"]}}}`)); err == nil {
		t.Fatal("a contradictory multi-answer response resolved to a vote")
	}
	// A non-approver clicking the poll is still not an approver.
	if _, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@intruder:evil","content":{"m.poll.response":{"answers":["plan|INC-1"]}}}`)); err == nil {
		t.Fatal("a poll response from outside the approver set was accepted")
	}
}

// THE APPROVER SET IS READ LIVE. Revoking someone's approval rights is usually urgent, and taking effect
// at the next deploy is not good enough for a change to the trust boundary (INV-12).
//
// KILLING MUTATION: remove the liveApprovers consultation in ResolveVote. RED.
func TestLiveApproverSetIsConsulted(t *testing.T) {
	live := []string{"@promoted:example"}
	m := New("https://matrix.example", config.SecretRef("env:X"),
		[]string{"@boot:example"}, // the set captured at construction
		WithLiveConfig(func() []string { return live }, nil))

	// Someone the LIVE set trusts but the boot set does not.
	if _, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@promoted:example","content":{"body":"approve INC-1"}}`)); err != nil {
		t.Fatalf("a live-set approver was refused: %v", err)
	}
	// REVOCATION is the direction that matters: emptying the live set must take effect immediately.
	live = []string{"@someone-else:example"}
	if _, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@promoted:example","content":{"body":"approve INC-1"}}`)); err == nil {
		t.Fatal("a revoked approver's vote was still accepted — revocation did not take effect")
	}
	// And with the live accessor returning nothing, the constructed set must still stand rather than
	// collapsing to "trust nobody" (which would refuse every vote during a config-store outage).
	live = nil
	if _, err := m.ResolveVote(context.Background(),
		[]byte(`{"sender":"@boot:example","content":{"body":"approve INC-1"}}`)); err != nil {
		t.Fatalf("an empty live set erased the boot approver set: %v", err)
	}
}

// Routed rooms must be live too, or re-pointing approvals at a different room needs a redeploy.
func TestLiveRoomRoutingIsConsulted(t *testing.T) {
	f := &fakeDoer{}
	rooms, def := map[string]string{}, "!bootroom:example"
	m := New("https://matrix.example", config.SecretRef("env:MATRIX_TEST_TOKEN"), []string{"@a:b"},
		WithHTTPClient(f), WithDefaultRoom("!bootroom:example"),
		WithLiveConfig(nil, func() (map[string]string, string) { return rooms, def }))
	t.Setenv("MATRIX_TEST_TOKEN", "tok")

	def = "!liveroom:example"
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "INC-1", Body: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(f.paths[0], "liveroom") {
		t.Fatalf("the notice went to %s — a saved room change did not take effect", f.paths[0])
	}
}
