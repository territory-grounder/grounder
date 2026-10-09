package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/territory-grounder/grounder/core/auth"
	"github.com/territory-grounder/grounder/modules"
)

type fakeCaps struct{ caps []modules.Capability }

func (f fakeCaps) Capabilities() []modules.Capability { return f.caps }

func TestCapabilitiesHandler(t *testing.T) {
	t.Run("returns the declared fleet with enablement", func(t *testing.T) {
		caps := []modules.Capability{
			{Surface: "ingest", SourceType: "crowdsec", Capability: "ingest.crowdsec", Enabled: true},
			{Surface: "actuation", SourceType: "ssh", Capability: "actuation.ssh", Enabled: false},
		}
		w := httptest.NewRecorder()
		Deps{Capabilities: fakeCaps{caps: caps}}.capabilitiesHandler(w, httptest.NewRequest("GET", "/v1/capabilities", nil), auth.Principal{})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var got CapabilitiesPage
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Capabilities) != 2 {
			t.Fatalf("want 2 capabilities, got %+v", got.Capabilities)
		}
		// The disabled actuator must be visibly enabled:false (no execution path, INV-17).
		var sshEnabled = true
		for _, c := range got.Capabilities {
			if c.SourceType == "ssh" {
				sshEnabled = c.Enabled
			}
		}
		if sshEnabled {
			t.Error("the disabled actuator must report enabled:false so it is not mistaken for available")
		}
	})

	t.Run("nil reader fails closed to 503", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{}.capabilitiesHandler(w, httptest.NewRequest("GET", "/v1/capabilities", nil), auth.Principal{})
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
	})

	t.Run("empty fleet returns [] not null", func(t *testing.T) {
		w := httptest.NewRecorder()
		Deps{Capabilities: fakeCaps{caps: nil}}.capabilitiesHandler(w, httptest.NewRequest("GET", "/v1/capabilities", nil), auth.Principal{})
		var got map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if string(got["capabilities"]) != "[]" {
			t.Errorf("capabilities = %s, want []", got["capabilities"])
		}
	})
}
