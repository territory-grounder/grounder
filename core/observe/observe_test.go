package observe

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
)

// TestNilEmitterIsSilentNoOp proves the core nil-safety contract: a nil Emitter (the no-DB path / an
// oracle that never wires one) is a silent no-op through the package RecordX helpers AND a nil *Registry
// receiver is a no-op — neither panics, and a nil registry collects nothing.
func TestNilEmitterIsSilentNoOp(t *testing.T) {
	// nil interface via the helpers — must not panic.
	RecordAgentLoop(nil, AgentLoopStat{Outcome: "proposed", Duration: time.Second, ToolCalls: 2})
	RecordVerdict(nil, "match")
	RecordDecision(nil, "AUTO", false)

	// typed-nil *Registry stored in the interface — helpers see a non-nil interface, so the nil-RECEIVER
	// guards must carry the no-op.
	var nilReg *Registry
	var e Emitter = nilReg
	RecordAgentLoop(e, AgentLoopStat{Outcome: "stop"})
	RecordVerdict(e, "deviation")
	RecordDecision(e, "POLL_PAUSE", true)

	// a nil *Registry collects nothing (no panic).
	if got := nilReg.Collect(); got != nil {
		t.Fatalf("nil *Registry.Collect() must be nil, got %v", got)
	}
	// the process-global default is unset here → Collect() is a nil-safe no-op.
	if got := Collect(); got != nil {
		t.Fatalf("unset default Collect() must be nil, got %v", got)
	}
}

