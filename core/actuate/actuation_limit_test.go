package actuate

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/adapters/actuation"
	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/safety"
)

// actuation_limit_test.go — TG-166(a). THE DEFECT UNDER TEST: there was no rate limit and no concurrency cap
// at ANY scope on the mutating path. Every gate in the chain is per-ACTION, so a subverted agent that emits an
// in-grammar, allowlisted, reversible, evidence-bound, target-relevant proposal to restart a unit gets it
// executed — and then gets it executed again, and again, because nothing in the chain could see a SEQUENCE.
// The only rate control that existed (core/policy's RateGovernor) is keyed by op-class, clamps auto→approve
// rather than refusing, charges at policy-decide time rather than at the effect, and has no production caller
// at all (`WithRateGovernor` appears once in the tree, in a spec acceptance test).
//
// Every case here uses a fake, read-only-in-effect actuator, so a gate that wrongly PASSED mutates nothing —
// the assertion on act.execs is what catches it.

// fixedClock is an injectable clock for the trailing-window arithmetic, so these oracles are deterministic
// (this codebase forbids nondeterministic time in tests).
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFixedClock() *fixedClock {
	return &fixedClock{t: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
}
func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// limitedInterceptor builds an ACTUATING interceptor whose actuation-frequency governor is the supplied
// limiter, so a test can state the budget and the clock explicitly instead of relying on the defaults.
func limitedInterceptor(act actuation.Actuator, l *ActuationLimiter) *Interceptor {
	return actuatingInterceptor(safety.NewActuatingChokepoint(), act, audit.NewLedger()).WithActuationLimiter(l)
}

// loopRequest is a FULLY ADMISSIBLE mutating request — it passes admission, the never-auto floor, structure,
// evidence, territory, verifiability, policy, the mode chokepoint, the baseline and the necessity gate. That
// is the whole point: the repetition this file is about is made of individually perfect actions.
func loopRequest(t *testing.T, externalRef string) Request {
	t.Helper()
	r := goodRequest(t)
	r.ExternalRef = externalRef
	return r
}

// A SUBVERTED AGENT LOOP: one session emitting the same admissible restart over and over. The session budget
// must stop it, the effect leaf must stop being reached, and the refusal must SAY it was throttled.
//
// KILLING MUTATION (executed 2026-08-04): in interceptor.go gate 4h, replace the refusal branch with a
// pass — `lease, _ := i.limiter.Admit(...)`, dropping the `if limitRefusal != ""` block, i.e. the pre-TG-166
// state where nothing counted actuations. All six attempts then execute and this test FAILS at its vacuity
// floor with
//
//	"no attempt was rate-limited at all — this oracle matched nothing and would pass vacuously"
//
// which is the floor doing its job: with the gate gone the execs assertion below would still have caught it
// (6 executions against a budget of 2), but the floor catches it FIRST and says why. Restored → green.
func TestSubvertedAgentLoopIsThrottledPerSession(t *testing.T) {
	clock := newFixedClock()
	limiter := NewActuationLimiter(clock.now).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 2, TargetPerWindow: 99, SessionInFlight: 1, TargetInFlight: 1,
	})
	act := &fakeActuator{}
	i := limitedInterceptor(act, limiter)

	const attempts = 6
	throttled := 0
	for n := 0; n < attempts; n++ {
		out, err := i.Do(context.Background(), loopRequest(t, "TG-loop-1"))
		if err != nil {
			t.Fatalf("attempt %d failed loud instead of refusing: %v", n, err)
		}
		if !out.RateLimited {
			continue
		}
		throttled++
		// VISIBILITY (the ticket's own requirement): a throttled actuation must not read like an unrelated
		// failure. It carries the stable token AND the numbers an operator needs to act on.
		if !strings.Contains(out.Reason, RefusalRateLimited) {
			t.Fatalf("attempt %d was rate-limited but its reason does not say so (%q) — an operator "+
				"reading this cannot tell a throttle from a broken host", n, out.Reason)
		}
		if !strings.Contains(out.Reason, "session") || !strings.Contains(out.Reason, "TG-loop-1") {
			t.Fatalf("attempt %d: the refusal must name the scope and key that ran out of budget, got %q", n, out.Reason)
		}
		if out.Executed {
			t.Fatalf("attempt %d reported BOTH rate-limited and executed — a throttle that still mutated is worse than no throttle", n)
		}
	}

	// VACUITY FLOOR: this oracle counts throttled attempts out of a loop. A run where the chain refused
	// everything for some UNRELATED reason (a broken fixture, a gate above 4h) would leave execs=0 and prove
	// nothing about the limiter, and a run where nothing was throttled would leave this counting zero.
	if throttled == 0 {
		t.Fatal("no attempt was rate-limited at all — this oracle matched nothing and would pass vacuously")
	}
	if act.execs != 2 {
		t.Fatalf("the session actuated %d time(s) against web01; the budget is 2 per 10m0s. A subverted agent "+
			"that can produce ONE admissible restart can now produce an unbounded number of them, which is "+
			"TG-166 unfixed.", act.execs)
	}
}

