package axis

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// TG-180: coverage of the unmeasured is a scorecard dimension with three honest states — no snapshot (a
// named gap, no numbers), snapshot without an armed probe (denominator real, numerator "not tested" as a
// rule-of-three bound), and a measured ratio. KILLING MUTATION: render confirmed/unobservable as 0% when
// confirmed==0 — the bound assert and the "0%" absence assert both fail.
func TestTG180CoverageOfTheUnmeasuredIsReportedHonestly(t *testing.T) {
	base := db.AxisAgg{Since: time.Unix(0, 0).UTC(), Total: 10, Judged: 10, Bands: map[string]int{"POLL_PAUSE": 10}}

	// 1. No snapshot ever recorded: a NAMED gap, nothing rendered as measured.
	sc := Score(base, time.Hour)
	if sc.CoverageMeasured {
		t.Fatal("without a snapshot the dimension must read unmeasured")
	}
	if !strings.Contains(sc.CoverageGap, "0106") {
		t.Fatalf("the missing snapshot must be G7's own named gap citing migration 0106, got %q", sc.CoverageGap)
	}
	for _, g := range sc.Unmeasurable {
		if g.Axis == "G7" {
			t.Fatalf("G7 must not enter the eight-axis Unmeasurable contract, got %+v", sc.Unmeasurable)
		}
	}
	if !strings.Contains(sc.Text(), "G7  coverage of the unmeasured: not measured") {
		t.Fatalf("the unmeasured state must render in G7's own block, got:\n%s", sc.Text())
	}

	// 2. Snapshot, probe OFF, nothing confirmed: denominator measured, numerator bounded — never "0%".
	base.Coverage = &db.ObservationCoverage{Total: 40, Observed: 30, HealthyQuiet: 4, Unobservable: 6, Confirmed: 0, ProbeArmed: false}
	sc = Score(base, time.Hour)
	if !sc.CoverageMeasured || sc.CoverageUnobservable != 6 || sc.CoverageOfUnmeasured != 0 {
		t.Fatalf("snapshot not carried: %+v", sc)
	}
	if !strings.Contains(sc.CoverageBound, "rule of three") {
		t.Fatalf("a zero numerator over 6 must render the rule-of-three bound, got %q", sc.CoverageBound)
	}
	txt := sc.Text()
	if !strings.Contains(txt, "6 live entities produced no triageable signal") || !strings.Contains(txt, "probe-confirmed: 0 of 6") {
		t.Fatalf("text must state the denominator and the untested numerator, got:\n%s", txt)
	}
	if strings.Contains(txt, "= 0%") {
		t.Fatalf("an untested numerator must not render as 0%% coverage:\n%s", txt)
	}
	if !strings.Contains(sc.CoverageGap, "ARMED") || !strings.Contains(txt, "numerator not measured") {
		t.Fatalf("an unarmed probe must be named as the numerator gap in G7's own field and block, got gap=%q text:\n%s", sc.CoverageGap, txt)
	}

	// 3. Armed and partially confirmed: a real ratio.
	base.Coverage = &db.ObservationCoverage{Total: 40, Observed: 30, HealthyQuiet: 4, Unobservable: 6, Confirmed: 3, ProbeArmed: true}
	sc = Score(base, time.Hour)
	if sc.CoverageOfUnmeasured != 0.5 || sc.CoverageBound != "" {
		t.Fatalf("3 of 6 must be 0.5 with no bound, got %+v", sc)
	}
	if !strings.Contains(sc.Text(), "probe-confirmed: 3 of 6 = 50%") {
		t.Fatalf("text must render the measured ratio, got:\n%s", sc.Text())
	}
}