// TestFiveMetricFamilyEmitted proves a driven Registry records and renders the five agent metrics with the
// EXPECTED BOUNDED labels: runtime, tool-call count, tool errors, approximate tokens, and the by-outcome
// runs counter (the accuracy dimension). Also proves the verdict + governance-decision families render.
func TestFiveMetricFamilyEmitted(t *testing.T) {
	r := NewRegistry()
	r.AgentLoop(AgentLoopStat{Outcome: "proposed", Duration: 2 * time.Second, ToolCalls: 3, ToolErrors: 1, ApproxTokens: 400})
	r.AgentLoop(AgentLoopStat{Outcome: "stop", Duration: time.Second, ToolCalls: 1, ToolErrors: 0, ApproxTokens: 100})
	r.Verdict("match")
	r.Decision("AUTO", false)
	r.Decision("POLL_PAUSE", true)

	out := metrics.Render(r.Collect())
	for _, want := range []string{
		"# TYPE tg_agent_run_seconds_total counter",
		"tg_agent_run_seconds_total 3", // 2s + 1s summed
		"# TYPE tg_agent_tool_calls_total counter",
		"tg_agent_tool_calls_total 4", // 3 + 1
		"# TYPE tg_agent_tool_errors_total counter",
		"tg_agent_tool_errors_total 1",
		"# TYPE tg_agent_tokens_approx_total counter",
		"tg_agent_tokens_approx_total 500", // 400 + 100
		"# TYPE tg_agent_runs_total counter",
		`tg_agent_runs_total{outcome="proposed"} 1`,
		`tg_agent_runs_total{outcome="stop"} 1`,
		`tg_agent_verdicts_total{outcome="match"} 1`,
		`tg_governance_decisions_total{band="AUTO",withheld="false"} 1`,
		`tg_governance_decisions_total{band="POLL_PAUSE",withheld="true"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q; got:\n%s", want, out)
		}
	}
}

// TestUnknownLabelsClampedToBounded proves an out-of-enum label value can never introduce unbounded
// cardinality — it is folded to "other" (or "unset" for an absent verdict).
func TestUnknownLabelsClampedToBounded(t *testing.T) {
	r := NewRegistry()
	r.AgentLoop(AgentLoopStat{Outcome: "some-unexpected-outcome"})
	r.Verdict("")                          // absent verdict → "unset"
	r.Verdict("weird")                     // out-of-enum → "other"
	r.Decision("dc1web01-band", false) // a value that must never appear verbatim as a label

	out := metrics.Render(r.Collect())
	for _, want := range []string{
		`tg_agent_runs_total{outcome="other"} 1`,
		`tg_agent_verdicts_total{outcome="unset"} 1`,
		`tg_agent_verdicts_total{outcome="other"} 1`,
		`tg_governance_decisions_total{band="other",withheld="false"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing clamped %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "some-unexpected-outcome") || strings.Contains(out, "dc1web01-band") {
		t.Fatalf("an out-of-enum value must never appear verbatim as a label; got:\n%s", out)
	}
}

// TestCollectIsDeterministicAndSecretFree proves a scrape of an unchanged Registry is byte-identical every
// time (stable sorted order over the label maps) and that the exposition carries no secret-shaped content.
func TestCollectIsDeterministicAndSecretFree(t *testing.T) {
	r := NewRegistry()
	// drive several labelled samples whose map-iteration order would otherwise be random.
	for _, o := range []string{"proposed", "stop", "escalate", "hard-halt"} {
		r.AgentLoop(AgentLoopStat{Outcome: o, Duration: time.Second, ToolCalls: 1, ApproxTokens: 40})
	}
	for _, v := range []string{"match", "partial", "deviation"} {
		r.Verdict(v)
	}
	r.Decision("AUTO", false)
	r.Decision("AUTO_NOTICE", false)
	r.Decision("POLL_PAUSE", true)

	first := metrics.Render(r.Collect())
	for i := 0; i < 50; i++ {
		if got := metrics.Render(r.Collect()); got != first {
			t.Fatalf("Collect/Render must be deterministic across scrapes:\nfirst:\n%s\ngot:\n%s", first, got)
		}
	}
	// secret-free: only counts + bounded enum labels are emitted; assert no CREDENTIAL-shaped substrings
	// (bare "token" is excluded — it is a legitimate part of the token-count metric NAME) and that the ONLY
	// label keys present are the bounded ones.
	for _, banned := range []string{"secret", "password", "bearer ", "api_key", "one_key", "/secrets/", "authorization", "-----begin"} {
		if strings.Contains(strings.ToLower(first), banned) {
			t.Fatalf("exposition must be secret-free, found %q:\n%s", banned, first)
		}
	}
	for _, line := range strings.Split(first, "\n") {
		if !strings.Contains(line, "{") {
			continue
		}
		labelKeys := line[strings.Index(line, "{")+1 : strings.Index(line, "}")]
		for _, kv := range strings.Split(labelKeys, ",") {
			key := strings.SplitN(kv, "=", 2)[0]
			switch key {
			case "outcome", "band", "withheld":
			default:
				t.Fatalf("unexpected/unbounded label key %q in line %q", key, line)
			}
		}
	}
}

// TestConcurrentRecordingIsRaceFree drives the Registry from many goroutines; run under -race it proves the
// lock serializes the shared counters (the worker records from concurrent Temporal activities).
func TestConcurrentRecordingIsRaceFree(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.AgentLoop(AgentLoopStat{Outcome: "proposed", Duration: time.Millisecond, ToolCalls: 1, ApproxTokens: 8})
			r.Verdict("match")
			r.Decision("AUTO", false)
			_ = r.Collect()
		}()
	}
	wg.Wait()
	out := metrics.Render(r.Collect())
	if !strings.Contains(out, "tg_agent_tool_calls_total 64") {
		t.Fatalf("expected 64 tool calls after concurrent recording; got:\n%s", out)
	}
}

// TestDefaultRegistryExposureSeam proves the process-global default seam the /metrics handler uses: after
// SetDefault, package Collect() returns that registry's samples; it is reset to keep tests isolated.
func TestDefaultRegistryExposureSeam(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	r := NewRegistry()
	r.AgentLoop(AgentLoopStat{Outcome: "proposed", ToolCalls: 2})
	SetDefault(r)
	out := metrics.Render(Collect())
	if !strings.Contains(out, "tg_agent_tool_calls_total 2") {
		t.Fatalf("package Collect() must expose the default registry; got:\n%s", out)
	}
}
