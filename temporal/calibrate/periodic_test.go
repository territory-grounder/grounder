package calibrate

import (
	"context"
	"sync"
	"testing"
	"time"

	core "github.com/territory-grounder/grounder/core/calibrate"
)

// countingReader records how many passes actually read samples.
type countingReader struct {
	mu   sync.Mutex
	n    int
	err  error
	seen chan struct{}
}

func (r *countingReader) PairedSamples(ctx context.Context, limit int) ([]core.Sample, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	select {
	case r.seen <- struct{}{}:
	default:
	}
	if r.err != nil {
		return nil, r.err
	}
	return []core.Sample{{Confidence: 0.9, Clean: true}}, nil
}

func (r *countingReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// TestRunPeriodicallyPublishesBeforeTheFirstTick is the oracle for the deploy-time blind window.
//
// The defect it pins: `for range t.C` runs nothing until one full interval has elapsed, so after every
// worker start the calibration gauges were ABSENT — and absent is not zero, so the `== 0` alert could not
// see it either. The interval here is far longer than the wait, so a pass observed within the wait can ONLY
// have come from an immediate first run. If RunPeriodically reverts to a bare ticker loop, nothing arrives
// and this test fails on the timeout rather than on a count comparison.
func TestRunPeriodicallyPublishesBeforeTheFirstTick(t *testing.T) {
	t.Parallel()
	const interval = 30 * time.Second // >> the wait below: a tick CANNOT be what satisfies this test
	r := &countingReader{seen: make(chan struct{}, 1)}
	var emitted []core.Reliability
	var emu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPeriodically(ctx, Job{Reader: r, Emit: func(rel core.Reliability) {
		emu.Lock()
		emitted = append(emitted, rel)
		emu.Unlock()
	}}, interval, nil)

	select {
	case <-r.seen:
	case <-time.After(5 * time.Second):
		t.Fatalf("no calibration pass within 5s on a %s interval: the first pass is deferred to the first "+
			"tick, so the gauges do not exist for a full interval after every worker start — and a "+
			"`tg_confidence_samples == 0` alert cannot observe a metric that is not published", interval)
	}

	// The pass must also have EMITTED. A read that never reaches the sink leaves the gauge unpublished,
	// which is the same blind window by a different route.
	deadline := time.Now().Add(2 * time.Second)
	for {
		emu.Lock()
		got := len(emitted)
		emu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a pass ran but emitted nothing — the gauge stays unpublished, so the blind window remains")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunPeriodicallySurvivesAFailedPass proves a read error neither stops the loop nor propagates: the
// calibrator is observe-only and must never take the worker down with it.
func TestRunPeriodicallySurvivesAFailedPass(t *testing.T) {
	t.Parallel()
	r := &countingReader{seen: make(chan struct{}, 1), err: context.DeadlineExceeded}
	var errs int
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPeriodically(ctx, Job{Reader: r}, 50*time.Millisecond, func(error) {
		mu.Lock()
		errs++
		mu.Unlock()
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := errs
		mu.Unlock()
		if got >= 2 { // a SECOND failure proves the first did not stop the loop
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected the loop to keep ticking through failures; saw %d error(s) over %d pass(es)",
		errs, r.count())
}

// TestRunPeriodicallyStopsOnContextCancel proves the loop is bounded by its context and does not leak.
func TestRunPeriodicallyStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	r := &countingReader{seen: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunPeriodically(ctx, Job{Reader: r}, 20*time.Millisecond, nil); close(done) }()

	<-r.seen
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunPeriodically did not return after its context was cancelled — the goroutine leaks")
	}
}
