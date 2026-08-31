package perf

// TG-80 P1#2 — the FIRST latency/throughput measurement of TG's load-bearing safety decision.
//
// TG shipped 2600+ test files with ZERO latency/throughput lines, while the h-apache-stack peer leads its
// head-to-head narrative with "988 runs, 0 failures" over a 1/4/8/16/32 concurrency sweep. This closes that
// gap for the one component where the comparison is honest and reproducible: the policy GATE.
//
// WHY THE GATE IS THE RIGHT TARGET. TG's gate is 100% deterministic — a compiled Rego deny-overrides base +
// the fixed composition stages (confidence clamp, band, never-auto floor, mode). No LLM, no DB, no network is
// on Decide's path, so its latency is a PROPERTY OF THE CODE, not of a model's mood or a substrate's I/O — it
// reproduces run to run and machine to machine (modulo clock), which is exactly what a benchmark needs and
// what an LLM-in-the-loop e2e number can never be. It is also TG's analog of the peer's dual-gate.
//
// WHY IT LIVES IN perf/ (not core/policy/). core/policy is a protected decision-core path — a change there is
// a law-surface change requiring an owner trailer. A benchmark is a BLACK-BOX consumer of the gate's public
// API (policy.NewEngine / policy.Engine.Decide), never part of its law, so it belongs outside the protected
// core. That also keeps a perf change from ever being mistaken for a governed-behavior change.
//
// WHAT IS ASSERTED vs REPORTED. The 0-error and determinism checks are GATES (a valid input must never error,
// and a fixed input must yield one stable verdict — else a "throughput" is meaningless and the gate is unsafe
// under concurrency). The percentiles are REPORTED via t.Logf, never asserted: wall-clock latency is
// machine-dependent, so ratcheting it would be a flaky gate that measures the CI runner, not the code. Run
// `TG_GATE_BENCH=1 go test -run TestGateThroughput -v ./perf` for a full-scale sweep; `go test -race`
// exercises the concurrency-safety claim.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/credential"
	"github.com/territory-grounder/grounder/core/policy"
	"github.com/territory-grounder/grounder/core/safety"
)

// gateBenchEngine builds a representative gate: a mix of host / host-glob / group / device-class rules with
// deny/approve/auto verdicts, at a realistic deployment scale. Built ONCE (the Rego compile is setup); Decide
// is the measured hot path, exactly as production reuses one engine across every incident.
func gateBenchEngine(tb testing.TB, nRules int) *policy.Engine {
	tb.Helper()
	verdicts := []policy.Verdict{policy.VerdictAuto, policy.VerdictApprove, policy.VerdictDeny}
	kinds := []credential.SelectorKind{credential.KindHost, credential.KindHostGlob, credential.KindGroup, credential.KindDeviceClass}
	patterns := map[credential.SelectorKind][]string{
		credential.KindHost:        {"dc1demo-web01", "dc1demo-db01", "dc2demo-nas01", "dc1demo-fw01"},
		credential.KindHostGlob:    {"dc1demo-*", "dc2demo-*"},
		credential.KindGroup:       {"k8s-workers", "edge-firewalls", "storage"},
		credential.KindDeviceClass: {"cisco-asa", "proxmox-node", "synology-nas"},
	}
	rules := make([]policy.Rule, 0, nRules)
	for i := 0; i < nRules; i++ {
		k := kinds[i%len(kinds)]
		ps := patterns[k]
		sel := credential.Selector{Kind: k, Pattern: ps[i%len(ps)]}
		r, err := policy.NewRule(policy.Rule{ID: fmt.Sprintf("bench-%03d", i), Match: policy.Match{Selector: &sel}, Verdict: verdicts[i%len(verdicts)]})
		if err != nil {
			tb.Fatalf("NewRule %d (kind %s): %v", i, k, err)
		}
		rules = append(rules, r)
	}
	e, err := policy.NewEngine(context.Background(), policy.RuleSet{Rules: rules})
	if err != nil {
		tb.Fatalf("NewEngine(%d rules): %v", nRules, err)
	}
	return e
}

// gateBenchInputs is a representative incident mix: some match a rule, one falls through to the fail-closed
// default; varied host/group/device-class/band/mode/confidence so the measurement is a realistic PATH MIX,
// not one cached row. All hostnames are synthetic <site>demo-<name> (the console-fixture convention) — no
// estate hostname ships in the public-mirrored repo.
func gateBenchInputs() []policy.EvalInput {
	return []policy.EvalInput{
		{Host: "dc1demo-web01", OpClass: "restart-service", Confidence: 0.92, Band: safety.BandAuto, Mode: policy.ModeSemiAuto, Reversible: true},
		{Host: "dc1demo-db01", OpClass: "reload-service", Confidence: 0.71, Band: safety.BandAutoNotice, Mode: policy.ModeSemiAuto, Reversible: true},
		{Host: "dc2demo-nas01", OpClass: "prune-backups", Confidence: 0.55, Band: safety.BandPollPause, Mode: policy.ModeSemiAuto},
		{Host: "dc1demo-fw01", DeviceClass: "cisco-asa", OpClass: "deny-acl", Confidence: 0.60, Band: safety.BandPollPause, Mode: policy.ModeSemiAuto},
		{Host: "unmatched-host-zzz", OpClass: "start-guest", Confidence: 0.80, Band: safety.BandAuto, Mode: policy.ModeSemiAuto, Reversible: true},
		{Host: "dc1demo-k8sw3", Groups: []string{"k8s-workers"}, OpClass: "drain", Confidence: 0.88, Band: safety.BandAuto, Mode: policy.ModeFullAuto, Reversible: true},
	}
}

