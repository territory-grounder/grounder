package actuate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/breaker"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// THE BASELINE GATE — the 2026-07-28 false deviation (governance ledger 5153-5155), as oracles.
//
// The incident: in the one second where the TG-148 pair baseline was unestablished (`preObserved, _ :=
// r.Observe(ctx)` discarded the read's ok) and the post-read succeeded, a stale uncleared alert on an
// unrelated, already-recovered host adjudicated as the cascade of a start-guest on a DIFFERENT host. One
// manufactured verdict then: tripped the estate-wide mutation breaker, force-Shadowed every worker, demoted
// start-guest auto→approve, and halted all actuation for 1h49m. The ledger row could not even say what the
// verdict had been computed against — the baseline size was never recorded — so the mechanism stayed
// undiagnosable for hours.
//
// WHY THE EXISTING SUITE CERTIFIED THE BUG: every pre-read fake in this package (and in the spec/012,
// spec/013 acceptance suites) returns ok=true. The (nil,false) pre-read was structurally unreachable by any
// test, so the discard of the ok bool was green for six days while it was live. These oracles exist to make
// that input reachable — and O1 deliberately asserts over CLOSED ENUMERATIONS (the ledger decision set, the
// breaker state, the graduation call count), not substrings, so a new destructive reaction added to the
// deviation path cannot slip past them unnamed.
//
// NOTE ON MODELS (what these controls prove if the estate model behind them is wrong): O1/O2 make NO claim
// about which hosts were genuinely anomalous — their subject is the system's own epistemics: a verdict that
// could not be computed against ANY established baseline must not exist, and therefore cannot trip anything.
// That holds regardless of what was actually wrong on the estate that night. O3's host-arm subtraction IS
// conditional on the open-incident model (a host with an open incident at execution is not this action's
// cascade); its control proves the arm is load-bearing, not that the model is true — the model's own oracles
// live with OpenIncidentHosts in core/db.

func failingThenFailingObserve() func(context.Context) ([]verify.ObservedAlert, bool) {
	return func(context.Context) ([]verify.ObservedAlert, bool) { return nil, false }
}

// shrinkRetry makes the gate's bounded retry instant for the test and restores it after.
func shrinkRetry(t *testing.T) {
	t.Helper()
	prev := baselineRetryDelay
	baselineRetryDelay = time.Millisecond
	t.Cleanup(func() { baselineRetryDelay = prev })
}

// TestUnestablishedBaselineRefusesToExecute is O1+O2 — the load-bearing oracle.
//
// Pre-read fails on BOTH attempts, no host arm is wired, and the post-state WOULD show a surprise host. The
// old code executed anyway, computed the verdict against a nil baseline, and manufactured the deviation. Now:
// the action must be REFUSED at the `baseline` gate — zero executions — and NONE of the deviation path's
// destructive reactions may exist, asserted over the full ledger decision enumeration.
func TestUnestablishedBaselineRefusesToExecute(t *testing.T) {
	shrinkRetry(t)
	ctx := context.Background()
	cp := safety.NewActuatingChokepoint()
	mb, err := safety.NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	grad := &spyGradRecorder{}
	act := &fakeActuator{}
	ledger := audit.NewLedger()
	i := actuatingInterceptor(cp, act, ledger).WithMutationBreaker(mb).WithGraduationRecorder(grad)

	r := deviationRequest(t) // post-state carries surprise99 — the manufactured-deviation ingredient
	r.Observe = failingThenFailingObserve()
	r.PreAnomalous = nil // host arm unwired — NO baseline of any kind can be established

	out, err := i.Do(ctx, r)
	if err != nil {
		t.Fatalf("a refused action is a decision, not an error: %v", err)
	}
	// O2: never executed, and the refusing gate is the baseline gate by name.
	if !out.Refused || act.execs != 0 {
		t.Fatalf("an action whose baseline cannot be established must be REFUSED before execute (got refused=%t execs=%d) — "+
			"executing recreates the verdict-without-a-baseline that manufactured the 2026-07-28 deviation", out.Refused, act.execs)
	}
	if !strings.Contains(out.Reason, "baseline") {
		t.Fatalf("the refusal must come from the baseline gate, got reason: %q", out.Reason)
	}
	// O1: the destructive reactions must not exist — closed enumeration over every ledger decision, not a
	// substring probe, so ANY trip/demote/execute row fails this loudly.
	for _, e := range ledger.Entries() {
		switch {
		case strings.HasPrefix(e.Decision, "safety:breaker-trip"),
			strings.HasPrefix(e.Decision, "actuate:graduation"),
			strings.HasPrefix(e.Decision, "actuate:execute"),
			strings.HasPrefix(e.Decision, "actuate:exec-log"):
			t.Fatalf("destructive/execution decision %q recorded for an action that must never have executed", e.Decision)
		}
	}
	if mb.Tripped(ctx) {
		t.Fatal("the mutation breaker tripped on an action that never executed — the manufactured-verdict path is alive")
	}
	if grad.calls != 0 {
		t.Fatalf("graduation recorded %d outcome(s) for a refused action — the ladder must be untouched", grad.calls)
	}
}

