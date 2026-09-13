package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// TG-190a — the live-DB-leak tripwire, CONSTITUTION 4.9: "synthetic canaries against an isolated
// throwaway DB (live-DB-leak counter must stay 0)".
//
// A gauge whose HEALTHY value is zero is the hardest kind to get right, because zero is also what a
// broken query, an unwired store and an empty table produce. These oracles exist to make those four
// readings distinguishable — that is the whole design, not a defensive extra.

func leakSample(t *testing.T, s []metrics.Sample, name string) metrics.Sample {
	t.Helper()
	for _, x := range s {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("no sample %q in %v", name, leakSampleNames(s))
	return metrics.Sample{}
}

func leakSampleNames(s []metrics.Sample) []string {
	out := make([]string, 0, len(s))
	for _, x := range s {
		out = append(out, x.Name)
	}
	return out
}

type stubLeakReader struct {
	leak db.SyntheticLeak
	err  error
}

func (s stubLeakReader) LeakCount(context.Context) (db.SyntheticLeak, error) {
	return s.leak, s.err
}

// The clean case, and the reason the denominator is published at all.
func TestACleanDatabasePublishesItsDenominator(t *testing.T) {
	got := collectSyntheticLeak(context.Background(), stubLeakReader{leak: db.SyntheticLeak{Leaked: 0, Total: 3383}})

	if v := leakSample(t, got, "tg_synthetic_rows_live").Value; v != 0 {
		t.Errorf("a clean database must read 0 leaked, got %v", v)
	}
	if v := leakSample(t, got, "tg_synthetic_scan_population").Value; v != 3383 {
		t.Errorf("the denominator must be published — '0 of 3383' is evidence, '0' alone is not: got %v", v)
	}
	if v := leakSample(t, got, "tg_synthetic_scan_ok").Value; v != 1 {
		t.Errorf("a successful scan must say so, got %v", v)
	}
}

// THE FOUR ZEROS. Each of these produces tg_synthetic_rows_live == 0, and only one of them is a safety
// statement. If this test ever collapses, the gauge is back to being unreadable.
func TestTheFourWaysToReadZeroAreDistinguishable(t *testing.T) {
	cases := []struct {
		name            string
		samples         []metrics.Sample
		wantOK, wantPop float64
		safe            bool
	}{
		{"clean and measured", collectSyntheticLeak(context.Background(),
			stubLeakReader{leak: db.SyntheticLeak{Leaked: 0, Total: 3383}}), 1, 3383, true},
		{"no store wired", collectSyntheticLeak(context.Background(), nil), 0, 0, false},
		{"read failed", collectSyntheticLeak(context.Background(),
			stubLeakReader{err: errors.New("no column \"synthetic\"")}), 0, 0, false},
		{"empty table", collectSyntheticLeak(context.Background(),
			stubLeakReader{leak: db.SyntheticLeak{Leaked: 0, Total: 0}}), 1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := leakSample(t, tc.samples, "tg_synthetic_rows_live").Value; v != 0 {
				t.Fatalf("precondition: every case here reads 0 leaked, got %v", v)
			}
			if v := leakSample(t, tc.samples, "tg_synthetic_scan_ok").Value; v != tc.wantOK {
				t.Errorf("scan_ok = %v, want %v — this is the series that separates a measured zero from "+
					"an unmeasured one", v, tc.wantOK)
			}
			if v := leakSample(t, tc.samples, "tg_synthetic_scan_population").Value; v != tc.wantPop {
				t.Errorf("population = %v, want %v", v, tc.wantPop)
			}
			// The composed judgement an alert would make.
			ok := leakSample(t, tc.samples, "tg_synthetic_scan_ok").Value == 1
			pop := leakSample(t, tc.samples, "tg_synthetic_scan_population").Value > 0
			if got := ok && pop; got != tc.safe {
				t.Errorf("an alert reading these three series concludes safe=%v, want %v", got, tc.safe)
			}
		})
	}
}

// A LEAK MUST BE LOUD. The failure this whole register exists for.
func TestALeakedCanaryRowIsCounted(t *testing.T) {
	got := collectSyntheticLeak(context.Background(),
		stubLeakReader{leak: db.SyntheticLeak{Leaked: 2, Total: 3385}})

	if v := leakSample(t, got, "tg_synthetic_rows_live").Value; v != 2 {
		t.Fatalf("two leaked canary rows must be counted, got %v — a canary that escaped its throwaway "+
			"database is now in the corpus the judge scores and the flywheel learns from", v)
	}
	if v := leakSample(t, got, "tg_synthetic_scan_ok").Value; v != 1 {
		t.Errorf("the scan succeeded, so scan_ok must be 1 even though the result is bad, got %v", v)
	}
}

// The register must be emitted UNCONDITIONALLY. A register that goes silent when its store is missing
// publishes exactly what a clean database publishes: nothing.
func TestTheRegisterEmitsEvenWithNoStore(t *testing.T) {
	got := collectSyntheticLeak(context.Background(), nil)
	if len(got) != 3 {
		t.Fatalf("want all three series even with no store, got %v", leakSampleNames(got))
	}
	for _, n := range []string{"tg_synthetic_rows_live", "tg_synthetic_scan_population", "tg_synthetic_scan_ok"} {
		leakSample(t, got, n) // fatals if absent
	}
}

// Every series must carry the warning that reading the leak alone is unsafe. The help text IS the
// interface here: whoever writes the alert reads it and nothing else.
func TestTheHelpTextRefusesToBeReadAlone(t *testing.T) {
	got := collectSyntheticLeak(context.Background(), stubLeakReader{leak: db.SyntheticLeak{Total: 1}})
	leak := leakSample(t, got, "tg_synthetic_rows_live")
	if !strings.Contains(leak.Help, "tg_synthetic_scan_ok") {
		t.Errorf("the leak gauge's help must point at the series that qualifies it, or an alert will be "+
			"written against a zero that an unwired store also produces: %q", leak.Help)
	}
	if !strings.Contains(strings.ToLower(leak.Help), "0") {
		t.Errorf("the leak gauge's help must state that 0 is the required value: %q", leak.Help)
	}
}

// THE COMPOSITION-ROOT GUARD, and the reason it is not optional here. This ticket exists because a
// consumer (judge_liveness) reserved a Synthetic flag that NOTHING produced. Shipping a register that is
// defined and never chained would reproduce that exact defect one layer up — a tripwire present in the
// tree and absent from the process.
func TestTheLeakRegisterIsChainedAtTheCompositionRoot(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), "withSyntheticLeak(") {
		t.Fatal("cmd/worker/main.go never calls withSyntheticLeak — the live-DB-leak tripwire is defined " +
			"and unwired, which is the same defect shape as the Synthetic flag this ticket was filed on")
	}
	if !strings.Contains(string(src), "collectSyntheticLeak(") {
		t.Fatal("main.go chains the register without calling collectSyntheticLeak — it would publish nothing")
	}
}
