package metrics

// THE OUTCOME ENUM MUST COVER ITS PRODUCER, AND BE PROVEN TO.
//
// ClampModelOutcome folds anything it does not recognise into "other". That is the right shape for a bounded
// label — an unbounded one is a cardinality incident — but it means a class the producer emits and this set
// forgets becomes INVISIBLE by being renamed to "we do not know".
//
// It had already happened. adapters/model/breaker.go declares:
//
//	// ClassBreakerOpen is the bounded ModelError class (and the metric outcome label) for a call the
//	// breaker short-circuited. It is deliberately DISTINCT from every upstream-failure class so an operator
//	// reading tg_model_calls_total{outcome="breaker_open"} sees "TG refused to call" rather than "the
//	// provider failed".
//	const ClassBreakerOpen = "breaker_open"
//
// The intent is written down, the constant exists, and the worker log carries it verbatim
// (`modelcall: tier=primary outcome=breaker_open`). This set discarded it. Measured on dc1tg01,
// 2026-08-06, from the running worker's own log:
//
//	234  tier=embed    outcome=breaker_open
//	216  tier=primary  outcome=breaker_open
//	 67  tier=embed    outcome=provider_error
//	 54  tier=primary  outcome=provider_error
//
// 216 of primary's 636 calls — a third of the model plane's traffic — sat in `other` on the dashboard while
// the log explained every one of them.
//
// So this test does not check for `breaker_open`. Checking for the one class we just fixed would pass
// forever and catch the next drift never. It SCANS THE PRODUCER and fails on any class the enum lacks.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// producerClassRe matches the shapes adapters/model uses to name an outcome class:
//
//  1. the `Class:` field of a ModelError literal;
//  2. the bare string returned by classifyStatus / classifyTransport / the completion path;
//  3. THE OUTCOME PASSED STRAIGHT TO observe() — `g.observe(tier, caller, "<outcome>", …)`.
//
// Shape 3 was the guard's blind spot (TG-535), and a blind spot in a coverage guard is worse than no
// guard: it reports "everything checked" while an entire emission shape goes unread. adapters/model
// emits at least four outcomes this way, and one of them — "concurrency_wait_timeout", the full-gateway
// path that parks a caller until its context dies — was folded into "other" on the dashboard while THIS
// test passed. TG-534's "rate_limit_retry" hit the same blind spot and was hand-pinned around it; that
// hand-pin was a symptom, and this pattern is the fix for the shape.
//
// The outcome is observe()'s THIRD argument, so the two preceding arguments are consumed explicitly
// rather than with a lazy `.*` that would also match a tier or a caller literal.
var producerClassRe = regexp.MustCompile(
	`(?:Class:\s*"([a-z_]+)")` +
		`|(?:return\s+(?:[a-zA-Z]+\s*,\s*)*"([a-z_]+)"(?:\s*,|\s*$|\s*\n))` +
		`|(?:observe\(\s*[^,()]+,\s*[^,()]+,\s*"([a-z_]+)"\s*,)`)

