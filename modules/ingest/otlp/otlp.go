// Package otlp is TG's OpenTelemetry (OTLP) log ingest adapter (TG-32): it speaks the vendor-neutral,
// self-hostable observability standard rather than any single SaaS. It maps ALERT-WORTHY OTLP log records to
// TG's canonical IncidentEnvelope through the same validator every other ingest source uses. This is slice 1 —
// the adapter (normalize) only; the OTLP receiver endpoint and its registry gate (INV-17) are a later slice.
//
// It is READ-ONLY ingest: it turns a provider payload into incidents, it never actuates. OTLP carries mostly
// non-alert telemetry, so the load-bearing decision is WHICH records are incidents — only severity ERROR and
// above are alerts (WARN is a warning); quieter records are dropped. Per-record isolation (INV-04): a
// malformed request is rejected whole, but a single record failing validation is dropped without discarding
// its well-formed siblings.
package otlp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	adaptingest "github.com/territory-grounder/grounder/adapters/ingest"
	coreingest "github.com/territory-grounder/grounder/core/ingest"
)

// SourceType is the stable ingest source identifier for this adapter.
const SourceType = "otlp"

// compile-time proof the adapter satisfies the stable ingest interfaces (single + batch).
var (
	_ adaptingest.Ingester      = (*Module)(nil)
	_ adaptingest.BatchIngester = (*Module)(nil)
)

// maxSlugLen bounds the derived external_ref / alert_rule slugs well under the validator's own caps.
const maxSlugLen = 64

// Module is the OTLP log ingest adapter. sourceID is the authenticated ingest source name; the empty value
// falls back to the source type.
type Module struct{ sourceID string }

// New builds the adapter for a named ingest source (e.g. "otlp-dc1").
func New(sourceID string) *Module {
	if strings.TrimSpace(sourceID) == "" {
		sourceID = SourceType
	}
	return &Module{sourceID: sourceID}
}

// SourceType implements adapters/ingest.Ingester.
func (m *Module) SourceType() string { return SourceType }

// Normalize handles the single-record transport case (via NormalizeBatch). A request that does not yield
// exactly one alert-worthy envelope is rejected — grouped exports go through NormalizeBatch.
func (m *Module) Normalize(ctx context.Context, raw []byte) (coreingest.IncidentEnvelope, error) {
	envs, err := m.NormalizeBatch(ctx, raw)
	if err != nil {
		return coreingest.IncidentEnvelope{}, err
	}
	if len(envs) != 1 {
		return coreingest.IncidentEnvelope{}, fmt.Errorf("otlp: Normalize expects exactly one alert-worthy record, got %d (below-threshold records are dropped; use NormalizeBatch)", len(envs))
	}
	return envs[0], nil
}

