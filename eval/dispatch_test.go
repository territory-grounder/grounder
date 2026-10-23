package eval

// Unit oracle for the bounded, order-stable dispatch (TG-467). The on-box harness that USES Dispatch only runs
// behind TG_EVAL_GATEWAY, so these are the only tests that exercise its three invariants in `make all`. Each
// test names the killing mutation it catches, so a future edit that reintroduces the bug goes red here.

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/eval/gate"
)

// observeMax raises *max to cur if cur is larger (lock-free), so a test can record the peak in-flight count.
func observeMax(cur int64, max *int64) {
	for {
		m := atomic.LoadInt64(max)
		if cur <= m || atomic.CompareAndSwapInt64(max, m, cur) {
			return
		}
	}
}

// TestDispatchRespectsConcurrencyCap: never more than `limit` fns run at once. KILLING MUTATION: drop
// errgroup.SetLimit (or widen it), and the (limit+1)th session enters while the first `limit` are still held.
func TestDispatchRespectsConcurrencyCap(t *testing.T) {
	const limit = 3
	const n = 12
	var inFlight, maxInFlight int64
	entered := make(chan struct{}, n) // buffered n so a fn's "I started" send never blocks
	release := make(chan struct{})    // held closed until the test lets every parked fn finish
	done := make(chan struct{})

	go func() {
		_, _ = Dispatch(make([]int, n), limit, func(i int, _ int) (int, error) {
			cur := atomic.AddInt64(&inFlight, 1)
			observeMax(cur, &maxInFlight)
			entered <- struct{}{}
			<-release // occupy the slot until the whole test releases everyone at once
			atomic.AddInt64(&inFlight, -1)
			return i, nil
		})
		close(done)
	}()

	// Exactly `limit` fns may start before a slot must free. These reads are guaranteed under any impl.
	for k := 0; k < limit; k++ {
		<-entered
	}
	// With the cap enforced, NO further fn can start while all `limit` slots are parked on <-release.
	select {
	case <-entered:
		t.Fatalf("a %dth session started while %d were already in flight — the concurrency cap was not enforced", limit+1, limit)
	case <-time.After(250 * time.Millisecond):
		// good: the dispatcher is holding the line at `limit`
	}
	close(release)
	<-done

	if maxInFlight > limit {
		t.Fatalf("max in-flight was %d, exceeds the cap of %d", maxInFlight, limit)
	}
	if maxInFlight != limit {
		t.Fatalf("expected the cap %d to be fully saturated, but the peak in-flight was only %d", limit, maxInFlight)
	}
}

// TestDispatchSequentialWhenLimitLEOne: limit <= 1 (and non-positive, clamped) is the sequential opt-out —
// strictly one fn at a time. KILLING MUTATION: remove the `limit < 1` clamp (SetLimit(0) would deadlock) or
// ignore the limit.
func TestDispatchSequentialWhenLimitLEOne(t *testing.T) {
	for _, limit := range []int{1, 0, -4} {
		var inFlight, maxInFlight int64
		out, err := Dispatch(make([]int, 5), limit, func(i int, _ int) (int, error) {
			cur := atomic.AddInt64(&inFlight, 1)
			observeMax(cur, &maxInFlight)
			time.Sleep(time.Millisecond) // widen the overlap window so a broken bound would be caught
			atomic.AddInt64(&inFlight, -1)
			return i, nil
		})
		if err != nil {
			t.Fatalf("limit=%d: unexpected error %v", limit, err)
		}
		if len(out) != 5 {
			t.Fatalf("limit=%d: got %d results, want 5", limit, len(out))
		}
		if maxInFlight != 1 {
			t.Fatalf("limit=%d must run sequentially, but saw %d concurrent", limit, maxInFlight)
		}
	}
}

