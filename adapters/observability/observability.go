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
