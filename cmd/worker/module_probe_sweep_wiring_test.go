package main

import (
	"os"
	"strings"
	"testing"
)

// Composition-root guard (TG-501): pin that wireModuleProbeSweep still wires the scheduled sweep — the
// emitting-probe exclusion sourced from the catalog descriptors, its own notifier fanout (deliberately
// distinct from the governed seam's), and the immediate-then-ticked run loop — so the god-file carve that
// extracted it from main() cannot silently drop a piece. It returns nothing observable from outside the
// package (a fire-and-forget background loop gated on an interval), so — the same reasoning
// worker_wiring_inventory_test.go and worker_model_budget_test.go rely on — the guard reads the source as
// text and asserts the wiring, rather than exercising a live sweep.
func TestWireModuleProbeSweepWiresTheScheduledSweep(t *testing.T) {
	src, err := os.ReadFile("module_probe_sweep_wiring.go")
	if err != nil {
		t.Fatalf("read module_probe_sweep_wiring.go: %v", err)
	}
	s := string(src)
	for _, want := range []string{
		`getenv("TG_MODULE_PROBE_INTERVAL", "")`,
		`if described, cerr := catalog.All(); cerr == nil {`,
		`sweepSink := notifier.NewFanout(notifierSinks...)`,
		`sweep := newProbeSweep(moduleProbers, emitting, sweepNotify, log.Printf, 30*time.Second)`,
		`sweep.run(context.Background())`,
		`t := time.NewTicker(d)`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wireModuleProbeSweep no longer wires %q — a probe-sweep piece was dropped in the carve", want)
		}
	}
}

// main() must actually CALL the extracted constructor — a carve that leaves it uncalled is a dark seam
// (present in the tree, absent from the process; the same class TG-315's authlog collector shipped as).
func TestMainCallsWireModuleProbeSweep(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainSrc), "wireModuleProbeSweep(moduleProbers, notifierSinks)") {
		t.Error("main.go no longer calls wireModuleProbeSweep(moduleProbers, notifierSinks) — the extracted module-probe-sweep wiring is unreferenced")
	}
}
