package librenms

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/agent"
)

// fakeDoer models LibreNMS: an exact path (or path?query) route returns its canned body; anything else 404s.
// This distinguishes GET /api/v0/devices/{host} (a per-host lookup that MISSES for a sysName) from
// GET /api/v0/devices (the authoritative list) — the exact behaviour resolveDevice's fallback depends on.
type fakeDoer struct{ routes map[string]string }

func (f fakeDoer) Do(req *http.Request) (*http.Response, error) {
	key := req.URL.Path
	if req.URL.RawQuery != "" {
		key += "?" + req.URL.RawQuery
	}
	if body, ok := f.routes[key]; ok {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	if body, ok := f.routes[req.URL.Path]; ok {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"status":"error"}`))}, nil
}

func toolByName(tools []agent.Tool, name string) agent.Tool {
	for _, x := range tools {
		if x.Name() == name {
			return x
		}
	}
	return nil
}

func TestLibreNMSTools(t *testing.T) {
	os.Setenv("LNTEST_TOKEN", "tok-abc")
	deps := []Deployment{{Site: "nl", BaseURL: "https://ln.test", TokenRef: "env:LNTEST_TOKEN"}}

	// web01: a named server (hostname == its name) → resolves on the fast path. Its row carries SNMP secrets
	// that MUST NOT reach tool output. netsw01: a switch whose hostname is an IP → only its sysName matches,
	// exercising the list fallback (a direct /devices/netsw01 lookup 404s).
	devBody := `{"devices":[{"device_id":7,"hostname":"web01","sysName":"web01.local","status":0,"os":"linux","type":"server","hardware":"KVM guest","uptime":7200,"last_polled":"2026-07-17 10:00:00","community":"PUBLICSECRET","authpass":"AUTHSECRET","cryptopass":"CRYPTOSECRET"}]}`
	listBody := `{"devices":[{"device_id":7,"hostname":"web01","sysName":"web01.local","status":0,"os":"linux","type":"server"},{"device_id":8,"hostname":"192.0.2.9","sysName":"netsw01.example.net","status":1,"os":"ios","type":"network","community":"SWSECRET"}]}`
	rulesBody := `{"rules":[{"id":1,"name":"Devices up/down","severity":"critical"}]}`
	alertsBody := `{"alerts":[{"id":9,"device_id":7,"rule_id":1,"state":1,"timestamp":"2026-07-17 09:55:00"}]}`
	eventsBody := `{"logs":[{"datetime":"2026-07-17 09:54:00","type":"reachability","message":"Device went down (ICMP)","severity":5}]}`
	doer := fakeDoer{routes: map[string]string{
		"/api/v0/devices/web01":   devBody,
		"/api/v0/devices":         listBody,
		"/api/v0/rules":           rulesBody,
		"/api/v0/alerts?state=1":  alertsBody,
		"/api/v0/logs/eventlog/7": eventsBody,
	}}
	tools := NewTools(deps, doer)
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tools))
	}

	// get-device-status: real status + NO secret leak (fast path).
	res, _ := toolByName(tools, "get-device-status").Invoke(context.Background(), map[string]string{"host": "web01"})
	if !res.Success || !strings.Contains(res.Output, "status=DOWN") || !strings.Contains(res.Output, "os=linux") {
		t.Fatalf("device-status: want DOWN linux, got success=%v %q", res.Success, res.Output)
	}
	for _, secret := range []string{"PUBLICSECRET", "AUTHSECRET", "CRYPTOSECRET", "SWSECRET"} {
		if strings.Contains(res.Output, secret) {
			t.Fatalf("SNMP SECRET LEAKED (%s) in tool output: %q", secret, res.Output)
		}
	}
	if res.ID != "lnms-dev-web01" {
		t.Errorf("stable tool id expected, got %q", res.ID)
	}

	// sysName-only host resolves via the list fallback (direct /devices/netsw01 404s).
	rs, _ := toolByName(tools, "get-device-status").Invoke(context.Background(), map[string]string{"host": "netsw01"})
	if !rs.Success || !strings.Contains(rs.Output, "status=UP") || !strings.Contains(rs.Output, "os=ios") {
		t.Fatalf("sysName fallback: want UP ios, got success=%v %q", rs.Success, rs.Output)
	}
	if strings.Contains(rs.Output, "SWSECRET") {
		t.Fatalf("SNMP SECRET LEAKED (SWSECRET) via list fallback: %q", rs.Output)
	}

	// get-active-alerts: enriched with rule name + severity, filtered to the device.
	ra, _ := toolByName(tools, "get-active-alerts").Invoke(context.Background(), map[string]string{"host": "web01"})
	if !ra.Success || !strings.Contains(ra.Output, "Devices up/down") || !strings.Contains(ra.Output, "critical") {
		t.Fatalf("active-alerts: want enriched rule, got %q", ra.Output)
	}

	// get-device-eventlog: the recent events (queried by resolved device_id).
	re, _ := toolByName(tools, "get-device-eventlog").Invoke(context.Background(), map[string]string{"host": "web01"})
	if !re.Success || !strings.Contains(re.Output, "ICMP") {
		t.Fatalf("eventlog: want the ICMP event, got %q", re.Output)
	}

	// unknown host: fail-soft (Success=false, reason as data), never a Go error.
	rn, err := toolByName(tools, "get-device-status").Invoke(context.Background(), map[string]string{"host": "ghost99"})
	if err != nil {
		t.Fatalf("unknown host must not error: %v", err)
	}
	if rn.Success || !strings.Contains(rn.Output, "ghost99") {
		t.Fatalf("unknown host: want fail-soft with reason, got success=%v %q", rn.Success, rn.Output)
	}

	// every tool is read-only.
	for _, x := range tools {
		if !x.ReadOnly() {
			t.Errorf("tool %s must be read-only", x.Name())
		}
	}
}

func TestNormHost(t *testing.T) {
	cases := map[string]string{
		"dc1ap01.example.net": "dc1ap01",
		"dc1bookwyrm01":               "dc1bookwyrm01",
		"192.168.181.1":                   "192.168.181.1", // dotted IP kept intact
		"dc1cl01file01  # comment":    "dc1cl01file01",
		"  Web01.Local ":                  "web01",
	}
	for in, want := range cases {
		if got := normHost(in); got != want {
			t.Errorf("normHost(%q) = %q, want %q", in, got, want)
		}
	}
}
