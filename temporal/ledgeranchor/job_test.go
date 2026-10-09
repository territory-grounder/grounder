package ledgeranchor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
)

type fakeHead struct {
	hs   audit.HeadState
	err  error
	mu   sync.Mutex
	n    int
	seen chan struct{}
}

func (f *fakeHead) Head(ctx context.Context, window int) (audit.HeadState, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	if f.seen != nil {
		select {
		case f.seen <- struct{}{}:
		default:
		}
	}
	return f.hs, f.err
}

type fakeSink struct {
	mu       sync.Mutex
	recorded []audit.Anchor
	err      error
}

func (s *fakeSink) Record(ctx context.Context, a audit.Anchor) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.recorded = append(s.recorded, a)
	s.mu.Unlock()
	return nil
}

func (s *fakeSink) all() []audit.Anchor {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]audit.Anchor, len(s.recorded))
	copy(out, s.recorded)
	return out
}

func sampleHead() audit.HeadState {
	return audit.HeadState{Seq: 3, Hash: "h3", Recent: []audit.RowRef{{Seq: 1, Hash: "h1"}, {Seq: 2, Hash: "h2"}, {Seq: 3, Hash: "h3"}}}
}

func TestRun_RecordsTheComputedAnchor(t *testing.T) {
	hs := sampleHead()
	want := audit.ComputeAnchor(hs)
	when := time.Unix(1_700_000_000, 0).UTC()
	sink := &fakeSink{}
	j := Job{Head: &fakeHead{hs: hs}, Sink: sink, Now: func() time.Time { return when }}

	a, recorded, err := j.Run(context.Background())
	if err != nil || !recorded {
		t.Fatalf("Run(intact ledger) = (recorded %v, err %v), want (true, nil)", recorded, err)
	}
	if a.Digest != want.Digest || a.Seq != want.Seq {
		t.Fatalf("recorded anchor %v does not match ComputeAnchor %v", a, want)
	}
	if !a.At.Equal(when) {
		t.Errorf("anchor.At = %v, want the injected clock %v", a.At, when)
	}
	got := sink.all()
	if len(got) != 1 || got[0].Digest != want.Digest {
		t.Fatalf("sink recorded %d anchor(s) %v, want exactly the computed one", len(got), got)
	}
}

func TestRun_EmptyLedgerRecordsNothing(t *testing.T) {
	sink := &fakeSink{}
	j := Job{Head: &fakeHead{hs: audit.HeadState{}}, Sink: sink} // Seq 0

	a, recorded, err := j.Run(context.Background())
	if err != nil || recorded {
		t.Fatalf("Run(empty ledger) = (recorded %v, err %v), want (false, nil) — anchoring nothing plants a phantom fixed point", recorded, err)
	}
	if len(sink.all()) != 0 {
		t.Fatalf("an empty ledger was still witnessed: %v", sink.all())
	}
	_ = a
}

func TestRun_ReadErrorRecordsNothing(t *testing.T) {
	sink := &fakeSink{}
	boom := errors.New("head read failed")
	j := Job{Head: &fakeHead{err: boom}, Sink: sink}
	if _, recorded, err := j.Run(context.Background()); recorded || !errors.Is(err, boom) {
		t.Fatalf("a HEAD read error must propagate and record nothing: recorded=%v err=%v", recorded, err)
	}
	if len(sink.all()) != 0 {
		t.Fatal("a read failure still wrote to the sink")
	}
}

func TestRun_SinkErrorPropagates(t *testing.T) {
	boom := errors.New("record failed")
	j := Job{Head: &fakeHead{hs: sampleHead()}, Sink: &fakeSink{err: boom}}
	if _, recorded, err := j.Run(context.Background()); recorded || !errors.Is(err, boom) {
		t.Fatalf("a sink error must propagate and report not-recorded: recorded=%v err=%v", recorded, err)
	}
}

// TestRunPeriodicallyRecordsBeforeTheFirstTick is the deploy-time blind-window oracle (same shape as the
// calibrator's): the interval is far longer than the wait, so a recorded anchor within the wait can only be
// the IMMEDIATE first pass. If RunPeriodically ever reverts to a bare ticker loop, nothing arrives and this
// fails on the timeout.
func TestRunPeriodicallyRecordsBeforeTheFirstTick(t *testing.T) {
	t.Parallel()
	const interval = 30 * time.Second // >> the wait below
	head := &fakeHead{hs: sampleHead(), seen: make(chan struct{}, 1)}
	sink := &fakeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPeriodically(ctx, Job{Head: head, Sink: sink}, interval, nil)

	select {
	case <-head.seen:
	case <-time.After(5 * time.Second):
		t.Fatalf("no anchor pass within 5s on a %s interval — the first pass is deferred to the first tick, so "+
			"the spine is unwitnessed for a full interval after every worker start", interval)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(sink.all()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a pass ran but recorded no anchor — the witness never lands")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunPeriodicallySurvivesAFailedPass proves a failing pass neither stops the loop nor propagates.
func TestRunPeriodicallySurvivesAFailedPass(t *testing.T) {
	t.Parallel()
	j := Job{Head: &fakeHead{hs: sampleHead()}, Sink: &fakeSink{err: errors.New("record failed")}}
	var mu sync.Mutex
	var errs int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunPeriodically(ctx, j, 40*time.Millisecond, func(error) {
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
	t.Fatal("the loop did not keep ticking through failures")
}

func TestRunPeriodicallyStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	head := &fakeHead{hs: sampleHead(), seen: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunPeriodically(ctx, Job{Head: head, Sink: &fakeSink{}}, 20*time.Millisecond, nil); close(done) }()
	<-head.seen
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunPeriodically did not return after cancel — the goroutine leaks")
	}
}
