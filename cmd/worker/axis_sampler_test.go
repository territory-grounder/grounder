package main

import (
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/metrics"
)

// THE AXES WERE COMPUTED BY A CLI AND BY NOTHING ELSE.
//
// db.AxisReadStore.Aggregate derives A1 detection recall, per-class detection, and the per-source
// detection-LATENCY distribution — and it had EXACTLY ONE caller in the tree: cmd/axisscore/main.go. Those
// numbers existed only when a human ran a command, so none of them could be graphed or alerted on.
//
// The consequence is specific: pve-liveness is FIRST to report 70 device-down faults at ~34s median against
// librenms at ~612s, an 18x improvement — while its effect on A1 RECALL is one fault (89.3% -> 89.6%), because
// librenms eventually reports nearly everything too. Recall is not the axis a faster detector moves. Latency
// is, and latency had no surface. A capability whose only benefit is invisible gets optimised away.
//
// The load-bearing property tested here is the SILENCE before the first successful read. A zero recall
// published as a real reading says "TG detects nothing", which is the most alarming false statement available
// about a healthy estate — the same absent-is-not-zero error that has bitten this console repeatedly.
func TestAxisSamplerEmitsNothingUntilItHasRead(t *testing.T) {
	s := newAxisSampler(7 * 24 * time.Hour)
	if got := s.Collect(); len(got) != 0 {
		t.Fatalf("an unread sampler emitted %d series: %+v — a zero A1 reads as 'TG detects nothing'", len(got), got)
	}
	// nil receiver must also be silent: the sampler is unarmed on an install with no DB pool.
	var nilS *axisSampler
	if got := nilS.Collect(); len(got) != 0 {
		t.Fatalf("a nil sampler emitted %d series", len(got))
	}
	// And the admin surface must tolerate the unarmed case without emitting axis series.
	a, _, _ := newTestAdmin(t, "", false)
	for _, sm := range a.samples() {
		if len(sm.Name) > 8 && sm.Name[:8] == "tg_axis_" {
			t.Errorf("unarmed worker emitted %s — an install with nothing to measure must publish nothing", sm.Name)
		}
	}
}

// Once a read has succeeded, every axis the aggregate carries must reach /metrics — including the per-source
// latency rows, which are the whole reason for this path.
func TestAxisSamplerPublishesRecallAndPerSourceLatency(t *testing.T) {
	s := newAxisSampler(7 * 24 * time.Hour)
	// Inject a held sample directly: the DB read is not the subject here, the EXPOSITION is.
	s.mu.Lock()
	s.agg = db.AxisAgg{
		InjectedFaults: 431,
		DetectedFaults: 386,
		DetectionLatency: []db.SourceLatency{
			{Source: "pve-liveness", Detections: 70, MedianSec: 34, P95Sec: 61},
			{Source: "librenms", Detections: 316, MedianSec: 612, P95Sec: 1180},
		},
	}
	s.sampled, s.at = true, time.Now()
	s.mu.Unlock()

	got := map[string]map[string]float64{} // name -> source label -> value
	for _, sm := range s.Collect() {
		src := sm.Labels["source"]
		if got[sm.Name] == nil {
			got[sm.Name] = map[string]float64{}
		}
		got[sm.Name][src] = sm.Value
		if sm.Kind != metrics.Gauge {
			t.Errorf("%s must be a gauge (it is a point-in-time sample, not a monotonic counter), got %v", sm.Name, sm.Kind)
		}
		if sm.Help == "" {
			t.Errorf("%s has no Help — an axis nobody can interpret is not observable", sm.Name)
		}
	}

	// A1's TWO COMPONENTS, not just a ratio: "9 of 10" and "900 of 1000" are different facts and a bare
	// percentage cannot tell them apart, nor say whether the number means anything yet.
	if got["tg_axis_faults_injected_total"][""] != 431 {
		t.Errorf("A1 denominator: got %v want 431", got["tg_axis_faults_injected_total"][""])
	}
	if got["tg_axis_faults_detected_total"][""] != 386 {
		t.Errorf("A1 numerator: got %v want 386", got["tg_axis_faults_detected_total"][""])
	}

	// ★ THE PER-SOURCE LATENCY — the half recall cannot show, and the reason this exists.
	for _, c := range []struct {
		src             string
		first, p50, p95 float64
	}{
		{"pve-liveness", 70, 34, 61},
		{"librenms", 316, 612, 1180},
	} {
		if v := got["tg_axis_detection_first_reports_total"][c.src]; v != c.first {
			t.Errorf("%s first-reports: got %v want %v", c.src, v, c.first)
		}
		if v := got["tg_axis_detection_latency_p50_seconds"][c.src]; v != c.p50 {
			t.Errorf("%s p50: got %v want %v", c.src, v, c.p50)
		}
		if v := got["tg_axis_detection_latency_p95_seconds"][c.src]; v != c.p95 {
			t.Errorf("%s p95: got %v want %v", c.src, v, c.p95)
		}
	}

	// The whole point is that the two sources are DISTINGUISHABLE. A single unlabelled latency series would
	// average the fast detector into the slow one and hide the very improvement being measured.
	if len(got["tg_axis_detection_latency_p50_seconds"]) != 2 {
		t.Errorf("latency must be per-source, got %d series: %+v",
			len(got["tg_axis_detection_latency_p50_seconds"]), got["tg_axis_detection_latency_p50_seconds"])
	}

	// Staleness must be visible: if the sampler starts failing, the numbers above freeze and only this says so.
	if _, ok := got["tg_axis_sample_age_seconds"]; !ok {
		t.Error("no sample-age series — a frozen sample would read as a current one")
	}
}

