package calibrate

import (
	"context"
	"errors"
	"math"
	"testing"

	core "github.com/territory-grounder/grounder/core/calibrate"
)

// fakeReader is the in-memory SampleReader oracle: it returns a fixed sample set (honouring limit) or an
// error — the same repository-interface + fake discipline the pgx store's DSN test twins.
type fakeReader struct {
	samples []core.Sample
	err     error
}

func (f fakeReader) PairedSamples(_ context.Context, limit int) ([]core.Sample, error) {
	if f.err != nil {
		return nil, f.err
	}
	if limit > 0 && limit < len(f.samples) {
		return f.samples[:limit], nil
	}
	return f.samples, nil
}

// A run over a perfectly-calibrated 0.8 cohort computes ECE 0 and emits the scorecard.
func TestJobRunComputesAndEmits(t *testing.T) {
	var s []core.Sample
	for i := 0; i < 10; i++ {
		s = append(s, core.Sample{Confidence: 0.8, Clean: i < 8}) // 8/10 clean = perfectly calibrated
	}
	var emitted *core.Reliability
	j := Job{Reader: fakeReader{samples: s}, Emit: func(r core.Reliability) { emitted = &r }}
	r, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.N != 10 {
		t.Fatalf("N=%d, want 10", r.N)
	}
	if math.Abs(r.ECE) > 1e-9 {
		t.Fatalf("perfectly-calibrated ECE must be 0, got %v", r.ECE)
	}
	if emitted == nil || emitted.N != 10 {
		t.Fatalf("Emit was not called with the reliability")
	}
}

// An EMPTY sample set yields the zero-value reliability — an honest "no evidence yet", never a fabricated
// curve. This is the state today (the confidence + external_ref plumbing is new; 0 paired rows).
func TestJobRunEmptyIsHonestNoEvidence(t *testing.T) {
	j := Job{Reader: fakeReader{samples: nil}}
	r, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.N != 0 || r.Brier != 0 || r.ECE != 0 {
		t.Fatalf("empty sample set must be zero-value reliability, got %+v", r)
	}
}

// A reader error propagates (best-effort caller logs it) — the job never fabricates a curve on a read
// failure.
func TestJobRunPropagatesReadError(t *testing.T) {
	j := Job{Reader: fakeReader{err: errors.New("db down")}}
	if _, err := j.Run(context.Background()); err == nil {
		t.Fatal("expected the read error to propagate")
	}
}

// Bins/Limit default when unset (no divide-by-zero, no empty-bin panic).
func TestJobRunDefaults(t *testing.T) {
	j := Job{Reader: fakeReader{samples: []core.Sample{{Confidence: 0.5, Clean: true}}}}
	r, err := j.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(r.Bins) != 10 {
		t.Fatalf("default bins must be 10, got %d", len(r.Bins))
	}
}
