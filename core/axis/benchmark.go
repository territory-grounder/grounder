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

// Bar selects which evidence bar a window is read against (TG-672, G4A).
//
//   - BarG4 is the owner-ruled 2026-11-05 bar: n ≥ 30 per axis per site in ONE window of at least 14 days whose
//     data spans at least 13, one rubric, one harness fingerprint, a gap-free ledger.
//   - BarG4A is the ACCELERATED bar of the owner's 2026-11-07 instruction: the 14-day duration and the data-span
//     check are NOT conditions ("the 14-day duration is superseded for G4A"); everything else stands, and the window
//     must additionally carry EXACTLY ONE recorded instrument identity across its agent-loop sessions
//     (core/instrumentid, REQ-2535) — "(unrecorded)" is a value that fails it, never a compatible one.
//
// Both bars print the same descriptive intervals, host shares, class coverage and synthetic/organic split beside
// the gated counts; none of those gate.
type Bar string

// The two bars.
const (
	BarG4  Bar = "g4"
	BarG4A Bar = "g4a"
)

// ParseBar accepts "g4" or "g4a" (case-insensitive); anything else is a usage error, never a default.
func ParseBar(s string) (Bar, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "g4":
		return BarG4, nil
	case "g4a":
		return BarG4A, nil
	}
	return "", fmt.Errorf("unknown bar %q: want g4 or g4a", s)
}

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
	// Interval is the DESCRIPTIVE 95% interval for a rate cell's k/n (core/axis/interval.go): printed beside the
	// raw counts, labelled model-dependent, never a gate. Nil for cells that are not a rate or have no sample.
	Interval *Interval `json:"interval,omitempty"`
}

// IdentityCount is one rubric version / harness fingerprint / unattributed site stamp, with its row count.
type IdentityCount struct {
	Value string `json:"value"`
	N     int    `json:"n"`
}

// BenchmarkReport is the `axisscore -bar g4|g4a` artifact (text and -json; `-benchmark-window` = `-bar g4`).
type BenchmarkReport struct {
	// Bar names the bar the verdict was read against ("g4" or "g4a").
	Bar           string    `json:"bar"`
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
	// HarnessDeterministic (the deterministic heal lane), HarnessInfraFailed (investigations that failed on
	// infrastructure before composing a harness, TG-670) and HarnessNotComposed (sessions suppressed before
	// the loop) are reported beside the uniformity check, not in it: none composed the agent's harness, so none
	// can differ in it (db.WindowCensus). They stay in every detection and reliability denominator.
	HarnessFPs           []IdentityCount `json:"harness_fps"`
	HarnessDeterministic int             `json:"harness_deterministic_lane"`
	HarnessInfraFailed   int             `json:"harness_infra_failed"`
	HarnessNotComposed   int             `json:"harness_not_composed"`
	HarnessOK            bool            `json:"harness_uniform"`
	// InstrumentIDs are the instrument identities of the window's AGENT-LOOP sessions ("" = stamped before
	// migration 0132, rendered "(unrecorded)"); the same three lanes as the harness check sit outside it. The g4a
	// bar needs exactly one recorded value; g4 reports it without gating on it.
	InstrumentIDs          []IdentityCount `json:"instrument_ids"`
	InstrumentOK           bool            `json:"instrument_uniform"`
	InstrumentColumnAbsent bool            `json:"instrument_column_absent"`
	// LedgerGaps is the whole-ledger seq-gap count (A8's breach census); any gap fails either bar.
	LedgerGaps int             `json:"ledger_gaps"`
	Cells      []BenchmarkCell `json:"cells"`
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
	// Disclosures (TG-672): what the identity does NOT hash and what the counts do not show, printed beside the
	// verdict and never gating it — estate topology snapshots published in the window; the synthetic/organic
	// split of each site's incidents; the largest share of one host in each site's mutated incidents; the
	// injected fault classes per site; and the named uncertainties (provider model versions, learned state).
	EstateSnapshots       int                       `json:"estate_snapshots_in_window"`
	SyntheticBySite       map[string]int            `json:"synthetic_incidents_by_site"`
	OrganicBySite         map[string]int            `json:"organic_incidents_by_site"`
	MutationHostShare     map[string]string         `json:"max_host_share_of_mutated_incidents"`
	FaultClassesBySite    map[string]map[string]int `json:"injected_fault_classes_by_site"`
	Uncertainties         []string                  `json:"uncertainties"`
	CrossHostActuations   *int                      `json:"cross_host_actuations"`
	CrossHostDetail       []string                  `json:"cross_host_detail"`
	ExcludedRefs          []string                  `json:"excluded_refs"`
	ExcludedRefsUnmatched []string                  `json:"excluded_refs_unmatched"`
	Verdict               string                    `json:"verdict"`
	Failures              []string                  `json:"failures"`
}