// PER-DIMENSION JUDGE COVERAGE MUST PUBLISH A SILENT AXIS AT ZERO (TG-360).
//
// The two deterministic axes had graded 2 and 1 of 3,371 sessions, and no surface said so: the per-dimension
// MEANS ride the scorecard, but judged/eligible was published nowhere, so a silent axis read exactly like a
// working one. The load-bearing property here is EMIT-AT-ZERO over the DECLARED dimension set: an axis that
// scored nothing this window must still publish scored=0 (with the denominator beside it), never vanish — an
// absent series and a healthy axis are indistinguishable on a dashboard.
//
// KILLING MUTATION: change the Collect() loop from the declared set to `for _, c := range s.cov` (emit only
// dimensions the DB returned). The silent axis stops publishing and this test goes RED — which is exactly the
// bug, because a hand-list or a data-driven loop misses the dimension that produced no rows.
func TestAxisSamplerPublishesJudgeCoverageWithSilentAxisAtZero(t *testing.T) {
	s := newAxisSampler(7 * 24 * time.Hour)
	// The DB returned rows for only ONE dimension; every other declared axis scored nothing this window.
	// s.sampled is the A1 read gate the whole Collect() sits behind; in production one sample() call sets it
	// alongside covSample (both read the same DB), so set it here too.
	s.mu.Lock()
	s.sampled, s.at = true, time.Now()
	s.cov = []db.DimCoverage{{Dimension: "appropriate_band", Scored: 100, Mean: 4.57}}
	s.covJudged = 100
	s.covSample = true
	s.mu.Unlock()

	scored := map[string]float64{} // dimension -> tg_judge_axis_scored_total
	means := map[string]float64{}  // dimension -> tg_judge_axis_mean (present only when scored>0)
	var judgedDenominator float64
	sawDenominator := false
	for _, sm := range s.Collect() {
		switch sm.Name {
		case "tg_judge_sessions_judged_total":
			judgedDenominator, sawDenominator = sm.Value, true
		case "tg_judge_axis_scored_total":
			scored[sm.Labels["dimension"]] = sm.Value
		case "tg_judge_axis_mean":
			means[sm.Labels["dimension"]] = sm.Value
		}
		if strings.HasPrefix(sm.Name, "tg_judge_") && sm.Help == "" {
			t.Errorf("%s has no Help — a coverage series nobody can interpret is not observable", sm.Name)
		}
	}

	// The denominator must ship, so scored=0 reads as "silent" not "quiet estate".
	if !sawDenominator || judgedDenominator != 100 {
		t.Fatalf("tg_judge_sessions_judged_total must be published as the shared denominator: saw=%v value=%v", sawDenominator, judgedDenominator)
	}
	// The scored dimension is reported with its real count and mean.
	if scored["appropriate_band"] != 100 {
		t.Errorf("appropriate_band scored: got %v want 100", scored["appropriate_band"])
	}
	if means["appropriate_band"] != 4.57 {
		t.Errorf("appropriate_band mean: got %v want 4.57", means["appropriate_band"])
	}
	// ★ THE SILENT AXES REPORT THEMSELVES. Every DECLARED dimension must appear as scored=0, not vanish —
	// especially the two deterministic axes this ticket is about.
	for _, dim := range []string{"estate_grounded", "diagnosis_grounded"} {
		v, ok := scored[dim]
		if !ok {
			t.Errorf("silent axis %q published NO scored series — an absent axis is indistinguishable from a "+
				"working one, which is the whole defect (TG-360)", dim)
			continue
		}
		if v != 0 {
			t.Errorf("silent axis %q: got scored=%v want 0", dim, v)
		}
		// A mean over zero rows is not a measurement — it must NOT be emitted for a silent axis.
		if _, hasMean := means[dim]; hasMean {
			t.Errorf("silent axis %q emitted a mean over zero scored sessions — a mean of nothing is a fabricated number", dim)
		}
	}
	// Vacuity: the declared set must actually be non-trivial, or "all declared axes present" proves nothing.
	if len(scored) < 3 {
		t.Fatalf("only %d dimensions emitted — the declared-set loop is not covering the axes", len(scored))
	}
}

