package alertmanager

import (
	"context"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

func fixedNow() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

func mod() *Module { return New(WithClock(fixedNow)) }

const firing = `{"status":"firing","alerts":[{"status":"firing",
  "labels":{"alertname":"MeshBFDSessionDown","instance":"nl-frr01:9100","severity":"warning"},
  "annotations":{"summary":"BFD session down"},"startsAt":"2026-07-15T12:00:00Z","fingerprint":"abc"}]}`

const resolved = `{"status":"resolved","alerts":[{"status":"resolved",
  "labels":{"alertname":"MeshBFDSessionDown","instance":"nl-frr01:9100","severity":"warning"},
  "annotations":{"summary":"BFD session recovered"},"startsAt":"2026-07-15T12:00:00Z","fingerprint":"abc"}]}`

func TestNormalizeFiringAlert(t *testing.T) {
	env, err := mod().Normalize(context.Background(), []byte(firing))
	if err != nil {
		t.Fatalf("a well-formed firing alert must normalize: %v", err)
	}
	if env.AlertRule != "MeshBFDSessionDown" {
		t.Errorf("AlertRule = %q, want MeshBFDSessionDown", env.AlertRule)
	}
	if env.Severity != coreingest.SeverityWarning {
		t.Errorf("Severity = %v, want warning", env.Severity)
	}
	if env.Host != "nl-frr01" { // stripped of :9100
		t.Errorf("Host = %q, want nl-frr01", env.Host)
	}
	if env.ExternalRef != "am-MeshBFDSessionDown-nl-frr01" {
		t.Errorf("ExternalRef = %q, want am-MeshBFDSessionDown-nl-frr01", env.ExternalRef)
	}
}

// A firing alert and its later resolved transition for the same series share ONE correlation key, so the
// admission chain collapses them to a single incident (REQ-802).
func TestFiringThenResolvedCorrelateToOneIncident(t *testing.T) {
	m := mod()
	f, err := m.Normalize(context.Background(), []byte(firing))
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Normalize(context.Background(), []byte(resolved))
	if err != nil {
		t.Fatal(err)
	}
	if f.CorrelationKey() != r.CorrelationKey() {
		t.Fatalf("firing and resolved must share a correlation key: %q vs %q", f.CorrelationKey(), r.CorrelationKey())
	}
	if r.Severity != coreingest.SeverityInfo {
		t.Errorf("a resolved transition must map to info, got %v", r.Severity)
	}
	// through admission: two non-duplicate envelopes, same correlation group ⇒ one triage.requested.
	batch := coreingest.NewPipeline().Process([]coreingest.IncidentEnvelope{f, r}, fixedNow())
	pub := &coreingest.RecordingPublisher{}
	n, err := coreingest.PublishTriage(context.Background(), pub, batch, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a firing→resolved transition must correlate to one incident, published %d", n)
	}
}

func TestNormalizeBatchReturnsOnePerAlert(t *testing.T) {
	multi := `{"status":"firing","alerts":[
	  {"status":"firing","labels":{"alertname":"A","instance":"h1:1","severity":"critical"},"startsAt":"2026-07-15T12:00:00Z"},
	  {"status":"firing","labels":{"alertname":"B","instance":"h2:2","severity":"warning"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	envs, err := mod().NormalizeBatch(context.Background(), []byte(multi))
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 {
		t.Fatalf("NormalizeBatch must return one envelope per alert, got %d", len(envs))
	}
	// the single-alert interface method rejects a multi-alert webhook.
	if _, err := mod().Normalize(context.Background(), []byte(multi)); err == nil {
		t.Fatal("Normalize must reject a multi-alert webhook")
	}
}

// The always-firing meta-alerts (Watchdog / InfoInhibitor) and info-severity noise are dropped at the ingest
// boundary, matching the predecessor receiver; a real warning/critical alert in the same batch survives. A
// RESOLVED transition of a real alert must NOT be dropped (its raw severity label is still warning/critical,
// even though toEnvelope maps the resolved severity to info).
func TestWatchdogAndInfoAlertsAreFiltered(t *testing.T) {
	batch := `{"status":"firing","alerts":[
	  {"status":"firing","labels":{"alertname":"Watchdog","severity":"none"},"startsAt":"2026-07-15T12:00:00Z"},
	  {"status":"firing","labels":{"alertname":"InfoInhibitor","severity":"info"},"startsAt":"2026-07-15T12:00:00Z"},
	  {"status":"firing","labels":{"alertname":"DiskWillFill","instance":"h9:9","severity":"info"},"startsAt":"2026-07-15T12:00:00Z"},
	  {"status":"resolved","labels":{"alertname":"MeshBFDSessionDown","instance":"nl-frr01:9100","severity":"warning"},"startsAt":"2026-07-15T12:00:00Z"},
	  {"status":"firing","labels":{"alertname":"RealDown","instance":"h1:1","severity":"critical"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	envs, err := mod().NormalizeBatch(context.Background(), []byte(batch))
	if err != nil {
		t.Fatal(err)
	}
	// survivors: the resolved MeshBFDSessionDown (raw severity warning) and the firing RealDown; the Watchdog,
	// InfoInhibitor, and info-severity DiskWillFill are dropped.
	if len(envs) != 2 {
		t.Fatalf("Watchdog/InfoInhibitor/info alerts must be filtered, got %d survivors: %+v", len(envs), envs)
	}
	got := map[string]bool{}
	for _, e := range envs {
		got[e.AlertRule] = true
	}
	if !got["MeshBFDSessionDown"] || !got["RealDown"] {
		t.Fatalf("a resolved real alert and a firing critical must survive, got %+v", got)
	}
}

func TestRejectsMissingAlertnameAndMalformed(t *testing.T) {
	noName := `{"alerts":[{"status":"firing","labels":{"instance":"h:1","severity":"warning"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	if _, err := mod().Normalize(context.Background(), []byte(noName)); err == nil {
		t.Fatal("an alert missing its alertname must be rejected")
	}
	if _, err := mod().NormalizeBatch(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("a malformed webhook must be rejected")
	}
	if _, err := mod().NormalizeBatch(context.Background(), []byte(`{"alerts":[]}`)); err == nil {
		t.Fatal("a webhook with no alerts must be rejected")
	}
}

// One grammar-failing alert in a grouped webhook is rejected INDIVIDUALLY — its well-formed siblings still
// normalize, so a single bad series cannot suppress the rest of the group's incidents.
func TestBadAlertDoesNotDiscardSiblings(t *testing.T) {
	batch := `{"status":"firing","alerts":[
	  {"status":"firing","labels":{"instance":"h:1","severity":"warning"},"startsAt":"2026-07-15T12:00:00Z"},
	  {"status":"firing","labels":{"alertname":"RealDown","instance":"h1:1","severity":"critical"},"startsAt":"2026-07-15T12:00:00Z"}]}`
	envs, err := mod().NormalizeBatch(context.Background(), []byte(batch))
	if err != nil {
		t.Fatalf("a batch with one bad alert must not fail whole: %v", err)
	}
	if len(envs) != 1 || envs[0].AlertRule != "RealDown" {
		t.Fatalf("the well-formed sibling must survive, got %+v", envs)
	}
}
