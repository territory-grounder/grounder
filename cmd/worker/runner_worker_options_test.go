package main

import "testing"

// TG-384: the runner worker must bound concurrent activities from TG_MAX_CONCURRENT_INVESTIGATIONS so an
// alert burst cannot become a burst of simultaneous model-consuming investigations (the pve03 cascade tripped
// the model breaker in 6s at 157 concurrent). It ships INERT — an unset env leaves the option off worker.
// Options so Temporal keeps its default (1000) — and an operator arms it deliberately.
//
// Killing mutation: drop the `if n > 0 { opts.MaxConcurrentActivityExecutionSize = n }` assignment — the
// armed case then reads 0 (unbounded) and the "12" assertion goes RED, while the unset case is unchanged.
func TestRunnerWorkerOptionsBoundsConcurrencyFromEnv(t *testing.T) {
	// Unset ⇒ 0 ⇒ the option is left at its zero value so worker.New applies Temporal's default (inert).
	unset := runnerWorkerOptions(func(_ string, def int) int { return def })
	if unset.MaxConcurrentActivityExecutionSize != 0 {
		t.Fatalf("an unset TG_MAX_CONCURRENT_INVESTIGATIONS must leave MaxConcurrentActivityExecutionSize=0 "+
			"(Temporal default, inert), got %d — arming it by default would silently throttle every deploy",
			unset.MaxConcurrentActivityExecutionSize)
	}

	// Armed ⇒ the exact bound is applied.
	armed := runnerWorkerOptions(func(k string, def int) int {
		if k == "TG_MAX_CONCURRENT_INVESTIGATIONS" {
			return 12
		}
		return def
	})
	if armed.MaxConcurrentActivityExecutionSize != 12 {
		t.Fatalf("TG_MAX_CONCURRENT_INVESTIGATIONS=12 must bound the runner worker to 12 concurrent activities, "+
			"got %d — the belt that stops a 157-alert cascade is not wired", armed.MaxConcurrentActivityExecutionSize)
	}

	// A non-positive value is treated as unset (inert), matching envInt's own contract — never a literal 0
	// that would mean "run zero activities".
	zero := runnerWorkerOptions(func(k string, def int) int {
		if k == "TG_MAX_CONCURRENT_INVESTIGATIONS" {
			return 0
		}
		return def
	})
	if zero.MaxConcurrentActivityExecutionSize != 0 {
		t.Fatalf("a 0/absent value must stay inert (Temporal default), got %d", zero.MaxConcurrentActivityExecutionSize)
	}
}
