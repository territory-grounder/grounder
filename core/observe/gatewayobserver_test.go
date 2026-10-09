package observe

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/metrics"
)

// The GatewayObserver records EVERY call in the metrics registry but only LOGS the notable ones — any
// non-ok outcome, or a success slower than the threshold — so the log stays signal-dense while the metrics
// remain complete. The healthy fast path is recorded-but-silent.
func TestGatewayObserverRecordsAllLogsNotable(t *testing.T) {
	r := NewRegistry()
	var logs []string
	obs := NewGatewayObserver(r, 30*time.Second, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })

	obs.ObserveCall("primary", "test-caller", "ok", 200, 2.0, "")           // healthy fast → recorded, NOT logged
	obs.ObserveCall("primary", "test-caller", "ok", 200, 55.0, "")          // slow success → recorded AND logged
	obs.ObserveCall("primary", "test-caller", "timeout", 0, 120.0, "ctx")   // failure → recorded AND logged
	obs.ObserveCall("fast", "test-caller", "rate_limit", 429, 0.3, "slow down")

	// metrics: all four recorded (two primary/ok, one primary/timeout, one fast/rate_limit)
	out := metrics.Render(r.Collect())
	for _, want := range []string{
		`tg_model_calls_total{model="primary",outcome="ok"} 2`,
		`tg_model_calls_total{model="primary",outcome="timeout"} 1`,
		`tg_model_calls_total{model="fast",outcome="rate_limit"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q; got:\n%s", want, out)
		}
	}

	// logs: exactly the three notable calls (the healthy 2s ok is silent)
	if len(logs) != 3 {
		t.Fatalf("want 3 notable log lines, got %d: %v", len(logs), logs)
	}
	joined := strings.Join(logs, "\n")
	for _, want := range []string{"outcome=timeout", "outcome=rate_limit", "outcome=ok"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("logs missing %q; got:\n%s", want, joined)
		}
	}
	for _, line := range logs {
		if strings.Contains(line, "outcome=ok") && !strings.Contains(line, "55.0") {
			t.Fatalf("the only logged ok must be the slow one; got %q", line)
		}
	}
}

// A nil registry still logs notable calls (metrics record is a no-op) — never panics.
func TestGatewayObserverNilRegistrySafe(t *testing.T) {
	var logs []string
	obs := NewGatewayObserver(nil, 0, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	obs.ObserveCall("primary", "test-caller", "provider_error", 503, 1.0, "upstream down")
	if len(logs) != 1 {
		t.Fatalf("want 1 log line with a nil registry, got %d", len(logs))
	}
}