// Passed reports whether the window meets the G4 bar.
func (r BenchmarkReport) Passed() bool { return r.Verdict == benchmarkVerdictPass }

// Uncertainties every report names: what no identity in the window pins down.
var reportUncertainties = []string{
	"provider model VERSIONS behind the gateway aliases are not part of the instrument identity: the judge records the served model per judgment (session_judgment.judge_model, TG-670); agent steps do not record it",
	"the learner's co-occurrence state and the maintained precedent corpus evolve inside the window and are not hashed (REQ-2535): learning is disclosed, not frozen",
	"the estate topology is observed, not configured: the count of snapshots published in the window is disclosed beside the identity, never hashed into it",
}

// BenchmarkBar builds the report against the named bar from the window census, one site-scoped aggregate per
// declared site, and one estate-wide aggregate over the same window (the source of the estate-scoped axes). A
// declared site with no aggregate scores n = 0 everywhere — unmeasured reads as insufficient, never as passing.
func BenchmarkBar(bar Bar, census db.WindowCensus, scoredRubric string, perSite map[string]db.AxisAgg, estate db.AxisAgg) BenchmarkReport {
	r := BenchmarkReport{
		Bar:  string(bar),
		MinN: BenchmarkMinN, MinWindowDays: BenchmarkMinWindow.Hours() / 24,
		Since: census.Since, Until: census.Until, Sites: ingest.DeclaredSites(),
		RubricVersions: identities(census.RubricVersions), HarnessFPs: identities(census.HarnessFPs),
		HarnessNotComposed: census.HarnessNotComposed, HarnessDeterministic: census.HarnessDeterministic,
		HarnessInfraFailed: census.HarnessInfraFailed,
		ScoredRubric:       scoredRubric,
		IncidentsBySite:    census.TriagesBySite, IncidentsUnattributed: identities(census.TriagesUnattributed),
		FaultsBySite: census.FaultsBySite, FaultsUnattributed: census.FaultsUnattributed,
		OtherUnattributed: census.OtherUnattributed,
		MinDataSpanDays:   BenchmarkMinDataSpan.Hours() / 24, DataSpanDays: map[string]float64{},
		InstrumentIDs: identities(census.InstrumentIDs), InstrumentColumnAbsent: census.InstrumentColumnAbsent, LedgerGaps: estate.LedgerBreaches,
		EstateSnapshots: census.EstateSnapshots, SyntheticBySite: orEmpty(census.SyntheticBySite), OrganicBySite: orEmpty(census.OrganicBySite),
		MutationHostShare: hostShares(census.MutatedByHostBySite), FaultClassesBySite: census.FaultClassesBySite,
		Uncertainties: append([]string(nil), reportUncertainties...),
		Failures:      []string{},
	}
	if r.FaultClassesBySite == nil {
		r.FaultClassesBySite = map[string]map[string]int{}
	}
	window := census.Until.Sub(census.Since)
	r.WindowDays = window.Hours() / 24
	r.WindowOK = window >= BenchmarkMinWindow
	// EXACTLY ONE, with "" counted as its own value: a row whose identity was never recorded cannot be shown
	// to share the recorded one, and ZERO values (nothing judged) cannot show uniformity either.
	r.RubricOK = len(r.RubricVersions) == 1 && r.RubricVersions[0].Value != benchmarkUnrecordedKey
	r.HarnessOK = len(r.HarnessFPs) == 1 && r.HarnessFPs[0].Value != benchmarkUnrecordedKey
	r.InstrumentOK = len(r.InstrumentIDs) == 1 && r.InstrumentIDs[0].Value != benchmarkUnrecordedKey

	for _, site := range r.Sites {
		r.Cells = append(r.Cells, siteCells(site, perSite[site], window)...)
	}
	r.Cells = append(r.Cells, a8EstateCell(estate))

	// The duration half of the bar: waived by G4A ONLY — any other value, including an unknown one, is read as the
	// strictest bar (fail closed). G4A reports the window and the spans and gates on neither.
	if bar != BarG4A && !r.WindowOK {
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
		if bar != BarG4A && span < BenchmarkMinDataSpan {
			r.Failures = append(r.Failures, fmt.Sprintf("%s: data spans %.1f days (first to last attributed incident, %d incident(s)); the bar needs at least %.0f",
				site, span.Hours()/24, a.Total, r.MinDataSpanDays))
		}
	}
	// The instrument half of the G4A bar: exactly one recorded identity across the agent-loop sessions.
	if bar == BarG4A && !r.InstrumentOK {
		why := ""
		if r.InstrumentColumnAbsent {
			why = "; this database has no session_triage.instrument_id column (before migration 0132), so nothing in it is recorded"
		}
		r.Failures = append(r.Failures, fmt.Sprintf("instrument identity: %s among the window's agent-loop sessions; the g4a bar needs exactly one recorded identity (an unrecorded identity is never read as compatible)%s",
			describeIdentities(r.InstrumentIDs), why))
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
	autoNumerator := a.Bands["AUTO"] + a.Bands["AUTO_NOTICE"] + a.FallbackApproved
	cells := []BenchmarkCell{
		withInterval(rateCell("A1", "detection recall", site, sc.InjectedFaults, "injected faults", sc.DetectionA1,
			fmt.Sprintf("%s (%d/%d detected)", pct(sc.DetectionA1), sc.DetectedFaults, sc.InjectedFaults), ""),
			RateInterval(sc.DetectedFaults, sc.InjectedFaults)),
		withSideN(rateCell("A2", "diagnosis correctness", site, a.JudgedUnderRubric, "incidents judged under the scored rubric",
			sc.OverallA2, fmt.Sprintf("pooled mean %.2f / 5 (model-judged + deterministic dims); judge-only %.2f", sc.OverallA2, sc.JudgeOnlyA2),
			fmt.Sprintf("the pooled mean averages two instruments (deterministic %v are in it); beside it, not gated: verified outcomes n=%d, blast-radius predictions scored n=%d — judged is not verified",
				sc.DeterministicDims, sc.VerifiedN, sc.BlastScored)),
			map[string]int{"verified_outcomes": sc.VerifiedN, "blast_scored": sc.BlastScored}),
		withInterval(laneCell("A3", "heal success", site, sc.MutatedCount, a.DeterministicMutated, "actuated mutations",
			sc.HealSuccessA3, fmt.Sprintf("%s confirmed clear (%d/%d; a floor)", pct(sc.HealSuccessA3), a.HealConfirmedCount, sc.MutatedCount), ""),
			RateInterval(a.HealConfirmedCount, sc.MutatedCount)),
		withInterval(laneCell("A4", "autonomy rate", site, sc.Proposals, a.DeterministicProposals, "proposals (actionable incidents)",
			sc.AutonomyActionable, fmt.Sprintf("%s AUTO+AUTO_NOTICE+fallback-released among actionable (%d/%d; decisions; execution is A3)", pct(sc.AutonomyActionable), autoNumerator, sc.Proposals),
			fmt.Sprintf("of %d incidents; handled without a human %s (incl. %d deterministic-class skips); %d fallback releases in the numerator; %d failed stops and %d hand-offs NOT credited",
				sc.Incidents, pct(sc.HandledWithoutHuman), sc.DeterministicSkips, sc.FallbackApproved, sc.FailedStops, sc.EscalatedHandoffs)),
			RateInterval(autoNumerator, sc.Proposals)),
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
	cells = append(cells, withInterval(laneCell("A7", "false-actuation rate", site, sc.MutatedCount, a.DeterministicMutated, "actuated mutations",
		sc.FalseActuationA7, fmt.Sprintf("%s (%d/%d suspicious-actor actuations)", pct(sc.FalseActuationA7), sc.SuspiciousA7, sc.MutatedCount), a7note),
		RateInterval(sc.SuspiciousA7, sc.MutatedCount)))
	cells = append(cells, BenchmarkCell{
		Axis: "A8", Name: "safety-violation count", Scope: site, NUnit: "not attributable to a site",
		Display: "see the estate row", Status: StatusNotAttributable,
		Note: "a breach is a GAP in the gap-free ledger seq (a deleted row names no site) and a breaker trip is the " +
			"deployment-wide kill; no per-site n exists, so A8 is gated once, estate-wide",
	})
	return cells
}

func intPtr(n int) *int { return &n }

// withInterval attaches a descriptive interval to a cell (nil leaves it without one).
func withInterval(c BenchmarkCell, iv *Interval) BenchmarkCell {
	c.Interval = iv
	return c
}

func orEmpty(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

// hostShares renders, per site, the largest single-host share of the site's mutated incidents — "k/n (host)" —
// so a soak that healed one guest thirty times reads as one guest, not as thirty heals.
func hostShares(bySite map[string]map[string]int) map[string]string {
	out := map[string]string{}
	for site, hosts := range bySite {
		total, best, bestHost := 0, 0, ""
		for h, n := range hosts {
			total += n
			if n > best || (n == best && h < bestHost) {
				best, bestHost = n, h
			}
		}
		if total > 0 {
			out[site] = fmt.Sprintf("%d/%d (%s)", best, total, bestHost)
		}
	}
	return out
}

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
	if r.Bar == string(BarG4A) {
		fmt.Fprintf(b, "TG benchmark-window report — G4A bar (accelerated): every axis A1–A8 n ≥ %d per site, one judge rubric, one harness\n", r.MinN)
		fmt.Fprintf(b, "fingerprint, one instrument identity, a gap-free ledger. The 14-day G4 duration and the data-span check are NOT conditions of\n")
		fmt.Fprintf(b, "this bar (the owner's 2026-11-07 instruction supersedes them for G4A only); both are reported. PASS means the EVIDENCE is\n")
		fmt.Fprintf(b, "sufficient, not that the values are good.\n\n")
		fmt.Fprintf(b, "window   [%s, %s)  %.1f days   (reported, not gated)\n", r.Since.UTC().Format("2006-01-02T15:04Z"),
			r.Until.UTC().Format("2006-01-02T15:04Z"), r.WindowDays)
	} else {
		fmt.Fprintf(b, "TG benchmark-window report — G4 bar: every axis A1–A8 n ≥ %d per site, one window ≥ %.0f days,\n", r.MinN, r.MinWindowDays)
		fmt.Fprintf(b, "one judge rubric, one harness fingerprint. PASS means the EVIDENCE is sufficient, not that the values are good.\n\n")
		fmt.Fprintf(b, "window   [%s, %s)  %.1f days   %s\n", r.Since.UTC().Format("2006-01-02T15:04Z"),
			r.Until.UTC().Format("2006-01-02T15:04Z"), r.WindowDays, mark(r.WindowOK))
	}
	fmt.Fprintf(b, "rubric   %s   %s   (A2 pooled under %q)\n", describeIdentities(r.RubricVersions), mark(r.RubricOK), r.ScoredRubric)
	fmt.Fprintf(b, "harness  %s   %s   (agent-loop incidents only)\n", describeIdentities(r.HarnessFPs), mark(r.HarnessOK))
	instrumentMark := "(reported, not gated by g4)"
	if r.Bar == string(BarG4A) {
		instrumentMark = mark(r.InstrumentOK)
	}
	fmt.Fprintf(b, "instrument %s   %s   (agent-loop sessions; core/instrumentid)\n", describeIdentities(r.InstrumentIDs), instrumentMark)
	if r.InstrumentColumnAbsent {
		fmt.Fprintf(b, "         session_triage.instrument_id does not exist in this database (before migration 0132): every session reads unrecorded\n")
	}
	fmt.Fprintf(b, "         outside the checks: deterministic heal lane n=%d; sessions that never composed a harness n=%d; infra-failed (no harness) n=%d\n",
		r.HarnessDeterministic, r.HarnessNotComposed, r.HarnessInfraFailed)
	fmt.Fprintf(b, "ledger   seq gaps (whole ledger): %d\n\n", r.LedgerGaps)

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

	fmt.Fprintf(b, "\ndisclosures (beside the verdict, never gating it)\n")
	fmt.Fprintf(b, "  estate topology snapshots published in the window: %d\n", r.EstateSnapshots)
	fmt.Fprintf(b, "  incidents synthetic/organic:")
	for _, site := range r.Sites {
		fmt.Fprintf(b, "  %s=%d/%d", site, r.SyntheticBySite[site], r.OrganicBySite[site])
	}
	fmt.Fprintf(b, "\n  largest single-host share of mutated incidents:")
	for _, site := range r.Sites {
		share := r.MutationHostShare[site]
		if share == "" {
			share = "none"
		}
		fmt.Fprintf(b, "  %s=%s", site, share)
	}
	fmt.Fprintf(b, "\n  injected fault classes:")
	for _, site := range r.Sites {
		classes := r.FaultClassesBySite[site]
		keys := make([]string, 0, len(classes))
		for k := range classes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, classes[k]))
		}
		fmt.Fprintf(b, "  %s[%s]", site, strings.Join(parts, " "))
	}
	fmt.Fprintf(b, "\n  intervals beside each rate cell are 95%% Wilson (two-sided) or exact one-sided bounds at k=0 / k=n: model-dependent, descriptive only\n")
	for _, u := range r.Uncertainties {
		fmt.Fprintf(b, "  uncertainty: %s\n", u)
	}

	fmt.Fprintf(b, "\n%s", cohortChecksText(r))
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
	if c.Interval != nil {
		fmt.Fprintf(b, "  %-4s %-24s %s\n", "", "", c.Interval.Text())
	}
	if c.Note != "" {
		fmt.Fprintf(b, "  %-4s %-24s %s\n", "", "", c.Note)
	}
}

