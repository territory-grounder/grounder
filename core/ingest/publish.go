package ingest

import (
	"context"
	"time"
)

// TriageRequested is the internal routing event published after the deterministic pre-model chain has
// run. It is keyed by external_ref and carries the canonical envelope plus the flap/burst annotations.
// The routing layer (a Temporal signal/queue in P1-7/8) starts or attaches a Runner workflow per
// external_ref; this event is the ONLY thing published — the untrusted raw body never leaves ingest.
type TriageRequested struct {
	ExternalRef string
	Envelope    IncidentEnvelope
	Flapping    bool
	InBurst     bool
	PublishedAt time.Time
}

// Publisher publishes triage.requested events to the routing layer. Implementations must be safe to
// call after the pre-model chain has run; they never receive a RawEvent.
type Publisher interface {
	Publish(ctx context.Context, ev TriageRequested) error
}

// PublishTriage publishes one triage.requested event per non-duplicate correlation representative,
// keyed by external_ref, from a BatchResult the Pipeline already produced. It runs AFTER the
// deterministic dedup → flap → burst → correlate chain (BatchResult.Order), which is the mechanical
// realization of "the chain runs in code before any triage.requested is published" (REQ-502). It
// returns the number of events published.
func PublishTriage(ctx context.Context, pub Publisher, r BatchResult, now time.Time) (int, error) {
	publishedGroup := map[string]bool{}
	n := 0
	for _, d := range r.Decisions {
		if d.Duplicate {
			continue // collapsed by dedup — never published
		}
		if publishedGroup[d.CorrelationKey] {
			continue // one event per correlation group
		}
		publishedGroup[d.CorrelationKey] = true
		ev := TriageRequested{
			ExternalRef: d.Envelope.ExternalRef,
			Envelope:    d.Envelope,
			Flapping:    d.Flapping,
			InBurst:     d.InBurst,
			PublishedAt: now,
		}
		if err := pub.Publish(ctx, ev); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// RecordingPublisher is an in-memory Publisher for oracles and local wiring: it records every event
// in publication order. The production publisher (Temporal routing) lands in P1-7/8.
type RecordingPublisher struct {
	Events []TriageRequested
}

// Publish records the event.
func (p *RecordingPublisher) Publish(_ context.Context, ev TriageRequested) error {
	p.Events = append(p.Events, ev)
	return nil
}
