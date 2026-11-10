package axis

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/core/db"
)

// TG-672 — the G4A bar beside the G4 bar. Same inputs, two readings: g4 gates on the duration, g4a on the
// instrument identity. Every expectation is read off the fixture (core/axis/benchmark_test.go's passingInputs).

const idA = "1111111111111111111111111111111111111111111111111111111111111111"

// shortWindow is a 1.25-day window with every cell on the bar and ONE recorded instrument identity.
func shortWindow() (db.WindowCensus, map[string]db.AxisAgg, db.AxisAgg) {
	c, per, est := passingInputs(14)
	c.Until = c.Since.Add(30 * 60 * 60 * 1e9)
	c.InstrumentIDs = map[string]int{idA: 120}
	c.EstateSnapshots = 7
	c.SyntheticBySite = map[string]int{"dc1": 50, "dc2": 48}
	c.OrganicBySite = map[string]int{"dc1": 10, "dc2": 12}
	c.MutatedByHostBySite = map[string]map[string]int{"dc1": {"guest-a": 20, "guest-b": 10}, "dc2": {"guest-c": 15, "guest-d": 15}}
	c.FaultClassesBySite = map[string]map[string]int{"dc1": {"device-down": 10, "service-down": 20}, "dc2": {"device-down": 30}}
	for site, a := range per {
		a.FirstIncident, a.LastIncident = c.Since, c.Until.Add(-1)
		per[site] = a
	}
	return c, per, est
}

func failuresText(r BenchmarkReport) string { return strings.Join(r.Failures, "\n") }

// The regression the plan demands: g4 STILL fails a short window; g4a does not fail it, and passes.
func TestBar_G4StillNeedsFourteenDaysAndG4ADoesNot(t *testing.T) {
	c, per, est := shortWindow()
	g4 := BenchmarkBar(BarG4, c, "r1", per, est)
	if g4.Passed() || !strings.Contains(failuresText(g4), "window is 1.2 days") || !strings.Contains(failuresText(g4), "data spans") {
		t.Fatalf("g4 on a 1.25-day window must FAIL on the window and the span: %s %v", g4.Verdict, g4.Failures)
	}
	if g4.Bar != "g4" || strings.Contains(failuresText(g4), "instrument identity") {
		t.Fatalf("g4 reports the instrument identity but does not gate on it: bar=%q failures=%v", g4.Bar, g4.Failures)
	}
	g4a := BenchmarkBar(BarG4A, c, "r1", per, est)
	if !g4a.Passed() || len(g4a.Failures) != 0 || g4a.Bar != "g4a" {
		t.Fatalf("g4a on the same window with one identity must PASS: %s %v", g4a.Verdict, g4a.Failures)
	}
	if g4a.WindowOK || g4a.WindowDays < 1.2 || g4a.DataSpanDays["dc1"] < 1.2 {
		t.Fatalf("g4a still REPORTS the window and the spans (window_ok=%v days=%.2f span=%v)", g4a.WindowOK, g4a.WindowDays, g4a.DataSpanDays)
	}
	if !g4a.InstrumentOK || len(g4a.InstrumentIDs) != 1 || g4a.InstrumentIDs[0].Value != idA || g4a.LedgerGaps != 0 {
		t.Fatalf("instrument identity / ledger gaps: %+v %d", g4a.InstrumentIDs, g4a.LedgerGaps)
	}
	// The two bars read the same cells: only the gating differs.
	for i, cg := range g4.Cells {
		if ca := g4a.Cells[i]; ca.Axis != cg.Axis || ca.Scope != cg.Scope || ca.Status != cg.Status || ca.Display != cg.Display {
			t.Fatalf("the two bars read the same cells differently: g4=%+v g4a=%+v", cg, ca)
		}
	}
}

// Two recorded identities, or one unrecorded one, fail g4a by name; an unrecorded identity is never "compatible".
func TestBar_G4ANeedsExactlyOneRecordedInstrumentIdentity(t *testing.T) {
	c, per, est := shortWindow()
	c.InstrumentIDs = map[string]int{idA: 100, strings.Repeat("2", 64): 20}
	if r := BenchmarkBar(BarG4A, c, "r1", per, est); r.Passed() || !strings.Contains(failuresText(r), "instrument identity: 2 value(s)") {
		t.Fatalf("two identities must fail g4a and be named: %s %v", r.Verdict, r.Failures)
	}
	c.InstrumentIDs = map[string]int{"": 120}
	if r := BenchmarkBar(BarG4A, c, "r1", per, est); r.Passed() || !strings.Contains(failuresText(r), "(unrecorded)") {
		t.Fatalf("an unrecorded identity must fail g4a: %s %v", r.Verdict, r.Failures)
	}
	c.InstrumentIDs = map[string]int{}
	if r := BenchmarkBar(BarG4A, c, "r1", per, est); r.Passed() || !strings.Contains(failuresText(r), "no value at all") {
		t.Fatalf("no identity at all must fail g4a: %s %v", r.Verdict, r.Failures)
	}
	// A database from before migration 0132: unrecorded, failing, and the report names the absent column.
	c.InstrumentIDs, c.InstrumentColumnAbsent = map[string]int{"": 120}, true
	if r := BenchmarkBar(BarG4A, c, "r1", per, est); r.Passed() || !strings.Contains(failuresText(r), "no session_triage.instrument_id column") || !strings.Contains(r.Text(), "does not exist in this database") {
		t.Fatalf("an absent column must be named, never read as compatible: %s %v", r.Verdict, r.Failures)
	}
	// MUTATION CONTROL: a ledger gap fails g4a exactly as it fails g4 — the duration waiver waives nothing else.
	c, per, est = shortWindow()
	est.LedgerBreaches = 1
	if r := BenchmarkBar(BarG4A, c, "r1", per, est); r.Passed() || r.LedgerGaps != 1 {
		t.Fatalf("a seq gap must fail g4a: %s %v gaps=%d", r.Verdict, r.Failures, r.LedgerGaps)
	}
	c, per, est = shortWindow()
	c.HarnessFPs = map[string]int{fpA: 100, strings.Repeat("3", 64): 20}
	if r := BenchmarkBar(BarG4A, c, "r1", per, est); r.Passed() {
		t.Fatal("two harness fingerprints must still fail g4a")
	}
}

