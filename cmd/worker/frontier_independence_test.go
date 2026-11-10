package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	coregov "github.com/territory-grounder/grounder/core/governance"
)

// TG-356. The boot-time independence check is correct and one-shot. It runs in the same startup window in
// which litellm may not be listening, and on failure the anchor arms on the tier-NAME comparison — exactly
// what this ticket was filed to replace. Measured live on dc1tg01 across two consecutive boots
// (01:37:37, and again 02:01:43 after a fresh deploy), so it is deterministic, not a fluke; the gateway
// answers HTTP 401 from the worker's own network seconds later.
//
// The property is NOT "resolution succeeds at boot" — it cannot be guaranteed to. It is that an
// UNVERIFIED claim keeps being re-checked, and that a confirmed same-model pair refuses to run at all.

type stubPairs struct{ calls int }

func (s *stubPairs) RecentCrossCheckPairs(context.Context) ([]coregov.CrossCheckPair, error) {
	s.calls++
	return []coregov.CrossCheckPair{{}}, nil
}

func gated(inner coregov.PairSource, resolve func(context.Context) (bool, bool, error), logs *[]string) *independenceGatedPairs {
	return &independenceGatedPairs{
		inner: inner, resolve: resolve,
		logf:     func(f string, a ...any) { *logs = append(*logs, sprintfSafe(f, a...)) },
		frontier: "fallback-mistral", local: "primary",
	}
}

func sprintfSafe(f string, a ...any) string {
	var b strings.Builder
	for range a {
		_ = b
	}
	return f
}

// TestUnresolvableRetriesOnEveryRun is the finding: the boot answer must not be final.
func TestUnresolvableRetriesOnEveryRun(t *testing.T) {
	inner := &stubPairs{}
	attempts := 0
	var logs []string
	g := gated(inner, func(context.Context) (bool, bool, error) {
		attempts++
		return false, false, errors.New("connection refused")
	}, &logs)

	for i := 0; i < 3; i++ {
		if _, err := g.RecentCrossCheckPairs(context.Background()); err != nil {
			t.Fatalf("run %d: an UNRESOLVABLE gateway must not stop the cross-check (fail-open is deliberate): %v", i, err)
		}
	}
	if attempts != 3 {
		t.Errorf("resolution was attempted %d time(s) across 3 runs, want 3. A single boot-time attempt is "+
			"the defect: the gateway is reachable seconds later and nothing ever asks again.", attempts)
	}
	if inner.calls != 3 {
		t.Errorf("the inner pair source ran %d time(s), want 3 — an unresolvable gateway must not silence a "+
			"cross-check that may well be independent", inner.calls)
	}
	if len(logs) != 3 {
		t.Errorf("the still-UNVERIFIED state was announced %d time(s) across 3 runs, want 3. Saying it once "+
			"at boot puts the warning in a startup line that has scrolled away by the time the degraded "+
			"run happens.", len(logs))
	}
}

// TestAnsweredButUnidentifiedNeverSettles is the case the first version of this file missed, and a
// mutation (`case err == nil:` in place of `case err == nil && resolved:`) survived because of it.
//
// SameUpstreamModel has THREE outcomes, not two: it can error, it can resolve, or it can answer cleanly
// while being unable to identify one of the tiers (resolved=false, err=nil — e.g. the gateway is up but
// /v1/model/info does not list that alias). Treating "no error" as "resolved" caches same=false forever
// and reports independence that was never established — which is TG-356 itself, arrived at by a
// different route.
func TestAnsweredButUnidentifiedNeverSettles(t *testing.T) {
	inner := &stubPairs{}
	attempts := 0
	var logs []string
	g := gated(inner, func(context.Context) (bool, bool, error) {
		attempts++
		return false, false, nil // clean answer, models NOT identified
	}, &logs)

	for i := 0; i < 3; i++ {
		if _, err := g.RecentCrossCheckPairs(context.Background()); err != nil {
			t.Fatalf("run %d: unidentified models must fail OPEN, not refuse: %v", i, err)
		}
	}
	if attempts != 3 {
		t.Errorf("resolution was attempted %d time(s) across 3 runs, want 3. A clean answer that did not "+
			"IDENTIFY the models establishes nothing; caching it as settled reports unverified "+
			"independence as verified.", attempts)
	}
	if len(logs) != 3 {
		t.Errorf("the still-UNVERIFIED state was announced %d time(s), want 3", len(logs))
	}
}