// The PER-TARGET scope is independent of the per-session one: a caller that simply issues each restart under
// a FRESH external_ref would walk straight past a session-only budget. Restarting one box repeatedly is the
// harm, whoever is asking.
//
// KILLING MUTATION (executed 2026-08-04): in limiter.go's Admit, drop the target row from the scope table
// (leave only the session row) — the "one scope is enough" simplification. This test then FAILS with
//
//	"nothing was refused — the oracle matched nothing and would pass vacuously"
//
// (the vacuity floor fires first; the execs assertion below is the one that names the consequence — five
// restarts of web01 under five session refs against a per-target budget of two). Restored → green.
func TestPerTargetBudgetSurvivesAFreshSessionRefEachTime(t *testing.T) {
	clock := newFixedClock()
	limiter := NewActuationLimiter(clock.now).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 99, TargetPerWindow: 2, SessionInFlight: 1, TargetInFlight: 1,
	})
	act := &fakeActuator{}
	i := limitedInterceptor(act, limiter)

	refusals := 0
	for _, ref := range []string{"TG-a", "TG-b", "TG-c", "TG-d", "TG-e"} {
		out, err := i.Do(context.Background(), loopRequest(t, ref)) // every request targets web01
		if err != nil {
			t.Fatalf("%s failed loud: %v", ref, err)
		}
		if out.RateLimited {
			refusals++
			if !strings.Contains(out.Reason, "target") || !strings.Contains(out.Reason, "web01") {
				t.Fatalf("%s: the refusal must name the TARGET scope that ran out of budget, got %q", ref, out.Reason)
			}
		}
	}
	if refusals == 0 {
		t.Fatal("nothing was refused — the oracle matched nothing and would pass vacuously")
	}
	if act.execs != 2 {
		t.Fatalf("web01 was actuated %d time(s) in the window under 5 different session refs; the per-target "+
			"budget is 2. A per-session-only limiter is defeated by issuing each restart under a fresh "+
			"external_ref.", act.execs)
	}
}

// blockingActuator holds its Exec open until released, so a SECOND actuation can be attempted while the first
// is genuinely in flight. Nothing here sleeps: the handshake is by channel, so the oracle is deterministic.
type blockingActuator struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	execs   int
}

func (a *blockingActuator) Capability() string { return "test.blocking" }
func (a *blockingActuator) ReadOnly() bool     { return false }
func (a *blockingActuator) Exec(_ context.Context, _ []string, _ []byte) (actuation.Result, error) {
	a.mu.Lock()
	a.execs++
	first := a.execs == 1
	a.mu.Unlock()
	a.started <- struct{}{}
	// ONLY the first Exec holds itself open. A later one returning immediately is what keeps this oracle a
	// FAILING test rather than a HANGING one when the in-flight cap is removed: with the cap gone the second
	// actuation reaches the leaf while the first is still parked, and a leaf that blocked every caller would
	// deadlock the two goroutines instead of letting the assertion below report what went wrong.
	if first {
		<-a.release
	}
	return actuation.Result{ExitCode: 0}, nil
}
func (a *blockingActuator) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.execs
}