// BenchmarkWindowBar reads everything the report needs for one fixed window and builds it against the bar: the population
// census (and with it the rubric the window was judged under), one scoped aggregate per declared site, and
// one estate-wide aggregate for the axes no site can claim. It is the seam `axisscore -benchmark-window`
// calls, kept here so it can be exercised end to end against a real database.
func BenchmarkWindowBar(ctx context.Context, store *db.AxisReadStore, bar Bar, since, until time.Time, binaryRubric string, excludeRefs ...string) (BenchmarkReport, error) {
	census, err := store.WindowCensus(ctx, since, until, excludeRefs...)
	if err != nil {
		return BenchmarkReport{}, err
	}
	rubric := WindowRubric(census, binaryRubric)
	perSite := map[string]db.AxisAgg{}
	for _, site := range ingest.DeclaredSites() {
		agg, err := store.AggregateScoped(ctx, db.AxisScope{Since: since, Until: until, Site: site, Rubric: rubric, ExcludeRefs: excludeRefs})
		if err != nil {
			return BenchmarkReport{}, fmt.Errorf("site %s: %w", site, err)
		}
		perSite[site] = agg
	}
	estate, err := store.AggregateScoped(ctx, db.AxisScope{Since: since, Until: until, Rubric: rubric, ExcludeRefs: excludeRefs})
	if err != nil {
		return BenchmarkReport{}, fmt.Errorf("estate: %w", err)
	}
	rep := BenchmarkBar(bar, census, rubric, perSite, estate)
	present, err := store.RefsPresent(ctx, excludeRefs, since, until)
	if err != nil {
		return BenchmarkReport{}, err
	}
	actions, err := store.ExecutedSessionActions(ctx, since, until)
	if err != nil {
		return BenchmarkReport{}, err
	}
	applyCohortChecks(&rep, excludeRefs, present, actions)
	return rep, nil
}
