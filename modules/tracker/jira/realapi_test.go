package jira

import (
	"encoding/json"
	"os"
	"testing"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
)

// testdata/issue.json is a REAL Jira Cloud GET /rest/api/2/issue/{key}?fields=summary,status response
// (trimmed to the fields this module reads, but keeping the ACTUAL nested shape: fields.status.name with
// $self/id/statusCategory siblings the parser must skip). It is checked in so the status walk is
// regression-tested against reality, not the flat shape a fake might assume. If Jira changes that
// structure — or the module stops reaching fields.status.name — parsing breaks here in CI, no live creds
// needed. The durable "test against reality" that catches assumption drift.
func TestParsesRealJiraIssue(t *testing.T) {
	body, err := os.ReadFile("testdata/issue.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw jiraIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("must parse the real Jira response: %v", err)
	}
	if raw.Fields.Summary == "" {
		t.Error("summary must parse from the real response")
	}
	// The status must be read through the nested fields.status.name against the real structure and fold
	// onto the tracker-agnostic enum: the live issue's "In Progress" -> StateInProgress.
	if got := fromJiraStatus(raw.Fields.Status.Name); got != tracker.StateInProgress {
		t.Errorf("real status must resolve; In Progress -> in_progress, got %q", got)
	}
}
