package crowdsec

import (
	"context"
	"testing"
	"time"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

func fixedNow() time.Time { return time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC) }

func mod() *Module { return New(WithClock(fixedNow)) }

const banEvent = `{
  "scenario":"crowdsecurity/ssh-bf","message":"Ip 1.2.3.4 performed ssh-bf",
  "start_at":"2026-07-15T12:00:00Z",
  "source":{"scope":"Ip","value":"1.2.3.4","ip":"1.2.3.4"},
  "decisions":[{"type":"ban","value":"1.2.3.4","scenario":"crowdsecurity/ssh-bf","origin":"crowdsec"}]
}`

func TestNormalizeBanDecision(t *testing.T) {
	env, err := mod().Normalize(context.Background(), []byte(banEvent))
	if err != nil {
		t.Fatalf("a well-formed decision event must normalize: %v", err)
	}
	if env.AlertRule != "crowdsecurity/ssh-bf" {
		t.Errorf("AlertRule = %q, want crowdsecurity/ssh-bf", env.AlertRule)
	}
	if env.Severity != coreingest.SeverityWarning {
		t.Errorf("a ban decision must map to warning, got %v", env.Severity)
	}
	if env.IP == nil || env.IP.String() != "1.2.3.4" {
		t.Errorf("IP = %v, want 1.2.3.4", env.IP)
	}
	if env.ExternalRef != "crowdsec-crowdsecurity/ssh-bf-1.2.3.4" {
		t.Errorf("ExternalRef = %q, want crowdsec-crowdsecurity/ssh-bf-1.2.3.4", env.ExternalRef)
	}
}

func TestAlertWithNoDecisionIsInfo(t *testing.T) {
	noDecision := `{"scenario":"crowdsecurity/http-probing","message":"probe","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"5.6.7.8","ip":"5.6.7.8"},"decisions":[]}`
	env, err := mod().Normalize(context.Background(), []byte(noDecision))
	if err != nil {
		t.Fatalf("an alert with no decision must normalize: %v", err)
	}
	if env.Severity != coreingest.SeverityInfo {
		t.Errorf("an alert with no enforcement decision must be info, got %v", env.Severity)
	}
}

// Severity is classified from the SCENARIO name, not merely the decision type: a CVE/exploit/RCE scenario is
// critical even when the only enforcement is a ban (the predecessor rates it critical). Before the fix a
// ban→warning mapping silently downgraded exploit alerts two tiers.
func TestExploitScenarioIsCritical(t *testing.T) {
	for _, sc := range []string{"crowdsecurity/http-cve-probing", "crowdsecurity/log4j-rce", "crowdsecurity/apache-backdoor-exploit"} {
		alert := `{"scenario":"` + sc + `","message":"x","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"9.9.9.9","ip":"9.9.9.9"},"decisions":[{"type":"ban","value":"9.9.9.9"}]}`
		env, err := mod().Normalize(context.Background(), []byte(alert))
		if err != nil {
			t.Fatalf("%s must normalize: %v", sc, err)
		}
		if env.Severity != coreingest.SeverityCritical {
			t.Errorf("%s must be critical, got %v", sc, env.Severity)
		}
	}
}

func TestRejectsMissingScenarioSourceAndMalformed(t *testing.T) {
	noScenario := `{"message":"x","source":{"scope":"Ip","value":"1.1.1.1"},"start_at":"2026-07-15T12:00:00Z"}`
	if _, err := mod().Normalize(context.Background(), []byte(noScenario)); err == nil {
		t.Fatal("an alert missing its scenario must be rejected")
	}
	noSource := `{"scenario":"crowdsecurity/ssh-bf","message":"x","start_at":"2026-07-15T12:00:00Z"}`
	if _, err := mod().Normalize(context.Background(), []byte(noSource)); err == nil {
		t.Fatal("an alert missing its source value must be rejected")
	}
	if _, err := mod().Normalize(context.Background(), []byte(`{not json`)); err == nil {
		t.Fatal("a malformed alert must be rejected")
	}
}