// Intervals ride beside the raw counts, labelled, never gating: 0/2 reads 77.6% (not the rule-of-three 150%),
// 27/30 reads a Wilson interval, and a cell without a sample has none.
func TestBar_IntervalsAreDescriptiveAndNeverAbove100Percent(t *testing.T) {
	c, per, est := shortWindow()
	a := per["dc1"]
	a.MutatedCount, a.HealConfirmedCount = 2, 2
	per["dc1"] = a
	r := BenchmarkBar(BarG4A, c, "r1", per, est)
	a7 := cell(t, r, "dc1", "A7", "false-actuation rate")
	if a7.Interval == nil || a7.Interval.Method != MethodExactOneSided || a7.Interval.Upper > 0.777 || a7.Interval.Upper < 0.776 {
		t.Fatalf("A7 0/2 interval = %+v, want the exact one-sided bound 77.6%%", a7.Interval)
	}
	if !strings.Contains(a7.Note, "77.6%") || strings.Contains(a7.Note, "150") {
		t.Fatalf("A7 note = %q", a7.Note)
	}
	a1 := cell(t, r, "dc1", "A1", "detection recall")
	if a1.Interval == nil || a1.Interval.Method != MethodWilson || a1.Interval.Lower < 0.74 || a1.Interval.Upper > 0.97 {
		t.Fatalf("A1 27/30 interval = %+v, want Wilson ≈ [74.4%%, 96.5%%]", a1.Interval)
	}
	if a1.Status != StatusPass {
		t.Fatal("an interval must not change a cell's gating status")
	}
	// The interval is on the cell for n=2 (INSUFFICIENT-N) too: descriptive, independent of the gate.
	if a7.Status != StatusInsufficientN {
		t.Fatalf("A7 n=2 must still be insufficient: %s", a7.Status)
	}
	// No sample, no interval.
	a = per["dc2"]
	a.InjectedFaults, a.DetectedFaults = 0, 0
	per["dc2"] = a
	r = BenchmarkBar(BarG4A, c, "r1", per, est)
	if cell(t, r, "dc2", "A1", "detection recall").Interval != nil {
		t.Fatal("an empty sample grew an interval")
	}
	txt := r.Text()
	for _, want := range []string{"G4A bar (accelerated)", "NOT conditions", "wilson-95-two-sided", "model-dependent", "estate topology snapshots published in the window: 7",
		"dc1=50/10", "dc1=20/30 (guest-a)", "incidents synthetic/organic:", "largest single-host share of mutated incidents:", "device-down=10 service-down=20", "uncertainty: provider model VERSIONS"} {
		if !strings.Contains(txt, want) {
			t.Errorf("g4a text lacks %q:\n%s", want, txt)
		}
	}
}

// The JSON carries what tools/g4avalidate reads: bar, verdict, until, sites, the three identity lists, ledger_gaps.
func TestBar_JSONCarriesTheValidatorsFields(t *testing.T) {
	c, per, est := shortWindow()
	b, err := json.Marshal(BenchmarkBar(BarG4A, c, "r1", per, est))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bar", "verdict", "since", "until", "sites", "rubric_versions", "harness_fps", "instrument_ids", "ledger_gaps", "failures",
		"estate_snapshots_in_window", "synthetic_incidents_by_site", "max_host_share_of_mutated_incidents", "injected_fault_classes_by_site", "uncertainties"} {
		if _, ok := m[k]; !ok {
			t.Errorf("json lacks %q", k)
		}
	}
	if m["bar"] != "g4a" || m["ledger_gaps"] != float64(0) {
		t.Fatalf("bar=%v ledger_gaps=%v", m["bar"], m["ledger_gaps"])
	}
}

func TestParseBar(t *testing.T) {
	for in, want := range map[string]Bar{"g4": BarG4, "G4A": BarG4A, " g4a ": BarG4A} {
		if got, err := ParseBar(in); err != nil || got != want {
			t.Errorf("ParseBar(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "g5", "bogus", "g4-a"} {
		if _, err := ParseBar(in); err == nil {
			t.Errorf("ParseBar(%q) accepted", in)
		}
	}
}
