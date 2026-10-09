package otlp

import (
	"context"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func logRec(sev logspb.SeverityNumber, body string, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		SeverityNumber: sev,
		SeverityText:   sev.String(),
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		Attributes:     attrs,
		TimeUnixNano:   uint64(time.Now().Add(-time.Minute).UnixNano()),
	}
}

func request(hostAttrs []*commonpb.KeyValue, recs ...*logspb.LogRecord) []byte {
	req := &collectorlogs.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource:  &resourcepb.Resource{Attributes: hostAttrs},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}},
		}},
	}
	b, err := proto.Marshal(req)
	if err != nil {
		panic(err)
	}
	return b
}

// The load-bearing decision: only ERROR+ and WARN records are incidents; INFO/DEBUG are dropped. KILLING
// MUTATION: drop the below-threshold skip in NormalizeBatch and the INFO/DEBUG records become envelopes,
// making this want-2 assertion fail.
func TestOTLP_MapsAlertWorthyRecordsOnly(t *testing.T) {
	raw := request([]*commonpb.KeyValue{strKV("host.name", "web01")},
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "disk full on /var", strKV("event.name", "DiskFull")),
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_WARN, "latency high"),
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "service started"),
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, "trace span"),
	)
	envs, err := New("otlp-test").NormalizeBatch(context.Background(), raw)
	if err != nil {
		t.Fatalf("NormalizeBatch: %v", err)
	}
	if len(envs) != 2 {
		t.Fatalf("want 2 alert-worthy envelopes (ERROR+WARN), got %d — INFO/DEBUG must be dropped", len(envs))
	}
	byRule := map[string]coreingest.IncidentEnvelope{}
	for _, e := range envs {
		byRule[e.AlertRule] = e
	}
	crit, ok := byRule["diskfull"]
	if !ok {
		t.Fatalf("no envelope keyed on the event.name slug 'diskfull'; got rules %v", byRule)
	}
	if crit.Severity != coreingest.SeverityCritical {
		t.Errorf("ERROR record severity = %v, want critical", crit.Severity)
	}
	if crit.Host != "web01" {
		t.Errorf("host = %q, want web01 (from the host.name resource attr)", crit.Host)
	}
	if crit.Summary != "disk full on /var" {
		t.Errorf("summary = %q, want the log body", crit.Summary)
	}
	if crit.SourceID != "otlp-test" {
		t.Errorf("source_id = %q, want otlp-test", crit.SourceID)
	}
}

func TestOTLP_MalformedRequestRejected(t *testing.T) {
	if _, err := New("").NormalizeBatch(context.Background(), []byte("not-a-protobuf-\xff\xfe")); err == nil {
		t.Error("a malformed OTLP request must be rejected whole")
	}
}

func TestOTLP_NoAlertWorthyRecordsRejected(t *testing.T) {
	raw := request([]*commonpb.KeyValue{strKV("host.name", "web01")},
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "started"),
	)
	if _, err := New("").NormalizeBatch(context.Background(), raw); err == nil {
		t.Error("a request with no alert-worthy records must be rejected (nothing to ingest)")
	}
}

func TestOTLP_NormalizeSingle(t *testing.T) {
	raw := request([]*commonpb.KeyValue{strKV("host.name", "db01")},
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, "oom killed", strKV("event.name", "OOMKill")),
	)
	env, err := New("otlp").Normalize(context.Background(), raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if env.Severity != coreingest.SeverityCritical || env.Host != "db01" || env.AlertRule != "oomkill" {
		t.Errorf("envelope = %+v, want critical/db01/oomkill", env)
	}
}

// A non-RFC-1123 host (Docker container names carry underscores) must be SANITIZED, not silently dropped —
// the record is an ERROR incident and must reach triage. KILLING MUTATION: pass the raw host to Normalize
// instead of sanitizeHost and this record drops (0 envelopes).
func TestOTLP_SanitizesHostSoValidAlertNotDropped(t *testing.T) {
	raw := request([]*commonpb.KeyValue{strKV("host.name", "My_Container_01")},
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "boom", strKV("event.name", "Crash")))
	envs, err := New("otlp").NormalizeBatch(context.Background(), raw)
	if err != nil || len(envs) != 1 {
		t.Fatalf("a non-RFC-1123 host silently dropped the alert: err=%v envs=%d", err, len(envs))
	}
	if envs[0].Host != "my-container-01" {
		t.Errorf("host = %q, want the sanitized my-container-01", envs[0].Host)
	}
}

