package sessionspan

import (
	"context"
	"fmt"
	"strings"
)

// Sink ships one completed session's ordered spans to a trace store. It is the seam the investigate
// activity depends on, so temporal/runner never learns what an exporter is — the SAME shape as the
// AgentStep/AgentStepEvidence sinks beside it.
//
// The method signature matches modules/observability/openobserve.Module.ExportSpans exactly, so the
// production module satisfies this interface with no adapter: the method that has existed and gone
// uncalled since spec/008 is the method that is now wired.
type Sink interface {
	ExportSpans(ctx context.Context, sessionID string, spans []string) error
}

// Fanout ships the same spans to every configured sink and returns a JOINED error naming each failure.
//
// It does NOT stop at the first error. A deployment with two trace stores has two of them because it wants
// both, and an exporter whose endpoint is wrong must not make its healthy sibling stop receiving traces —
// that is the per-source-isolation rule the estate sources and the credential engine already follow.
type Fanout []Sink

// ExportSpans ships to each sink; every error is collected and reported together. An empty Fanout is a
// no-op returning nil (a deployment with no trace store configured is a legitimate deployment).
func (f Fanout) ExportSpans(ctx context.Context, sessionID string, spans []string) error {
	var failures []string
	for _, s := range f {
		if s == nil {
			continue
		}
		if err := s.ExportSpans(ctx, sessionID, spans); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("sessionspan: %d of %d trace sink(s) failed: %s", len(failures), len(f), strings.Join(failures, "; "))
}

var _ Sink = (Fanout)(nil)
