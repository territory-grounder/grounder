package langfuse

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/territory-grounder/grounder/core/config"
	exportotel "github.com/territory-grounder/grounder/modules/export/otel"
)

type fakeDoer struct {
	bodies []string
	status int
	resp   string
}

func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.bodies = append(f.bodies, string(b))
	st := f.status
	if st == 0 {
		st = 200
	}
	body := f.resp
	if body == "" {
		body = "{}"
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func newTestExporter(t *testing.T, fake Doer) *Exporter {
	t.Helper()
	t.Setenv("TG_TEST_LFLLM_PUB", "pk-test")
	t.Setenv("TG_TEST_LFLLM_SEC", "sk-test")
	return New("https://lf.example/", config.SecretRef("env:TG_TEST_LFLLM_PUB"), config.SecretRef("env:TG_TEST_LFLLM_SEC"), WithHTTPClient(fake))
}

type capExporter struct{ spans []sdktrace.ReadOnlySpan }

func (c *capExporter) ExportSpans(_ context.Context, s []sdktrace.ReadOnlySpan) error {
	c.spans = append(c.spans, s...)
	return nil
}
func (c *capExporter) Shutdown(context.Context) error { return nil }

// captureSpan builds one real ended span with the given name+attributes and returns it as a ReadOnlySpan.
func captureSpan(t *testing.T, name string, attrs ...attribute.KeyValue) sdktrace.ReadOnlySpan {
	t.Helper()
	cap := &capExporter{}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(cap))
	_, span := tp.Tracer("test").Start(context.Background(), name)
	span.SetAttributes(attrs...)
	span.End()
	_ = tp.Shutdown(context.Background())
	if len(cap.spans) != 1 {
		t.Fatalf("captureSpan produced %d spans, want 1", len(cap.spans))
	}
	return cap.spans[0]
}

// drive routes the given span through a real TracerProvider whose syncer IS the exporter under test.
func drive(t *testing.T, e *Exporter, name string, attrs ...attribute.KeyValue) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(e))
	_, span := tp.Tracer("drive").Start(context.Background(), name)
	span.SetAttributes(attrs...)
	span.End()
	_ = tp.Shutdown(context.Background())
}

func TestExportMapsRedactedSubset(t *testing.T) {
	fake := &fakeDoer{}
	e := newTestExporter(t, fake)
	drive(t, e, exportotel.SpanName,
		attribute.String(exportotel.AttrSessionID, "sess-9"),
		attribute.String(exportotel.AttrModel, "azure/gpt-4.1"),
		attribute.String(exportotel.AttrInput, "diagnose [redacted]"),
		attribute.String(exportotel.AttrOutput, "restart [redacted]"),
	)
	if len(fake.bodies) != 1 {
		t.Fatalf("want 1 POST to Langfuse, got %d", len(fake.bodies))
	}
	body := fake.bodies[0]
	for _, want := range []string{"sess-9", "azure/gpt-4.1", "diagnose [redacted]", "restart [redacted]", "generation-create", "GENERATION"} {
		if !strings.Contains(body, want) {
			t.Errorf("export body missing %q\nbody: %s", want, body)
		}
	}
}

func TestAllowlistDropsGovernanceAndEstate(t *testing.T) {
	fake := &fakeDoer{}
	e := newTestExporter(t, fake)
	drive(t, e, exportotel.SpanName,
		attribute.String(exportotel.AttrSessionID, "sess-9"),
		attribute.String(exportotel.AttrModel, "m"),
		attribute.String(exportotel.AttrInput, "in"),
		attribute.String(exportotel.AttrOutput, "out"),
		// These must never leave the process — governance + estate attributes off the allowlist:
		attribute.String("band", "AUTO-HIGH"),
		attribute.String("verdict", "clean-graduate"),
		attribute.String("host", "prod-db-07.corp"),
		attribute.String("rule", "db-down-rule"),
	)
	if len(fake.bodies) != 1 {
		t.Fatalf("want 1 POST, got %d", len(fake.bodies))
	}
	for _, forbidden := range []string{"AUTO-HIGH", "clean-graduate", "prod-db-07.corp", "db-down-rule", "band", "verdict"} {
		if strings.Contains(fake.bodies[0], forbidden) {
			t.Errorf("export leaked a non-allowlisted attribute %q (the lane carries the LLM subset only)\nbody: %s", forbidden, fake.bodies[0])
		}
	}
}

func TestNonLLMSpanSkipped(t *testing.T) {
	fake := &fakeDoer{}
	e := newTestExporter(t, fake)
	drive(t, e, "session.investigate", attribute.String(exportotel.AttrSessionID, "s"))
	if len(fake.bodies) != 0 {
		t.Fatalf("a non-llm.call span must be skipped, got %d POSTs", len(fake.bodies))
	}
}

func TestUnkeyedSubsetSkipped(t *testing.T) {
	fake := &fakeDoer{}
	e := newTestExporter(t, fake)
	drive(t, e, exportotel.SpanName, attribute.String(exportotel.AttrModel, "m")) // no session.id
	if len(fake.bodies) != 0 {
		t.Fatalf("an unkeyed subset (no session id) must be skipped, got %d POSTs", len(fake.bodies))
	}
}

func TestIngestionErrorsSurface(t *testing.T) {
	fake := &fakeDoer{status: 200, resp: `{"errors":[{"id":"x","status":400,"message":"bad event"}]}`}
	e := newTestExporter(t, fake)
	sp := captureSpan(t, exportotel.SpanName,
		attribute.String(exportotel.AttrSessionID, "s"),
		attribute.String(exportotel.AttrModel, "m"),
		attribute.String(exportotel.AttrInput, "i"),
		attribute.String(exportotel.AttrOutput, "o"),
	)
	if err := e.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{sp}); err == nil {
		t.Fatal("a 2xx response with a non-empty errors array must surface as an error, never a silent drop (INV-15)")
	}
}

func TestEmptyBatchIsNoOp(t *testing.T) {
	fake := &fakeDoer{}
	e := newTestExporter(t, fake)
	if err := e.ExportSpans(context.Background(), nil); err != nil {
		t.Fatalf("empty batch must be a no-op, got %v", err)
	}
	if len(fake.bodies) != 0 {
		t.Fatalf("empty batch posted %d times", len(fake.bodies))
	}
}
