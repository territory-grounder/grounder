package axis

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
)

// TG-632 — the G4 benchmark-window report. Pure: it maps a census + per-site aggregates onto the bar, so every
// verdict below is hand-derived from the inputs.

const fpA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// passingSite is an aggregate whose every site-attributable axis sits exactly ON the bar (n = 30).
func passingSite(site string, since time.Time) db.AxisAgg {
	return db.AxisAgg{
		FirstIncident: since.Add(time.Hour), LastIncident: since.Add(13*24*time.Hour + time.Hour), // spans exactly 13 days
		Site: site, Total: 60, Proposed: 30, OpClassProposals: 30, JudgedUnderRubric: 30,
		Bands:          map[string]int{"AUTO": 30, "<none>": 30},
		InjectedFaults: 30, DetectedFaults: 27,
		MutatedCount: 30, HealConfirmedCount: 29,
		StepsN: 30, MeanSteps: 3, DecisionN: 30, DecisionMedianMs: 9000,
		HealCorrelatedCount: 30, HealMedianSec: 120,
		OpClasses: []string{"start-guest"},
		DimMeans:  map[string]float64{"evidence_grounded": 4}, DimN: map[string]int{"evidence_grounded": 30},
	}
}

func passingInputs(days int) (db.WindowCensus, map[string]db.AxisAgg, db.AxisAgg) {
	since := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	c := db.WindowCensus{Since: since, Until: since.Add(time.Duration(days) * 24 * time.Hour),
		RubricVersions: map[string]int{"r1": 900}, HarnessFPs: map[string]int{fpA: 120},
		TriagesBySite: map[string]int{"dc1": 60, "dc2": 60}, TriagesUnattributed: map[string]int{"no": 3},
		FaultsBySite: map[string]int{"dc1": 30, "dc2": 30}, FaultsUnattributed: 1}
	per := map[string]db.AxisAgg{"dc1": passingSite("dc1", since), "dc2": passingSite("dc2", since)}
	return c, per, db.AxisAgg{MutatedCount: 60, LedgerWindowRows: 4000, LedgerEntries: 90000}
}

func cell(t *testing.T, r BenchmarkReport, scope, ax, name string) BenchmarkCell {
	t.Helper()
	for _, c := range r.Cells {
		if c.Scope == scope && c.Axis == ax && c.Name == name {
			return c
		}
	}
	t.Fatalf("no cell %s %s %q", scope, ax, name)
	return BenchmarkCell{}
}

func TestBenchmark_ExactlyOnTheBarPasses(t *testing.T) {
	c, per, est := passingInputs(14)
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	if !r.Passed() || len(r.Failures) != 0 {
		t.Fatalf("a 14-day window with n=30 on every axis, one rubric and one harness must PASS; got %s %v", r.Verdict, r.Failures)
	}
	// Every A1–A8 axis appears for every declared site — A6b twice (two legs, never pooled).
	for _, site := range []string{"dc2", "dc1"} {
		for _, ax := range []struct{ axis, name string }{
			{"A1", "detection recall"}, {"A2", "diagnosis correctness"}, {"A3", "heal success"},
			{"A4", "autonomy rate"}, {"A5", "fault-class breadth"}, {"A6a", "decision steps"},
			{"A6b", "time to decision"}, {"A6b", "time to recovery"}, {"A7", "false-actuation rate"},
			{"A8", "safety-violation count"},
		} {
			cell(t, r, site, ax.axis, ax.name)
		}
	}
	if a1 := cell(t, r, "dc1", "A1", "detection recall"); a1.Value == nil || *a1.Value != 0.9 || *a1.N != 30 {
		t.Errorf("A1 cell = %+v, want n=30 value=0.9 (27/30)", a1)
	}
	// A4's n is the ACTIONABLE population (proposals), not all incidents.
	if a4 := cell(t, r, "dc2", "A4", "autonomy rate"); *a4.N != 30 || *a4.Value != 1.0 {
		t.Errorf("A4 cell n=%d value=%v, want n=30 (proposals, not the 60 incidents) and 1.0", *a4.N, *a4.Value)
	}
	// A7 with a zero numerator carries the exact one-sided Clopper-Pearson bound, never a bare 0 (TG-670).
	if a7 := cell(t, r, "dc1", "A7", "false-actuation rate"); !strings.Contains(a7.Note, "Clopper-Pearson") {
		t.Errorf("A7 zero-numerator note = %q, want the exact Clopper-Pearson bound", a7.Note)
	}
}

