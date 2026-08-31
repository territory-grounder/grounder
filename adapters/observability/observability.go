// Package observability is the stable interface for the observability surface: metrics exposition, trace
// and log export, dashboard provisioning, and external dead-man pings.
//
// Provenance: [O] INV-15 (every exported series is stamped with a freshness timestamp so an absent()-
// guarded staleness check pages on a dead writer rather than reading as healthy), INV-14 (the session
// trajectory is reconstructable), spec/008. Prometheus/OpenObserve/Langfuse/Healthchecks.io day-1.
package observability

import (
	"context"
	"time"
)

// Sample is one exported observation stamped with the time it was produced (freshness), so a dead writer
// pages via an absent()-guarded staleness check rather than reading as healthy (INV-15).
type Sample struct {
	Name    string
	Value   float64
	Stamped time.Time
	Labels  map[string]string
}

// Exporter exports samples/traces to a configured sink. A backend stamps freshness and never silently
// drops — a dead writer must be observable from the outside.
type Exporter interface {
	// SourceType is the source/vendor slug (e.g. "prometheus", "openobserve", "langfuse", "healthchecks").
	SourceType() string
	// Export ships a batch of freshness-stamped samples to the configured sink.
	Export(ctx context.Context, samples []Sample) error
}

// TraceExporter is an Exporter that can ALSO ship a completed session's ordered spans, so its trajectory
// is reconstructable from outside TG (INV-14).
//
// It is a separate, OPTIONAL interface discovered by type assertion rather than a method added to Exporter,
// for two reasons. First, compatibility: healthchecks.io is a dead-man ping with no trace concept, and
// forcing it to carry a stub ExportSpans would put a method on it that can only lie. Second, and the reason
// it is being introduced at all (TG-44): openobserve.Module has HAD this exact method since spec/008 and
// no composition root ever called it — the capability existed and the trace store stayed empty. Naming the
// capability in the stable interface is what lets a composition root ask "which of my configured exporters
// can take a trace?" instead of hard-coding one module.
type TraceExporter interface {
	Exporter
	// ExportSpans ships the ordered spans of ONE completed session, keyed by its session id. An
	// implementation with tracing withdrawn returns nil without shipping; it must never silently drop
	// a span batch it accepted.
	ExportSpans(ctx context.Context, sessionID string, spans []string) error
}
