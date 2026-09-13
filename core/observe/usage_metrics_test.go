package observe

import (
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
	"github.com/territory-grounder/grounder/core/metrics"
)

// renderOf collects the registry and returns the exposition text.
func renderOf(t *testing.T, r *Registry) string {
	t.Helper()
	return metrics.Render(r.Collect())
}

// TestMeasuredTokensAreExposedByTierAndKind. The whole point of a separate series from
// tg_agent_tokens_approx_total is that this one can be read as spend without a caveat.
func TestMeasuredTokensAreExposedByTierAndKind(t *testing.T) {
	r := NewRegistry()
	r.Usage("fast", 1603, 4, true)
	r.Usage("fast", 100, 10, true)
	out := renderOf(t, r)
	for _, want := range []string{
		`tg_model_tokens_total{kind="prompt",model="fast"} 1703`,
		`tg_model_tokens_total{kind="completion",model="fast"} 14`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}
}

// TestUnmeasuredCallsGoToTheMissingCounterNotTheTokenSeries is the honesty oracle for /metrics.
//
// The token values here are NON-ZERO on purpose, and that is the whole test. The realistic regression is
// not a provider sending zeros — it is a future caller "helpfully" forwarding the chars/4 ESTIMATE through
// this same method with measured=false, because the estimate is right there and the series looks like the
// place for tokens. The early return is what makes that harmless. A version of this test that passed
// (0, 0, false) would be VACUOUS: the `> 0` guards below would swallow the estimate anyway and the branch
// could be deleted with every assertion still green — which is exactly what happened on the first draft.
//
// KILLING MUTATION (EXECUTED 2026-08-04): delete the `return` from the `if !measured` branch in
// Registry.Usage so an unmeasured call falls through into the token counters. This test fails with
//
//	an UNMEASURED call landed in tg_model_tokens_total — the measured series is now part guess,
//	and the only series a cost dashboard can trust has been quietly poisoned (TG-44)
//
// Return restored, green.
func TestUnmeasuredCallsGoToTheMissingCounterNotTheTokenSeries(t *testing.T) {
	r := NewRegistry()
	r.Usage("fast", 852, 0, false) // the chars/4 estimate for a 3.4k-char prompt — a guess, not a measurement
	r.Usage("fast", 852, 0, false)
	out := renderOf(t, r)
	if !strings.Contains(out, `tg_model_usage_missing_total{model="fast"} 2`) {
		t.Fatalf("exposition must count the 2 unmeasured calls\n---\n%s", out)
	}
	// Match the SERIES line, not any mention: the estimate's own HELP text names tg_model_tokens_total (that
	// is the point of it), so a bare substring check would fail against correct output.
	if strings.Contains(out, "\ntg_model_tokens_total{") {
		t.Fatalf("an UNMEASURED call landed in tg_model_tokens_total — the measured series is now part guess, "+
			"and the only series a cost dashboard can trust has been quietly poisoned (TG-44)\n---\n%s", out)
	}
	// VACUITY FLOOR: the negative assertion above passes trivially if the token series NEVER renders. Prove
	// it renders on the measured path.
	r2 := NewRegistry()
	r2.Usage("fast", 5, 1, true)
	if !strings.Contains(renderOf(t, r2), "\ntg_model_tokens_total{") {
		t.Fatal("tg_model_tokens_total never renders at all — the absence assertion above proves nothing")
	}
}

// TestApproxTokensHelpNamesTheMeasuredSeries. The estimate is kept (it counts a different thing and
// dashboards read it) but a wrong number that is trusted is worse than one that is missing, so its HELP
// must send a reader to the measured series and say how wrong it is.
func TestApproxTokensHelpNamesTheMeasuredSeries(t *testing.T) {
	r := NewRegistry()
	r.AgentLoop(AgentLoopStat{Outcome: "stop", ApproxTokens: 100})
	out := renderOf(t, r)
	var help string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# HELP "+metrics.MetricAgentTokensApprox) {
			help = line
		}
	}
	if help == "" {
		t.Fatalf("no HELP line for %s\n---\n%s", metrics.MetricAgentTokensApprox, out)
	}
	if !strings.Contains(help, metrics.MetricModelTokens) {
		t.Errorf("the estimate's HELP must point at the measured series %s: %q", metrics.MetricModelTokens, help)
	}
	if !strings.Contains(help, "ESTIMATED") {
		t.Errorf("the estimate's HELP must say it is an ESTIMATE: %q", help)
	}
}

// TestUsageLabelsAreClamped: an operator-set eval-arm tier is unbounded by design and must not become an
// unbounded metric label.
func TestUsageLabelsAreClamped(t *testing.T) {
	r := NewRegistry()
	r.Usage("arm-haiku-2026-08", 10, 1, true)
	out := renderOf(t, r)
	if strings.Contains(out, "arm-haiku") {
		t.Fatalf("an unbounded tier reached a metric label\n---\n%s", out)
	}
	if !strings.Contains(out, `model="other"`) {
		t.Fatalf("the unbounded tier must fold to other\n---\n%s", out)
	}
	// VACUITY FLOOR: a KNOWN tier must survive verbatim, or the clamp is just erasure.
	r2 := NewRegistry()
	r2.Usage("primary", 10, 1, true)
	if !strings.Contains(renderOf(t, r2), `model="primary"`) {
		t.Fatal("a known tier was folded away too — the clamp must bound cardinality, not blank the label")
	}
}

// TestGatewayObserverForwardsUsageAndWarnsOncePerTier: the composition-root glue is what makes the gateway
// the single point where no model call is invisible.
func TestGatewayObserverForwardsUsageAndWarnsOncePerTier(t *testing.T) {
	r := NewRegistry()
	var logged []string
	o := NewGatewayObserver(r, 0, func(f string, a ...any) { logged = append(logged, f) })

	o.ObserveUsage("fast", model.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25, Measured: true})
	o.ObserveUsage("primary", model.Usage{})
	o.ObserveUsage("primary", model.Usage{})

	out := renderOf(t, r)
	if !strings.Contains(out, `tg_model_tokens_total{kind="prompt",model="fast"} 20`) {
		t.Errorf("measured usage did not reach the registry\n---\n%s", out)
	}
	if !strings.Contains(out, `tg_model_usage_missing_total{model="primary"} 2`) {
		t.Errorf("unmeasured calls did not reach the missing counter\n---\n%s", out)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d warnings for 2 unmeasured calls on one tier, want exactly 1 — silent hides the "+
			"guess, per-call buries it", len(logged))
	}
	if !strings.Contains(logged[0], "ESTIMATE") {
		t.Errorf("the warning %q must say the fallback is an ESTIMATE", logged[0])
	}
}

// TestNilRegistryUsageIsSafe — the nil-safe contract every other Registry method holds.
func TestNilRegistryUsageIsSafe(t *testing.T) {
	var r *Registry
	r.Usage("fast", 1, 1, true) // must not panic
	if got := r.Collect(); got != nil {
		t.Fatalf("nil registry collected %v, want nil", got)
	}
}
