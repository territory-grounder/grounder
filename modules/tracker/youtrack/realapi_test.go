package youtrack

import (
	"encoding/json"
	"os"
	"testing"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/config"
)

// testdata/issue.json is a REAL YouTrack issue response (captured from the live API, trimmed to the fields
// this module requests: idReadable, summary, customFields(name,value(name))). It is checked in so the
// customFields walk is regression-tested against the ACTUAL nested shape (name + value.name, with $type
// siblings), not the shape my fake assumes. If YouTrack changes that structure, parsing breaks here in CI.
// The durable "test against reality": no live creds needed, catches assumption drift.
func TestParsesRealYouTrackIssue(t *testing.T) {
	body, err := os.ReadFile("testdata/issue.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw ytIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("must parse the real YouTrack response: %v", err)
	}
	if raw.Summary == "" {
		t.Error("summary must parse from the real response")
	}
	// The State custom field must be found by name and read through value.name against the real structure.
	// The live issue's State was "Submitted", which folds to the least-progressed tracker state (Open). A
	// default module reads the default "State" field.
	m := New("https://yt.example", config.SecretRef("env:X"))
	if got := m.stateOf(raw); got != tracker.StateOpen {
		t.Errorf("real State custom field must resolve; Submitted -> Open, got %q", got)
	}
}
