package openobserve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
)

// ingestToken is a realistic OpenObserve ingest token: the base64-encoded Basic credential
// (base64("root@example.com:Complexpass#123")) that the OpenObserve UI hands out and that clients present
// verbatim after "Basic ". It is supplied ONLY through a secret reference (INV-13), never as a literal in
// the module.
const ingestToken = "cm9vdEBleGFtcGxlLmNvbTpDb21wbGV4cGFzcyMxMjM="

type recordedReq struct {
	method, path, auth, contentType string
	body                            string
}

// fakeDoer records every request and returns a canned response (status + body), so the oracle drives the
// module's REAL request-building path against a fake OpenObserve endpoint — asserting the exact
// vendor-correct request (verb, path, auth scheme, body, timestamp unit) without a live API.
type fakeDoer struct {
	reqs    []recordedReq
	status  int
	respRet string
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}
	f.reqs = append(f.reqs, recordedReq{
		method:      req.Method,
		path:        req.URL.Path,
		auth:        req.Header.Get("Authorization"),
		contentType: req.Header.Get("Content-Type"),
		body:        body,
	})
	st := f.status
	if st == 0 {
		st = 200
	}
	resp := f.respRet
	if resp == "" {
		resp = "{}"
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_OO_TOKEN", ingestToken)
	f := &fakeDoer{}
	m := New("https://openobserve.example/", config.SecretRef("env:TG_TEST_OO_TOKEN"), WithHTTPClient(f))
	return m, f
}

// wantBasicAuth is the vendor-correct Authorization header for OpenObserve ingest: HTTP Basic with the
// ingest token (already a base64-encoded credential) as the credential. A bare "Bearer <token>" — the bug
// this locks — 401s on every OpenObserve ingest request.
func wantBasicAuth() string { return "Basic " + ingestToken }

// logsEnvelope / logRecord mirror the exact wire shape the module serializes to the OTLP logs route, so the
// test decodes the real bytes and asserts the timestamp UNIT precisely (not by substring).
type logRecord struct {
	Name         string            `json:"name"`
	Value        float64           `json:"value"`
	TimeUnixNano int64             `json:"timeUnixNano"`
	Attributes   map[string]string `json:"attributes"`
}

type logsEnvelope struct {
	SourceType string      `json:"sourceType"`
	Records    []logRecord `json:"records"`
}

type tracePayload struct {
	SourceType string   `json:"sourceType"`
	SessionID  string   `json:"sessionId"`
	Spans      []string `json:"spans"`
}

// TestExportPostsLogsWithBasicAuthAndCarriesEveryStampedSample is the core protocol lock: Export must POST to
// the ingest logs route under HTTP Basic auth (never Bearer), and every sample must reach the endpoint
// (no silent drop) carrying its own freshness stamp faithfully — the freshness timestamp an absent()-guarded
// staleness check depends on to page on a dead writer (INV-15).
func TestExportPostsLogsWithBasicAuthAndCarriesEveryStampedSample(t *testing.T) {
	m, f := newFixture(t)

	// Distinct sub-second stamps so the assertion pins the exact unit: a coarse Unix()/seconds serialization
	// would collapse the fractional part and be caught here.
	stampA := time.Date(2026, 7, 15, 12, 0, 0, 123456789, time.UTC)
	stampB := time.Date(2026, 7, 15, 12, 0, 5, 987654321, time.UTC)
	err := m.Export(context.Background(), []observability.Sample{
		{Name: "session_duration_seconds", Value: 12, Stamped: stampA, Labels: map[string]string{"session": "sess-1"}},
		{Name: "tool_calls_total", Value: 3, Stamped: stampB, Labels: map[string]string{"session": "sess-1"}},
	})
	if err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("Export must issue exactly one POST, got %d", len(f.reqs))
	}
	r := f.reqs[0]

	// Verb + ingest path/stream: the OTLP logs route the module ships metrics/logs to.
	if r.method != http.MethodPost || r.path != "/v1/logs" {
		t.Errorf("export request = %s %s, want POST /v1/logs", r.method, r.path)
	}
	// REGRESSION: OpenObserve ingest auth is HTTP Basic (base64 credential), never a bare Bearer token.
	if r.auth != wantBasicAuth() {
		t.Errorf("Authorization = %q, want HTTP Basic %q", r.auth, wantBasicAuth())
	}
	if strings.HasPrefix(r.auth, "Bearer ") {
		t.Errorf("Authorization must not be a bare Bearer token (that 401s on OpenObserve): %q", r.auth)
	}
	if r.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.contentType)
	}

	var env logsEnvelope
	if err := json.Unmarshal([]byte(r.body), &env); err != nil {
		t.Fatalf("export body must be valid JSON: %v\nbody=%s", err, r.body)
	}
	if env.SourceType != SourceType {
		t.Errorf("envelope sourceType = %q, want %q", env.SourceType, SourceType)
	}
	// Every sample present — no silent drop of a series (INV-15).
	if len(env.Records) != 2 {
		t.Fatalf("all 2 samples must ship (no silent drop), got %d records: %s", len(env.Records), r.body)
	}
	byName := map[string]logRecord{}
	for _, rec := range env.Records {
		byName[rec.Name] = rec
	}
	for name, want := range map[string]struct {
		val   float64
		stamp time.Time
	}{
		"session_duration_seconds": {12, stampA},
		"tool_calls_total":         {3, stampB},
	} {
		rec, ok := byName[name]
		if !ok {
			t.Fatalf("sample %q must be present in the export body: %s", name, r.body)
		}
		if rec.Value != want.val {
			t.Errorf("%s value = %v, want %v", name, rec.Value, want.val)
		}
		// The freshness stamp must round-trip EXACTLY. The module ships it on the OTLP logs route as
		// timeUnixNano (OTLP's nanosecond unit — the unit the spec-locked /v1/logs route expects); asserting
		// == stamped.UnixNano() pins the unit and catches any truncation (e.g. a seconds-granularity bug) that
		// would corrupt the freshness signal.
		if rec.TimeUnixNano != want.stamp.UnixNano() {
			t.Errorf("%s freshness timestamp = %d, want %d (Sample.Stamped must round-trip exactly)", name, rec.TimeUnixNano, want.stamp.UnixNano())
		}
		if rec.Attributes["session"] != "sess-1" {
			t.Errorf("%s must carry its labels, got %v", name, rec.Attributes)
		}
	}
}

