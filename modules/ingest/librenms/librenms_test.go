package librenms

import (
	"context"
	"errors"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// fixedNow is a deterministic clock so the observed-timestamp window check is reproducible.
func fixedNow() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

func nlgr() *Module {
	return New([]Deployment{
		{Site: "NL", BaseURL: "https://librenms.nl.example", TokenRef: "env:LIBRENMS_NL_TOKEN"},
		{Site: "GR", BaseURL: "https://librenms.gr.example", TokenRef: "env:LIBRENMS_GR_TOKEN"},
	}, WithClock(fixedNow))
}

const deviceDownNL = `{
  "site": "NL", "id": "42", "rule": "Device Down", "severity": "critical",
  "host": "192.0.2.1", "hostname": "sw-core-01", "sysName": "sw-core-01",
  "title": "Device Down: sw-core-01", "timestamp": "2026-07-15 12:00:00", "state": 1
}`

func TestNormalizeDeviceDownFromNL(t *testing.T) {
	env, err := nlgr().Normalize(context.Background(), []byte(deviceDownNL))
	if err != nil {
		t.Fatalf("a well-formed device-down event must normalize: %v", err)
	}
	if env.SourceID != "librenms-NL" {
		t.Errorf("SourceID = %q, want librenms-NL", env.SourceID)
	}
	if env.ExternalRef != "librenms-NL-42" {
		t.Errorf("ExternalRef = %q, want librenms-NL-42", env.ExternalRef)
	}
	if env.AlertRule != "Device-Down" { // whitespace slugified for the join-safe grammar
		t.Errorf("AlertRule = %q, want Device-Down", env.AlertRule)
	}
	if env.Severity != coreingest.SeverityCritical {
		t.Errorf("Severity = %v, want critical", env.Severity)
	}
	if env.Host != "sw-core-01" {
		t.Errorf("Host = %q, want sw-core-01", env.Host)
	}
	if env.IP == nil || env.IP.String() != "192.0.2.1" {
		t.Errorf("IP = %v, want 192.0.2.1", env.IP)
	}
	if env.Site != "NL" {
		t.Errorf("Site = %q, want NL", env.Site)
	}
}

func TestRecoveryMapsToInfo(t *testing.T) {
	recovery := `{"site":"GR","id":"7","rule":"Device Down","severity":"critical","hostname":"sw-edge-02","timestamp":"2026-07-15 12:01:00","state":0}`
	env, err := nlgr().Normalize(context.Background(), []byte(recovery))
	if err != nil {
		t.Fatalf("a recovery event must normalize: %v", err)
	}
	if env.Severity != coreingest.SeverityInfo {
		t.Errorf("a recovery (state 0) must map to info regardless of configured severity, got %v", env.Severity)
	}
	if env.SourceID != "librenms-GR" {
		t.Errorf("SourceID = %q, want librenms-GR", env.SourceID)
	}
}

// Real LibreNMS posts the rule name in `title` and sends no `rule` field. Before the title fallback, the
// empty `rule` produced an empty alert_rule and the alert was rejected before triage.
func TestRuleIdentityFallsBackToTitle(t *testing.T) {
	realLibreNMS := `{"site":"NL","id":"99","title":"Device Down","severity":"critical","host":"192.0.2.2","hostname":"sw-core-02","timestamp":"2026-07-15 12:00:00","state":1}`
	env, err := nlgr().Normalize(context.Background(), []byte(realLibreNMS))
	if err != nil {
		t.Fatalf("a real-LibreNMS alert (rule name in title, no rule field) must normalize, not be dropped: %v", err)
	}
	if env.AlertRule != "Device-Down" {
		t.Errorf("AlertRule must fall back to title, got %q want Device-Down", env.AlertRule)
	}
}

func TestEventFromUnconfiguredSiteHasNoPath(t *testing.T) {
	// INV-18: the module serves exactly its configured rows; an event from an unknown instance is rejected.
	unknown := `{"site":"ZZ","id":"1","rule":"Device Down","severity":"critical","hostname":"h","timestamp":"2026-07-15 12:00:00","state":1}`
	if _, err := nlgr().Normalize(context.Background(), []byte(unknown)); err == nil {
		t.Fatal("an event from an unconfigured site must be rejected")
	}
}

// Real LibreNMS does not send a site field (the predecessor used one receiver URL per site). A
// single-deployment module must normalize a real (site-less) payload using its sole config row; a
// multi-deployment module must refuse a site-less payload as ambiguous. Regression for the finding that
// the module required a site field real LibreNMS omits.
func TestSitelessPayloadUsesSoleDeploymentElseAmbiguous(t *testing.T) {
	siteless := `{"id":"7","rule":"Device Down","severity":"critical","host":"192.0.2.9","hostname":"sw-edge-09","title":"Device Down","timestamp":"2026-07-15 12:00:00","state":1}`
	// single deployment → the sole config row is used, no site field required.
	single := New([]Deployment{{Site: "NL", BaseURL: "https://librenms.nl", TokenRef: "env:X"}}, WithClock(fixedNow))
	env, err := single.Normalize(context.Background(), []byte(siteless))
	if err != nil {
		t.Fatalf("a single-deployment module must normalize a site-less real payload: %v", err)
	}
	if env.SourceID != "librenms-NL" || env.Site != "NL" {
		t.Errorf("the sole deployment must be applied, got SourceID=%q Site=%q", env.SourceID, env.Site)
	}
	// multiple deployments + no site → refused as ambiguous.
	if _, err := nlgr().Normalize(context.Background(), []byte(siteless)); err == nil {
		t.Fatal("a multi-deployment module must refuse a site-less payload as ambiguous")
	}
}

// LibreNMS renders its alert `$timestamp` in the SERVER's local timezone (a naive datetime, no offset).
// Parsing it as UTC made every alert from a UTC+N server look N hours future-dated, and the freshness window
// DROPPED it (the live 400 seen in prod for the Athens/Amsterdam servers). With the deployment's Timezone
// set, the naive local time resolves to the correct UTC instant and the alert normalizes.
func TestLocalTimestampResolvedInSiteTimezone(t *testing.T) {
	athens := New([]Deployment{
		{Site: "GR", BaseURL: "https://librenms.gr", TokenRef: "env:X", Timezone: "Europe/Athens"}, // UTC+3 in July
	}, WithClock(fixedNow)) // now = 2026-07-15 12:05:00 UTC
	// 15:00 Athens (EEST, UTC+3) == 12:00:00 UTC — five minutes BEFORE now, so it is valid, not future.
	local := `{"site":"GR","id":"5","rule":"Device Down","severity":"critical","host":"10.0.0.3","hostname":"sw-gr-03","timestamp":"2026-07-15 15:00:00","state":1}`
	env, err := athens.Normalize(context.Background(), []byte(local))
	if err != nil {
		t.Fatalf("a local-time Athens timestamp must resolve in-zone and normalize, not be dropped as future: %v", err)
	}
	if want := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC); !env.ObservedAt.Equal(want) {
		t.Errorf("ObservedAt = %s, want %s (15:00 Athens = 12:00 UTC)", env.ObservedAt.UTC(), want)
	}
}

