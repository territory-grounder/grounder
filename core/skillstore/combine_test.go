package skillstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubNotableSource struct {
	incs []NotableIncident
	err  error
}

func (s stubNotableSource) NotableIncidents(context.Context, time.Duration) ([]NotableIncident, error) {
	return s.incs, s.err
}

// TestCombineNotableSources pins the fan-out the TG-52 caution feed rides: the union of every live source,
// nil sources skipped (no live source ⇒ dormant nil), and — the load-bearing property — a PARTIAL failure
// returns the good subset without erroring (one dead feed must not starve the others, since generateLessons
// aborts the whole run on any error), while a TOTAL failure is still surfaced.
func TestCombineNotableSources(t *testing.T) {
	a := stubNotableSource{incs: []NotableIncident{{ExternalRef: "a1"}}}
	b := stubNotableSource{incs: []NotableIncident{{ExternalRef: "b1"}, {ExternalRef: "b2"}}}

	got, err := CombineNotableSources(a, b).NotableIncidents(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 incidents from the union, got %d", len(got))
	}

	if CombineNotableSources(nil, nil) != nil {
		t.Errorf("no live sources must yield a nil (dormant) combined source")
	}
	if CombineNotableSources() != nil {
		t.Errorf("no sources must yield a nil (dormant) combined source")
	}

	partial, err := CombineNotableSources(a, stubNotableSource{err: errors.New("boom")}).NotableIncidents(context.Background(), time.Hour)
	if err != nil {
		t.Errorf("a partial failure must NOT error (one dead feed must not starve the others), got %v", err)
	}
	if len(partial) != 1 || partial[0].ExternalRef != "a1" {
		t.Errorf("a partial failure must return the good subset, got %v", partial)
	}

	if _, err := CombineNotableSources(stubNotableSource{err: errors.New("x")}, stubNotableSource{err: errors.New("y")}).NotableIncidents(context.Background(), time.Hour); err == nil {
		t.Errorf("a total failure (every live source errored) must surface an error")
	}
}