// TestExportStampsUnstampedSampleAtSendTime locks INV-15's other half: a sample that arrives without a
// freshness stamp must be stamped at send time, so no record ever leaves undated (which would read as
// perpetually stale/absent).
func TestExportStampsUnstampedSampleAtSendTime(t *testing.T) {
	m, f := newFixture(t)
	before := time.Now().UnixNano()
	if err := m.Export(context.Background(), []observability.Sample{{Name: "unstamped", Value: 1}}); err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}
	after := time.Now().UnixNano()

	var env logsEnvelope
	if err := json.Unmarshal([]byte(f.reqs[0].body), &env); err != nil {
		t.Fatalf("export body must be valid JSON: %v", err)
	}
	if len(env.Records) != 1 {
		t.Fatalf("want 1 record, got %d", len(env.Records))
	}
	ts := env.Records[0].TimeUnixNano
	if ts < before || ts > after {
		t.Errorf("an unstamped sample must be stamped at send time, got %d not in [%d,%d]", ts, before, after)
	}
}

// TestExportNon2xxIsError: a failed ship must surface as an error carrying the endpoint's status and body —
// never a silent success that would let a dead writer read as healthy.
func TestExportNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = http.StatusUnauthorized // the exact failure a bare-Bearer auth bug produces on OpenObserve.
	f.respRet = "unauthorized"
	err := m.Export(context.Background(), []observability.Sample{{Name: "x", Value: 1, Stamped: time.Now()}})
	if err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error must carry the status code, got %v", err)
	}
}

