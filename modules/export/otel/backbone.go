// Package otel is the Tier-3 LLM-observability export backbone (spec/020 REQ-2020 / T-020-14).
//
// It is an OTel span backbone — the one REQ-2020 says does not exist yet (TG runs Prometheus aggregate
// counters only) — over which an OPTIONAL, OFF-BY-DEFAULT lane exports the REDACTED LLM SUBSET of a session:
// the model slug and the redacted prompt/completion text, and NOTHING ELSE. It carries no governance field
// (band, verdict, rule, confidence, gate), no estate identifier, and no host — the LLMSubset TYPE has no slot
// for them, so the lane cannot export governance or estate data even when a caller holds it (the same
// schema-enforced separation as core/trace/layers.go, REQ-2017).
//
// This is observability, not authority: it does NOT touch INV-08. INV-08 governs EFFECT authority (no
// LLM-produced token becomes control flow, a command string, or a query fragment) — this lane only ships a
// redacted copy OUT to a third-party store and nothing it exports ever re-enters TG's trust path. The one
// INV-08-adjacent rule it DOES honor is the sessionspan bounded-field rule (model text must not reach an
// export sink verbatim): every Input/Output goes through the injected Scrub/redaction path (REQ-2008 /
// REQ-2015) BEFORE it becomes a span attribute, and an enabled lane with no redactor fails closed to OFF.
//
// Three invariants hold BY CONSTRUCTION (REQ-2020):
//   - NOT the system of record — the span is a transient copy; TG's authoritative store persists only
//     provenance (core/trace/record.go Prompts = prompt_version/seed_hash), never the raw text, so nothing
//     here is read back as truth.
//   - NOT on the decision path — RecordModelCall returns nothing, is called AFTER the completion is in hand,
//     and swallows every error, so an enabled OR broken lane cannot change what the model returns or whether
//     an action proceeds.
//   - OFF BY DEFAULT — a Backbone built disabled (or with a nil exporter) holds no TracerProvider, and every
//     RecordModelCall is a pure no-op: nothing is exported until an org admin turns the lane on.
package otel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// Attribute keys for the redacted LLM subset. These four — and ONLY these four — are the lane's payload. The
// sink (modules/export/langfuse) allowlists exactly this set and drops anything else, so a mis-instrumented
// span cannot leak a governance field. Kept exported so the sink and the oracle share one definition.
const (
	AttrSessionID = "session.id"
	AttrModel     = "llm.model"
	AttrInput     = "llm.input"
	AttrOutput    = "llm.output"
)

// SpanName is the single span kind the lane emits.
const SpanName = "llm.call"

// LLMSubset is the redacted LLM I/O — the ONLY content the Tier-3 lane carries (REQ-2020). Model is a KIND
// (the model slug, never a secret); Input and Output are the prompt and completion. The type deliberately has
// NO governance field and NO estate field, so the export is generalizable-by-construction: even a caller with
// the full session governance state cannot put it on the wire through this type.
type LLMSubset struct {
	Model  string
	Input  string
	Output string
}

// Redactor removes estate identifiers/secrets from a string, returning the redacted text (the count of
// redactions is ignored here). It matches core/estatedoc's redactor shape so the live wiring can hand its
// estate redactor straight in; a nil Redactor means "no redaction needed" (the oracle's no-estate case).
type Redactor func(string) (string, int)

// Backbone is the OTel span backbone for the Tier-3 lane. The zero value and a disabled Backbone are both
// safe no-ops. Construct with NewBackbone.
type Backbone struct {
	tp     *sdktrace.TracerProvider // nil ⇒ disabled ⇒ RecordModelCall is a no-op
	tracer oteltrace.Tracer
	redact Redactor
}

// NewBackbone builds the backbone. The lane is OFF — the returned Backbone holds no TracerProvider and
// exports nothing (REQ-2020 default-off) — unless ALL THREE of enabled, a non-nil exp, AND a non-nil redact
// are supplied. Requiring the redactor is a fail-closed safety property, not a convenience: the lane's whole
// license to carry model text is that the text is scrubbed first (REQ-2008 / REQ-2015), so an enabled lane
// with no redactor must export NOTHING rather than ship verbatim model text. When enabled, spans are exported
// to exp SYNCHRONOUSLY (WithSyncer) so a call's subset reaches the sink deterministically — the lane is
// low-volume (one span per model call) and off the decision path, so synchronous export costs nothing that
// matters and removes the batch-flush race a test or an operator would otherwise have to reason about.
func NewBackbone(exp sdktrace.SpanExporter, enabled bool, redact Redactor) *Backbone {
	if !enabled || exp == nil || redact == nil {
		return &Backbone{} // fail closed to OFF — no redactor means no export, never verbatim model text.
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return &Backbone{tp: tp, tracer: tp.Tracer("groundnet/tier3-llm-export"), redact: redact}
}

// Enabled reports whether the lane will export (a constructed, enabled backbone with a sink).
func (b *Backbone) Enabled() bool { return b != nil && b.tracer != nil }

// RecordModelCall records ONE model call as a redacted-LLM-subset span (REQ-2020). It is BEST-EFFORT and OFF
// THE DECISION PATH: it returns nothing, and when the lane is disabled it does exactly nothing. Call it AFTER
// the completion is obtained — its whole contract is that it cannot change the completion or gate anything.
// Input and Output are redacted here, so a raw estate identifier in the prompt never reaches a span.
func (b *Backbone) RecordModelCall(ctx context.Context, sessionID string, sub LLMSubset) {
	if !b.Enabled() {
		return // OFF: nothing is exported while the lane is disabled.
	}
	_, span := b.tracer.Start(ctx, SpanName)
	span.SetAttributes(
		attribute.String(AttrSessionID, sessionID),
		attribute.String(AttrModel, sub.Model),
		attribute.String(AttrInput, b.red(sub.Input)),
		attribute.String(AttrOutput, b.red(sub.Output)),
	)
	span.End() // WithSyncer ⇒ End() drives the export inline.
}

// red applies the injected estate redactor (identity when none is set).
func (b *Backbone) red(s string) string {
	if b.redact == nil {
		return s
	}
	out, _ := b.redact(s)
	return out
}

// Shutdown flushes and stops the backbone. Safe (a no-op) on a disabled or zero Backbone.
func (b *Backbone) Shutdown(ctx context.Context) error {
	if b == nil || b.tp == nil {
		return nil
	}
	return b.tp.Shutdown(ctx)
}