// TestBaselineRetryRecoversATransientBlip — the observed failure was a one-second window; one bounded retry
// must turn a single transient pre-read failure back into a normal, fully-verified execution.
func TestBaselineRetryRecoversATransientBlip(t *testing.T) {
	shrinkRetry(t)
	ctx := context.Background()
	r := goodRequest(t)
	call := 0
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		call++
		if call == 1 {
			return nil, false // the transient blip — attempt 1 of the pair arm fails
		}
		return []verify.ObservedAlert{}, true // retry + post-read: quiet estate
	}
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("a single transient baseline blip must be absorbed by the bounded retry (got %+v) — "+
			"refusing on one blip would make healing unavailable exactly when monitoring hiccups", out)
	}
}

// TestPreAnomalousHostDoesNotDeviate is O3 — the ledger-5153 shape, exactly.
//
// The pair baseline is established but EMPTY (the stale-alert case: the surprise host's alert was firing in
// the post-read yet absent from the pre-snapshot — the key-shift/staleness family), and the host arm reports
// the surprise host as already holding an OPEN incident. The verdict must be MATCH: an already-broken host is
// that incident evolving, not this action's cascade. CONDITIONAL on the open-incident model (see the file
// header); the arm's load-bearing-ness is what this proves.
func TestPreAnomalousHostDoesNotDeviate(t *testing.T) {
	shrinkRetry(t)
	ctx := context.Background()
	cp := safety.NewActuatingChokepoint()
	mb, err := safety.NewMutationBreaker(cp, breaker.NewMemStore(), 1, nil)
	if err != nil {
		t.Fatalf("arm breaker: %v", err)
	}
	r := deviationRequest(t) // pre: quiet pair snapshot; post: surprise99 firing
	r.PreAnomalous = func(context.Context) (map[string]bool, bool) {
		return map[string]bool{"surprise99": true}, true // surprise99 already held an OPEN incident pre-execution
	}
	out, err := actuatingInterceptor(cp, &fakeActuator{}, audit.NewLedger()).WithMutationBreaker(mb).Do(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("a host already holding an OPEN incident before execution must not adjudicate as this action's "+
			"cascade (got %+v) — this is byte-for-byte the ledger-5153 false deviation", out)
	}
	if mb.Tripped(ctx) {
		t.Fatal("the breaker tripped on a pre-anomalous host — the estate-wide kill fired on a stale incident again")
	}

	// Control within the oracle: the SAME action with the host arm reporting nothing open must still DEVIATE —
	// the arm must never blind a genuinely new cascade.
	r2 := deviationRequest(t)
	r2.PreAnomalous = func(context.Context) (map[string]bool, bool) { return map[string]bool{}, true }
	out2, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(ctx, r2)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Verdict != safety.VerdictDeviation {
		t.Fatalf("with no open incident on the surprise host the deviation must stand (got %v) — "+
			"the host arm may only subtract what was already broken", out2.Verdict)
	}
}