// TestExportSpansPostsTracesWithSessionID locks INV-14: the completed session's ordered spans ship to the
// traces route keyed by the session id, so the trajectory is reconstructable from the outside — under HTTP
// Basic auth.
func TestExportSpansPostsTracesWithSessionID(t *testing.T) {
	m, f := newFixture(t)
	spans := []string{"triage", "decide", "notify"}
	if err := m.ExportSpans(context.Background(), "sess-1", spans); err != nil {
		t.Fatalf("ExportSpans must succeed: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Fatalf("ExportSpans must issue exactly one POST, got %d", len(f.reqs))
	}
	r := f.reqs[0]
	if r.method != http.MethodPost || r.path != "/v1/traces" {
		t.Errorf("trace request = %s %s, want POST /v1/traces", r.method, r.path)
	}
	if r.auth != wantBasicAuth() {
		t.Errorf("trace export must use Basic auth, got %q", r.auth)
	}
	var tp tracePayload
	if err := json.Unmarshal([]byte(r.body), &tp); err != nil {
		t.Fatalf("trace body must be valid JSON: %v\nbody=%s", err, r.body)
	}
	if tp.SourceType != SourceType {
		t.Errorf("trace sourceType = %q, want %q", tp.SourceType, SourceType)
	}
	if tp.SessionID != "sess-1" { // the correlation key that makes the trajectory attributable (INV-14).
		t.Errorf("trace must be keyed by the session id, got %q", tp.SessionID)
	}
	if strings.Join(tp.Spans, ",") != strings.Join(spans, ",") {
		t.Errorf("trace spans = %v, want ordered %v", tp.Spans, spans)
	}
}

// TestExportSpansEmptySessionRejected: an unkeyed trajectory is worse than none — it cannot be attributed to
// a session, so it must be rejected rather than shipped anonymously.
func TestExportSpansEmptySessionRejected(t *testing.T) {
	m, f := newFixture(t)
	if err := m.ExportSpans(context.Background(), "", []string{"triage"}); err == nil {
		t.Fatal("an empty session id must be rejected")
	}
	if len(f.reqs) != 0 {
		t.Errorf("a rejected trace export must not issue a request, got %d", len(f.reqs))
	}
}

// TestTracingDisabledSuppressesTraceExport: with tracing withdrawn there is no trace-export path — ExportSpans
// is a no-op and issues no request, while metric/log export is unaffected.
func TestTracingDisabledSuppressesTraceExport(t *testing.T) {
	t.Setenv("TG_TEST_OO_TOKEN", ingestToken)
	f := &fakeDoer{}
	m := New("https://openobserve.example", config.SecretRef("env:TG_TEST_OO_TOKEN"), WithHTTPClient(f), WithTracing(false))
	if m.Tracing() {
		t.Fatal("WithTracing(false) must report tracing off")
	}
	if err := m.ExportSpans(context.Background(), "sess-1", []string{"triage"}); err != nil {
		t.Fatalf("ExportSpans with tracing off must be a no-op, got %v", err)
	}
	if len(f.reqs) != 0 {
		t.Errorf("tracing off must issue no trace request, got %d", len(f.reqs))
	}
}

// TestUnresolvedSecretRefIsError: the ingest token is a secret reference resolved per request (INV-13); an
// unresolved reference must fail rather than authenticate with an empty credential, and no request may go out.
func TestUnresolvedSecretRefIsError(t *testing.T) {
	os.Unsetenv("TG_TEST_OO_MISSING")
	f := &fakeDoer{}
	m := New("https://openobserve.example", config.SecretRef("env:TG_TEST_OO_MISSING"), WithHTTPClient(f))
	if err := m.Export(context.Background(), []observability.Sample{{Name: "x", Value: 1, Stamped: time.Now()}}); err == nil {
		t.Fatal("an unresolved secret reference must be an error")
	}
	if len(f.reqs) != 0 {
		t.Errorf("no request may be sent when the credential is unresolved, got %d", len(f.reqs))
	}
}

// TestSourceTypeSlugAndTracingDefaultOn pins the registry slug and the default-on tracing contract.
func TestSourceTypeSlugAndTracingDefaultOn(t *testing.T) {
	m := New("https://openobserve.example", config.SecretRef("env:X"))
	if got := m.SourceType(); got != "openobserve" {
		t.Errorf("SourceType() = %q, want openobserve", got)
	}
	if !m.Tracing() {
		t.Error("tracing must be default-on so the session trajectory is reconstructable")
	}
	// compile-time interface satisfaction is enforced by the package's var _ observability.Exporter guard.
	var _ observability.Exporter = m
}

// TestExportAcceptsRealSuccessResponse drives the module's real response path against a REALISTIC OpenObserve
// _json bulk-ingest success reply (checked into testdata) — code 200 with a per-stream status carrying
// successful/failed counts — and asserts the ship is accepted. If OpenObserve's success envelope drifts, this
// exercises the exact bytes the module handles, no live creds needed.
func TestExportAcceptsRealSuccessResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/ingest_success.json")
	if err != nil {
		t.Fatal(err)
	}
	m, f := newFixture(t)
	f.status = 200
	f.respRet = string(body)
	if err := m.Export(context.Background(), []observability.Sample{{Name: "ok", Value: 1, Stamped: time.Now()}}); err != nil {
		t.Fatalf("a realistic 200 ingest response must be accepted: %v", err)
	}
}

// TestExportSurfacesRealErrorResponse drives the response path against a REALISTIC OpenObserve error reply
// (checked into testdata: an auth rejection with code/message) and asserts the module surfaces the vendor
// status and message rather than dropping the failure silently (INV-15).
func TestExportSurfacesRealErrorResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/ingest_error.json")
	if err != nil {
		t.Fatal(err)
	}
	m, f := newFixture(t)
	f.status = http.StatusUnauthorized
	f.respRet = string(body)
	err = m.Export(context.Background(), []observability.Sample{{Name: "x", Value: 1, Stamped: time.Now()}})
	if err == nil {
		t.Fatal("a non-2xx ingest reply must surface as an error")
	}
	if !strings.Contains(err.Error(), "Unauthorized Access") {
		t.Errorf("error must surface the vendor message from the real response, got %v", err)
	}
}