// CONCURRENCY, the case no per-action gate can see: two mutations in flight against the SAME box at the same
// moment. Each is individually admissible — that is precisely why every gate above 4h passes both — and the
// pair is what damages the host. A rate limit alone does NOT cover this: two simultaneous actuations are one
// count in the window until they finish.
//
// KILLING MUTATION (executed 2026-08-04): in limiter.go's Admit, delete the in-flight check
// (`if c.inFlight >= c.cap { … }`) and the two `l.inFlight[…]++` lines, leaving the trailing-window rate cap
// alone — the "the rate limit already covers it" simplification. This test then FAILS with
//
//	"a concurrent mutation against the same target must be refused as an in-flight violation, got
//	 {Executed:true Refused:false … RateLimited:false}"
//
// — the second mutation ran against web01 while the first had not returned. A trailing-window rate cap does
// not see concurrency: N simultaneous actuations are one count until they finish, which is why the in-flight
// cap is a separate control. Restored → green.
func TestConcurrentActuationsAgainstOneTargetAreRefused(t *testing.T) {
	clock := newFixedClock()
	// A generous RATE budget on purpose: the only thing that may refuse the second actuation here is the
	// in-flight cap, so this cannot pass for the wrong reason.
	limiter := NewActuationLimiter(clock.now).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 99, TargetPerWindow: 99, SessionInFlight: 1, TargetInFlight: 1,
	})
	// `started` is BUFFERED so the later, non-concurrent actuations in this test do not block on a listener
	// that has already moved on; `release` is closed once, after which every later Exec passes straight through.
	act := &blockingActuator{started: make(chan struct{}, 8), release: make(chan struct{})}
	i := limitedInterceptor(act, limiter)

	done := make(chan Outcome, 1)
	go func() {
		out, _ := i.Do(context.Background(), loopRequest(t, "TG-slow"))
		done <- out
	}()
	<-act.started // the first mutation is now genuinely mid-flight inside the effect leaf

	// A DIFFERENT session, the SAME target: only the per-TARGET in-flight cap can refuse this.
	second, err := i.Do(context.Background(), loopRequest(t, "TG-other-session"))
	if err != nil {
		t.Fatalf("the concurrent attempt failed loud instead of refusing: %v", err)
	}
	if !second.RateLimited || !strings.Contains(second.Reason, "in flight") {
		t.Fatalf("a concurrent mutation against the same target must be refused as an in-flight violation, got %+v", second)
	}
	if got := act.count(); got != 1 {
		t.Fatalf("two mutations ran against web01 concurrently (leaf reached %d times while the first had not "+
			"returned). A trailing-window rate cap does not see concurrency: N simultaneous actuations are one "+
			"count until they finish, which is why the in-flight cap is a separate control.", got)
	}

	close(act.release)
	first := <-done
	if !first.Executed {
		t.Fatalf("the FIRST actuation must have executed normally — the cap refuses the second, never the first: %+v", first)
	}

	// The slot must come back when the first actuation finishes, or the cap degrades from a concurrency
	// control into a one-shot kill switch.
	third, err := i.Do(context.Background(), loopRequest(t, "TG-after"))
	if err != nil {
		t.Fatalf("post-release attempt failed loud: %v", err)
	}
	if !third.Executed {
		t.Fatalf("the in-flight slot was not released when the first actuation finished — the cap has become a "+
			"permanent refusal, which would halt healing after ONE heal: %+v", third)
	}
}

// AN ABSENT KEY MUST NOT BE AN EXEMPTION. `key == "" ⇒ pass` is the vacuous-true shape this codebase keeps
// finding (it is exactly what TG-166's own relevance bug was), so the governor puts every unattributed
// actuation in ONE shared bucket: losing the session ref can only ever make the limiter STRICTER.
//
// KILLING MUTATION (executed 2026-08-04): in limiter.go's scopeKey, return "" for a blank key and add an
// early `if key == "" { return lease, "" }` exemption in Admit — the "no session id, nothing to limit"
// reading. This test then FAILS with
//
//	"6 actuations with NO session ref were admitted against a budget of 2 — an absent key bought unlimited
//	 actuation, which is the vacuous-true shape TG-166 is about."
//
// Restored → green.
func TestAbsentSessionRefSharesOneBucketAndNeverExempts(t *testing.T) {
	clock := newFixedClock()
	limiter := NewActuationLimiter(clock.now).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 2, TargetPerWindow: 99, SessionInFlight: 1, TargetInFlight: 1,
	})
	act := &fakeActuator{}
	i := limitedInterceptor(act, limiter)

	for n := 0; n < 6; n++ {
		if _, err := i.Do(context.Background(), loopRequest(t, "")); err != nil { // no session ref at all
			t.Fatalf("attempt %d failed loud: %v", n, err)
		}
	}
	if act.execs != 2 {
		t.Fatalf("%d actuations with NO session ref were admitted against a budget of 2 — an absent key bought "+
			"unlimited actuation, which is the vacuous-true shape TG-166 is about.", act.execs)
	}
}

// The budget is a TRAILING WINDOW, not a lifetime quota: once the window rolls past, healing resumes. Without
// this the control would be a slow-motion outage — the third genuine incident of the day on a host would
// never be healed.
func TestBudgetReturnsAfterTheWindowRollsPast(t *testing.T) {
	clock := newFixedClock()
	limiter := NewActuationLimiter(clock.now).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 1, TargetPerWindow: 1, SessionInFlight: 1, TargetInFlight: 1,
	})
	act := &fakeActuator{}
	i := limitedInterceptor(act, limiter)

	if out, _ := i.Do(context.Background(), loopRequest(t, "TG-w")); !out.Executed {
		t.Fatalf("the first actuation must execute: %+v", out)
	}
	if out, _ := i.Do(context.Background(), loopRequest(t, "TG-w")); !out.RateLimited {
		t.Fatalf("the second actuation inside the window must be throttled: %+v", out)
	}
	clock.advance(11 * time.Minute)
	out, _ := i.Do(context.Background(), loopRequest(t, "TG-w"))
	if !out.Executed {
		t.Fatalf("after the window rolled past, the budget must return — a lifetime quota would leave the third "+
			"genuine incident of the day unhealable: %+v", out)
	}
	if act.execs != 2 {
		t.Fatalf("want exactly 2 executions (one per window), got %d", act.execs)
	}
}

