package faultinjector

import (
	"context"
	"errors"
	"testing"
	"time"
)

// snapRetryStep scripts one Exec.Run result for snapRetryRunner.
type snapRetryStep struct {
	out  string
	code int
	err  error
}

// snapRetryRunner replays scripted (out, code, err) results in order — call N returns step N, and any
// call past the end repeats the last step. It exists to drive Engine.Snapshot's transport retry; the
// name is deliberately distinct from the other faultinjector test runners (fakeRunner, orderRunner,
// appProbeRunner) so a rename never silently aliases one of them.
type snapRetryRunner struct {
	calls int
	steps []snapRetryStep
}

func (r *snapRetryRunner) Run(_ context.Context, _ string, _ []string) (string, int, error) {
	i := r.calls
	if i >= len(r.steps) {
		i = len(r.steps) - 1
	}
	r.calls++
	s := r.steps[i]
	return s.out, s.code, s.err
}

// TestSnapshotRetriesTransientPveshFailure is the oracle for the campaign-#3 fix: a single-shot
// Snapshot skipped ~40% of ticks on transient pvesh non-zeros. The retry must recover when a later
// attempt succeeds.
func TestSnapshotRetriesTransientPveshFailure(t *testing.T) {
	defer func(b time.Duration) { snapshotRetryBackoff = b }(snapshotRetryBackoff)
	snapshotRetryBackoff = 0 // no real sleeps under test

	const okJSON = `[{"vmid":101,"status":"running"},{"vmid":102,"status":"stopped"}]`
	r := &snapRetryRunner{steps: []snapRetryStep{
		{"", 255, nil}, // transient pvesh non-zero
		{"", 0, errors.New("ssh: connection reset")}, // transient transport error
		{okJSON, 0, nil}, // recovers
	}}
	eng := &Engine{Exec: r, SnapNode: "dc1pve01"}

	st, err := eng.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot should recover after transient failures, got %v", err)
	}
	if r.calls != 3 {
		t.Fatalf("expected 3 attempts (2 transient + 1 success), got %d", r.calls)
	}
	if st["101"] != "running" || st["102"] != "stopped" {
		t.Fatalf("parsed snapshot wrong after retry: %v", st)
	}
}

// TestSnapshotGivesUpFailClosed proves the retry preserves the fail-closed contract: when EVERY attempt
// fails, Snapshot still errors (the tick then skips), and it makes exactly snapshotAttempts attempts —
// it does not loop unbounded.
func TestSnapshotGivesUpFailClosed(t *testing.T) {
	defer func(b time.Duration) { snapshotRetryBackoff = b }(snapshotRetryBackoff)
	defer func(a int) { snapshotAttempts = a }(snapshotAttempts)
	snapshotRetryBackoff = 0
	snapshotAttempts = 3

	r := &snapRetryRunner{steps: []snapRetryStep{{"", 255, nil}}} // always fails
	eng := &Engine{Exec: r, SnapNode: "dc1pve01"}

	if _, err := eng.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot must still FAIL CLOSED when every attempt fails")
	}
	if r.calls != snapshotAttempts {
		t.Fatalf("expected exactly %d attempts, got %d", snapshotAttempts, r.calls)
	}
}

// TestSnapshotParseErrorNotRetried proves a malformed-JSON response is surfaced immediately (one
// transport call), not masked by retries — parse failures are deterministic, not transient.
func TestSnapshotParseErrorNotRetried(t *testing.T) {
	defer func(b time.Duration) { snapshotRetryBackoff = b }(snapshotRetryBackoff)
	snapshotRetryBackoff = 0

	r := &snapRetryRunner{steps: []snapRetryStep{{"not json", 0, nil}}}
	eng := &Engine{Exec: r, SnapNode: "dc1pve01"}

	if _, err := eng.Snapshot(context.Background()); err == nil {
		t.Fatal("a malformed snapshot must error")
	}
	if r.calls != 1 {
		t.Fatalf("a parse failure must not retry the transport: expected 1 call, got %d", r.calls)
	}
}

// cancelOnCallRunner fails and cancels the surrounding context on its first Run, so the ensuing backoff
// select must return via ctx.Done() rather than the timer.
type cancelOnCallRunner struct {
	calls  int
	cancel context.CancelFunc
}

func (r *cancelOnCallRunner) Run(_ context.Context, _ string, _ []string) (string, int, error) {
	r.calls++
	r.cancel()
	return "", 255, nil
}

// TestSnapshotHonorsContextCancellation proves the backoff wait is interruptible: a cancel during the
// inter-attempt delay returns ctx.Err() promptly instead of sleeping out the backoff and retrying, so a
// campaign shutdown/SIGTERM is not blocked by a flapping snapshot.
func TestSnapshotHonorsContextCancellation(t *testing.T) {
	defer func(b time.Duration) { snapshotRetryBackoff = b }(snapshotRetryBackoff)
	snapshotRetryBackoff = time.Hour // only ctx.Done() — not the timer — can end the wait

	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelOnCallRunner{cancel: cancel}
	eng := &Engine{Exec: r, SnapNode: "dc1pve01"}

	if _, err := eng.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from the backoff wait, got %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("a cancel during backoff must stop retrying: expected 1 attempt, got %d", r.calls)
	}
}
