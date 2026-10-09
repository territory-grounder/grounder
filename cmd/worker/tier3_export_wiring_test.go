package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/config"
	exportotel "github.com/territory-grounder/grounder/modules/export/otel"
)

type fakeCompleter struct {
	out string
	err error
}

func (f *fakeCompleter) Complete(context.Context, string, string, []model.Message) (string, error) {
	return f.out, f.err
}

// signalExporter forwards each exported span's attributes on a channel so the async recording is observable.
type signalExporter struct{ ch chan map[string]string }

func (s *signalExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	for _, sp := range spans {
		attrs := map[string]string{"__name": sp.Name()}
		for _, kv := range sp.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		s.ch <- attrs
	}
	return nil
}
func (s *signalExporter) Shutdown(context.Context) error { return nil }

func idRedactor(s string) (string, int) { return s, 0 }

func TestTier3DisabledLeavesCompleterUntouched(t *testing.T) {
	inner := &fakeCompleter{out: "x"}
	got, log := wrapTier3Export(inner, tier3ExportConfig{}) // zero config = disabled
	if got != inner {
		t.Error("a disabled lane must return the inner completer untouched (zero overhead)")
	}
	if !strings.Contains(log, "DARK") {
		t.Errorf("the disabled boot log must say DARK, got %q", log)
	}
}

func TestTier3EnabledWrapsAndArms(t *testing.T) {
	inner := &fakeCompleter{out: "x"}
	cfg := tier3ExportConfig{
		enabled: true, endpoint: "https://lf.example",
		publicRef: config.SecretRef("env:TG_TEST_PUB"), secretRef: config.SecretRef("env:TG_TEST_SEC"),
	}
	got, log := wrapTier3Export(inner, cfg)
	if _, ok := got.(*recordingCompleter); !ok {
		t.Errorf("an enabled lane must wrap the completer in a *recordingCompleter, got %T", got)
	}
	if !strings.Contains(log, "ARMED") {
		t.Errorf("the enabled boot log must say ARMED, got %q", log)
	}
}

func TestRecordingCompleterPassesThroughAndRecords(t *testing.T) {
	sig := &signalExporter{ch: make(chan map[string]string, 4)}
	bb := exportotel.NewBackbone(sig, true, idRedactor)
	rc := &recordingCompleter{inner: &fakeCompleter{out: "COMPLETION-TEXT"}, bb: bb}

	out, err := rc.Complete(context.Background(), "sess-7", "azure/gpt-4.1",
		[]model.Message{{Role: "system", Content: "be careful"}, {Role: "user", Content: "diagnose the host"}})
	if out != "COMPLETION-TEXT" || err != nil {
		t.Fatalf("the completion must pass through unchanged, got %q / %v", out, err)
	}
	select {
	case attrs := <-sig.ch:
		if attrs["__name"] != exportotel.SpanName {
			t.Errorf("span name = %q, want %q", attrs["__name"], exportotel.SpanName)
		}
		if attrs[exportotel.AttrSessionID] != "sess-7" || attrs[exportotel.AttrModel] != "azure/gpt-4.1" {
			t.Errorf("subset identity wrong: %+v", attrs)
		}
		if !strings.Contains(attrs[exportotel.AttrInput], "diagnose the host") {
			t.Errorf("the input must carry the joined prompt, got %q", attrs[exportotel.AttrInput])
		}
		if attrs[exportotel.AttrOutput] != "COMPLETION-TEXT" {
			t.Errorf("the output must be the completion, got %q", attrs[exportotel.AttrOutput])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the async export did not arrive within 2s")
	}
}

func TestRecordingCompleterSkipsOnError(t *testing.T) {
	sig := &signalExporter{ch: make(chan map[string]string, 1)}
	bb := exportotel.NewBackbone(sig, true, idRedactor)
	rc := &recordingCompleter{inner: &fakeCompleter{err: errors.New("model down")}, bb: bb}

	if _, err := rc.Complete(context.Background(), "s", "m", nil); err == nil {
		t.Fatal("the inner error must propagate")
	}
	select {
	case attrs := <-sig.ch:
		t.Errorf("a failed model call must NOT be recorded, got %+v", attrs)
	case <-time.After(300 * time.Millisecond):
		// good — nothing exported
	}
}

func TestTier3ConfigEnabledRequiresAllFields(t *testing.T) {
	full := tier3ExportConfig{enabled: true, endpoint: "https://lf", publicRef: "env:p", secretRef: "env:s"}
	if !full.Enabled() {
		t.Error("a fully-configured lane must be Enabled")
	}
	for _, c := range []tier3ExportConfig{
		{enabled: false, endpoint: "https://lf", publicRef: "env:p", secretRef: "env:s"},
		{enabled: true, endpoint: "", publicRef: "env:p", secretRef: "env:s"},
		{enabled: true, endpoint: "https://lf", publicRef: "", secretRef: "env:s"},
		{enabled: true, endpoint: "https://lf", publicRef: "env:p", secretRef: ""},
	} {
		if c.Enabled() {
			t.Errorf("a lane missing a required field must be disabled: %+v", c)
		}
	}
}
