package netbox

import (
	"os"
	"testing"
)

// testdata/device.json is a REAL NetBox device-detail response (captured from the live API), checked in so
// the parser is regression-tested against the actual field shape — not the shape my fake assumes. If NetBox
// ever changes a field name (name/display/status.value/site.name), toEntity breaks here in CI. This is the
// durable form of "test against reality": no live creds needed, catches assumption drift.
func TestToEntityParsesRealNetBoxResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/device.json")
	if err != nil {
		t.Fatal(err)
	}
	e, err := toEntity("device", "18", body)
	if err != nil {
		t.Fatalf("must parse the real NetBox response: %v", err)
	}
	if e.ID != "18" || e.Name != "ankh" {
		t.Errorf("id/name mismatch vs real response: %+v", e)
	}
	if e.Attributes["status"] != "active" {
		t.Errorf("status.value must resolve from the real response, got %q", e.Attributes["status"])
	}
	if e.Attributes["site"] != "dc1" {
		t.Errorf("site.name must resolve from the real response, got %q", e.Attributes["site"])
	}
}
