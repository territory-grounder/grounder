package email

import (
	"context"
	"os"
	"testing"
)

// testdata/inbound_reply.json is a realistic inbound-reply payload as a mail gateway would normalize it for
// this module: the approver's From, the Re: subject echoing the decision tag, Message-ID / In-Reply-To
// threading headers, a date, and a body whose first line is the vote verb + decision id followed by the
// client's quoted original. The module's inbound shape reads only `from` and `body`; this test proves the
// REAL, fuller payload still parses and binds — the parser tolerates the extra fields a live gateway carries
// and recovers the vote from a quoted reply body, not just the minimal two-field object my other tests use.
// The durable "test against reality": no live mailbox, catches assumption drift if the inbound shape changes.
func TestResolvesRealInboundReply(t *testing.T) {
	raw, err := os.ReadFile("testdata/inbound_reply.json")
	if err != nil {
		t.Fatal(err)
	}
	m := New("smtp.test:25", "grounder@example.com", []string{"ops@example.com"}, []string{"oncall@example.com"})

	v, err := m.ResolveVote(context.Background(), raw)
	if err != nil {
		t.Fatalf("a real inbound approver reply must resolve: %v", err)
	}
	if v.DecisionID != "TG-9#restart" {
		t.Errorf("the vote must bind to the decision id cited in the real reply, got %q", v.DecisionID)
	}
	if v.Sender != "oncall@example.com" {
		t.Errorf("the vote sender must be the authenticated approver, got %q", v.Sender)
	}
	if !v.Approve {
		t.Errorf("the real reply is an approval; Approve must be true, got %+v", v)
	}
}
