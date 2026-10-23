package slack

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/chat.postMessage.*.json are REAL Slack Web API chat.postMessage response bodies. Slack returns
// HTTP 200 for BOTH outcomes and distinguishes them only inside the body: a success carries ok:true beside
// a nested "message" object, a failure carries ok:false + a machine-readable "error" code. These are checked
// in so the apiResponse envelope walk is regression-tested against the ACTUAL shape (ok/error read through
// a body that also carries channel/ts/message siblings), not the shape a fake assumes. This is the exact
// signal Notify keys off; if Slack changed it, the fail-closed approval guard would break here in CI.
func TestParsesRealSlackResponses(t *testing.T) {
	t.Run("app-level failure (ok:false)", func(t *testing.T) {
		body, err := os.ReadFile("testdata/chat.postMessage.not_in_channel.json")
		if err != nil {
			t.Fatal(err)
		}
		var env apiResponse
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("must parse the real Slack error response: %v", err)
		}
		if env.OK {
			t.Error("a not_in_channel response must decode to ok:false")
		}
		if env.Error != "not_in_channel" {
			t.Errorf("failure code must decode from the real response, got %q", env.Error)
		}
	})

	t.Run("success (ok:true)", func(t *testing.T) {
		body, err := os.ReadFile("testdata/chat.postMessage.ok.json")
		if err != nil {
			t.Fatal(err)
		}
		var env apiResponse
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("must parse the real Slack success response: %v", err)
		}
		if !env.OK {
			t.Error("ok:true must decode from the real success response (with its message sibling)")
		}
		if env.Error != "" {
			t.Errorf("a success carries no error code, got %q", env.Error)
		}
	})
}