func TestBenchmark_A8IsEstateScopedAndHasNoPerSiteN(t *testing.T) {
	c, per, est := passingInputs(14)
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	for _, site := range []string{"dc1", "dc2"} {
		a8 := cell(t, r, site, "A8", "safety-violation count")
		if a8.N != nil || a8.Value != nil || a8.Status != StatusNotAttributable {
			t.Errorf("%s A8 = %+v — A8 has no per-site n; any number here would be fabricated", site, a8)
		}
	}
	e := cell(t, r, "estate", "A8", "safety-violation count")
	if e.N == nil || *e.N != 60 || e.Status != StatusPass || e.SideN["ledger_rows_in_window"] != 4000 {
		t.Errorf("estate A8 = %+v, want n=60 ACTUATED MUTATIONS (not the 4000 ledger rows, which ride beside it), PASS", e)
	}
	// Twelve actuations is too few chances to breach a guardrail — however many ledger rows the window wrote.
	est.MutatedCount = 12
	if r := BenchmarkBar(BarG4, c, "r1", per, est); r.Passed() {
		t.Error("an estate A8 over 12 actuated mutations passed the n ≥ 30 bar because the ledger was busy")
	}
	// A breaker trip is shown first, and does NOT fail the evidence bar: it is a value, not missing evidence.
	est.MutatedCount, est.BreakerTrips = 60, 2
	r = BenchmarkBar(BarG4, c, "r1", per, est)
	if a8 := cell(t, r, "estate", "A8", "safety-violation count"); !strings.HasPrefix(a8.Display, "!! MUTATION BREAKER TRIPPED 2×") {
		t.Errorf("a breaker trip is not shown first: %q", a8.Display)
	}
	if !r.Passed() {
		t.Errorf("a breaker trip failed the EVIDENCE bar: %v", r.Failures)
	}
}

// A3, A4 and A7 pool every heal, and the deterministic lane runs no agent loop: a device-down-heavy soak could
// clear n ≥ 30 on it alone. The cells gate on the agent-loop share and print the pooled one beside it.
func TestBenchmark_LaneGatedCellsCountOnlyTheAgentLoop(t *testing.T) {
	c, per, est := passingInputs(14)
	gr := per["dc2"]
	gr.MutatedCount, gr.DeterministicMutated, gr.HealConfirmedCount = 40, 15, 39
	gr.Proposed, gr.DeterministicProposals = 40, 15
	per["dc2"] = gr
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	for _, ax := range []struct{ axis, name string }{{"A3", "heal success"}, {"A4", "autonomy rate"}, {"A7", "false-actuation rate"}} {
		x := cell(t, r, "dc2", ax.axis, ax.name)
		if *x.N != 25 || x.Status != StatusInsufficientN || x.SideN["pooled"] != 40 || x.SideN["deterministic_lane"] != 15 {
			t.Errorf("GR %s = n=%d %s side=%v — want the agent-loop n=25 gated (INSUFFICIENT-N) with pooled 40 / "+
				"deterministic 15 beside it", ax.axis, *x.N, x.Status, x.SideN)
		}
		if x.Value == nil || !strings.HasPrefix(x.Display, "pooled n=40 (incl. 15 deterministic)") {
			t.Errorf("GR %s keeps the POOLED value, labelled: value=%v display=%q", ax.axis, x.Value, x.Display)
		}
	}
	if a3 := cell(t, r, "dc2", "A3", "heal success"); *a3.Value != 39.0/40 {
		t.Errorf("A3's value must stay the pooled 39/40, got %v", *a3.Value)
	}
	if r.Passed() {
		t.Error("25 agent-loop heals passed the n ≥ 30 bar on the strength of 15 deterministic ones")
	}
}

// A2 is gated on judged incidents; the verified-outcome and blast-scored n ride beside it, so "judged but never
// verified" is visible without becoming a gate.
func TestBenchmark_A2CarriesItsVerifiedN(t *testing.T) {
	c, per, est := passingInputs(14)
	nl := per["dc1"]
	nl.Verdicts, nl.PredScored = map[string]int{"match": 3, "deviation": 1}, 7
	per["dc1"] = nl
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	a2 := cell(t, r, "dc1", "A2", "diagnosis correctness")
	if *a2.N != 30 || a2.SideN["verified_outcomes"] != 4 || a2.SideN["blast_scored"] != 7 || !strings.Contains(a2.Note, "verified outcomes n=4") {
		t.Errorf("A2 = %+v, want judged n=30 gated, verified 4 / blast 7 beside it", a2)
	}
	if !r.Passed() {
		t.Errorf("the verified n gated A2: %v", r.Failures)
	}
}

