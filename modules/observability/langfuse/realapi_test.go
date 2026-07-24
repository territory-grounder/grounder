package langfuse

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata/ingestion_response.json is a REALISTIC Langfuse ingestion reply (207 Multi-Status shape):
// a top-level successes[] plus errors[], where each error carries id/status/message and a nested "error"
// detail array. It is checked in so the ingestion-response parser is regression-tested against the ACTUAL
// nested shape — the exact structure this module decodes to decide whether events were dropped (INV-15).
// If Langfuse changes that structure, parsing breaks here in CI, no live creds needed.
func TestParsesRealIngestionResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/ingestion_response.json")
	if err != nil {
		t.Fatal(err)
	}
	var ir ingestResponse
	if err := json.Unmarshal(body, &ir); err != nil {
		t.Fatalf("must parse the real Langfuse ingestion response: %v", err)
	}
	if len(ir.Successes) != 1 || ir.Successes[0].ID != "9a1f0b8c4d5e6f70" {
		t.Errorf("successes must parse from the real response, got %+v", ir.Successes)
	}
	if len(ir.Errors) != 1 {
		t.Fatalf("the errors array must parse (a dropped event), got %d", len(ir.Errors))
	}
	// The dropped-event id and its message are what the module surfaces so a drop is never silent.
	if ir.Errors[0].ID != "1c2d3e4f5a6b7c8d" {
		t.Errorf("dropped event id must resolve, got %q", ir.Errors[0].ID)
	}
	if ir.Errors[0].Status != 400 || ir.Errors[0].Message == "" {
		t.Errorf("dropped event status/message must resolve, got %+v", ir.Errors[0])
	}
}
