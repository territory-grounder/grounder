package teams

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/config"
)

// testdata/activity.json is a REAL Bot Framework inbound message Activity (the shape Teams POSTs to a bot's
// messaging endpoint), trimmed to a realistic-but-representative subset: the deeply nested from.id, the
// text carrying the vote, and the sibling channelData/conversation/recipient fields the parser must ignore.
// It is checked in so the vote-resolution walk (from.id + text) is regression-tested against the ACTUAL
// nested shape, not the shape a fake assumes. If Teams changes that structure, parsing breaks here in CI —
// the durable "test against reality", no live creds needed.
func TestParsesRealTeamsActivity(t *testing.T) {
	body, err := os.ReadFile("testdata/activity.json")
	if err != nil {
		t.Fatal(err)
	}

	// The from.id must be read through the real nested from{ id } object, past the many sibling fields.
	var raw teamsActivity
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("must parse the real Teams activity: %v", err)
	}
	const realSender = "29:1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	if raw.From.ID != realSender {
		t.Errorf("from.id must parse from the real nested shape, got %q", raw.From.ID)
	}

	// End-to-end: the real activity resolves to a vote bound to the cited decision id and authenticated
	// against the approver set (INV-12). Token is a secret reference, never a literal — ResolveVote does not
	// touch the network, so no HTTP fake is needed.
	t.Setenv("TG_TEST_TEAMS_TOKEN", "bf_secret")
	m := New("https://smba.trafficmanager.net/amer/", "19:abc123def456@thread.tacv2",
		config.SecretRef("env:TG_TEST_TEAMS_TOKEN"), []string{realSender})
	v, err := m.ResolveVote(context.Background(), body)
	if err != nil {
		t.Fatalf("the real approver activity must resolve to a vote: %v", err)
	}
	if v.DecisionID != "TG-9#restart" {
		t.Errorf("vote must bind to the decision id in the real activity text, got %q", v.DecisionID)
	}
	if v.Sender != realSender || !v.Approve {
		t.Errorf("vote sender/intent wrong for the real activity: %+v", v)
	}
}