// THE MULTI-INTERCEPTOR DRIFT GUARD. cmd/worker builds one direct interceptor PLUS one per regime lane from a
// builder; if each held a private limiter, "3 per target per 10 minutes" would silently mean 3 PER LANE and a
// loop that alternated lanes would never be throttled at all. The seam must make a SHARED limiter possible and
// the composition root must use it.
//
// KILLING MUTATION (executed 2026-08-04): make `WithActuationLimiter` ignore its argument (`return i`
// without assigning), so every interceptor keeps the constructor's own private default — the realistic
// regression, since the seam does look redundant next to a constructor that already installs a limiter, and
// it is the same failure as deleting the `ic = ic.WithActuationLimiter(bActuationLimiter)` line from
// cmd/worker/main.go's per-lane interceptorBuilder. This test then FAILS with
//
//	"lane B must be throttled by the SHARED per-target budget: {Executed:true Refused:false … }"
//
// — two lanes, two private windows, one target restarted twice against a budget of one. Restored → green.
func TestASharedLimiterCountsAcrossEveryInterceptor(t *testing.T) {
	clock := newFixedClock()
	shared := NewActuationLimiter(clock.now).WithLimits(ActuationLimits{
		Window: 10 * time.Minute, SessionPerWindow: 99, TargetPerWindow: 1, SessionInFlight: 1, TargetInFlight: 1,
	})
	actA, actB := &fakeActuator{}, &fakeActuator{}
	laneA := limitedInterceptor(actA, shared)
	laneB := limitedInterceptor(actB, shared) // a DIFFERENT lane, the SAME governor

	if out, _ := laneA.Do(context.Background(), loopRequest(t, "TG-lane")); !out.Executed {
		t.Fatalf("lane A must execute the first actuation: %+v", out)
	}
	out, err := laneB.Do(context.Background(), loopRequest(t, "TG-lane2"))
	if err != nil {
		t.Fatalf("lane B failed loud: %v", err)
	}
	if !out.RateLimited {
		t.Fatalf("lane B must be throttled by the SHARED per-target budget: %+v", out)
	}
	if total := actA.execs + actB.execs; total != 1 {
		t.Fatalf("web01 was actuated %d time(s) against a shared per-target budget of 1 — the second interceptor "+
			"counted against its own window, so the cap multiplies by the number of actuation lanes.", total)
	}
}

// THERE IS NO WAY TO SPELL "UNLIMITED". A zero/negative cap takes the conservative default rather than
// switching the scope off — the `0 means no limit` convention is how a rate limiter ends up looking armed
// while counting nothing (which is exactly the state core/policy's rate_limit is in today: configured in
// conservative.json, never attached to an engine).
func TestNoLimitCanBeConfiguredAway(t *testing.T) {
	got := NewActuationLimiter(nil).WithLimits(ActuationLimits{
		Window: -time.Hour, SessionPerWindow: 0, TargetPerWindow: -5, SessionInFlight: 0, TargetInFlight: -1,
	}).Limits()
	if got != DefaultActuationLimits {
		t.Fatalf("a zero/negative budget must fall back to the conservative default, got %+v want %+v — if 0 or "+
			"a negative means 'no limit', every mis-typed config silently disarms the governor", got, DefaultActuationLimits)
	}
}

// A nil governor REFUSES rather than passes. It is unreachable today (NewInterceptor installs one and
// WithActuationLimiter ignores nil), and that is the point: if a future construction path does leave it nil,
// the hole must surface as a refused mutation, not as an ungoverned one.
func TestANilLimiterRefusesRatherThanPasses(t *testing.T) {
	var nilLimiter *ActuationLimiter
	lease, refusal := nilLimiter.Admit("TG-x", "web01")
	if lease != nil || refusal == "" {
		t.Fatalf("a nil actuation limiter must refuse (fail closed), got lease=%v refusal=%q", lease, refusal)
	}
	// And the interceptor's own seam must refuse to remove the control.
	i := NewInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger())
	if i.WithActuationLimiter(nil).limiter == nil {
		t.Fatal("WithActuationLimiter(nil) removed the governor — the seam must ignore nil so the control has no 'off'")
	}
}
