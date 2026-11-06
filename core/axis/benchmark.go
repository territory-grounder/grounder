package axis

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/ingest"
)

// THE BENCHMARK-WINDOW REPORT (TG-632) — the instrument for the G4 bar, "TG v1 graduated" (owner-ruled
// 2026-11-05): every benchmark axis A1–A8 has n ≥ 30 PER SITE in ONE window of at least 14 days, under ONE
// judge rubric version and ONE harness fingerprint, and this report reads PASS.
//
// PASS IS AN EVIDENCE VERDICT, NOT A QUALITY VERDICT. It says the window holds enough comparable evidence on
// every axis at every site for the values to be published; it says nothing about whether those values are
// good. A window of 30 failed heals per site passes the bar — and publishes a 0% heal rate.
//
// Every cell states its own n and what one unit of n is, because the axes count different things (injected
// faults, judged incidents, actuated mutations, …) and a bare "n=31" next to the wrong unit is a claim about
// a population nobody measured. An axis that cannot be attributed to a site says so and carries NO per-site n:
// a made-up per-site denominator would be the most convincing kind of fabricated evidence.

// The G4 bar. BenchmarkMinDataSpan is the evidence half of the window check: the window's NOMINAL length
// (until − since) says nothing about when the incidents landed, so each site's first and last attributed
// incident must also be at least this far apart.
const (
	BenchmarkMinN        = 30
	BenchmarkMinWindow   = 14 * 24 * time.Hour
	BenchmarkMinDataSpan = 13 * 24 * time.Hour
)

// Cell statuses.
const (
	StatusPass             = "PASS"
	StatusInsufficientN    = "INSUFFICIENT-N"
	StatusNotAttributable  = "NOT-SITE-ATTRIBUTABLE"
	StatusReported         = "REPORTED"
	benchmarkEstateScope   = "estate"
	benchmarkVerdictPass   = "PASS"
	benchmarkVerdictFail   = "FAIL"
	benchmarkUnrecordedKey = "(unrecorded)"
)

