package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGatewayBoundsConcurrentCompletions is the TG-384 guard.
//
// A cascade turned 157 alerts into 157 simultaneous investigations that tripped the model breaker in six
// seconds against an 8-slot sidecar. SetMaxConcurrency bounds in-flight completions so the fan-out cannot
// arise: excess callers WAIT for a slot (defer-release, never dropped), never overrun the backend.
//
//	Killing mutation: delete the `g.Concurrency.Acquire`/`defer ...Release` block in CompleteWithUsage —
//	max-simultaneous jumps to N and this test goes RED on the observed-max assertion (NOT on total calls,
//	which the mutation leaves unchanged, exactly as the ticket requires).
//	Vacuity guard: asserts total completions == N >= cap+1, so "the gateway was never called" cannot pass as
//	"the cap held".
func TestGatewayBoundsConcurrentCompletions(t *testing.T) {
	const cap = 3
	const N = cap + 8

	var inflight, maxInflight, total int64
	arrived := make(chan struct{}, N)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&total, 1)
		cur := atomic.AddInt64(&inflight, 1)
		for { // record the high-water mark of simultaneous in-flight calls
			m := atomic.LoadInt64(&maxInflight)
			if cur <= m || atomic.CompareAndSwapInt64(&maxInflight, m, cur) {
				break
			}
		}
		arrived <- struct{}{}
		<-release // hold the slot open until the test lets go, forcing overlap
		atomic.AddInt64(&inflight, -1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	// A NIL observer, deliberately: capObs is a bare struct with no synchronisation (it serves the sequential
	// tests), and this test makes `cap` concurrent calls through g.observe — a shared observer would be a data
	// race in the HARNESS (caught by go test -race), not the subject. With nil, observe() is a no-op.
	g := testGateway(t, srv.URL, nil)
	g.SetMaxConcurrency(cap)

	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = g.Complete(context.Background(), "u", "primary", []Message{{Role: "user", Content: "x"}})
		}(i)
	}

	// With the bound, exactly `cap` calls reach the server; the rest block on Acquire. Reading `cap` arrivals is
	// guaranteed. A (cap+1)th arrival within the window is only possible if the bound did NOT hold — but we do
	// not fatal on it here (that would deadlock the blocked goroutines); the maxInflight assertion below is the
	// real check, and it fires on exactly that case.
	for i := 0; i < cap; i++ {
		<-arrived
	}
	select {
	case <-arrived:
		// a (cap+1)th entered concurrently — the mutation's signature; maxInflight will catch it below.
	case <-time.After(150 * time.Millisecond):
		// good: no further call entered while the first `cap` held their slots.
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&maxInflight); got > cap {
		t.Fatalf("max simultaneous completions was %d, cap is %d — SetMaxConcurrency did not bound in-flight "+
			"calls, so a cascade can still self-DoS the brain (TG-384)", got, cap)
	}
	if atomic.LoadInt64(&maxInflight) < 1 {
		t.Fatal("vacuity: no completion was ever observed in flight — the harness proved nothing")
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("completion %d returned %v — a bounded gateway must PARK excess and complete every call, never drop", i, err)
		}
	}
	if got := atomic.LoadInt64(&total); got != N {
		t.Fatalf("total completions = %d, want %d — every parked call must eventually run (defer, never drop)", got, N)
	}
}