// NormalizeBatch protobuf-decodes an OTLP ExportLogsServiceRequest and maps every alert-worthy log record to a
// validated IncidentEnvelope. now is read once so a batch stamps consistently.
func (m *Module) NormalizeBatch(_ context.Context, raw []byte) ([]coreingest.IncidentEnvelope, error) {
	var req collectorlogs.ExportLogsServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("otlp: malformed ExportLogsServiceRequest: %w", err)
	}
	now := time.Now().UTC()
	var out []coreingest.IncidentEnvelope
	for _, rl := range req.GetResourceLogs() {
		var resAttrs []*commonpb.KeyValue
		if r := rl.GetResource(); r != nil {
			resAttrs = r.GetAttributes()
		}
		host := sanitizeHost(firstNonEmpty(attr(resAttrs, "host.name"), attr(resAttrs, "host.id"), attr(resAttrs, "k8s.node.name")))
		for _, sl := range rl.GetScopeLogs() {
			for _, rec := range sl.GetLogRecords() {
				sev := alertSeverity(rec.GetSeverityNumber())
				if sev == "" {
					continue // below the alert threshold — OTLP telemetry that is not an incident
				}
				env, err := coreingest.Normalize(m.toRawEvent(host, rec, sev), now)
				if err != nil {
					continue // a single record failing validation is dropped; its siblings are kept (INV-04)
				}
				out = append(out, env)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("otlp: request carried no alert-worthy log records")
	}
	return out, nil
}

// toRawEvent maps one OTLP log record to an unvalidated RawEvent; core.Normalize does the validation.
func (m *Module) toRawEvent(host string, rec *logspb.LogRecord, sev string) coreingest.RawEvent {
	recAttrs := rec.GetAttributes()
	rule := slugify(firstNonEmpty(attr(recAttrs, "event.name"), attr(recAttrs, "alert.name"), rec.GetSeverityText(), "otlp-log"))
	body := anyValueString(rec.GetBody())
	// Correlate by (host, rule) — the CONDITION identity, not the body. Keying on the body would defeat dedup:
	// a log line embedding a timestamp/counter/request-id fires a fresh incident per occurrence (the flooding
	// risk the slice-1 review flagged). Distinct log MESSAGES of the same rule on a host are one incident here;
	// their bodies live in the summary, and the envelope's content-addressed DedupKey separates true repeats.
	// Finer per-condition sub-keying belongs with slice 2's receiver + burst suppression.
	ref := slugify(host + "-" + rule)
	labels := map[string]string{}
	for _, kv := range recAttrs {
		if v := kv.GetValue(); v != nil {
			labels[kv.GetKey()] = v.GetStringValue()
		}
	}
	re := coreingest.NewRawEvent(m.sourceID, marshal(rec))
	re.ExternalRef = ref
	re.AlertRule = rule
	re.Severity = sev
	re.Host = host
	re.Summary = body
	re.Labels = labels
	if t := rec.GetTimeUnixNano(); t > 0 {
		re.ObservedAt = time.Unix(0, int64(t)).UTC()
	}
	return re
}

// alertSeverity maps an OTLP SeverityNumber to a TG severity STRING (core.Normalize parses it), or "" if the
// record is below the alert threshold. TG ingests ALERTS: ERROR (17) and above are critical, WARN (13-16) is a
// warning, everything quieter (TRACE/DEBUG/INFO) is not an incident.
func alertSeverity(n logspb.SeverityNumber) string {
	switch {
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_ERROR:
		return "critical"
	case n >= logspb.SeverityNumber_SEVERITY_NUMBER_WARN:
		return "warning"
	default:
		return ""
	}
}

func attr(kvs []*commonpb.KeyValue, key string) string {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			if v := kv.GetValue(); v != nil {
				return v.GetStringValue()
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// slugify reduces arbitrary provider text to the lowercase [a-z0-9-] slug the envelope validator accepts,
// bounded, never empty (a blank input yields "otlp" so the required external_ref/alert_rule fields are set).
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > maxSlugLen {
		out = strings.Trim(out[:maxSlugLen], "-")
	}
	if out == "" {
		return SourceType
	}
	return out
}

// sanitizeHost coerces a provider host attribute into an RFC-1123 hostname (lowercase; underscores and other
// stray characters to hyphens) so a Docker container name ("my_svc") or a k8s node name the core validator
// would reject does not SILENTLY drop an otherwise-valid alert. It sanitizes PER LABEL (splitting on dots),
// trimming each label of hyphens and dropping empty labels, so dot-adjacent junk ("web01..example.com",
// "a. b") reduces to a valid dotted hostname rather than a string the validator still rejects. A value that
// cannot reduce to any valid label collapses to "" (not host-scoped) rather than dropping the incident.
func sanitizeHost(s string) string {
	var labels []string
	for _, lab := range strings.Split(strings.ToLower(strings.TrimSpace(s)), ".") {
		var b strings.Builder
		for _, r := range lab {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('-')
			}
		}
		l := strings.Trim(b.String(), "-")
		for strings.Contains(l, "--") {
			l = strings.ReplaceAll(l, "--", "-")
		}
		if l != "" && len(l) <= 63 { // drop empty labels (from "..") and RFC-1123-over-long ones
			labels = append(labels, l)
		}
	}
	out := strings.Join(labels, ".")
	if out == "" || len(out) > 253 {
		return ""
	}
	return out
}

// anyValueString renders an OTLP AnyValue log body to a string summary. Most bodies are strings; a structured
// body (int/double/bool/bytes/kvlist/array) would otherwise yield an empty, unhelpful incident, so it is
// rendered rather than dropped to "".
func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_BytesValue:
		return fmt.Sprintf("%d bytes", len(x.BytesValue))
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", x)) // kvlist/array — a representation, never silently empty
	}
}

func marshal(rec *logspb.LogRecord) []byte {
	b, err := proto.Marshal(rec)
	if err != nil {
		return nil
	}
	return b
}