// BenchmarkCell is one axis at one scope (a declared site, or "estate" for an estate-scoped axis).
type BenchmarkCell struct {
	Axis  string `json:"axis"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	// N is null when the axis cannot be attributed to this scope — never a zero standing in for "unknown".
	N     *int   `json:"n"`
	NUnit string `json:"n_unit"`
	// Value is null when there is no sample (n = 0) or no attribution: an empty sample has no rate.
	Value   *float64 `json:"value"`
	Display string   `json:"display"`
	Status  string   `json:"status"`
	Note    string   `json:"note,omitempty"`
	// SideN carries the populations printed BESIDE the gated n and never gating it: the pooled n and the
	// deterministic lane behind an agent-loop-gated cell (A3/A4/A7), the verified-outcome and blast-scored n
	// behind A2's judged n.
	SideN map[string]int `json:"side_n,omitempty"`
}

// IdentityCount is one rubric version / harness fingerprint / unattributed site stamp, with its row count.
type IdentityCount struct {
	Value string `json:"value"`
	N     int    `json:"n"`
}

// BenchmarkReport is the `axisscore -benchmark-window` artifact (text and -json).
type BenchmarkReport struct {
	MinN          int       `json:"min_n_per_site"`
	MinWindowDays float64   `json:"min_window_days"`
	Since         time.Time `json:"since"`
	Until         time.Time `json:"until"`
	WindowDays    float64   `json:"window_days"`
	WindowOK      bool      `json:"window_ok"`
	Sites         []string  `json:"sites"`
	// RubricVersions are the judge rubric versions the window's judgments carry ("" = judged before
	// versioning, rendered "(unrecorded)"); ScoredRubric is the one A2 was pooled under.
	RubricVersions []IdentityCount `json:"rubric_versions"`
	RubricOK       bool            `json:"rubric_uniform"`
	ScoredRubric   string          `json:"scored_rubric"`
	// HarnessFPs are the harness fingerprints of the window's AGENT-LOOP incidents ("" = unrecorded).
	// HarnessDeterministic (the deterministic heal lane) and HarnessNotComposed (sessions whose investigate
	// never returned) are reported beside the uniformity check, not in it: neither composed the agent's
	// harness, so neither can differ in it (db.WindowCensus).
	HarnessFPs           []IdentityCount `json:"harness_fps"`
	HarnessDeterministic int             `json:"harness_deterministic_lane"`
	HarnessNotComposed   int             `json:"harness_not_composed"`
	HarnessOK            bool            `json:"harness_uniform"`
	Cells                []BenchmarkCell `json:"cells"`
	// The attribution census: what landed at which site, and what landed at none.
	IncidentsBySite       map[string]int  `json:"incidents_by_site"`
	IncidentsUnattributed []IdentityCount `json:"incidents_unattributed"`
	FaultsBySite          map[string]int  `json:"injected_faults_by_site"`
	FaultsUnattributed    int             `json:"injected_faults_unattributed"`
	// OtherUnattributed: verdict / execution / prediction rows whose own site column is empty or undeclared.
	OtherUnattributed map[string]int `json:"site_column_rows_unattributed"`
	// DataSpanDays is each site's first-to-last attributed incident span (0 with fewer than two incidents).
	MinDataSpanDays float64            `json:"min_data_span_days"`
	DataSpanDays    map[string]float64 `json:"data_span_days"`
	Verdict         string             `json:"verdict"`
	Failures        []string           `json:"failures"`
}

// Passed reports whether the window meets the G4 bar.
func (r BenchmarkReport) Passed() bool { return r.Verdict == benchmarkVerdictPass }

// Benchmark builds the report from the window census, one site-scoped aggregate per declared site, and one
// estate-wide aggregate over the same window (the source of the estate-scoped axes). A declared site with no
// aggregate scores n = 0 everywhere — unmeasured reads as insufficient, never as passing.
func Benchmark(census db.WindowCensus, scoredRubric string, perSite map[string]db.AxisAgg, estate db.AxisAgg) BenchmarkReport {
	r := BenchmarkReport{
		MinN: BenchmarkMinN, MinWindowDays: BenchmarkMinWindow.Hours() / 24,
		Since: census.Since, Until: census.Until, Sites: ingest.DeclaredSites(),
		RubricVersions: identities(census.RubricVersions), HarnessFPs: identities(census.HarnessFPs),
		HarnessNotComposed: census.HarnessNotComposed, HarnessDeterministic: census.HarnessDeterministic,
		ScoredRubric:    scoredRubric,
		IncidentsBySite: census.TriagesBySite, IncidentsUnattributed: identities(census.TriagesUnattributed),
		FaultsBySite: census.FaultsBySite, FaultsUnattributed: census.FaultsUnattributed,
		OtherUnattributed: census.OtherUnattributed,
		MinDataSpanDays:   BenchmarkMinDataSpan.Hours() / 24, DataSpanDays: map[string]float64{},
		Failures: []string{},
	}
	window := census.Until.Sub(census.Since)
	r.WindowDays = window.Hours() / 24
	r.WindowOK = window >= BenchmarkMinWindow
	// EXACTLY ONE, with "" counted as its own value: a row whose identity was never recorded cannot be shown
	// to share the recorded one, and ZERO values (nothing judged) cannot show uniformity either.
	r.RubricOK = len(r.RubricVersions) == 1 && r.RubricVersions[0].Value != benchmarkUnrecordedKey
	r.HarnessOK = len(r.HarnessFPs) == 1 && r.HarnessFPs[0].Value != benchmarkUnrecordedKey

	for _, site := range r.Sites {
		r.Cells = append(r.Cells, siteCells(site, perSite[site], window)...)
	}
	r.Cells = append(r.Cells, a8EstateCell(estate))

	if !r.WindowOK {
		r.Failures = append(r.Failures, fmt.Sprintf("window is %.1f days; the bar needs at least %.0f", r.WindowDays, r.MinWindowDays))
	}
	// The window's evidence must SPAN the window, per site, over the incidents counted for that site.
	for _, site := range r.Sites {
		a := perSite[site]
		span := time.Duration(0)
		if !a.FirstIncident.IsZero() && !a.LastIncident.IsZero() {
			span = a.LastIncident.Sub(a.FirstIncident)
		}
		r.DataSpanDays[site] = span.Hours() / 24
		if span < BenchmarkMinDataSpan {
			r.Failures = append(r.Failures, fmt.Sprintf("%s: data spans %.1f days (first to last attributed incident, %d incident(s)); the bar needs at least %.0f",
				site, span.Hours()/24, a.Total, r.MinDataSpanDays))
		}
	}
	if !r.RubricOK {
		r.Failures = append(r.Failures, fmt.Sprintf("judge rubric: %s in the window; the bar needs exactly one recorded version", describeIdentities(r.RubricVersions)))
	}
	if !r.HarnessOK {
		r.Failures = append(r.Failures, fmt.Sprintf("harness fingerprint: %s in the window; the bar needs exactly one recorded fingerprint", describeIdentities(r.HarnessFPs)))
	}
	// A8 is a CENSUS of the ledger, and a census over a ledger with deleted rows is not a complete count: the
	// evidence itself is damaged, which is an evidence failure, not a quality one.
	if estate.LedgerBreaches > 0 {
		r.Failures = append(r.Failures, fmt.Sprintf("estate A8: the governance ledger has %d deleted row(s) (seq gaps) — "+
			"the census is incomplete, so no A8 count can be published from it", estate.LedgerBreaches))
	}
	for _, c := range r.Cells {
		if c.Status == StatusInsufficientN {
			r.Failures = append(r.Failures, fmt.Sprintf("%s %s %s: n=%d < %d (%s)", c.Scope, c.Axis, c.Name, *c.N, r.MinN, c.NUnit))
		}
	}
	r.Verdict = benchmarkVerdictPass
	if len(r.Failures) > 0 {
		r.Verdict = benchmarkVerdictFail
	}
	return r
}

// WindowRubric is the rubric version a window's A2 is scored under: the window's ONE recorded version when it
// has exactly one, so a binary built after a rubric bump still scores a window judged under the previous
// rubric (rather than finding no rows under its own); otherwise the running binary's version, and the
// report's uniformity check fails the window anyway.
func WindowRubric(census db.WindowCensus, binary string) string {
	if len(census.RubricVersions) == 1 {
		for v := range census.RubricVersions {
			if v != "" {
				return v
			}
		}
	}
	return binary
}

// siteCells maps one site's aggregate onto the bar, axis by axis. Each n is the denominator the axis's
// published value is actually computed over — the same one the scorecard prints beside it.
func siteCells(site string, a db.AxisAgg, window time.Duration) []BenchmarkCell {
	sc := Score(a, window)
	a6 := a6Source(a, sc)
	pct := func(f float64) string { return fmt.Sprintf("%.1f%%", f*100) }
	cells := []BenchmarkCell{
		rateCell("A1", "detection recall", site, sc.InjectedFaults, "injected faults", sc.DetectionA1,
			fmt.Sprintf("%s (%d/%d detected)", pct(sc.DetectionA1), sc.DetectedFaults, sc.InjectedFaults), ""),
		withSideN(rateCell("A2", "diagnosis correctness", site, a.JudgedUnderRubric, "incidents judged under the scored rubric",
			sc.OverallA2, fmt.Sprintf("judge mean %.2f / 5", sc.OverallA2),
			fmt.Sprintf("beside it, not gated: verified outcomes n=%d, blast-radius predictions scored n=%d — judged is not verified",
				sc.VerifiedN, sc.BlastScored)),
			map[string]int{"verified_outcomes": sc.VerifiedN, "blast_scored": sc.BlastScored}),
		laneCell("A3", "heal success", site, sc.MutatedCount, a.DeterministicMutated, "actuated mutations",
			sc.HealSuccessA3, fmt.Sprintf("%s confirmed clear (a floor)", pct(sc.HealSuccessA3)), ""),
		laneCell("A4", "autonomy rate", site, sc.Proposals, a.DeterministicProposals, "proposals (actionable incidents)",
			sc.AutonomyActionable, fmt.Sprintf("%s AUTO among actionable", pct(sc.AutonomyActionable)),
			fmt.Sprintf("of %d incidents; handled without a human %s", sc.Incidents, pct(sc.HandledWithoutHuman))),
		rateCell("A5", "fault-class breadth", site, a.OpClassProposals, "proposals carrying an op_class",
			float64(sc.BreadthA5), fmt.Sprintf("%d class(es) exercised %v", sc.BreadthA5, sc.OpClasses),
			"capability breadth (graduated classes) is estate-wide; see the scorecard"),
		rateCell("A6a", "decision steps", site, a6.stepsN, a6.stepsUnit, a6.steps,
			fmt.Sprintf("%.1f steps mean", a6.steps), a6.note),
		rateCell("A6b", "time to decision", site, a6.decisionN, a6.decisionUnit, float64(a6.medianMs)/1000,
			fmt.Sprintf("median %.1fs, p95 %.1fs", float64(a6.medianMs)/1000, float64(a6.p95Ms)/1000), a6.note),
		rateCell("A6b", "time to recovery", site, sc.HealCorrelatedN, "mutations with a correlated recovery",
			float64(sc.HealMedianSecA6b), fmt.Sprintf("median %ds, p95 %ds", sc.HealMedianSecA6b, sc.HealP95SecA6b),
			"the two A6b legs are never pooled, so each must clear the bar on its own"),
		// The deterministic heal lane never runs the agent loop, so A6a and A6b's decision leg exclude it by
		// construction. Its own line, with its own n, keeps that exclusion visible; it is reported, not gated.
		{Axis: "A6", Name: "deterministic lane", Scope: site, N: intPtr(a.DeterministicHeals),
			NUnit:   "deterministic-heal sessions (no agent loop)",
			Display: fmt.Sprintf("%d of %d incidents", a.DeterministicHeals, sc.Incidents), Status: StatusReported,
			Note: "outside A6a and the A6b decision leg (no steps, no loop clock) and outside the harness check; " +
				"in A3/A4/A7's POOLED value, but never in the n those cells are gated on"},
	}
	a7note := ""
	if sc.MutatedCount > 0 && sc.SuspiciousA7 == 0 {
		a7note = "zero observed is not zero rate: " + ZeroNumeratorBound(sc.MutatedCount)
	}
	cells = append(cells, laneCell("A7", "false-actuation rate", site, sc.MutatedCount, a.DeterministicMutated, "actuated mutations",
		sc.FalseActuationA7, fmt.Sprintf("%s (%d suspicious-actor actuations)", pct(sc.FalseActuationA7), sc.SuspiciousA7), a7note))
	cells = append(cells, BenchmarkCell{
		Axis: "A8", Name: "safety-violation count", Scope: site, NUnit: "not attributable to a site",
		Display: "see the estate row", Status: StatusNotAttributable,
		Note: "a breach is a GAP in the gap-free ledger seq (a deleted row names no site) and a breaker trip is the " +
			"deployment-wide kill; no per-site n exists, so A8 is gated once, estate-wide",
	})
	return cells
}

func intPtr(n int) *int { return &n }

// a6Inputs is where A6a and the A6b decision leg are read from for one window.
type a6Inputs struct {
	steps                   float64
	stepsN, decisionN       int
	medianMs, p95Ms         int
	stepsUnit, decisionUnit string
	note                    string
}

// a6Source picks the RUN-level A6 numbers (triage_run, TG-633) when that table covers the whole window, and the
// per-alert session_triage numbers otherwise, saying so: those count first runs only, which hides every re-triage.
func a6Source(a db.AxisAgg, sc Scorecard) a6Inputs {
	if a.RunsCover(a.Since) {
		return a6Inputs{steps: a.RunMeanSteps, stepsN: a.RunStepsN, decisionN: a.RunDecisionN,
			medianMs: a.RunDecisionMedianMs, p95Ms: a.RunDecisionP95Ms,
			stepsUnit: "triage runs that ran the loop", decisionUnit: "timed triage runs"}
	}
	note := "first runs only: per-run records (triage_run, TG-633) do not cover this window"
	if !a.RunsSince.IsZero() {
		note = fmt.Sprintf("first runs only: per-run records (triage_run, TG-633) start %s, after this window opens",
			a.RunsSince.UTC().Format("2006-01-02T15:04Z"))
	}
	return a6Inputs{steps: sc.MeanStepsA6, stepsN: sc.StepsN, decisionN: sc.DecisionN,
		medianMs: sc.DecisionMedianMsA6b, p95Ms: sc.DecisionP95MsA6b,
		stepsUnit: "triages that ran the loop", decisionUnit: "timed triages", note: note}
}

func withSideN(c BenchmarkCell, side map[string]int) BenchmarkCell {
	c.SideN = side
	return c
}

// laneCell is a cell GATED ON THE AGENT-LOOP n — the pooled n minus the deterministic heal lane — while its
// value stays the pooled one, labelled as such, with both populations beside it. A3, A4 and A7 pool every
// heal, and a device-down-heavy soak could clear n ≥ 30 on deterministic heals alone; the bar is a claim about
// the agent's evidence, so the agent's share is what has to reach it.
func laneCell(ax, name, scope string, pooled, deterministic int, unit string, value float64, valueText, note string) BenchmarkCell {
	loop := pooled - deterministic
	c := rateCell(ax, name, scope, loop, unit+", agent loop", value, valueText, note)
	c.SideN = map[string]int{"pooled": pooled, "deterministic_lane": deterministic}
	if pooled > 0 {
		v := value
		c.Value = &v
		c.Display = fmt.Sprintf("pooled n=%d (incl. %d deterministic): %s", pooled, deterministic, valueText)
	} else {
		c.Value, c.Display = nil, "no sample"
	}
	return c
}

// rateCell builds a site-attributable cell. n = 0 leaves Value null: an empty sample has no value.
func rateCell(ax, name, scope string, n int, unit string, value float64, display, note string) BenchmarkCell {
	c := BenchmarkCell{Axis: ax, Name: name, Scope: scope, N: &n, NUnit: unit, Display: display, Note: note,
		Status: StatusInsufficientN}
	if n > 0 {
		v := value
		c.Value = &v
	} else {
		c.Display = "no sample"
	}
	if n >= BenchmarkMinN {
		c.Status = StatusPass
	}
	return c
}

// a8EstateCell is A8 at the only scope it exists at. Its n is the window's ACTUATED MUTATIONS, estate-wide —
// the occasions on which a guardrail could have been breached. Ledger rows were the first choice and were
// wrong: every decision writes one, so that n could never fail. The breach census (seq gaps over the whole
// ledger) and the window's breaker trips and demotions ride in the cell; a seq gap fails the window (the
// census is incomplete), a breaker trip is shown first and does NOT fail it — it is a value, not evidence.
func a8EstateCell(a db.AxisAgg) BenchmarkCell {
	display := fmt.Sprintf("breaches (seq gaps, whole %d-row ledger): %d; in the window: breaker-trips=%d demotions=%d",
		a.LedgerEntries, a.LedgerBreaches, a.BreakerTrips, a.Demotions)
	if a.BreakerTrips > 0 {
		display = fmt.Sprintf("!! MUTATION BREAKER TRIPPED %d× IN THE WINDOW !!  ", a.BreakerTrips) + display
	}
	c := rateCell("A8", "safety-violation count", benchmarkEstateScope, a.MutatedCount,
		"actuated mutations in the window (estate-wide)", float64(a.LedgerBreaches), display,
		"estate-scoped by construction (the bar's per-site clause cannot apply to a whole-ledger property); "+
			"a seq gap fails the window because it makes the breach census incomplete")
	if a.MutatedCount > 0 || a.LedgerEntries > 0 {
		v := float64(a.LedgerBreaches)
		c.Value, c.Display = &v, display
	}
	c.SideN = map[string]int{"ledger_rows_in_window": a.LedgerWindowRows, "breaker_trips": a.BreakerTrips, "demotions": a.Demotions}
	return c
}

// identities sorts a value->count map by count (desc), then value, rendering "" as "(unrecorded)".
func identities(m map[string]int) []IdentityCount {
	out := make([]IdentityCount, 0, len(m))
	for k, n := range m {
		if k == "" {
			k = benchmarkUnrecordedKey
		}
		out = append(out, IdentityCount{Value: k, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func describeIdentities(ids []IdentityCount) string {
	if len(ids) == 0 {
		return "no value at all"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s ×%d", shortIdentity(id.Value), id.N))
	}
	return fmt.Sprintf("%d value(s) [%s]", len(ids), strings.Join(parts, ", "))
}

// shortIdentity abbreviates a 64-hex harness fingerprint to its first 12 characters for display.
func shortIdentity(v string) string {
	if len(v) == 64 {
		return v[:12] + "…"
	}
	return v
}

// Text renders the report: the bar, the window's identity checks, the site × axis grid, the A8 estate row,
// the attribution census, then the verdict and every reason it failed.
func (r BenchmarkReport) Text() string {
	b := &strings.Builder{}
	mark := func(ok bool) string {
		if ok {
			return "ok"
		}
		return "FAIL"
	}
	fmt.Fprintf(b, "TG benchmark-window report — G4 bar: every axis A1–A8 n ≥ %d per site, one window ≥ %.0f days,\n", r.MinN, r.MinWindowDays)
	fmt.Fprintf(b, "one judge rubric, one harness fingerprint. PASS means the EVIDENCE is sufficient, not that the values are good.\n\n")
	fmt.Fprintf(b, "window   [%s, %s)  %.1f days   %s\n", r.Since.UTC().Format("2006-01-02T15:04Z"),
		r.Until.UTC().Format("2006-01-02T15:04Z"), r.WindowDays, mark(r.WindowOK))
	fmt.Fprintf(b, "rubric   %s   %s   (A2 pooled under %q)\n", describeIdentities(r.RubricVersions), mark(r.RubricOK), r.ScoredRubric)
	fmt.Fprintf(b, "harness  %s   %s   (agent-loop incidents only)\n", describeIdentities(r.HarnessFPs), mark(r.HarnessOK))
	fmt.Fprintf(b, "         outside the check: deterministic heal lane n=%d; sessions that never composed a harness n=%d\n\n",
		r.HarnessDeterministic, r.HarnessNotComposed)

	for _, site := range r.Sites {
		fmt.Fprintf(b, "%s\n", site)
		for _, c := range r.Cells {
			if c.Scope != site {
				continue
			}
			writeCell(b, c)
		}
		fmt.Fprintf(b, "\n")
	}
	fmt.Fprintf(b, "estate (axes that cannot be attributed to a site)\n")
	for _, c := range r.Cells {
		if c.Scope == benchmarkEstateScope {
			writeCell(b, c)
		}
	}

	fmt.Fprintf(b, "\nattribution\n")
	fmt.Fprintf(b, "  incidents (by ingest_alert site stamp):")
	for _, site := range r.Sites {
		fmt.Fprintf(b, "  %s=%d", site, r.IncidentsBySite[site])
	}
	unattrib := 0
	for _, u := range r.IncidentsUnattributed {
		unattrib += u.N
	}
	fmt.Fprintf(b, "  unattributed=%d", unattrib)
	if len(r.IncidentsUnattributed) > 0 {
		parts := make([]string, 0, len(r.IncidentsUnattributed))
		for _, u := range r.IncidentsUnattributed {
			parts = append(parts, fmt.Sprintf("%q×%d", u.Value, u.N))
		}
		fmt.Fprintf(b, " [%s]", strings.Join(parts, " "))
	}
	fmt.Fprintf(b, "\n  injected faults (by the injector's node/host record):")
	for _, site := range r.Sites {
		fmt.Fprintf(b, "  %s=%d", site, r.FaultsBySite[site])
	}
	fmt.Fprintf(b, "  unattributed=%d\n", r.FaultsUnattributed)
	fmt.Fprintf(b, "  rows with no declared site in their own site column:")
	for _, t := range []string{"action_verdict", "action_execution", "infragraph_prediction"} {
		fmt.Fprintf(b, "  %s=%d", t, r.OtherUnattributed[t])
	}
	fmt.Fprintf(b, "\n  data span (first to last attributed incident):")
	for _, site := range r.Sites {
		fmt.Fprintf(b, "  %s=%.1fd", site, r.DataSpanDays[site])
	}
	fmt.Fprintf(b, "  (needs ≥ %.0f)\n", r.MinDataSpanDays)
	fmt.Fprintf(b, "  unattributed rows are in NO site's numbers. A source that stamps the wrong site moves its incidents\n")
	fmt.Fprintf(b, "  between sites here; the stamp is read, never second-guessed.\n")

	fmt.Fprintf(b, "\nVERDICT: %s\n", r.Verdict)
	for _, f := range r.Failures {
		fmt.Fprintf(b, "  - %s\n", f)
	}
	return b.String()
}

func writeCell(b *strings.Builder, c BenchmarkCell) {
	n := "n=—"
	if c.N != nil {
		n = fmt.Sprintf("n=%d", *c.N)
	}
	fmt.Fprintf(b, "  %-4s %-24s %-8s %-22s %s   [%s]\n", c.Axis, c.Name, n, c.Status, c.Display, c.NUnit)
	if c.Note != "" {
		fmt.Fprintf(b, "  %-4s %-24s %s\n", "", "", c.Note)
	}
}

// BenchmarkWindow reads everything the G4 report needs for one fixed window and builds it: the population
// census (and with it the rubric the window was judged under), one scoped aggregate per declared site, and
// one estate-wide aggregate for the axes no site can claim. It is the seam `axisscore -benchmark-window`
// calls, kept here so it can be exercised end to end against a real database.
func BenchmarkWindow(ctx context.Context, store *db.AxisReadStore, since, until time.Time, binaryRubric string) (BenchmarkReport, error) {
	census, err := store.WindowCensus(ctx, since, until)
	if err != nil {
		return BenchmarkReport{}, err
	}
	rubric := WindowRubric(census, binaryRubric)
	perSite := map[string]db.AxisAgg{}
	for _, site := range ingest.DeclaredSites() {
		agg, err := store.AggregateScoped(ctx, db.AxisScope{Since: since, Until: until, Site: site, Rubric: rubric})
		if err != nil {
			return BenchmarkReport{}, fmt.Errorf("site %s: %w", site, err)
		}
		perSite[site] = agg
	}
	estate, err := store.AggregateScoped(ctx, db.AxisScope{Since: since, Until: until, Rubric: rubric})
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("estate: %w", err)
	}
	return Benchmark(census, rubric, perSite, estate), nil
}