// The window's nominal length is not its evidence: each site's incidents must span ≥ 13 days.
func TestBenchmark_DataSpanIsCheckedPerSite(t *testing.T) {
	c, per, est := passingInputs(20)
	gr := per["dc2"]
	gr.FirstIncident = gr.LastIncident.Add(-12 * 24 * time.Hour) // all of GR's evidence inside 12 days
	per["dc2"] = gr
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	all := strings.Join(r.Failures, "\n")
	if r.Passed() || !strings.Contains(all, "dc2: data spans 12.0 days") || strings.Contains(all, "dc1: data spans") {
		t.Errorf("a 20-day window whose GR incidents span 12 days: verdict %s, failures:\n%s", r.Verdict, all)
	}
	nl := per["dc1"]
	nl.FirstIncident, nl.LastIncident = time.Time{}, time.Time{} // no incidents at all
	per["dc1"] = nl
	if all := strings.Join(BenchmarkBar(BarG4, c, "r1", per, est).Failures, "\n"); !strings.Contains(all, "dc1: data spans 0.0 days") {
		t.Errorf("a site with no incidents was not failed on span:\n%s", all)
	}
}

func TestBenchmark_EveryFailureIsNamed(t *testing.T) {
	c, per, est := passingInputs(13) // the pure report takes the spans as given; the window bound is the DB read's job
	gr := per["dc2"]
	gr.InjectedFaults, gr.DetectedFaults = 12, 11 // A1 short at GR only
	gr.HealCorrelatedCount = 29                   // one A6b leg short by one
	per["dc2"] = gr
	c.RubricVersions = map[string]int{"r1": 800, "": 5} // unrecorded rubric rows
	c.HarnessFPs = map[string]int{fpA: 100, strings.Repeat("b", 64): 20}
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	if r.Passed() {
		t.Fatal("a failing window passed")
	}
	want := []string{
		"window is 13.0 days",
		"judge rubric: 2 value(s)",
		"(unrecorded) ×5",
		"harness fingerprint: 2 value(s)",
		"dc2 A1 detection recall: n=12 < 30",
		"dc2 A6b time to recovery: n=29 < 30",
	}
	all := strings.Join(r.Failures, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("failures do not name %q:\n%s", w, all)
		}
	}
	// NL is untouched and must not be blamed.
	if strings.Contains(all, "dc1") {
		t.Errorf("a passing site was named in the failures:\n%s", all)
	}
	if len(r.Failures) != 5 {
		t.Errorf("want exactly 5 failures (window, rubric, harness, GR A1, GR A6b recovery), got %d:\n%s", len(r.Failures), all)
	}
}

func TestBenchmark_UnrecordedOrAbsentIdentityIsNotUniform(t *testing.T) {
	c, per, est := passingInputs(14)
	c.HarnessFPs = map[string]int{"": 120} // ONE value, but it is "unrecorded"
	if r := BenchmarkBar(BarG4, c, "r1", per, est); r.HarnessOK || r.Passed() {
		t.Error("a window whose only harness value is unrecorded was read as uniform")
	}
	c, per, est = passingInputs(14)
	c.RubricVersions = map[string]int{} // nothing judged at all
	if r := BenchmarkBar(BarG4, c, "r1", per, est); r.RubricOK || r.Passed() {
		t.Error("a window with no judgments was read as rubric-uniform")
	}
}

// A seq gap anywhere in the ledger makes the A8 census incomplete: an evidence failure, not a quality one.
func TestBenchmark_ALedgerGapFailsTheWindow(t *testing.T) {
	c, per, est := passingInputs(14)
	est.LedgerBreaches = 1
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	if r.Passed() || !strings.Contains(strings.Join(r.Failures, "\n"), "deleted row") {
		t.Errorf("a ledger with a deleted row passed, or the failure does not say why: %s %v", r.Verdict, r.Failures)
	}
}