// TestDispatchStableOrderRegardlessOfCompletion: results come back in INPUT order even when the calls finish
// in the exact reverse order. KILLING MUTATION: `out = append(out, r)` instead of `out[i] = r` scrambles the
// slice into completion order.
func TestDispatchStableOrderRegardlessOfCompletion(t *testing.T) {
	const n = 8
	// Force strict reverse completion: item i may return only after item i+1 has (and then frees item i-1).
	// This needs all n in flight at once, so run with limit == n.
	gates := make([]chan struct{}, n)
	for i := range gates {
		gates[i] = make(chan struct{})
	}
	items := make([]int, n)
	for i := range items {
		items[i] = i*10 + 1 // a value distinct from the index, so a mis-indexed write is visible
	}
	close(gates[n-1]) // pre-open the LAST item: it completes first

	out, err := Dispatch(items, n, func(i int, item int) (int, error) {
		<-gates[i] // wait my turn (strict reverse order)
		if i > 0 {
			close(gates[i-1]) // let my predecessor complete next
		}
		return item, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := range items {
		if out[i] != items[i] {
			t.Fatalf("result out of order at %d: got %d, want %d — append-on-completion instead of indexed assignment scrambles this", i, out[i], items[i])
		}
	}
}

// TestDispatchSurfacesFirstErrorWithoutDropping: a fn error is RETURNED (never swallowed) and every other slot
// is still written at its index (never dropped). KILLING MUTATION: swallow the error (return nil from Wait) or
// collect only successful results.
func TestDispatchSurfacesFirstErrorWithoutDropping(t *testing.T) {
	const n = 6
	items := make([]int, n)
	for i := range items {
		items[i] = i
	}
	wantErr := errors.New("simulated persistent 429")
	out, err := Dispatch(items, 3, func(i int, item int) (int, error) {
		if i == 4 {
			return -1, wantErr
		}
		return item * 2, nil
	})
	if err == nil {
		t.Fatalf("expected the fn error to surface; got nil — a swallowed error lets a degraded arm pool silently")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("surfaced error = %v, want %v", err, wantErr)
	}
	for i := range items {
		want := items[i] * 2
		if i == 4 {
			want = -1
		}
		if out[i] != want {
			t.Fatalf("out[%d]=%d want %d — an errored slot must still be written at its index, never dropped", i, out[i], want)
		}
	}
}

// TestDispatchDegradedSessionRefusesArm: the DoD end-to-end tie — a simulated persistently-429 session flows
// through the SAME dispatcher the harness uses, its Session.Err survives at its index, Aggregate counts it, and
// the deterministic TG-64 integrity gate REFUSES the arm. KILLING MUTATION: any dispatch change that drops or
// reorders the errored session zeroes sc.Errors and VerifyIntegrity would wrongly accept the arm.
func TestDispatchDegradedSessionRefusesArm(t *testing.T) {
	const n = 4
	const degraded = 2
	sessions, err := Dispatch(make([]int, n), 3, func(i int, _ int) (Session, error) {
		s := Session{Ref: fmt.Sprintf("inc-%d", i)}
		if i == degraded {
			// runOne's shape: a failure that persisted past the retries is RECORDED on the session (soft), not
			// returned as a hard error — so Dispatch returns nil and the run completes for the gate to judge.
			s.Err = "gateway 429 after 3 retries (contended arm)"
		}
		return s, nil
	})
	if err != nil {
		t.Fatalf("a soft (recorded) session failure must not hard-error the dispatch: %v", err)
	}
	if sessions[degraded].Err == "" {
		t.Fatalf("the degraded session was dropped or reordered — its Session.Err vanished (index-stable collection preserves it)")
	}

	// Judge every HEALTHY session (the errored one is not judged), so Overall>0 and Judged<N — the real shape
	// of a contended arm. full = a clean 4/5 on every canonical dimension.
	full := map[string]int{}
	for _, d := range Dimensions {
		full[d] = 4
	}
	var scores []Score
	for i, s := range sessions {
		if i == degraded {
			continue
		}
		scores = append(scores, Score{Ref: s.Ref, Scores: full})
	}
	card := Aggregate(sessions, scores)
	if card.Errors != 1 {
		t.Fatalf("Aggregate counted %d errored sessions, want 1 (eval.go must fold Session.Err into sc.Errors)", card.Errors)
	}

	// Round-trip into the gate's scorecard shape exactly as eval-gate.sh does (scorecard.json -> gate.Scorecard),
	// then apply the deterministic TG-64 refusal.
	var gc gate.Scorecard
	b, mErr := json.Marshal(card)
	if mErr != nil {
		t.Fatalf("marshal scorecard: %v", mErr)
	}
	if uErr := json.Unmarshal(b, &gc); uErr != nil {
		t.Fatalf("scorecard JSON did not round-trip into gate.Scorecard: %v", uErr)
	}
	problems := gate.VerifyIntegrity("candidate", []gate.Scorecard{gc}, n)
	if len(problems) == 0 {
		t.Fatalf("VerifyIntegrity ACCEPTED a degraded arm (n=%d errors=%d judged=%d) — the TG-64 refusal was weakened", gc.N, gc.Errors, gc.Judged)
	}
}

// TestConcurrencyEnvKnob pins the TG_EVAL_CONCURRENCY contract: default when unset, honoured when a positive
// int, and a safe fall-back to the default on a non-positive or garbage value (never 0 concurrency, which would
// deadlock the dispatcher).
func TestConcurrencyEnvKnob(t *testing.T) {
	cases := []struct {
		set  string
		want int
	}{
		{"", DefaultConcurrency},
		{"1", 1},
		{"4", 4},
		{"0", DefaultConcurrency},
		{"-3", DefaultConcurrency},
		{"garbage", DefaultConcurrency},
	}
	for _, c := range cases {
		t.Setenv("TG_EVAL_CONCURRENCY", c.set)
		if got := Concurrency(); got != c.want {
			t.Fatalf("TG_EVAL_CONCURRENCY=%q -> Concurrency()=%d, want %d", c.set, got, c.want)
		}
	}
}