// sanitizeHost must produce a VALID RFC-1123 hostname even for dot-adjacent junk (a "web01..example.com" from
// a template misconfig, a space after a dot) — the alert must ingest, not silently drop for a bad host.
func TestOTLP_SanitizeHostHandlesDotAdjacentJunk(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"web01..example.com", "web01.example.com"},
		{"a. b", "a.b"},
		{"us-east. #1.example.com", "us-east.1.example.com"},
	} {
		envs, err := New("otlp").NormalizeBatch(context.Background(),
			request([]*commonpb.KeyValue{strKV("host.name", tc.in)},
				logRec(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "x", strKV("event.name", "E"))))
		if err != nil || len(envs) != 1 {
			t.Fatalf("host %q dropped instead of sanitized to a valid hostname: err=%v envs=%d", tc.in, err, len(envs))
		}
		if envs[0].Host != tc.want {
			t.Errorf("host %q sanitized to %q, want %q", tc.in, envs[0].Host, tc.want)
		}
	}
}

// A structured (non-string) OTLP body must be RENDERED, not silently emptied — an incident with no summary
// is unhelpful.
func TestOTLP_RendersNonStringBody(t *testing.T) {
	rec := &logspb.LogRecord{
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
		Body:           &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 42}},
		Attributes:     []*commonpb.KeyValue{strKV("event.name", "Code")},
		TimeUnixNano:   uint64(time.Now().Add(-time.Minute).UnixNano()),
	}
	envs, err := New("otlp").NormalizeBatch(context.Background(), request([]*commonpb.KeyValue{strKV("host.name", "h1")}, rec))
	if err != nil || len(envs) != 1 {
		t.Fatalf("NormalizeBatch: err=%v envs=%d", err, len(envs))
	}
	if envs[0].Summary != "42" {
		t.Errorf("non-string body summary = %q, want the rendered 42, not empty", envs[0].Summary)
	}
}

// Correlate by (host, rule): two ERROR records of the same rule on a host, with DIFFERENT bodies (a retry
// counter), must share an ExternalRef so they collapse to one incident rather than flooding a new one per
// line. KILLING MUTATION: fold the body back into the ExternalRef and these diverge.
func TestOTLP_ExternalRefStableAcrossBodies(t *testing.T) {
	raw := request([]*commonpb.KeyValue{strKV("host.name", "h1")},
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "conn refused, retry 1", strKV("event.name", "ConnRefused")),
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "conn refused, retry 2", strKV("event.name", "ConnRefused")))
	envs, err := New("otlp").NormalizeBatch(context.Background(), raw)
	if err != nil || len(envs) != 2 {
		t.Fatalf("NormalizeBatch: err=%v envs=%d", err, len(envs))
	}
	if envs[0].ExternalRef != envs[1].ExternalRef {
		t.Errorf("same host+rule, different bodies produced different ExternalRefs (%q vs %q) — that floods a new incident per log line", envs[0].ExternalRef, envs[1].ExternalRef)
	}
}

func TestOTLP_BoundarySeverities(t *testing.T) {
	for _, tc := range []struct {
		n    logspb.SeverityNumber
		want coreingest.Severity
	}{
		{logspb.SeverityNumber_SEVERITY_NUMBER_WARN2, coreingest.SeverityWarning},   // 14
		{logspb.SeverityNumber_SEVERITY_NUMBER_ERROR2, coreingest.SeverityCritical}, // 18
		{logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, coreingest.SeverityCritical},  // 21
	} {
		envs, err := New("otlp").NormalizeBatch(context.Background(),
			request([]*commonpb.KeyValue{strKV("host.name", "h1")}, logRec(tc.n, "x", strKV("event.name", "E"))))
		if err != nil || len(envs) != 1 {
			t.Fatalf("severity %v: err=%v envs=%d", tc.n, err, len(envs))
		}
		if envs[0].Severity != tc.want {
			t.Errorf("severity %v mapped to %v, want %v", tc.n, envs[0].Severity, tc.want)
		}
	}
}

// slugify must yield the lowercase [a-z0-9-] slug the envelope validator accepts — a record whose only
// rule source is messy provider text must still normalize, not be dropped for a bad slug.
func TestOTLP_SlugifyYieldsValidSlug(t *testing.T) {
	raw := request([]*commonpb.KeyValue{strKV("host.name", "h1")},
		logRec(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "boom", strKV("event.name", "Disk Full: /var (90%)!!")),
	)
	envs, err := New("otlp").NormalizeBatch(context.Background(), raw)
	if err != nil || len(envs) != 1 {
		t.Fatalf("messy event.name did not normalize to a valid slug: err=%v envs=%d", err, len(envs))
	}
}
