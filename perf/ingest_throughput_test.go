package perf

// TG-80 P1#2 (companion to the gate benchmark) — the ingest FRONT-DOOR throughput.
//
// Every alert that reaches TG passes through ingest.Normalize (raw provider event → validated
// IncidentEnvelope): slug/severity/host/IP validation + canonicalization, no LLM / DB / network on the path,
// so like the gate it is deterministic and reproducible. This is the direct analog of the peer's "POST
// /v1/ingest" benchmark front — it answers "how many alerts/sec can TG's front door admit and normalize?"
//
// Placement + assertions mirror gate_throughput_test.go: this lives in perf/ (a black-box public-API
// consumer, not part of core/ingest's surface); it GATES on 0 errors on valid input + determinism (a fixed
// event yields one stable envelope key) and REPORTS the percentiles (latency is machine-dependent). All
// hostnames are synthetic <site>demo-<name> and all IPs are RFC5737 documentation ranges (192.0.2.0/24,
// 198.51.100.0/24, 203.0.113.0/24) — no estate host or IP ships in the public-mirrored repo.

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/ingest"
)

// ingestNow is a FIXED clock so the normalization (and its determinism check) is reproducible.
var ingestNow = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

// ingestBenchInputs is a representative provider mix (prometheus / librenms / crowdsec, varied severity), all
// with synthetic demo-hostnames and RFC5737 documentation IPs.
func ingestBenchInputs() []ingest.RawEvent {
	mk := func(src, ref, rule, sev, host, ip, summary string) ingest.RawEvent {
		r := ingest.NewRawEvent(src, nil)
		r.ExternalRef = ref
		r.AlertRule = rule
		r.Severity = sev
		r.Host = host
		r.IP = ip
		r.Site = "dc1"
		r.Summary = summary
		r.ObservedAt = ingestNow.Add(-30 * time.Second)
		return r
	}
	return []ingest.RawEvent{
		mk("prometheus-demo", "demo-4617", "MeshBFDSessionDown", "warning", "dc1demo-frr01", "192.0.2.3", "BFD session down"),
		mk("librenms-demo", "demo-22194", "DiskPressure", "critical", "dc1demo-web01", "192.0.2.10", "imagefs at 92%"),
		mk("crowdsec-demo", "demo-4471", "SSHBruteforce", "warning", "dc1demo-fw01", "198.51.100.5", "ssh bruteforce from 12 IPs"),
		mk("prometheus-demo", "demo-1180", "BackupJobLatency", "info", "dc2demo-nas01", "203.0.113.7", "volume1 usage 88%"),
		mk("librenms-demo", "demo-22160", "InterfaceFlap", "info", "dc2demo-pve02", "192.0.2.44", "eno2 flapped 4x in 5m"),
	}
}

// ingestRunLevel runs `total` normalizations across `conc` goroutines, returning latencies, the error count,
// and the level's wall-clock (the throughput basis). Per-goroutine collection, merged after the barrier.
func ingestRunLevel(events []ingest.RawEvent, conc, total int) (lat []time.Duration, errs int, wall time.Duration) {
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
				ev := events[(g*per+i)%len(events)]
				t0 := time.Now()
				_, err := ingest.Normalize(ev, ingestNow)
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

// TestIngestThroughput measures the ingest front-door's normalize latency + throughput under a concurrency
// sweep, gating on 0 errors + determinism and reporting the percentiles. See gate_throughput_test.go for the
// rationale on why latency is reported not asserted. TG_GATE_BENCH=1 scales the sweep.
func TestIngestThroughput(t *testing.T) {
	events := ingestBenchInputs()

	// GATE 1 — every representative event is VALID (Normalize accepts it) and DETERMINISTIC (a fixed event
	// yields one stable envelope external_ref).
	first, err := ingest.Normalize(events[0], ingestNow)
	if err != nil {
		t.Fatalf("Normalize(events[0]) rejected a representative event: %v", err)
	}
	for i := 0; i < 500; i++ {
		e, err := ingest.Normalize(events[0], ingestNow)
		if err != nil {
			t.Fatalf("Normalize iteration %d: %v", i, err)
		}
		if e.ExternalRef != first.ExternalRef {
			t.Fatalf("ingest is non-deterministic: external_ref %q then %q for the same event", first.ExternalRef, e.ExternalRef)
		}
	}

	perLevel := 3000
	if os.Getenv("TG_GATE_BENCH") != "" {
		perLevel = 80000
	}
	t.Logf("INGEST FRONT-DOOR THROUGHPUT — %d normalizations/level, deterministic (no LLM / DB / network on the path)", perLevel)
	t.Logf("  %-5s %-11s %-11s %-11s %-11s %-16s", "conc", "p50", "p95", "p99", "max", "throughput")
	for _, conc := range []int{1, 4, 8, 16, 32} {
		lat, errs, wall := ingestRunLevel(events, conc, perLevel)
		if errs > 0 {
			t.Fatalf("concurrency %d: %d/%d Normalize errors — the front door must never reject a valid event", conc, errs, perLevel)
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		thru := float64(len(lat)) / wall.Seconds()
		t.Logf("  %-5d %-11s %-11s %-11s %-11s %-16s", conc,
			gatePercentile(lat, 0.50).Round(time.Nanosecond),
			gatePercentile(lat, 0.95).Round(time.Nanosecond),
			gatePercentile(lat, 0.99).Round(time.Nanosecond),
			lat[len(lat)-1].Round(time.Nanosecond),
			fmt.Sprintf("%.0f ev/s", thru))
	}
}

// BenchmarkIngestNormalize is the idiomatic ns/op companion (go test -bench=IngestNormalize ./perf).
func BenchmarkIngestNormalize(b *testing.B) {
	events := ingestBenchInputs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := ingest.Normalize(events[i%len(events)], ingestNow); err != nil {
				b.Fatalf("Normalize: %v", err)
			}
			i++
		}
	})
}