func gatePercentile(sortedAsc []time.Duration, q float64) time.Duration {
	if len(sortedAsc) == 0 {
		return 0
	}
	idx := int(q * float64(len(sortedAsc)))
	if idx >= len(sortedAsc) {
		idx = len(sortedAsc) - 1
	}
	return sortedAsc[idx]
}

// gateRunLevel runs `total` decisions across `conc` goroutines over a shared engine, returning every call's
// latency, the error count, and the wall-clock of the whole level (the basis for throughput). Latencies are
// collected per-goroutine and merged after the barrier, so no lock contention perturbs the measurement.
func gateRunLevel(ctx context.Context, e *policy.Engine, inputs []policy.EvalInput, conc, total int) (lat []time.Duration, errs int, wall time.Duration) {
	per := total / conc
	latByG := make([][]time.Duration, conc)
	errByG := make([]int, conc)
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			local := make([]time.Duration, 0, per)
			for i := 0; i < per; i++ {
				in := inputs[(g*per+i)%len(inputs)]
				t0 := time.Now()
				_, err := e.Decide(ctx, in)
				d := time.Since(t0)
				if err != nil {
					errByG[g]++
					continue
				}
				local = append(local, d)
			}
			latByG[g] = local
		}(g)
	}
	wg.Wait()
	wall = time.Since(start)
	for _, l := range latByG {
		lat = append(lat, l...)
	}
	for _, ec := range errByG {
		errs += ec
	}
	return lat, errs, wall
}

// TestGateThroughput measures the deterministic gate's decision latency + throughput under a concurrency
// sweep, gating on 0 errors + determinism and reporting the percentiles. See the file header for why the gate
// is the right, honest, reproducible target and why latency is reported not asserted.
func TestGateThroughput(t *testing.T) {
	const nRules = 24
	e := gateBenchEngine(t, nRules)
	inputs := gateBenchInputs()
	ctx := context.Background()

	// GATE 1 — determinism: a fixed input must yield one stable verdict, or a "throughput" is meaningless.
	first, err := e.Decide(ctx, inputs[0])
	if err != nil {
		t.Fatalf("Decide(inputs[0]): %v", err)
	}
	for i := 0; i < 500; i++ {
		d, err := e.Decide(ctx, inputs[0])
		if err != nil {
			t.Fatalf("Decide iteration %d: %v", i, err)
		}
		if d.Verdict() != first.Verdict() {
			t.Fatalf("gate is non-deterministic: verdict %q then %q for the same input", first.Verdict(), d.Verdict())
		}
	}

	perLevel := 2400
	if os.Getenv("TG_GATE_BENCH") != "" {
		perLevel = 60000
	}
	t.Logf("GATE THROUGHPUT — %d-rule Rego engine, %d decisions/level, deterministic (no LLM / DB / network on the path)", nRules, perLevel)
	t.Logf("  %-5s %-11s %-11s %-11s %-11s %-16s", "conc", "p50", "p95", "p99", "max", "throughput")
	for _, conc := range []int{1, 4, 8, 16, 32} {
		lat, errs, wall := gateRunLevel(ctx, e, inputs, conc, perLevel)
		// GATE 2 — a valid input must NEVER error, at any concurrency (also proves Decide is concurrency-safe).
		if errs > 0 {
			t.Fatalf("concurrency %d: %d/%d Decide errors — the gate must never error on a valid input", conc, errs, perLevel)
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		thru := float64(len(lat)) / wall.Seconds()
		t.Logf("  %-5d %-11s %-11s %-11s %-11s %-16s", conc,
			gatePercentile(lat, 0.50).Round(time.Nanosecond),
			gatePercentile(lat, 0.95).Round(time.Nanosecond),
			gatePercentile(lat, 0.99).Round(time.Nanosecond),
			lat[len(lat)-1].Round(time.Nanosecond),
			fmt.Sprintf("%.0f dec/s", thru))
	}
}

// BenchmarkGateDecide is the idiomatic ns/op companion (go test -bench=GateDecide ./perf): b.RunParallel
// hammers one shared engine from GOMAXPROCS goroutines, the way production concurrency actually looks.
func BenchmarkGateDecide(b *testing.B) {
	e := gateBenchEngine(b, 24)
	inputs := gateBenchInputs()
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := e.Decide(ctx, inputs[i%len(inputs)]); err != nil {
				b.Fatalf("Decide: %v", err)
			}
			i++
		}
	})
}
