package githubissues

import (
	"encoding/json"
	"os"
	"testing"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
)

// testdata/issue.json is a REAL GitHub Issues GET /repos/{owner}/{repo}/issues/{number} response (the
// canonical API example, kept full: the parser must reach top-level number/title/state past the dozens of
// sibling fields — url, labels, user, milestone, reactions, state_reason, closed_by, ...). It is checked in
// so the state fold is regression-tested against reality, not the three-field shape the fake assumes. If
// GitHub moves state off the top level — or the module stops reading it there — parsing breaks here in CI,
// no live creds needed. The durable "test against reality" that catches assumption drift.
func TestParsesRealGitHubIssue(t *testing.T) {
	body, err := os.ReadFile("testdata/issue.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw ghIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("must parse the real GitHub response: %v", err)
	}
	if raw.Number != 1347 {
		t.Errorf("number must parse from the real response, got %d", raw.Number)
	}
	if raw.Title == "" {
		t.Error("title must parse from the real response")
	}
	// The real fixture is a closed issue; GitHub's open|closed must fold onto the tracker enum with
	// closed -> Resolved (state_reason "completed" is ignored — only open|closed drives the fold).
	if got := fromGitHubState(raw.State); got != tracker.StateResolved {
		t.Errorf("real state must resolve; closed -> resolved, got %q", got)
	}
}
