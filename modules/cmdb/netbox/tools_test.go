package netbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/config"
)

// nbToolDoer is a configurable read seam for the investigation-tool tests — the package's other fakeDoer is
// hardwired to the by-id resolve path, and these tests drive the by-name lookup. It records the URL it saw
// so the name query can be asserted.
type nbToolDoer struct {
	status int
	body   string
	err    error
	sawURL string
}

func (d *nbToolDoer) Do(req *http.Request) (*http.Response, error) {
	d.sawURL = req.URL.String()
	if d.err != nil {
		return nil, d.err
	}
	st := d.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(d.body)), Header: make(http.Header)}, nil
}

func toolFixture(t *testing.T, d *nbToolDoer) *Module {
	t.Setenv("TG_TEST_NETBOX_TOKEN", "nb_secret")
	return New("https://netbox.example/", config.SecretRef("env:TG_TEST_NETBOX_TOKEN"), WithHTTPClient(d))
}

// TG-56: the tool is read-only, decodes only SAFE fields, and never leaks an undeclared vendor field into the
// model loop. The query is a by-NAME device lookup (an alert carries a hostname, not a NetBox numeric id).
func TestInventoryDeviceToolReadOnlyAndNarrows(t *testing.T) {
	// A record deliberately carrying an unsafe custom field: it MUST NOT survive the narrowing into the model.
	body := `{"results":[{"name":"edge-sw-07","status":{"value":"active","label":"Active"},` +
		`"site":{"name":"DC1"},"role":{"name":"access-switch"},"device_type":{"model":"C9300"},` +
		`"primary_ip":{"address":"203.0.113.7/24"},"custom_fields":{"api_token":"MUST-NOT-APPEAR"}}]}`
	d := &nbToolDoer{body: body}
	tools := NewTools(toolFixture(t, d))
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	tl := tools[0]
	if tl.Name() != "get-inventory-device" || !tl.ReadOnly() {
		t.Fatalf("want read-only get-inventory-device, got name=%q readonly=%v", tl.Name(), tl.ReadOnly())
	}
	res, err := tl.Invoke(context.Background(), map[string]string{"host": "edge-sw-07"})
	if err != nil {
		t.Fatalf("Invoke returned a Go error (should be a soft ToolResult): %v", err)
	}
	if !res.Success {
		t.Fatalf("want success, got Output=%q", res.Output)
	}
	for _, want := range []string{"status=Active", "site=DC1", "role=access-switch", "model=C9300", "203.0.113.7"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("narrowed output missing %q: %s", want, res.Output)
		}
	}
	if strings.Contains(res.Output, "MUST-NOT-APPEAR") {
		t.Fatalf("an undeclared vendor field leaked to the model: %s", res.Output)
	}
	// A trailing-slash base + the by-name query compose without a double slash, host escaped.
	if !strings.Contains(d.sawURL, "/api/dcim/devices/?name=edge-sw-07") {
		t.Fatalf("expected a by-name device query, got URL %q", d.sawURL)
	}
}

// TG-56: the tool registers on the READ-ONLY ToolSet — the structural proof it is agent-reachable and cannot
// mutate. KILLING MUTATION: flip inventoryDeviceTool.ReadOnly() to false and RegisterFrom returns
// ErrWriteToolWithheld → this goes RED. That is the reachability+read-only guarantee, not a mock.
func TestInventoryDeviceToolRegistersOnReadOnlyToolSet(t *testing.T) {
	ts := agent.NewReadOnlyToolSet()
	tools := NewTools(toolFixture(t, &nbToolDoer{body: `{"results":[]}`}))
	if len(tools) == 0 {
		t.Fatal("no tools to register")
	}
	for _, tl := range tools {
		if err := ts.RegisterFrom("netbox", tl); err != nil {
			t.Fatalf("a read-only tool must register on the read-only ToolSet, got %v", err)
		}
	}
}

// TG-56: a lookup miss / missing host / transport error are SOFT ToolResult{Success:false} the agent adapts
// to — never a Go error that aborts the session.
func TestInventoryDeviceToolSoftFailures(t *testing.T) {
	cases := []struct {
		name string
		doer *nbToolDoer
		args map[string]string
	}{
		{"not-found", &nbToolDoer{body: `{"results":[]}`}, map[string]string{"host": "ghost"}},
		{"missing-host", &nbToolDoer{body: `{"results":[]}`}, map[string]string{}},
		{"transport-error", &nbToolDoer{err: errors.New("connrefused")}, map[string]string{"host": "h"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tl := NewTools(toolFixture(t, c.doer))[0]
			res, err := tl.Invoke(context.Background(), c.args)
			if err != nil {
				t.Fatalf("%s must be a soft ToolResult, got Go error %v", c.name, err)
			}
			if res.Success {
				t.Fatalf("%s must be a non-success, got Output=%q", c.name, res.Output)
			}
		})
	}
}

// TG-56: a nil module yields no tools — the config gate lives at the wiring site, and an unconfigured worker
// simply has no NetBox tool (dormant), never a nil-deref.
func TestNewToolsNilModuleIsDormant(t *testing.T) {
	if got := NewTools(nil); got != nil {
		t.Fatalf("a nil module must yield no tools (dormant), got %d", len(got))
	}
}
