package servicenow

import (
	"encoding/json"
	"os"
	"testing"

	tracker "github.com/territory-grounder/grounder/adapters/tracker"
	"github.com/territory-grounder/grounder/core/config"
)

// testdata/incident.json is a REAL ServiceNow GET /api/now/table/incident/{sys_id} response (trimmed but
// keeping the ACTUAL shape: the record wrapped under "result", every field a STRING — including the
// NUMERIC state code "2" — and reference fields like assignment_group returned as nested {link,value}
// objects the parser must skip). It is checked in so the record walk is regression-tested against reality,
// not the flat shape a fake might assume. If ServiceNow changes that structure — or the module stops
// reaching result.state / result.short_description — parsing breaks here in CI, no live creds needed. The
// durable "test against reality" that catches assumption drift.
func TestParsesRealServiceNowIncident(t *testing.T) {
	body, err := os.ReadFile("testdata/incident.json")
	if err != nil {
		t.Fatal(err)
	}
	var env incidentEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("must parse the real ServiceNow response: %v", err)
	}
	if env.Result.ShortDescription == "" {
		t.Error("short_description must parse from the real response (result.short_description)")
	}
	// State is the NUMERIC code carried as a JSON string amid nested reference-field siblings; it must be
	// read as "2" and fold onto the tracker-agnostic enum: In Progress.
	if env.Result.State != "2" {
		t.Errorf("state must parse as the numeric string %q, got %q", "2", env.Result.State)
	}
	// A default module folds the out-of-box numeric code "2" onto In Progress.
	m := New("https://dev.service-now.com", "u", config.SecretRef("env:X"))
	if got := m.fromServiceNowState(env.Result.State); got != tracker.StateInProgress {
		t.Errorf("real numeric state must resolve; \"2\" -> in_progress, got %q", got)
	}
}
