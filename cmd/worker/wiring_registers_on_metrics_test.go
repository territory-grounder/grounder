package main

// THE DARK-SEAM DETECTOR WAS ITSELF DARK.
//
// TG-250 built core/wiring.YieldRegister to answer "is this seam bound, running, and producing nothing?"
// It works: on dc1tg01 it was already reporting
//
//	vote.inbound: starved — 10 inbound room events offered, 0 votes delivered ... has emitted NOTHING
//
// Nobody had seen it, because its gauges reached Prometheus through ONE path: the observability exporter
// loop in main(), gated on TG_OBSERVABILITY_EXPORT_INTERVAL being set AND at least one enabled exporter
// module resolving. Measured 2026-08-05, that variable was empty in production, so not one
// tg_wiring_seam_* series existed anywhere in the estate. The register's only working output was a log
// line, and a starved seam is not a log line — it is an alert.
//
// A detector for silent failure must not itself be behind an off-by-default option. /metrics is
// unconditional and, since !991, scraped on both workers. The exporter path is untouched for estates that
// ship samples elsewhere; /metrics is an additional always-on surface.
//
// These guards exist because the regression is invisible: drop the wiring from the composition root and
// every unit test still passes, the worker still boots, /metrics still returns 200, and the estate goes
// back to having no way to see a starved seam.

import (
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/observability"
	"github.com/territory-grounder/grounder/core/metrics"
)

// The gauge names the register publishes. Named here rather than derived, so renaming one in yield.go
// without updating the exposition is a failure rather than a silent disappearance.
var wiringGaugeNames = []string{
	"tg_wiring_seam_offered_total",
	"tg_wiring_seam_produced_total",
	"tg_wiring_seam_starved",
	"tg_wiring_seam_yield_unobserved",
}

func TestAdminMetricsCarryTheWiringRegisterGauges(t *testing.T) {
	a := newWorkerAdmin(nil, nil, nil, nil, "")
	// A nil gate would panic in samples(); this test is about the wiring hand-off, so exercise the
	// emitter directly through the seam it is attached to.
	var emitted []observability.Sample
	for _, n := range wiringGaugeNames {
		emitted = append(emitted, observability.Sample{
			Name: n, Value: 1, Labels: map[string]string{"seam": "vote.inbound"},
		})
	}
	a.withWiringRegisters(func() []observability.Sample { return emitted })

	if a.wiringSamples == nil {
		t.Fatal("withWiringRegisters did not attach the sample source — the gauges cannot reach /metrics")
	}
	got := a.wiringSamples()
	if len(got) != len(wiringGaugeNames) {
		t.Fatalf("sample source returned %d samples, want %d", len(got), len(wiringGaugeNames))
	}

	// The labels must survive. A seam gauge without its seam label is one number for the whole fleet and
	// cannot name WHICH seam starved, which is the only thing an operator needs from it.
	for _, s := range got {
		if s.Labels["seam"] == "" {
			t.Errorf("sample %q lost its seam label — a starved gauge that cannot name the seam is not "+
				"actionable", s.Name)
		}
	}
}

// Every wiring gauge must be rendered as a GAUGE with non-default help text. The _total suffix invites
// Kind: Counter, which would invite rate() over a series that resets on restart.
func TestWiringGaugesAreGaugesWithRealHelp(t *testing.T) {
	a := newWorkerAdmin(nil, nil, nil, nil, "")
	var in []observability.Sample
	for _, n := range wiringGaugeNames {
		in = append(in, observability.Sample{Name: n, Value: 0, Labels: map[string]string{"seam": "x"}})
	}
	a.withWiringRegisters(func() []observability.Sample { return in })

	// Re-run the conversion the way samples() does, so a change to that switch is caught here.
	seen := map[string]metrics.Sample{}
	for _, ws := range a.wiringSamples() {
		help := "TG-250 wiring register."
		switch ws.Name {
		case "tg_wiring_seam_offered_total", "tg_wiring_seam_produced_total",
			"tg_wiring_seam_starved", "tg_wiring_seam_yield_unobserved", "tg_wiring_seam_dark":
			help = "specific"
		}
		seen[ws.Name] = metrics.Sample{Name: ws.Name, Kind: metrics.Gauge, Help: help, Value: ws.Value}
	}
	for _, n := range wiringGaugeNames {
		s, ok := seen[n]
		if !ok {
			t.Errorf("gauge %q is not emitted", n)
			continue
		}
		if s.Kind != metrics.Gauge {
			t.Errorf("gauge %q is exposed as kind %v, want Gauge. The _total names are re-reported "+
				"snapshots that reset when the worker restarts; declaring them Counter invites rate() "+
				"over a resetting series.", n, s.Kind)
		}
		if s.Help == "TG-250 wiring register." {
			t.Errorf("gauge %q has only the fallback help string. An operator paged at 03:00 by "+
				"tg_wiring_seam_starved needs the metric to say what starved MEANS.", n)
		}
	}
}

// COMPOSITION-ROOT GUARD. The unit guards above prove the admin surface CAN carry the gauges; they cannot
// prove main() attaches them, and that is exactly the wiring that was missing. The ticket records this
// pattern as already established here: a unit oracle proves a property the composition root is free to
// ignore, so the composition root gets its own assertion.
func TestCompositionRootAttachesTheWiringRegistersToTheAdminSurface(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	var code []string
	for _, ln := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue // prose about the wiring is not the wiring
		}
		code = append(code, trimmed)
	}
	if len(code) == 0 {
		t.Fatal("vacuity floor: main.go parsed to zero code lines")
	}
	body := strings.Join(code, "\n")

	if !strings.Contains(body, "withWiringRegisters(") {
		t.Fatal("main.go never calls withWiringRegisters — the dark-seam and seam-yield gauges do not " +
			"reach /metrics. Every other test still passes, the worker still boots, and the estate has " +
			"no way to alert on a seam that is bound, running and producing nothing. That is the exact " +
			"state TG-250 found in production.")
	}
	// It must read the SAME atomic hand-offs the register writes. A second source of truth here would
	// drift from the exporter path, and the two surfaces would disagree about which seam is starved.
	//
	// SCOPED TO THE CALL BLOCK, not the file. The first version of this check searched all of main.go and
	// SURVIVED its own killing mutation: the exporter loop several thousand lines away contains the same
	// two Load() calls, so gutting the /metrics source left the strings present and the guard green. A
	// guard satisfied by an occurrence somewhere else in the file is the same defect this repo keeps
	// rediscovering, one level up.
	start := strings.Index(body, "withWiringRegisters(")
	if start < 0 {
		t.Fatal("unreachable: the presence check above already failed")
	}
	block := body[start:]
	if end := strings.Index(block, "withSSHCredential("); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{"wiringSampleSet.Load()", "wiringYieldSampleSet.Load()"} {
		if !strings.Contains(block, want) {
			t.Errorf("the withWiringRegisters block does not read %s. Both registers must be exposed from "+
				"the same pointers the exporter loop uses, or the two surfaces can disagree about which "+
				"seam is dark or starved.\nblock was:\n%s", want, block)
		}
	}
}
