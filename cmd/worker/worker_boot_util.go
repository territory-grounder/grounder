package main

// Small boot-time UTILITIES carved out of main() (TG-501 LOC-debt paydown): logConfigWriteLatency logs the
// per-step + total latency of a config-write operation (observability only, no control-flow effect), and
// boolCount maps a bool to 0/1 for the wiring-yield produced/admitted counters. Pure relocation — the
// wiringYield.Observe(...) call sites (with their boolCount(...) arguments) stay in main() where the
// yield-observation guards read them literally. Behaviour is unchanged by the move.

import (
	_ "time/tzdata" // embed the IANA zoneinfo DB so time.LoadLocation resolves on distroless (no OS tzdata)

	"fmt"
	"log"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/temporal/configwrite"
)

// logConfigWriteLatency renders one governed config/secret write's per-step timing (TG-277).
//
// The line always names EVERY step and its cost, including on failure, because the question this has to
// answer is "which of the three steps took the time?" — the question nobody could answer on 2026-08-04
// when a sealed-secret write hit its 15s StartToCloseTimeout. The measured baseline is on the line so a
// reader can tell a normal write from a stalled one without going and looking it up: ~12ms total.
func logConfigWriteLatency(op string, steps []configwrite.StepLatency, total time.Duration, err error) {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, fmt.Sprintf("%s=%s", s.Step, s.Took.Round(time.Millisecond)))
	}
	outcome := "ok"
	if err != nil {
		outcome = "FAILED: " + err.Error()
	}
	log.Printf("configwrite %s: total=%s [%s] %s (baseline ~12ms measured live 2026-08-04; seconds here are the substrate, not the chain append — TG-277)",
		op, total.Round(time.Millisecond), strings.Join(parts, " "), outcome)
}

// boolCount renders a success flag as the yield register's PRODUCED count: 1 when the effect actually
// happened, 0 when it did not. It exists so a call site cannot accidentally count an ATTEMPT as an
// outcome, which would report a healthy yield straight through a total outage.
func boolCount(ok bool) int {
	if ok {
		return 1
	}
	return 0
}
