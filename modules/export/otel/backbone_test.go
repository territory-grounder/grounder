package otel

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// capExporter captures the spans the backbone exports, so a test can assert exactly what reached the sink.
type capExporter struct{ spans []sdktrace.ReadOnlySpan }

func (c *capExporter) ExportSpans(_ context.Context, s []sdktrace.ReadOnlySpan) error {
	c.spans = append(c.spans, s...)
	return nil
}
func (c *capExporter) Shutdown(context.Context) error { return nil }

// redactSECRET is a stand-in for the estate Scrub/redaction path: it masks the token "SECRET".
func redactSECRET(s string) (string, int) {
	n := strings.Count(s, "SECRET")
	return strings.ReplaceAll(s, "SECRET", "[redacted]"), n
}

func TestDisabledLaneExportsNothing(t *testing.T) {
	exp := &capExporter{}
	b := NewBackbone(exp, false, redactSECRET)
	if b.Enabled() {
		t.Fatal("a disabled lane must not report Enabled()")
	}
	b.RecordModelCall(context.Background(), "sess-1", LLMSubset{Model: "m", Input: "hi", Output: "ok"})
	if len(exp.spans) != 0 {
		t.Fatalf("a disabled lane exported %d spans, want 0 (REQ-2020: nothing exported while disabled)", len(exp.spans))
	}
}

func TestEnabledLaneExportsRedactedSubsetOnly(t *testing.T) {
	exp := &capExporter{}
	b := NewBackbone(exp, true, redactSECRET)
	if !b.Enabled() {
		t.Fatal("an enabled lane with an exporter and a redactor must report Enabled()")
	}
	b.RecordModelCall(context.Background(), "sess-1", LLMSubset{
		Model:  "azure/gpt-4.1",
		Input:  "host is SECRET-db, diagnose",
		Output: "the SECRET creds rotated",
	})
	if len(exp.spans) != 1 {
		t.Fatalf("want exactly 1 span exported, got %d", len(exp.spans))
	}
	span := exp.spans[0]
	if span.Name() != SpanName {
		t.Errorf("span name = %q, want %q", span.Name(), SpanName)
	}
	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	// The subset is present...
	if attrs[AttrSessionID] != "sess-1" || attrs[AttrModel] != "azure/gpt-4.1" {
		t.Errorf("subset identity wrong: %+v", attrs)
	}
	// ...redacted (model text never reaches the sink verbatim — the Scrub path ran)...
	if strings.Contains(attrs[AttrInput], "SECRET") || strings.Contains(attrs[AttrOutput], "SECRET") {
		t.Errorf("model text reached the span UNREDACTED: input=%q output=%q", attrs[AttrInput], attrs[AttrOutput])
	}
	if !strings.Contains(attrs[AttrInput], "[redacted]") {
		t.Errorf("the redaction path did not run on the input: %q", attrs[AttrInput])
	}
	// ...and the span carries the LLM subset ONLY — no governance/estate attribute.
	for k := range attrs {
		switch k {
		case AttrSessionID, AttrModel, AttrInput, AttrOutput:
		default:
			t.Errorf("span carries a non-allowlisted attribute %q — the lane carries the LLM subset only (REQ-2020)", k)
		}
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestEnabledWithNilRedactorFailsClosed(t *testing.T) {
	exp := &capExporter{}
	b := NewBackbone(exp, true, nil)
	if b.Enabled() {
		t.Fatal("enabled with a nil redactor must fail closed to OFF — the lane must never ship verbatim model text")
	}
	b.RecordModelCall(context.Background(), "s", LLMSubset{Input: "SECRET stays home"})
	if len(exp.spans) != 0 {
		t.Fatalf("a fail-closed lane exported %d spans, want 0", len(exp.spans))
	}
}

func TestNilExporterFailsClosed(t *testing.T) {
	b := NewBackbone(nil, true, redactSECRET)
	if b.Enabled() {
		t.Fatal("a nil exporter must fail closed to OFF")
	}
	b.RecordModelCall(context.Background(), "s", LLMSubset{Input: "x"}) // must not panic on a disabled lane
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown on a disabled lane must be a no-op, got %v", err)
	}
}

func TestZeroValueBackboneIsSafe(t *testing.T) {
	var b Backbone
	if b.Enabled() {
		t.Fatal("the zero-value backbone must be disabled")
	}
	b.RecordModelCall(context.Background(), "s", LLMSubset{Input: "x"}) // must not panic
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("zero-value shutdown: %v", err)
	}
}
