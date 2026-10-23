package librenms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// pathDoer routes a GET to a canned body by URL path prefix.
type pathDoer struct{ byPath map[string]string }

func (f pathDoer) Do(req *http.Request) (*http.Response, error) {
	body, ok := f.byPath[req.URL.Path]
	code := http.StatusOK
	if !ok {
		body, code = `{}`, http.StatusNotFound
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

// The device response deliberately carries SNMP secrets; the puller MUST drop them (apiDevice never declares
// them). We assert no secret string reaches any envelope.
const (
	secretCommunity = "PUBLIC-BUT-SECRET-COMMUNITY-STRING"
	secretAuthpass  = "SUPER-SECRET-AUTHPASS"
)

func newFakeAlertSource(t *testing.T, alertsBody string) *AlertSource {
	t.Helper()
	t.Setenv("TG_TEST_LN_TOKEN", "test-token")
	fake := pathDoer{byPath: map[string]string{
		"/api/v0/rules":   `{"rules":[{"id":1,"name":"Device Down","severity":"critical"}]}`,
		"/api/v0/devices": `{"devices":[{"device_id":42,"hostname":"web01.nl.example","sysName":"web01","community":"` + secretCommunity + `","authpass":"` + secretAuthpass + `","cryptopass":"x"}]}`,
		"/api/v0/alerts":  alertsBody,
	}}
	fixedNow := func() time.Time { return time.Date(2026, 7, 17, 10, 5, 0, 0, time.UTC) }
	return NewAlertSource(
		[]Deployment{{Site: "nl", BaseURL: "https://ln.test", TokenRef: "env:TG_TEST_LN_TOKEN"}},
		WithAlertHTTPClient(fake), WithAlertClock(fixedNow),
	)
}

func TestAlertSourceEnrichesAndNormalizes(t *testing.T) {
	src := newFakeAlertSource(t, `{"status":"ok","alerts":[{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`)
	envs, _, err := src.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("FetchActive: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(envs))
	}
	e := envs[0]
	if e.ExternalRef != "librenms-nl-7" {
		t.Errorf("external_ref = %q, want librenms-nl-7", e.ExternalRef)
	}
	if e.Host != "web01.nl.example" {
		t.Errorf("host = %q, want web01.nl.example (enriched from device map)", e.Host)
	}
	if e.AlertRule != "Device-Down" {
		t.Errorf("alert_rule = %q, want Device-Down (enriched + slugified from rule map)", e.AlertRule)
	}
	if e.Severity != coreingest.SeverityCritical {
		t.Errorf("severity = %v, want critical (enriched from rule map)", e.Severity)
	}
	// No SNMP secret from the device response may reach the envelope.
	blob, _ := json.Marshal(e)
	if strings.Contains(string(blob), secretCommunity) || strings.Contains(string(blob), secretAuthpass) {
		t.Fatalf("SNMP secret leaked into the envelope: %s", blob)
	}
}

func TestAlertSourceExternalRefStable(t *testing.T) {
	body := `{"status":"ok","alerts":[{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	a, _, _ := newFakeAlertSource(t, body).FetchActive(context.Background())
	b, _, _ := newFakeAlertSource(t, body).FetchActive(context.Background())
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want 1 each, got %d and %d", len(a), len(b))
	}
	if a[0].ExternalRef != b[0].ExternalRef {
		t.Fatalf("external_ref not stable: %q vs %q (breaks REJECT_DUPLICATE dedup)", a[0].ExternalRef, b[0].ExternalRef)
	}
}

func TestAlertSourceGracefulOnUnknownDeviceAndRule(t *testing.T) {
	// device 99 and rule 99 are absent from the maps: host degrades to empty (a not-host-scoped alert is
	// still valid) and the rule name falls back to a stable label so alert_rule is never empty.
	src := newFakeAlertSource(t, `{"status":"ok","alerts":[{"id":9,"device_id":99,"rule_id":99,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`)
	envs, _, err := src.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("FetchActive: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("want 1 envelope even with unknown device/rule, got %d", len(envs))
	}
	e := envs[0]
	if e.Host != "" {
		t.Errorf("host = %q, want empty for an unknown device (graceful)", e.Host)
	}
	if e.AlertRule != "librenms-alert-rule-99" {
		t.Errorf("alert_rule = %q, want the stable fallback librenms-alert-rule-99", e.AlertRule)
	}
}

func TestAlertSourceNoActiveAlerts(t *testing.T) {
	src := newFakeAlertSource(t, `{"status":"ok","alerts":[]}`)
	envs, _, err := src.FetchActive(context.Background())
	if err != nil {
		t.Fatalf("FetchActive: %v", err)
	}
	if len(envs) != 0 {
		t.Fatalf("want 0 envelopes for a quiet estate, got %d", len(envs))
	}
}

// The minAge SAFETY-NET gate (WithAlertMinAge): the fake clock is 10:05 and the alert fired at 10:00 (age 5m).
// minAge=0 pulls it (air-gapped primary intake); a minAge below 5m pulls it (stale = a missed push); a minAge
// above 5m withholds it (too young — a transient push's anti-flap delay would suppress it). Guards the
// resilience fix for the push-dependency exposed by the 2026-07-24 guinea-pig drill.
func TestAlertSourceMinAgeSafetyNet(t *testing.T) {
	const body = `{"status":"ok","alerts":[{"id":7,"device_id":42,"rule_id":1,"state":1,"timestamp":"2026-07-17 10:00:00"}]}`
	cases := []struct {
		name   string
		minAge time.Duration
		want   int
	}{
		{"minAge=0 pulls everything (air-gapped primary intake, behaviour-preserving)", 0, 1},
		{"minAge below firing age pulls it (stale = a missed push)", 2 * time.Minute, 1},
		{"minAge above firing age withholds it (too young = a transient)", 10 * time.Minute, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := newFakeAlertSource(t, body)
			src.minAge = c.minAge
			envs, _, err := src.FetchActive(context.Background())
			if err != nil {
				t.Fatalf("FetchActive: %v", err)
			}
			if len(envs) != c.want {
				t.Fatalf("minAge=%s: got %d envelope(s), want %d", c.minAge, len(envs), c.want)
			}
		})
	}

	// A blank/unparseable LibreNMS timestamp is stamped at receipt (age≈0), so the safety-net withholds it
	// (minAge>0) but the primary-intake mode still pulls it (minAge=0) — no alert is silently lost by mode.
	const blank = `{"status":"ok","alerts":[{"id":8,"device_id":42,"rule_id":1,"state":1,"timestamp":"not-a-timestamp"}]}`
	gated := newFakeAlertSource(t, blank)
	gated.minAge = 2 * time.Minute
	if envs, wh, _ := gated.FetchActive(context.Background()); len(envs) != 0 || wh != 1 {
		t.Fatalf("blank timestamp under minAge=2m: got %d env, %d withheld; want 0 env, 1 withheld (observable, not silent)", len(envs), wh)
	}
	if envs, wh, _ := newFakeAlertSource(t, blank).FetchActive(context.Background()); len(envs) != 1 || wh != 0 {
		t.Fatalf("blank timestamp under minAge=0: got %d env, %d withheld; want 1 env, 0 withheld (primary intake pulls it)", len(envs), wh)
	}
}
