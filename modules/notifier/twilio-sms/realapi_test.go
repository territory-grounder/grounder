package twiliosms

import (
	"context"
	"os"
	"strings"
	"testing"

	notifier "github.com/territory-grounder/grounder/adapters/notifier"
	"github.com/territory-grounder/grounder/core/config"
)

// testdata/message_created.json and testdata/error_response.json are REAL Twilio POST
// /2010-04-01/Accounts/{SID}/Messages.json responses (a 201 Created success envelope and a 400 error
// envelope), checked in so the module's response handling is regression-tested against the ACTUAL vendor
// shapes with no live account: Twilio's success status is 201 (not 200) and must be accepted, and a real
// error body's vendor "message" must survive verbatim into the returned error rather than be swallowed.
// The durable "test against reality" that catches assumption drift.

func realFixture(t *testing.T, status int, respFile string) (*Module, *fakeDoer) {
	t.Helper()
	t.Setenv("TG_TEST_TWILIO_TOKEN", testToken)
	body, err := os.ReadFile(respFile)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDoer{status: status, resp: string(body)}
	m := New("https://api.twilio.test", testSID, fromNum, toNum, config.SecretRef("env:TG_TEST_TWILIO_TOKEN"), WithHTTPClient(f))
	return m, f
}

func TestAcceptsRealTwilioSuccessResponse(t *testing.T) {
	m, f := realFixture(t, 201, "testdata/message_created.json")
	if err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "page"}); err != nil {
		t.Fatalf("a real Twilio 201 Created response must be accepted as success: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("expected exactly one send, got %d", len(f.reqs))
	}
}

func TestSurfacesRealTwilioErrorResponse(t *testing.T) {
	m, _ := realFixture(t, 400, "testdata/error_response.json")
	err := m.Notify(context.Background(), notifier.Notice{DecisionID: "TG-9#restart", Body: "page"})
	if err == nil {
		t.Fatal("a real Twilio 400 error response must be an error")
	}
	// the vendor's own error message must survive into the returned error, not be swallowed.
	if !strings.Contains(err.Error(), "is not a valid phone number") {
		t.Errorf("error must surface the Twilio message, got %v", err)
	}
}