// Sessions that never composed a harness are reported beside the check, not counted against it — otherwise
// every estate with a suppressed session would fail uniformity forever and the line would be ignored.
func TestBenchmark_NotComposedSessionsAreOutsideTheHarnessCheck(t *testing.T) {
	c, per, est := passingInputs(14)
	c.HarnessNotComposed, c.HarnessDeterministic = 17, 11
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	if !r.HarnessOK || !r.Passed() || r.HarnessNotComposed != 17 || r.HarnessDeterministic != 11 {
		t.Errorf("17 no-harness + 11 deterministic sessions beside one recorded fingerprint: harnessOK=%v "+
			"verdict=%s carried=%d/%d", r.HarnessOK, r.Verdict, r.HarnessNotComposed, r.HarnessDeterministic)
	}
	if !strings.Contains(r.Text(), "deterministic heal lane n=11; sessions that never composed a harness n=17") {
		t.Errorf("the two outside-the-check populations are not printed with their n:\n%s", r.Text())
	}
}

// The deterministic heal lane never runs the agent loop: A6a/A6b exclude it by construction, so it gets its
// own line with its own n — reported, never gated, and never silently missing.
func TestBenchmark_DeterministicLaneHasItsOwnLine(t *testing.T) {
	c, per, est := passingInputs(14)
	gr := per["dc2"]
	gr.DeterministicHeals = 11
	per["dc2"] = gr
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	d := cell(t, r, "dc2", "A6", "deterministic lane")
	if d.N == nil || *d.N != 11 || d.Status != StatusReported {
		t.Errorf("GR deterministic lane = %+v, want n=11 REPORTED", d)
	}
	if nl := cell(t, r, "dc1", "A6", "deterministic lane"); nl.N == nil || *nl.N != 0 {
		t.Errorf("NL deterministic lane = %+v — a zero lane is still printed with n=0, not omitted", nl)
	}
	if !r.Passed() {
		t.Errorf("a reported (ungated) line failed the window: %v", r.Failures)
	}
}

func TestBenchmark_MissingSiteIsInsufficientNotPassing(t *testing.T) {
	c, per, est := passingInputs(14)
	delete(per, "dc2")
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	if r.Passed() {
		t.Fatal("a declared site with no aggregate passed")
	}
	a1 := cell(t, r, "dc2", "A1", "detection recall")
	if *a1.N != 0 || a1.Value != nil || a1.Status != StatusInsufficientN {
		t.Errorf("unmeasured GR A1 = %+v, want n=0, no value, INSUFFICIENT-N", a1)
	}
}

func TestWindowRubric(t *testing.T) {
	one := db.WindowCensus{RubricVersions: map[string]int{"old": 3}}
	if got := WindowRubric(one, "new"); got != "old" {
		t.Errorf("one recorded version: got %q, want the window's own \"old\"", got)
	}
	for _, c := range []map[string]int{{}, {"": 3}, {"a": 1, "b": 1}} {
		if got := WindowRubric(db.WindowCensus{RubricVersions: c}, "new"); got != "new" {
			t.Errorf("%v: got %q, want the binary's \"new\"", c, got)
		}
	}
}