// The REAL CrowdSec http notification plugin serializes the whole []models.Alert with its default
// `format: {{ .|toJson }}` — TG receives a JSON ARRAY, not a single object. Before the batch fix the
// single-object Normalize rejected every real push ("cannot unmarshal array into Go struct"). NormalizeBatch
// must fan the array out to one envelope per alert.
func TestNormalizeBatchArrayFansOut(t *testing.T) {
	arr := `[` + banEvent + `,` +
		`{"scenario":"crowdsecurity/http-probing","message":"probe","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"5.6.7.8","ip":"5.6.7.8"},"decisions":[]}` + `]`
	envs, err := mod().NormalizeBatch(context.Background(), []byte(arr))
	if err != nil {
		t.Fatalf("a CrowdSec array body must normalize: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("a 2-alert array must yield 2 envelopes, got %d", len(envs))
	}
	if envs[0].ExternalRef != "crowdsec-crowdsecurity/ssh-bf-1.2.3.4" {
		t.Errorf("first envelope ExternalRef = %q", envs[0].ExternalRef)
	}
}

// A non-Ip scope (Range/Username/Country/AS) carries a CIDR/name/code in `value`, NOT an address. Force-
// fitting it into the IP field made core's validateIP return ErrBadIP → a 400 that dropped an otherwise
// valid security signal. The fix leaves IP empty (valid) and still triages the alert by scenario+value.
func TestNonIPScopeLeavesIPEmpty(t *testing.T) {
	rangeAlert := `{"scenario":"crowdsecurity/http-bf","message":"range ban","start_at":"2026-07-15T12:00:00Z","source":{"scope":"Range","value":"1.2.3.0/24","range":"1.2.3.0/24"},"decisions":[{"type":"ban","value":"1.2.3.0/24"}]}`
	env, err := mod().Normalize(context.Background(), []byte(rangeAlert))
	if err != nil {
		t.Fatalf("a Range-scoped alert must normalize (not 400 on a CIDR-in-IP): %v", err)
	}
	if env.IP != nil {
		t.Errorf("a Range-scoped source must leave IP empty, got %v", env.IP)
	}
	if env.ExternalRef != "crowdsec-crowdsecurity/http-bf-1.2.3.0/24" {
		t.Errorf("ExternalRef must still carry the offending range, got %q", env.ExternalRef)
	}
}

// A simulation-mode alert is a what-if (CrowdSec would have banned but did not enforce) — it must not
// auto-escalate as a real critical. The scenario-based classifier still runs; simulated only caps critical.
func TestSimulatedCapsCritical(t *testing.T) {
	sim := `{"scenario":"crowdsecurity/log4j-rce","message":"sim","simulated":true,"start_at":"2026-07-15T12:00:00Z","source":{"scope":"Ip","value":"9.9.9.9","ip":"9.9.9.9"},"decisions":[{"type":"ban","value":"9.9.9.9","simulated":true}]}`
	env, err := mod().Normalize(context.Background(), []byte(sim))
	if err != nil {
		t.Fatalf("a simulated alert must normalize: %v", err)
	}
	if env.Severity != coreingest.SeverityWarning {
		t.Errorf("a simulated exploit must cap at warning (not critical), got %v", env.Severity)
	}
}

// One malformed alert in a grouped push must be dropped individually — its well-formed siblings still
// normalize, so a single bad series cannot suppress the rest of the batch (INV-04 per alert).
func TestBatchDropsBadAlertKeepsSiblings(t *testing.T) {
	arr := `[{"message":"no scenario","source":{"scope":"Ip","value":"1.1.1.1"}},` + banEvent + `]`
	envs, err := mod().NormalizeBatch(context.Background(), []byte(arr))
	if err != nil {
		t.Fatalf("a batch with one bad alert must not fail whole: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("the bad alert must be dropped and the good one kept, got %d envelopes", len(envs))
	}
}

// The normalized event must route through the shared in-code admission chain and publish a triage.requested.
func TestRoutesThroughSharedAdmission(t *testing.T) {
	env, err := mod().Normalize(context.Background(), []byte(banEvent))
	if err != nil {
		t.Fatal(err)
	}
	batch := coreingest.NewPipeline().Process([]coreingest.IncidentEnvelope{env}, fixedNow())
	pub := &coreingest.RecordingPublisher{}
	n, err := coreingest.PublishTriage(context.Background(), pub, batch, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the decision event must route through admission and publish one triage.requested, got %d", n)
	}
}
