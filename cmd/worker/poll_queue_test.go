package main

// Guards for the depth gauge on the human gate (TG-173).
//
// The defect these exist to prevent is not a crash — it is the gauge quietly reading zero while the
// operator is being drowned, which is indistinguishable from a calm estate and is precisely the state
// OWASP Agentic T10 describes. Every test below aims at a way this could go silent while looking installed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
	"github.com/territory-grounder/grounder/core/persist"
)

type fakePollQueue struct {
	rows []persist.PendingDecision
	err  error
}

func (f *fakePollQueue) OpenDecisions(context.Context) ([]persist.PendingDecision, error) {
	return f.rows, f.err
}

func pollSample(ss []metrics.Sample, name string) (metrics.Sample, bool) {
	for _, s := range ss {
		if s.Name == name {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

// THE VACUITY FLOOR, and why it is a published zero rather than an absent series. With no open polls every
// per-queue series would be absent, and a rule written over an absent series goes quiet — silence that
// reads exactly like health. This queue is the control every residual safety property of Full-auto rests
// on; "the watcher sees nothing" and "nobody is waiting" must not publish identically.
func TestDepthIsEmittedAtZeroRatherThanOmitted(t *testing.T) {
	ss := pollQueueSamples(nil, time.Now().UTC())
	depth, ok := pollSample(ss, "tg_poll_queue_open")
	if !ok {
		t.Fatal("tg_poll_queue_open was not emitted for an empty queue. Absent and empty then render " +
			"identically, and a dead job looks exactly like a caught-up operator.")
	}
	if depth.Value != 0 {
		t.Errorf("depth = %v with no rows, want 0", depth.Value)
	}
	if _, ok := pollSample(ss, "tg_poll_queue_oldest_seconds"); ok {
		t.Error("an oldest-wait age was published with nothing waiting. There is no oldest poll to " +
			"measure from, and a fabricated 0 is indistinguishable from a poll opened this instant — the " +
			"one reading that must never trip a staleness rule.")
	}
}

// The flood signal is the PAIR. Depth alone cannot separate a busy estate from one fault fanning out, and
// only the second is a case where reviewing individually wastes the only reviewer.
func TestTheFloodSignalDistinguishesRepetitionFromVolume(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rows := make([]persist.PendingDecision, 0, 40)
	for i := 0; i < 40; i++ {
		rows = append(rows, persist.PendingDecision{
			ExternalRef: "r" + time.Duration(i).String(), Site: "dc1",
			Approaches: []string{"restart the kubelet"}, Reversible: true, OpenedAt: now.Add(-time.Minute),
		})
	}
	ss := pollQueueSamples(rows, now)

	depth, _ := pollSample(ss, "tg_poll_queue_open")
	shapes, ok := pollSample(ss, "tg_poll_queue_distinct_shapes")
	if !ok {
		t.Fatal("tg_poll_queue_distinct_shapes was not emitted — with only a depth gauge, a manufactured " +
			"flood of one repeated proposal is indistinguishable from a genuine multi-fault incident")
	}
	if depth.Value != 40 || shapes.Value != 1 {
		t.Errorf("depth=%v shapes=%v, want 40 and 1", depth.Value, shapes.Value)
	}
	largest, ok := pollSample(ss, "tg_poll_queue_largest_shape")
	if !ok || largest.Value != 40 {
		t.Errorf("largest shape = %v (present=%v), want 40 — this is the number saying how many waiting "+
			"polls ONE review would settle", largest.Value, ok)
	}
}

// Irreversible polls are counted separately: under flood they are the ones that must not be rubber-stamped.
func TestIrreversiblePollsAreCountedSeparately(t *testing.T) {
	now := time.Now().UTC()
	ss := pollQueueSamples([]persist.PendingDecision{
		{ExternalRef: "a", Approaches: []string{"restart"}, Reversible: true, OpenedAt: now},
		{ExternalRef: "b", Approaches: []string{"delete the volume"}, Reversible: false, OpenedAt: now},
	}, now)
	irr, ok := pollSample(ss, "tg_poll_queue_irreversible")
	if !ok {
		t.Fatal("no irreversible count published — a queue of 90 restarts and one deletion reads the same " +
			"as 91 restarts")
	}
	if irr.Value != 1 {
		t.Errorf("irreversible = %v, want 1", irr.Value)
	}
}

// A read error must NOT clear the last good reading. A zeroed depth looks exactly like the operator having
// caught up, which is the most misleading thing this particular gauge could say.
func TestAReadErrorKeepsTheLastDepthRatherThanZeroingIt(t *testing.T) {
	now := time.Now().UTC()
	f := &fakePollQueue{rows: []persist.PendingDecision{
		{ExternalRef: "a", Approaches: []string{"restart"}, OpenedAt: now.Add(-time.Hour)},
	}}
	// The refresh is DRIVEN, not waited for. The first version of this test set the error and re-read the
	// held pointer, which never refreshes — so it asserted nothing and the "clear on error" mutation
	// survived it.
	read, refresh := newPollQueueJob(context.Background(), f)
	first := read()
	if len(first) == 0 {
		t.Fatal("the job published nothing on its immediate first refresh — the gauges would not exist " +
			"until the first tick, leaving a window with no depth signal at all")
	}
	before, ok := pollSample(first, "tg_poll_queue_open")
	if !ok || before.Value != 1 {
		t.Fatalf("the first reading is %v (present=%v), want 1 — with nothing held, this test cannot "+
			"tell 'kept the previous reading' from 'never had one'", before.Value, ok)
	}

	f.err = errors.New("connection refused")
	refresh() // the error path, actually executed

	after, ok := pollSample(read(), "tg_poll_queue_open")
	if !ok {
		t.Fatal("the depth series VANISHED after a read error. An absent series silences every " +
			"approval-queue rule, and silence reads as a healthy queue.")
	}
	if after.Value != before.Value {
		t.Errorf("depth moved from %v to %v after a read error. A transient database blip would read as "+
			"the queue draining, and any flood rule would silently disarm.", before.Value, after.Value)
	}
}

// THE COMPOSITION ROOT (the failure this repo keeps paying for). Every guard above tests the job in
// isolation; none of them notices if the admin surface never CALLS it. Dropping the emission from
// samples() leaves the job running, the gauges computed, and /metrics silent — a control that is present,
// tested, CI-green, and does not reach the one path that matters.
func TestTheAdminSurfaceActuallyEmitsThePollQueueGauges(t *testing.T) {
	adm := &workerAdmin{}
	if got := len(adm.samples()); got == 0 {
		t.Fatal("the bare admin surface emitted nothing at all, so the comparison below is meaningless")
	}
	baseline := len(adm.samples())

	adm = adm.withPollQueue(func() []metrics.Sample {
		return pollQueueSamples([]persist.PendingDecision{
			{ExternalRef: "a", Approaches: []string{"restart"}, OpenedAt: time.Now().UTC()},
		}, time.Now().UTC())
	})
	var names []string
	for _, s := range adm.samples() {
		names = append(names, s.Name)
	}
	if len(names) == baseline {
		t.Fatal("wiring the poll-queue reader changed NOTHING on /metrics — samples() does not call it")
	}
	for _, want := range []string{"tg_poll_queue_open", "tg_poll_queue_distinct_shapes",
		"tg_poll_queue_largest_shape", "tg_poll_queue_irreversible", "tg_poll_queue_oldest_seconds"} {
		var found bool
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is computed by the job and never reaches /metrics. The human gate stays "+
				"unmeasured while every unit test above passes.", want)
		}
	}
}

// A nil store must degrade to silence, not a panic — and must say so at boot, because an unmeasured human
// gate is the condition this whole ticket is about.
func TestNilPollQueueStoreDegradesToSilenceNotPanic(t *testing.T) {
	if got := startPollQueueJob(context.Background(), nil, time.Hour)(); got != nil {
		t.Errorf("a nil store published %d samples, want none", len(got))
	}
}

// The typed-nil trap: db.NewPendingStore(nil) inside an interface is NON-nil, so a naive `store == nil`
// check would not fire and the first refresh would panic on a worker with no database.
func TestTypedNilPoolDoesNotBecomeANonNilPollQueueReader(t *testing.T) {
	if r := pollQueueStoreOrNil(nil); r != nil {
		t.Error("a nil pool produced a non-nil reader — the nil guard in startPollQueueJob cannot fire, " +
			"and a worker without a database panics on the first refresh instead of logging that the " +
			"human gate is unmeasured")
	}
}