// TestDegradedPairArmStillAdjudicatesViaHostArm — the availability half of the gate's design: pair arm down
// after retry, host arm established ⇒ the action still executes and the host arm alone separates pre-existing
// from caused. This is what distinguishes the gate from a blanket "LibreNMS down ⇒ no healing".
func TestDegradedPairArmStillAdjudicatesViaHostArm(t *testing.T) {
	shrinkRetry(t)
	ctx := context.Background()
	r := deviationRequest(t)
	pre, post := 0, 0
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) {
		// The gate makes two pre attempts; both fail. The post-read (step 6) succeeds with the surprise.
		if pre < 2 {
			pre++
			return nil, false
		}
		post++
		return []verify.ObservedAlert{{Host: "surprise99", Rule: "HostDown", Site: "nl"}}, true
	}
	r.PreAnomalous = func(context.Context) (map[string]bool, bool) {
		return map[string]bool{"surprise99": true}, true // the host arm knows surprise99 was already broken
	}
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("with the host arm established the action must execute and the pre-anomalous host must not "+
			"deviate (got %+v) — one arm is a baseline, zero arms is a refusal", out)
	}
	if post == 0 {
		t.Fatal("fixture wiring error: the post-read never ran, the verdict proved nothing")
	}
}

// TestDeviationReasonCarriesBaselineProvenance is O4 — the diagnosability oracle with the longest half-life.
//
// The 2026-07-28 mechanism stayed undiagnosable for hours because the execute:deviation row never said what
// the verdict was computed against. A genuine deviation's ledger reason must now carry the baseline
// provenance: baseline_ok/baseline (the pair arm) and pre_anomalous_ok/pre_anomalous (the host arm).
func TestDeviationReasonCarriesBaselineProvenance(t *testing.T) {
	shrinkRetry(t)
	ctx := context.Background()
	ledger := audit.NewLedger()
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, ledger).Do(ctx, deviationRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictDeviation {
		t.Fatalf("precondition: a genuine deviation must execute and verify as one, got %+v", out)
	}
	var reason string
	for _, e := range ledger.Entries() {
		if strings.Contains(e.Decision, "execute:deviation") {
			reason = e.Reason
		}
	}
	if reason == "" {
		t.Fatal("no execute:deviation ledger record found")
	}
	for _, tok := range []string{"baseline_ok=", "baseline=", "pre_anomalous_ok=", "pre_anomalous="} {
		if !strings.Contains(reason, tok) {
			t.Fatalf("the deviation reason must record its baseline provenance (%q missing) — without it the next "+
				"false deviation is undiagnosable again, got: %q", tok, reason)
		}
	}
}

// TestUnestablishedHostArmIsNotAnEmptyOne — (nil,false)≠(empty,true) at the host-arm seam, M3's oracle.
//
// A wired host arm that FAILS (both attempts) must contribute NOTHING to the verdict — the pair arm, which is
// established here and contains the pre-existing alert, still excludes it. If a future change collapses the
// failed read into an EMPTY set at the gate, this stays green (the pair arm covers it); what it pins is that
// the failure must not REFUSE the action when the pair arm is fine, and must not corrupt the verdict.
func TestUnestablishedHostArmIsNotAnEmptyOne(t *testing.T) {
	shrinkRetry(t)
	ctx := context.Background()
	unrelated := []verify.ObservedAlert{{Host: "unrelated-db07", Rule: "DiskFull", Site: "gr"}}
	r := goodRequest(t)
	r.Observe = func(context.Context) ([]verify.ObservedAlert, bool) { return unrelated, true } // pre==post
	hostArmCalls := 0
	r.PreAnomalous = func(context.Context) (map[string]bool, bool) { hostArmCalls++; return nil, false } // wired, failing
	out, err := actuatingInterceptor(safety.NewActuatingChokepoint(), &fakeActuator{}, audit.NewLedger()).Do(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Executed || out.Verdict != safety.VerdictMatch {
		t.Fatalf("with the pair arm established, a failing host arm must neither refuse nor corrupt the verdict, got %+v", out)
	}
	if hostArmCalls != 2 {
		t.Fatalf("the failing host arm must be retried exactly once (2 attempts), got %d — either the retry is "+
			"gone or it retries unboundedly", hostArmCalls)
	}
}