// modelOutcomeClassesFromProducer reads adapters/model's non-test sources and returns every outcome class it
// can emit. Comment lines are stripped: this file's own rationale quotes "breaker_open" and "other" in prose,
// and a comment is not a return statement.
func modelOutcomeClassesFromProducer(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "adapters", "model")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := map[string]bool{}
	files := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		files++
		var code []string
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "//") {
				continue
			}
			code = append(code, ln)
		}
		for _, m := range producerClassRe.FindAllStringSubmatch(strings.Join(code, "\n"), -1) {
			for _, g := range m[1:] {
				if g != "" {
					found[g] = true
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("scanned ZERO producer files — this guard is examining nothing and would pass on any enum")
	}
	out := make([]string, 0, len(found))
	for c := range found {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// KILLING MUTATION: remove "breaker_open" from modelOutcomeSet (the state before this change). RED — and it
// will be equally RED for the NEXT class someone adds to the adapter and forgets here, which is the property
// that matters.
func TestEveryModelOutcomeClassIsInTheEnum(t *testing.T) {
	classes := modelOutcomeClassesFromProducer(t)

	// VACUITY FLOOR. A regex that stops matching would return an empty set and this test would pass while
	// checking nothing. Pin both a floor on the count and two classes we know the adapter emits.
	if len(classes) < 7 {
		t.Fatalf("the producer scan found only %d outcome class(es) %v — the extraction has stopped "+
			"matching and this guard is no longer reading the adapter", len(classes), classes)
	}
	// One known class per EMISSION SHAPE, so a pattern that stops matching is caught by name rather than
	// by a count that other shapes can keep afloat. "concurrency_wait_timeout" is the observe-argument
	// witness (TG-535): if this line ever fails, shape 3 has gone blind again and the count floor alone
	// would not have said so.
	for _, must := range []string{"provider_error", "transport", "concurrency_wait_timeout"} {
		if !containsStr(classes, must) {
			t.Fatalf("the producer scan did not find %q, which adapters/model demonstrably emits — the "+
				"extraction is wrong, so a missing class would not be detected either. Found: %v", must, classes)
		}
	}

	var missing []string
	for _, c := range classes {
		// "other" is the clamp's own fallback, not a class the enum must carry.
		if c == "other" {
			continue
		}
		if ClampModelOutcome(c) != c {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Errorf("adapters/model emits %v, and modelOutcomeSet does not carry them — each is silently "+
			"relabelled \"other\" on tg_model_calls_total, which is the bucket that means \"we do not know\". "+
			"The producer scan saw: %v", missing, classes)
	}
}

// The specific regression, kept as its own named case so the failure reads as what it is.
//
// KILLING MUTATION: same as above. RED.
func TestABreakerRefusalIsNotReportedAsAProviderFailure(t *testing.T) {
	if got := ClampModelOutcome("breaker_open"); got != "breaker_open" {
		t.Fatalf("ClampModelOutcome(\"breaker_open\") = %q. A call TG refused to make is being counted in "+
			"the same bucket as an unclassified provider response, so an operator cannot tell a tripped "+
			"breaker from an unknown failure — which is exactly the distinction the class was created to "+
			"make (adapters/model/breaker.go:32-35)", got)
	}
	// NEGATIVE CONTROL: the clamp must still bound the label. An enum that accepts anything is a cardinality
	// incident, and this test would otherwise pass on `func ClampModelOutcome(s string) string { return s }`.
	if got := ClampModelOutcome("something-a-provider-made-up"); got != "other" {
		t.Fatalf("the clamp accepted an unknown label (%q) — the outcome label is no longer bounded", got)
	}
}

// The 429-backoff interim outcome (TG-534). It is emitted as an observe() ARGUMENT, not a ModelError Class or
// a return literal, so producerClassRe does not see it and the scan above cannot protect it — this named case
// pins it exactly the way TestABreakerRefusalIsNotReportedAsAProviderFailure pins breaker_open.
//
// KILLING MUTATION: remove "rate_limit_retry" from modelOutcomeSet. RED — every retried 429 would then land in
// `other`, indistinguishable from an unclassified outcome, and the "visible in metrics" claim at the emit site
// would be false for the counter a dashboard queries.
func TestARetriedRateLimitIsNotFoldedIntoOther(t *testing.T) {
	if got := ClampModelOutcome("rate_limit_retry"); got != "rate_limit_retry" {
		t.Fatalf("ClampModelOutcome(\"rate_limit_retry\") = %q. A 429 the gateway backed off and retried is "+
			"being counted in the `other` bucket, so an operator cannot see how much throttle the gateway is "+
			"absorbing — the exact visibility the outcome was created to give (adapters/model/model.go, the "+
			"rate_limit_retry observe in CompleteWithUsage)", got)
	}
	// NEGATIVE CONTROL: a surfaced (exhausted) rate_limit stays its own class, distinct from the interim one.
	if got := ClampModelOutcome("rate_limit"); got != "rate_limit" {
		t.Fatalf("ClampModelOutcome(\"rate_limit\") = %q, want rate_limit — the surfaced-vs-retried distinction is lost", got)
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
