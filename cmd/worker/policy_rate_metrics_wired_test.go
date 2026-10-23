package main

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// TG-339. The policy rate governor's counters exist and are unit-tested in core/policy. That is not the
// guard: this repo has repeatedly shipped a correct measurement that no composition root published, and
// the governor itself spent its whole life wired to nothing while a template advertised its limit.

func TestThePolicyRateGovernorReachesMetrics(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := stripGoComments(string(raw))

	if !strings.Contains(src, "withPolicyRateGovernor(") {
		t.Fatal("main.go never hands the policy rate governor to the admin surface — its counters are " +
			"computed and published by nothing, which is the state this ticket found the governor in")
	}
	if !strings.Contains(src, "policyRateGovForMetrics.Store(") {
		t.Fatal("the governor is never stored for the metrics hand-off, so the closure reads a nil pointer " +
			"on every scrape and reports absent forever")
	}
	// The SAME instance must be metered and used. Constructing a second governor for metrics would publish
	// a permanently-zero series next to a working control — worse than no series, because it reads as proof.
	if strings.Count(src, "policy.NewRateGovernor(") != 1 {
		t.Errorf("main.go constructs policy.NewRateGovernor %d times. The metered governor and the one the "+
			"engine uses must be one instance, or the counters describe a governor nothing consults.",
			strings.Count(src, "policy.NewRateGovernor("))
	}
}

func TestAbsentGovernorPublishesNothing(t *testing.T) {
	// No governor (no pool ⇒ no engine): the series must be ABSENT, not a confident zero.
	if _, ok := policyRateStats(nil); ok {
		t.Error("a nil stats func reported ok=true — a deployment with no policy engine would publish " +
			"tg_policy_rate_* at zero, which reads as \"the governor is running and has clamped nothing\"")
	}
	if _, ok := policyRateStats(func() (policy.RateGovernorStats, bool) {
		return policy.RateGovernorStats{}, false
	}); ok {
		t.Error("ok=false from the source was not propagated — absence must survive the indirection")
	}
	if st, ok := policyRateStats(func() (policy.RateGovernorStats, bool) {
		return policy.RateGovernorStats{Governed: 7}, true
	}); !ok || st.Governed != 7 {
		t.Errorf("a present governor did not propagate: ok=%v governed=%d", ok, st.Governed)
	}
}