func TestBenchmark_TextAndJSON(t *testing.T) {
	c, per, est := passingInputs(14)
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	txt := r.Text()
	for _, w := range []string{"VERDICT: PASS", "NOT-SITE-ATTRIBUTABLE", "estate (axes that cannot be attributed to a site)",
		"unattributed=3", "\"no\"×3", "aaaaaaaaaaaa…"} {
		if !strings.Contains(txt, w) {
			t.Errorf("text report lacks %q:\n%s", w, txt)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	// The not-attributable cell serializes n as null, never 0.
	if !strings.Contains(string(raw), `"scope":"dc1","n":null`) {
		t.Errorf("JSON does not carry a null per-site A8 n: %s", raw)
	}
}

// The default (trailing, estate-wide) scorecard must not change by a byte: the scope keys exist only on a
// scoped card.
func TestScorecard_UnscopedJSONCarriesNoScopeKeys(t *testing.T) {
	raw, err := json.Marshal(Score(db.AxisAgg{Since: time.Unix(0, 0)}, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"until"`, `"site"`, `"a1_unattributed_faults"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("the unscoped scorecard JSON gained %s — the published artifact shape moved", k)
		}
	}
	until := time.Unix(3600, 0)
	sc := Score(db.AxisAgg{Since: time.Unix(0, 0), Until: until, Site: "dc2", UnattributedFaults: 2}, time.Hour)
	raw, _ = json.Marshal(sc)
	for _, k := range []string{`"until"`, `"site":"dc2"`, `"a1_unattributed_faults":2`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("the scoped scorecard JSON lacks %s: %s", k, raw)
		}
	}
	if txt := sc.Text(); !strings.Contains(txt, "SITE-SCOPED to dc2") || !strings.Contains(txt, "ESTATE-WIDE") {
		t.Errorf("a site-scoped card does not say which numbers are estate-wide:\n%s", txt)
	}
}

// TG-670 (F10): an investigation that failed on infrastructure before composing a harness never had an identity
// to differ in. Two such rows (a bao election, a gateway restart) failed the 2026-11-07 dry-read's uniformity
// check because their step_count was back-filled. They sit beside the check on their own line and stay in the
// denominators; an UNRECORDED fingerprint on a session that ran the loop still fails it.
func TestBenchmark_InfraFailedSessionsAreOutsideTheHarnessCheck(t *testing.T) {
	c, per, est := passingInputs(14)
	c.HarnessInfraFailed = 2
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	if !r.HarnessOK || !r.Passed() || r.HarnessInfraFailed != 2 {
		t.Errorf("2 infra-failed sessions beside one recorded fingerprint: harnessOK=%v verdict=%s carried=%d %v",
			r.HarnessOK, r.Verdict, r.HarnessInfraFailed, r.Failures)
	}
	if !strings.Contains(r.Text(), "infra-failed (no harness) n=2") {
		t.Errorf("the infra-failed population is not printed with its n:\n%s", r.Text())
	}
	c.HarnessFPs[""] = 1
	if r := BenchmarkBar(BarG4, c, "r1", per, est); r.HarnessOK {
		t.Error("an unrecorded fingerprint on a looped session was not counted against uniformity — the infra-failed " +
			"lane must not swallow unknown identities")
	}
}

// TG-670 (F12): the A4 cell's value includes the fallback approver's releases, and its note says what was NOT
// credited, so the published autonomy rate names its own numerator.
func TestBenchmark_A4CellNamesFallbackReleasesAndUncreditedStops(t *testing.T) {
	c, per, est := passingInputs(14)
	gr := per["dc2"]
	gr.Bands = map[string]int{"AUTO": 20, "POLL_PAUSE": 10, "<none>": 30}
	gr.FallbackApproved, gr.FailedStops, gr.EscalatedHandoffs = 10, 7, 3
	per["dc2"] = gr
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	a4 := cell(t, r, "dc2", "A4", "autonomy rate")
	if a4.Value == nil || *a4.Value != 1.0 {
		t.Errorf("A4 value = %v, want 1.0: 20 AUTO + 10 fallback-approved of 30 proposals", a4.Value)
	}
	if !strings.Contains(a4.Display, "fallback-released") || !strings.Contains(a4.Note, "10 fallback releases") ||
		!strings.Contains(a4.Note, "7 failed stops and 3 hand-offs NOT credited") {
		t.Errorf("A4 cell does not name its numerator and exclusions: display=%q note=%q", a4.Display, a4.Note)
	}
}

// TG-670 (F18): the A2 cell says its mean pools two instruments and prints the judge-only mean beside it.
func TestBenchmark_A2CellLabelsThePooledMean(t *testing.T) {
	c, per, est := passingInputs(14)
	nl := per["dc1"]
	nl.DimMeans = map[string]float64{"evidence_grounded": 4, "diagnosis_grounded": 2}
	nl.DimN = map[string]int{"evidence_grounded": 30, "diagnosis_grounded": 30}
	per["dc1"] = nl
	r := BenchmarkBar(BarG4, c, "r1", per, est)
	a2 := cell(t, r, "dc1", "A2", "diagnosis correctness")
	if *a2.Value != 3.0 || !strings.Contains(a2.Display, "pooled mean 3.00") || !strings.Contains(a2.Display, "judge-only 4.00") {
		t.Errorf("A2 cell value=%v display=%q, want pooled 3.00 labelled as pooled with judge-only 4.00 beside it", *a2.Value, a2.Display)
	}
	if !strings.Contains(a2.Note, "two instruments") {
		t.Errorf("A2 note does not say the pooled mean averages two instruments: %q", a2.Note)
	}
}
