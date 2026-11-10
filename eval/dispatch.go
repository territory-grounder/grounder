package eval

// Bounded, order-stable session/judge dispatch (TG-467).
//
// The on-box harness (eval_integration_test.go) runs the corpus through the REAL Runner and then judges each
// session, both against the SINGLE box LiteLLM gateway. Running those per-session calls with a bounded
// concurrency cap cuts wall-time toward wall = (N/cap) x per-session instead of N x per-session. The dispatch
// loop lived inline in that integration test, which SKIPS unless TG_EVAL_GATEWAY is set — so `make all` never
// exercised it and its correctness invariants had no oracle at all. This file is the extraction: the same
// primitive the harness uses, in a form CI can kill (dispatch_test.go). Same split as eval/gate + tools/evalgate
// — the noisy on-box run stays on the box; the load-bearing logic is pure and unit-tested here.

import (
	"os"
	"strconv"

	"golang.org/x/sync/errgroup"
)

// DefaultConcurrency is the dispatch width when TG_EVAL_CONCURRENCY is unset. It is deliberately SMALL: every
// arm measures through one loopback LiteLLM gateway, and a wide burst is precisely what degrades an arm into
// the short/429 scorecard the integrity gate then refuses to pool (TG-64). 6 is the width the concurrent
// harness has shipped with since 2026-07-25, after a controlled on-box burst showed the gateway sustains it.
const DefaultConcurrency = 6

// Concurrency is the bounded dispatch width, read from TG_EVAL_CONCURRENCY (default DefaultConcurrency). A
// value <= 0 or unparseable falls back to the default; 1 preserves the fully-sequential behaviour that
// predated concurrency (the safe opt-out).
//
// ★ SYMMETRIC ACROSS ARMS, by construction. The change gate runs the candidate arm and the fresh origin/main
// base arm as SEPARATE `go test -run TestEvalCorpusOnBox` invocations that inherit ONE environment
// (eval/eval-gate.sh run_arm), so both arms read the same TG_EVAL_CONCURRENCY. Any gateway-contention effect
// the width induces therefore lands EQUALLY on both arms and cancels in the drift-cancelled A/B delta the gate
// reasons over (TG-64) — the symmetry is why a concurrency change is not an agent-behaviour change and does
// not need the eval to re-baseline.
func Concurrency() int {
	if v := os.Getenv("TG_EVAL_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultConcurrency
}

// Dispatch runs fn over items with AT MOST `limit` invocations in flight and returns the results in INPUT
// order: out[i] is fn(i, items[i]), whatever order the calls finish in. It is the one bounded-concurrency
// primitive the on-box harness uses for both the session phase and the judge phase.
//
// The three invariants are load-bearing, and each has a named killing mutation in dispatch_test.go:
//
//   - BOUNDED (errgroup.SetLimit). At most `limit` fns run concurrently. The cap exists to respect the single
//     gateway: an unbounded burst degrades the arm into the short/429 scorecard the integrity gate refuses to
//     pool (TG-64), so "cap ignored" is not a perf nit — it manufactures an INCONCLUSIVE arm. limit <= 1 runs
//     fully sequentially.
//
//   - STABLE ORDER. Results are written to out[i] by index, NEVER appended on completion, so the assembled
//     slice is byte-identical to a sequential run's for the same responses. Aggregate's sums are commutative,
//     but the per-incident sessions.json record and any concurrent-vs-sequential comparison are not:
//     append-on-completion would scramble the record while leaving the headline scorecard numbers intact — the
//     worst kind of silent corruption.
//
//   - NO SILENT DROP + first-error. Every slot is written exactly once even when its fn errors, and the FIRST
//     error (errgroup semantics) is returned so a hard failure surfaces rather than vanishing. Dispatch does
//     NOT cancel siblings on error (a plain errgroup.Group, no context): the harness's two callers route
//     PER-ITEM failures through the scorecard rather than aborting the whole arm — a session records
//     Session.Err (-> sc.Errors) and a judge failure leaves a nil score (-> Judged<N), and the integrity gate
//     (TG-64) is what refuses the degraded arm. A 200-session arm must not die on one transient blip, so
//     refuse-via-scorecard is kept and the returned error is there for a caller that wants to abort.
func Dispatch[T any, R any](items []T, limit int, fn func(i int, item T) (R, error)) ([]R, error) {
	out := make([]R, len(items))
	if limit < 1 {
		limit = 1 // a non-positive width is the sequential opt-out, never "spawn unbounded"
	}
	var g errgroup.Group
	g.SetLimit(limit)
	for i := range items {
		g.Go(func() error {
			r, err := fn(i, items[i])
			out[i] = r // index-addressed, never append-on-completion — the stable-order invariant
			return err
		})
	}
	err := g.Wait()
	return out, err
}
