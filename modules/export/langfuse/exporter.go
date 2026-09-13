// Package langfuse (modules/export/langfuse) is the SINK for the Tier-3 LLM-observability lane
// (spec/020 REQ-2020 / T-020-14): an OTel SpanExporter that ships the redacted-LLM-subset spans emitted by
// modules/export/otel to a Langfuse-OSS ingestion endpoint.
//
// It is distinct from modules/observability/langfuse (which exports GOVERNANCE session spans as bare names).
// This exporter carries the LLM subset — the model slug and the redacted prompt/completion — as a Langfuse
// generation, and it enforces the "never the governance fields" clause of REQ-2020 with an ALLOWLIST: it
// reads ONLY the four modules/export/otel attribute keys off each span and DROPS everything else, so a
// governance or estate attribute that somehow reached a span (a future mis-instrumentation) never leaves this
// process. Off-by-default is a property of the lane, not this type: modules/export/otel only routes spans
// here when the lane is enabled, and nothing constructs this exporter otherwise.
//
// Langfuse ingestion is HTTP Basic auth (public key : secret key), both secret references resolved per
// request, never literals (INV-13). A 2xx response whose errors array is non-empty is an error naming the
// dropped ids — never a silent drop (INV-15).
package langfuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/territory-grounder/grounder/core/config"
	exportotel "github.com/territory-grounder/grounder/modules/export/otel"
)

// ingestPath is the Langfuse batch write route.
//
// This package deliberately declares NO connector SourceType: it is not a console-configured observability
// connector (that is modules/observability/langfuse, which owns the "langfuse" catalog key and its dialog) —
// it is the internal sink of the Tier-3 lane, constructed by the worker and configured from env, so it has no
// configuration dialog by design. The catalog-descriptor gate only guards SourceType-bearing packages.
const ingestPath = "/api/public/ingestion"

// Doer is the minimal HTTP contract; *http.Client satisfies it, tests inject a fake Langfuse.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Exporter implements go.opentelemetry.io/otel/sdk/trace.SpanExporter for the Tier-3 LLM lane.
type Exporter struct {
	endpoint  string
	publicRef config.SecretRef
	secretRef config.SecretRef
	http      Doer
}

// Option configures an Exporter.
type Option func(*Exporter)

// WithHTTPClient injects the HTTP transport (a fake in tests, *http.Client in production).
func WithHTTPClient(d Doer) Option { return func(e *Exporter) { e.http = d } }

// New builds the exporter for a Langfuse base URL and its Basic-auth key pair (publicRef = public key /
// username, secretRef = secret key / password), both secret references resolved per request (INV-13).
func New(endpoint string, publicRef, secretRef config.SecretRef, opts ...Option) *Exporter {
	e := &Exporter{
		endpoint:  strings.TrimRight(endpoint, "/"),
		publicRef: publicRef,
		secretRef: secretRef,
		http:      http.DefaultClient,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

var _ sdktrace.SpanExporter = (*Exporter)(nil)

// ingestEvent is one Langfuse ingestion envelope.
type ingestEvent struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Body      any       `json:"body"`
}

type ingestResponse struct {
	Errors []struct {
		ID      string `json:"id"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	} `json:"errors"`
}

func eventID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

// llmSubset is the allowlisted projection of ONE span: exactly the four modules/export/otel keys, nothing
// else. Everything not on this allowlist is dropped when reading the span (REQ-2020 "never the governance
// fields").
type llmSubset struct {
	session string
	model   string
	input   string
	output  string
}

// project reads ONLY the allowlisted attributes off a span; every other attribute is ignored, so a governance
// or estate attribute cannot pass through even if it was set.
func project(s sdktrace.ReadOnlySpan) llmSubset {
	var sub llmSubset
	for _, kv := range s.Attributes() {
		switch string(kv.Key) {
		case exportotel.AttrSessionID:
			sub.session = kv.Value.AsString()
		case exportotel.AttrModel:
			sub.model = kv.Value.AsString()
		case exportotel.AttrInput:
			sub.input = kv.Value.AsString()
		case exportotel.AttrOutput:
			sub.output = kv.Value.AsString()
		}
	}
	return sub
}

// ExportSpans maps each llm.call span to a Langfuse trace + generation observation and posts the batch. Only
// modules/export/otel spans carry the allowlisted keys; any other span projects to an empty subset and is
// skipped. An empty batch is a no-op.
func (e *Exporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	events := make([]ingestEvent, 0, len(spans)*2)
	sent := time.Now().UTC()
	for _, s := range spans {
		if s.Name() != exportotel.SpanName {
			continue // not an LLM-subset span.
		}
		sub := project(s)
		if sub.session == "" {
			continue // an unkeyed subset is worse than none.
		}
		traceID := "llm-" + sub.session
		genID := eventID("gen", sub.session, s.SpanContext().SpanID().String())
		events = append(events,
			ingestEvent{
				ID:        eventID("trace", sub.session),
				Type:      "trace-create",
				Timestamp: sent,
				Body: map[string]any{
					"id":        traceID,
					"sessionId": sub.session,
					"name":      "llm-subset",
					"timestamp": sent,
				},
			},
			ingestEvent{
				ID:        genID,
				Type:      "generation-create",
				Timestamp: sent,
				Body: map[string]any{
					"id":        genID,
					"traceId":   traceID,
					"type":      "GENERATION",
					"name":      exportotel.SpanName,
					"model":     sub.model,
					"input":     sub.input,  // redacted upstream (modules/export/otel).
					"output":    sub.output, // redacted upstream.
					"startTime": s.StartTime().UTC(),
				},
			},
		)
	}
	if len(events) == 0 {
		return nil
	}
	return e.ingest(ctx, events)
}

// Shutdown implements SpanExporter. This exporter holds no buffered state (ExportSpans posts inline), so
// shutdown is a no-op.
func (e *Exporter) Shutdown(context.Context) error { return nil }

func (e *Exporter) ingest(ctx context.Context, events []ingestEvent) error {
	public, err := e.publicRef.Resolve()
	if err != nil {
		return fmt.Errorf("langfuse-llm: resolve public key: %w", err)
	}
	secret, err := e.secretRef.Resolve()
	if err != nil {
		return fmt.Errorf("langfuse-llm: resolve secret key: %w", err)
	}
	b, err := json.Marshal(map[string]any{"batch": events})
	if err != nil {
		return fmt.Errorf("langfuse-llm: marshal batch: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+ingestPath, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(public, secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("langfuse-llm: POST %s: status %d: %s", ingestPath, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	if len(bytes.TrimSpace(out)) > 0 {
		var ir ingestResponse
		if err := json.Unmarshal(out, &ir); err != nil {
			return fmt.Errorf("langfuse-llm: decode ingestion response (status %d): %w", resp.StatusCode, err)
		}
		if len(ir.Errors) > 0 {
			parts := make([]string, 0, len(ir.Errors))
			for _, x := range ir.Errors {
				parts = append(parts, x.ID)
			}
			return fmt.Errorf("langfuse-llm: ingestion rejected %d event(s): %s", len(ir.Errors), strings.Join(parts, "; "))
		}
	}
	return nil
}