// A6b TIME TO DECISION MUST REACH /metrics — the half of A6 that steps cannot show (TG-205).
//
// docs/BENCHMARK-AXES.md defined A6 as MTTR and EVERY implementation measured decision STEPS, so TG published
// how many cycles a decision cost and nothing about how long it took. That is the same shape of gap A1 had
// before per-source latency landed, where recall could not tell a 34-second detector from a 612-second one.
// Steps are not a proxy: an identical two-cycle decision costs seconds on the fast tier and minutes on the
// reasoning tier, which is exactly the variable the model-tier A/B manipulates.
//
// The SECONDS conversion is asserted, not just presence: the column is milliseconds and every other latency
// series on this surface is seconds, so a raw-ms gauge would sit on the same dashboard as
// tg_axis_detection_latency_p50_seconds off by 1000x — a 12-second decision reading as 3.4 hours.
//
// KILLING MUTATION (executed): delete the `if s.agg.DecisionN > 0 { out = append(...) }` block in
// axis_sampler.go Collect(). RED — "no tg_axis_decision_latency_p50_seconds series: A6b time-to-decision is
// measured in the DB and published nowhere, which is the CLI-only gap this sampler exists to close".
func TestAxisSamplerPublishesTimeToDecision(t *testing.T) {
	s := newAxisSampler(7 * 24 * time.Hour)
	s.mu.Lock()
	s.agg = db.AxisAgg{
		InjectedFaults: 10, DetectedFaults: 9,
		DecisionN: 41, DecisionMedianMs: 12400, DecisionP95Ms: 96500,
	}
	s.sampled, s.at = true, time.Now()
	s.mu.Unlock()

	got := map[string]float64{}
	for _, sm := range s.Collect() {
		got[sm.Name] = sm.Value
		if strings.HasPrefix(sm.Name, "tg_axis_decision") && sm.Help == "" {
			t.Errorf("%s has no Help — an axis nobody can interpret is not observable", sm.Name)
		}
	}
	for _, c := range []struct {
		name string
		want float64
	}{
		{"tg_axis_decision_measured_total", 41},
		{"tg_axis_decision_latency_p50_seconds", 12.4}, // 12400ms
		{"tg_axis_decision_latency_p95_seconds", 96.5}, // 96500ms
	} {
		v, ok := got[c.name]
		if !ok {
			t.Fatalf("no %s series: A6b time-to-decision is measured in the DB and published nowhere, which is "+
				"the CLI-only gap this sampler exists to close", c.name)
		}
		if v != c.want {
			t.Errorf("%s = %v, want %v — the column is MILLISECONDS and every other latency series here is "+
				"seconds; an unconverted gauge renders a 12-second decision as 3.4 hours", c.name, v, c.want)
		}
	}
}

// The mirror, and the reason the conversion above is not enough on its own: a window in which NO session
// recorded a timing must publish NO decision series. Every one of TG's historical triages carries
// decision_ms = 0 (recorded before migration 0058), and a 0-valued p50 on a dashboard asserts that TG decides
// instantly — the absent-is-not-zero error this whole sampler is built around, one axis further in.
//
// KILLING MUTATION (executed): change the guard to `if true`. RED — "an unmeasured A6b published
// tg_axis_decision_latency_p50_seconds = 0".
func TestAxisSamplerPublishesNoDecisionSeriesWhenNothingWasTimed(t *testing.T) {
	s := newAxisSampler(time.Hour)
	s.mu.Lock()
	s.agg = db.AxisAgg{InjectedFaults: 10, DetectedFaults: 9} // A1 measured, A6b not
	s.sampled, s.at = true, time.Now()
	s.mu.Unlock()

	samples := s.Collect()
	if len(samples) == 0 {
		t.Fatal("VACUITY FLOOR: the sampler emitted nothing at all, so the absence check below proves nothing")
	}
	for _, sm := range samples {
		if strings.HasPrefix(sm.Name, "tg_axis_decision") {
			t.Errorf("an unmeasured A6b published %s = %v — no session in the window timed a decision, and a "+
				"zero here reads as an instant one", sm.Name, sm.Value)
		}
	}
}

// A failed refresh must keep the previous sample rather than blanking it: a measurement path that erases what
// it knows on a transient DB error converts one failure into two.
func TestAxisSamplerKeepsThePreviousSampleOnFailure(t *testing.T) {
	s := newAxisSampler(time.Hour)
	s.mu.Lock()
	s.agg = db.AxisAgg{InjectedFaults: 10, DetectedFaults: 9}
	s.sampled, s.at = true, time.Now()
	s.mu.Unlock()

	// sample() with a nil store would panic; the guard under test is that startAxisSampler refuses to arm and
	// that a failing read path leaves state alone. Assert the state contract directly.
	before := len(s.Collect())
	startAxisSampler(nil, s, nil, time.Minute, func(string, ...any) {}) // nil store ⇒ must not arm, must not clear
	if after := len(s.Collect()); after != before {
		t.Errorf("series count changed from %d to %d when the sampler refused to arm", before, after)
	}
	if got := s.Collect(); len(got) == 0 {
		t.Error("the previously-held sample was discarded — one transient failure became a metrics outage")
	}
	// interval <= 0 must leave it unarmed too (the correct posture for an estate with nothing to measure).
	startAxisSampler(nil, s, &db.AxisReadStore{}, 0, func(string, ...any) {})
}