// Safety net: if the timezone is unset/mislabeled (defaults UTC here) so the timestamp still lands in the
// future, a real just-arrived push must NOT be dropped — the timestamp degrades to receive time instead of
// the whole alert being rejected by the freshness window.
func TestFutureTimestampClampsToNowNotRejected(t *testing.T) {
	utcDefault := New([]Deployment{
		{Site: "GR", BaseURL: "https://librenms.gr", TokenRef: "env:X"}, // no Timezone ⇒ UTC
	}, WithClock(fixedNow)) // now = 2026-07-15 12:05:00 UTC
	// 15:00 read as UTC is ~3h ahead of now — before the safety net this 400'd; now it clamps to now.
	future := `{"site":"GR","id":"6","rule":"Device Down","severity":"critical","host":"10.0.0.4","hostname":"sw-gr-04","timestamp":"2026-07-15 15:00:00","state":1}`
	env, err := utcDefault.Normalize(context.Background(), []byte(future))
	if err != nil {
		t.Fatalf("a future-dated push must clamp to receive time, not be rejected: %v", err)
	}
	if !env.ObservedAt.Equal(fixedNow()) {
		t.Errorf("a future timestamp must clamp to now (%s), got ObservedAt = %s", fixedNow(), env.ObservedAt.UTC())
	}
}

func TestMalformedPayloadRejected(t *testing.T) {
	if _, err := nlgr().Normalize(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("a malformed payload must be rejected")
	}
}

func TestUnknownSeverityRejectedByGrammar(t *testing.T) {
	// A fault with an unrecognized severity must be rejected by the core grammar (fail closed), not defaulted.
	bad := `{"site":"NL","id":"9","rule":"Device Down","severity":"wat","hostname":"h","timestamp":"2026-07-15 12:00:00","state":1}`
	if _, err := nlgr().Normalize(context.Background(), []byte(bad)); !errors.Is(err, coreingest.ErrBadSeverity) {
		t.Fatalf("an unknown severity must be rejected by the grammar, got %v", err)
	}
}