// TestSameUpstreamModelRefusesTheRun is the safety property.
func TestSameUpstreamModelRefusesTheRun(t *testing.T) {
	inner := &stubPairs{}
	var logs []string
	g := gated(inner, func(context.Context) (bool, bool, error) { return true, true, nil }, &logs)

	_, err := g.RecentCrossCheckPairs(context.Background())
	if err == nil {
		t.Fatal("the anchor ran while the frontier tier and the local judge tier resolve to the SAME " +
			"upstream model. That is the judge grading itself under a second alias — the exact blind spot " +
			"the anchor exists to close, and it would report agreement with itself as independent evidence.")
	}
	if inner.calls != 0 {
		t.Errorf("the inner pair source was consulted %d time(s) despite a confirmed same-model pair", inner.calls)
	}
	// And it must STAY refused without re-resolving — the answer is settled.
	if _, err2 := g.RecentCrossCheckPairs(context.Background()); err2 == nil {
		t.Error("the refusal did not persist on the second run")
	}
}

// TestVerifiedIndependenceStopsRetrying. A resolved-independent pair must cache: re-resolving forever
// would put a gateway round-trip on every scheduled run for an answer that cannot change without a
// restart, and would make the log unreadable.
func TestVerifiedIndependenceStopsRetrying(t *testing.T) {
	inner := &stubPairs{}
	attempts := 0
	var logs []string
	g := gated(inner, func(context.Context) (bool, bool, error) {
		attempts++
		return false, true, nil
	}, &logs)

	for i := 0; i < 3; i++ {
		if _, err := g.RecentCrossCheckPairs(context.Background()); err != nil {
			t.Fatalf("a VERIFIED-independent pair must run: %v", err)
		}
	}
	if attempts != 1 {
		t.Errorf("resolution ran %d time(s) after being settled, want 1 — a settled answer is cached", attempts)
	}
	if inner.calls != 3 {
		t.Errorf("the cross-check ran %d of 3 times", inner.calls)
	}
	if len(logs) != 1 {
		t.Errorf("the late VERIFICATION was announced %d time(s), want exactly 1", len(logs))
	}
}

// TestTheGateIsWiredAtTheCompositionRoot. The type existing and the monitor using it are different facts;
// this repo's signature defect is the first without the second.
func TestTheGateIsWiredAtTheCompositionRoot(t *testing.T) {
	src := stripGoComments(readWorkerMain(t))
	if len(src) < 10_000 {
		t.Fatalf("VACUITY FLOOR: main.go stripped to %d bytes", len(src))
	}
	i := strings.Index(src, "govActs.CrossCheck = &coregov.FrontierCrossCheckMonitor{")
	if i < 0 {
		t.Fatal("the frontier cross-check monitor is no longer constructed in main.go")
	}
	block := src[i:min(i+900, len(src))]
	if !strings.Contains(block, "&independenceGatedPairs{") {
		t.Fatal("the monitor's Pairs is not the independence-gated source. The gate type would exist and " +
			"nothing would consult it, leaving the anchor armed on the boot-time name check — the defect " +
			"TG-356 describes, with an unused type beside it.")
	}
	if !strings.Contains(block, "SameUpstreamModel(") {
		t.Error("the gate is wired without a resolver that asks the gateway which upstream model each tier " +
			"is — it would cache 'unresolvable' forever and change nothing")
	}
}
