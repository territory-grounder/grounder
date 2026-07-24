package langfuse

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/config"
)

type recordedReq struct {
	method, path, auth string
	body               string
}

// fakeDoer records every request and returns a canned response (status + body), so the oracle drives the
// module's real request-building path against a fake Langfuse.
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
	f.reqs = append(f.reqs, recordedReq{method: req.Method, path: req.URL.Path, auth: req.Header.Get("Authorization"), body: body})
	st := f.status
	if st == 0 {
		st = 200
	}
	resp := f.respRet
	if resp == "" {
		resp = `{"successes":[],"errors":[]}`
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(resp)), Header: make(http.Header)}, nil
}

func newFixture(t *testing.T) (*Module, *fakeDoer) {
	t.Setenv("TG_TEST_LF_PUBLIC", "pk-lf-public")
	t.Setenv("TG_TEST_LF_SECRET", "sk-lf-secret")
	f := &fakeDoer{}
	m := New("https://langfuse.example/", config.SecretRef("env:TG_TEST_LF_PUBLIC"), config.SecretRef("env:TG_TEST_LF_SECRET"), WithHTTPClient(f))
	return m, f
}

func wantBasic(t *testing.T) string {
	t.Helper()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-public:sk-lf-secret"))
}

// Bug (a): ingestion must authenticate with HTTP Basic (public:secret), never a Bearer token.
// Bug (c): each sample must be wrapped in a proper ingestion envelope (id/type/timestamp/body), not sent bare.
func TestExportUsesBasicAuthAndEnvelopesSamples(t *testing.T) {
	m, f := newFixture(t)
	stamped := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	err := m.Export(context.Background(), []observability.Sample{
		{Name: "cpu.load", Value: 0.42, Stamped: stamped, Labels: map[string]string{"host": "dc1tg01"}},
	})
	if err != nil {
		t.Fatalf("Export must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPost || r.path != "/api/public/ingestion" {
		t.Errorf("export request = %s %s, want POST /api/public/ingestion", r.method, r.path)
	}
	if r.auth != wantBasic(t) {
		t.Errorf("Authorization = %q, want %q", r.auth, wantBasic(t))
	}
	if strings.HasPrefix(r.auth, "Bearer ") {
		t.Errorf("ingestion must not use Bearer auth, got %q", r.auth)
	}
	// Envelope: batch wrapper, a valid event type, a unique id, and the freshness-stamped body fields.
	for _, want := range []string{`"batch"`, `"type":"event-create"`, `"cpu.load"`, "2026-07-15T12:00:00Z"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("enveloped body missing %s: %s", want, r.body)
		}
	}
	wantID := eventID("sample", "cpu.load", stamped.Format(time.RFC3339Nano), strconv.Itoa(0))
	if !strings.Contains(r.body, `"id":"`+wantID+`"`) {
		t.Errorf("event must carry its deterministic id %q: %s", wantID, r.body)
	}
}

// Bug (b): a 207 Multi-Status with a non-empty errors array means events were dropped; that must be an
// error naming the dropped id, not a silent success (INV-15).
func TestExport207WithErrorsIsNotSilentlyDropped(t *testing.T) {
	m, f := newFixture(t)
	f.status = http.StatusMultiStatus // 207
	f.respRet = `{"successes":[{"id":"ok-1","status":201}],"errors":[{"id":"bad-9","status":400,"message":"Invalid request data"}]}`
	err := m.Export(context.Background(), []observability.Sample{{Name: "mem.used", Value: 1, Stamped: time.Now().UTC()}})
	if err == nil {
		t.Fatal("a 207 with a non-empty errors array must be an error (no silent drop)")
	}
	if !strings.Contains(err.Error(), "bad-9") {
		t.Errorf("error must name the dropped event id, got %v", err)
	}
}

func TestExportCleanResponseSucceeds(t *testing.T) {
	m, f := newFixture(t)
	f.respRet = `{"successes":[{"id":"ok-1","status":201}],"errors":[]}`
	if err := m.Export(context.Background(), []observability.Sample{{Name: "ok", Value: 1, Stamped: time.Now().UTC()}}); err != nil {
		t.Fatalf("a clean 200 with no errors must succeed: %v", err)
	}
}

func TestExportNon2xxIsError(t *testing.T) {
	m, f := newFixture(t)
	f.status = 401
	f.respRet = "unauthorized"
	if err := m.Export(context.Background(), []observability.Sample{{Name: "x", Value: 1, Stamped: time.Now().UTC()}}); err == nil {
		t.Fatal("a non-2xx response must be an error")
	}
}

func TestExportEmptyBatchIsNoop(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Export(context.Background(), nil); err != nil {
		t.Fatalf("empty batch must be a no-op: %v", err)
	}
	if len(f.reqs) != 0 {
		t.Errorf("empty batch must not issue a request, got %d", len(f.reqs))
	}
}

// Bug (d): a per-session trace must be emitted as trace-create/observation-create events on the batch
// ingestion route, NOT POSTed to /api/public/traces/{id} (which is not a write route).
func TestRecordEmitsIngestionEventsOnBatchRoute(t *testing.T) {
	m, f := newFixture(t)
	if err := m.Record(context.Background(), "sess-7", []string{"llm.call", "agent.step", "tool.invoke"}); err != nil {
		t.Fatalf("Record must succeed: %v", err)
	}
	r := f.reqs[len(f.reqs)-1]
	if r.method != http.MethodPost || r.path != "/api/public/ingestion" {
		t.Errorf("record request = %s %s, want POST /api/public/ingestion", r.method, r.path)
	}
	if strings.HasPrefix(r.path, "/api/public/traces/") {
		t.Errorf("record must not POST to the non-write traces route, got %q", r.path)
	}
	if r.auth != wantBasic(t) {
		t.Errorf("Authorization = %q, want Basic auth", r.auth)
	}
	for _, want := range []string{`"type":"trace-create"`, `"type":"observation-create"`, `"sessionId":"sess-7"`, `"name":"llm.call"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("trace batch body missing %s: %s", want, r.body)
		}
	}
}

func TestRecordEmptySessionRejected(t *testing.T) {
	m, _ := newFixture(t)
	if err := m.Record(context.Background(), "", []string{"x"}); err == nil {
		t.Fatal("an empty session id must be rejected")
	}
}

// Missing secret reference must fail rather than authenticate with an empty credential.
func TestExportUnsetSecretIsError(t *testing.T) {
	os.Unsetenv("TG_TEST_LF_MISSING")
	m := New("https://langfuse.example", config.SecretRef("env:TG_TEST_LF_MISSING"), config.SecretRef("env:TG_TEST_LF_MISSING"), WithHTTPClient(&fakeDoer{}))
	if err := m.Export(context.Background(), []observability.Sample{{Name: "x", Value: 1, Stamped: time.Now().UTC()}}); err == nil {
		t.Fatal("an unresolved secret reference must be an error")
	}
}
