package wiring

import (
	"strings"
	"sync"
	"testing"
	"time"
)

var yieldNow = time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)

func yieldFindingFor(t *testing.T, findings []YieldFinding, s Seam) (YieldFinding, bool) {
	t.Helper()
	for _, f := range findings {
		if f.Seam == s {
			return f, true
		}
	}
	return YieldFinding{}, false
}

// THE REGISTER'S OWN VACUITY FLOOR, and the single most important property here.
//
// A register that nobody instruments must report EVERY seam as UNCOVERED, not as healthy. Without this,
// standing the register up and wiring zero Observe calls would produce a clean report over a system it
// never looked at — which is precisely the failure mode the register exists to detect, reproduced in the
// detector. The manifest earns its trust the same way (a seam no branch records reports dark-unrecorded).
//
// KILLING MUTATION: in Report, range over r.flow instead of All(). RED.
func TestYieldRegisterReportsEverySeamAsUncoveredWhenNothingObserves(t *testing.T) {
	r := NewYieldRegister()
	findings, samples := r.Report(yieldNow)

	specs := All()
	if len(specs) == 0 {
		t.Fatal("vacuity floor: the closed seam set is EMPTY, so this test certifies nothing")
	}
	if len(findings) != len(specs) {
		t.Fatalf("an uninstrumented register reported %d finding(s) over %d declared seam(s) — a seam that "+
			"nothing measures must read as UNCOVERED, never as fine", len(findings), len(specs))
	}
	for _, f := range findings {
		if f.State != YieldUnobserved {
			t.Errorf("seam %s reported %s with no observations, want unobserved", f.Seam, f.State)
		}
	}
	// A nil register must behave identically rather than panic: an unwired register is uncovered, not safe.
	if nilFindings, _ := (*YieldRegister)(nil).Report(yieldNow); len(nilFindings) != len(specs) {
		t.Fatalf("a nil register reported %d finding(s), want %d — an absent register must read as fully "+
			"uncovered", len(nilFindings), len(specs))
	}
	if len(samples) == 0 {
		t.Fatal("no samples emitted — the pair cannot be read from outside the process")
	}
}

// THE ALARM. Work offered, nothing produced: the seam is wired, running, and emitting nothing.
//
// KILLING MUTATION: in Report, drop the `case f.offered > 0: st = YieldStarved` arm so it falls to idle. RED.
func TestYieldStarvedIsWorkOfferedAndNothingProduced(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamSuppression, 500, 0, yieldNow)

	findings, _ := r.Report(yieldNow)
	f, ok := yieldFindingFor(t, findings, SeamSuppression)
	if !ok {
		t.Fatal("a seam offered 500 units that produced nothing was not reported at all")
	}
	if f.State != YieldStarved {
		t.Fatalf("state = %s, want starved — 500 offered and 0 produced is a broken lane, not an idle one", f.State)
	}
	if f.Offered != 500 || f.Produced != 0 {
		t.Fatalf("counts = %d/%d, want 500/0", f.Offered, f.Produced)
	}
	// The line must carry BOTH numbers and the seam's own consequence prose — a bare "starved" tells an
	// operator nothing they can act on.
	line := f.Reason()
	for _, want := range []string{"500", "starved", "alerts admitted"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report line is missing %q: %s", want, line)
		}
	}
	if f.Consequence == "" {
		t.Error("a starved seam must carry the same consequence prose an unbound one does")
	}
}

// IDLE IS NOT STARVED, and conflating them is what would make this register cry wolf on a quiet estate.
// Nothing offered means nothing arrived — the seam has done nothing wrong.
//
// KILLING MUTATION: treat produced == 0 as starved regardless of offered. RED.
func TestYieldIdleIsNotStarved(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamWikiCompile, 0, 0, yieldNow)

	findings, _ := r.Report(yieldNow)
	if f, ok := yieldFindingFor(t, findings, SeamWikiCompile); ok {
		t.Fatalf("an idle seam was reported as a finding (%s) — a register that alarms on a quiet estate "+
			"gets muted, and then it cannot report the real starvation either", f.State)
	}
}

// Flowing seams are not findings, but their numbers are still PUBLISHED — the pair is how a filter that
// quietly stops matching becomes visible from outside the code.
func TestYieldFlowingPublishesBothNumbersWithoutAlarming(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamGovNotify, 40, 12, yieldNow)

	findings, samples := r.Report(yieldNow)
	if f, ok := yieldFindingFor(t, findings, SeamGovNotify); ok {
		t.Fatalf("a producing seam was reported as a finding (%s)", f.State)
	}

	var offered, produced float64
	var sawOffered, sawProduced bool
	for _, s := range samples {
		if s.Labels["seam"] != string(SeamGovNotify) {
			continue
		}
		switch s.Name {
		case "tg_wiring_seam_offered_total":
			offered, sawOffered = s.Value, true
		case "tg_wiring_seam_produced_total":
			produced, sawProduced = s.Value, true
		}
	}
	if !sawOffered || !sawProduced {
		t.Fatal("a seam must publish BOTH numbers — one alone cannot distinguish 'nothing arrived' from " +
			"'everything arrived and was dropped'")
	}
	if offered != 40 || produced != 12 {
		t.Fatalf("published %v offered / %v produced, want 40/12 — the 28-unit gap is exactly the fact a "+
			"WHERE clause would otherwise swallow", offered, produced)
	}
}

// A partial yield is NOT an alarm. Filtering is most seams' job, and alarming on every gap would make the
// register useless. The gap is published; the judgement is the operator's.
func TestYieldPartialIsPublishedNotAlarmed(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamSuppression, 1000, 3, yieldNow)
	findings, _ := r.Report(yieldNow)
	if f, ok := yieldFindingFor(t, findings, SeamSuppression); ok {
		t.Fatalf("a seam producing 3 of 1000 was ALARMED as %s — suppression legitimately drops most of "+
			"what it sees, and an alarm here trains an operator to ignore the register", f.State)
	}
}

// Every seam in the closed set must declare a unit, or its findings render as bare integers an operator
// cannot act on — the "gov.notify: dark" failure the Consequence field already exists to prevent.
func TestEverySeamDeclaresAYieldUnit(t *testing.T) {
	specs := All()
	if len(specs) == 0 {
		t.Fatal("vacuity floor: the closed seam set is empty")
	}
	for _, sp := range specs {
		if strings.TrimSpace(sp.Unit.Offered) == "" || strings.TrimSpace(sp.Unit.Produced) == "" {
			t.Errorf("seam %s declares no yield unit (offered=%q produced=%q) — its starvation finding "+
				"would read as two unexplained integers", sp.ID, sp.Unit.Offered, sp.Unit.Produced)
		}
	}
}

// Counts accumulate across passes: a lane that produced once at boot and nothing for six hours must not
// read as healthy on the strength of that one row.
func TestYieldAccumulatesAcrossPassesAndTracksLastProduction(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamWorldDiscovery, 10, 2, yieldNow)
	r.Observe(SeamWorldDiscovery, 10, 0, yieldNow.Add(time.Hour))
	r.Observe(SeamWorldDiscovery, 10, 0, yieldNow.Add(2*time.Hour))

	findings, samples := r.Report(yieldNow.Add(3 * time.Hour))
	if f, ok := yieldFindingFor(t, findings, SeamWorldDiscovery); ok {
		t.Fatalf("a seam that has produced is not starved: %s", f.State)
	}
	var offered, produced float64
	for _, s := range samples {
		if s.Labels["seam"] != string(SeamWorldDiscovery) {
			continue
		}
		if s.Name == "tg_wiring_seam_offered_total" {
			offered = s.Value
		}
		if s.Name == "tg_wiring_seam_produced_total" {
			produced = s.Value
		}
	}
	if offered != 30 || produced != 2 {
		t.Fatalf("accumulated %v/%v, want 30/2", offered, produced)
	}
	// lastProducedAt is what makes "produced once, three hours ago" answerable at all.
	r.mu.Lock()
	last := r.flow[SeamWorldDiscovery].lastProducedAt
	r.mu.Unlock()
	if !last.Equal(yieldNow) {
		t.Fatalf("lastProducedAt = %v, want the FIRST pass %v — a later zero-yield pass must not advance it", last, yieldNow)
	}
}

// A miscounting caller must not be able to drive a total backwards and manufacture a clean report.
func TestYieldClampsNegativeCounts(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamLessonsFeed, 10, 5, yieldNow)
	r.Observe(SeamLessonsFeed, -100, -100, yieldNow)
	r.mu.Lock()
	f := *r.flow[SeamLessonsFeed]
	r.mu.Unlock()
	if f.offered != 10 || f.produced != 5 {
		t.Fatalf("negative counts moved the totals to %d/%d — a caller bug must not be able to erase "+
			"evidence of starvation", f.offered, f.produced)
	}
}

// The register is read by a periodic reporter while lanes write to it from their own goroutines.
func TestYieldRegisterIsConcurrencySafe(t *testing.T) {
	r := NewYieldRegister()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Observe(SeamGovNotify, 2, 1, yieldNow) }()
		wg.Add(1)
		go func() { defer wg.Done(); r.Report(yieldNow) }()
	}
	wg.Wait()
	r.mu.Lock()
	f := *r.flow[SeamGovNotify]
	r.mu.Unlock()
	if f.offered != 100 || f.produced != 50 {
		t.Fatalf("lost writes under concurrency: %d/%d, want 100/50", f.offered, f.produced)
	}
}

// The rendered block must name both failure modes distinctly; "uncovered" and "starved" call for
// different actions (instrument it vs fix it) and collapsing them wastes the distinction.
func TestYieldReportTextSeparatesStarvedFromUncovered(t *testing.T) {
	r := NewYieldRegister()
	r.Observe(SeamSuppression, 500, 0, yieldNow) // starved
	r.Observe(SeamGovNotify, 3, 3, yieldNow)     // flowing
	findings, _ := r.Report(yieldNow)

	txt := YieldReportText(findings)
	if !strings.Contains(txt, "1 starved") {
		t.Errorf("the summary does not count starved seams: %s", txt)
	}
	if !strings.Contains(txt, "uncovered") {
		t.Errorf("the summary does not count uncovered seams: %s", txt)
	}
	if strings.Contains(txt, string(SeamGovNotify)+": flowing") {
		t.Error("a flowing seam appeared in the findings block — the report is for what needs action")
	}
	if YieldReportText(nil) != "" {
		t.Error("an empty finding set must render nothing, not an empty header")
	}
}

// ObserveTotals SETS absolute totals; it must not accumulate them.
//
// The suppression gate exposes cumulative counters, and the reporting loop reads them every tick. If
// ObserveTotals added rather than set, each tick would re-add the entire history and the totals would
// climb quadratically — a metric that looks healthier the longer it runs, which is the exact direction
// this register exists to refuse.
//
// KILLING MUTATION: change ObserveTotals to `f.offered += offered` / `f.produced += produced`. RED.
func TestObserveTotalsSetsRatherThanAccumulates(t *testing.T) {
	r := NewYieldRegister()
	// The same cumulative reading, seen on five consecutive reporting ticks.
	for i := 0; i < 5; i++ {
		r.ObserveTotals(SeamSuppression, 5000, 12, yieldNow.Add(time.Duration(i)*time.Minute))
	}
	r.mu.Lock()
	f := *r.flow[SeamSuppression]
	r.mu.Unlock()
	if f.offered != 5000 || f.produced != 12 {
		t.Fatalf("five ticks of the SAME cumulative reading produced %d/%d, want 5000/12 — the totals are "+
			"accumulating, so the seam reads healthier the longer the worker runs", f.offered, f.produced)
	}
	// A genuinely higher reading advances.
	r.ObserveTotals(SeamSuppression, 6000, 15, yieldNow.Add(time.Hour))
	r.mu.Lock()
	f = *r.flow[SeamSuppression]
	r.mu.Unlock()
	if f.offered != 6000 || f.produced != 15 {
		t.Fatalf("a higher reading did not advance the totals: %d/%d", f.offered, f.produced)
	}
}

// Totals never wind BACKWARD. A counter reset — a restarted sub-component, a re-created gate — must not
// be able to erase evidence that the seam was starved.
func TestObserveTotalsNeverWindsBackward(t *testing.T) {
	r := NewYieldRegister()
	r.ObserveTotals(SeamSuppression, 5000, 12, yieldNow)
	r.ObserveTotals(SeamSuppression, 3, 0, yieldNow.Add(time.Hour)) // the counter reset
	r.mu.Lock()
	f := *r.flow[SeamSuppression]
	r.mu.Unlock()
	if f.offered != 5000 || f.produced != 12 {
		t.Fatalf("a counter reset wound the totals back to %d/%d — a component that forgets must not be "+
			"able to erase the record of what it did", f.offered, f.produced)
	}
}

// A seam observed only through ObserveTotals must still reach STARVED: 5,000 alerts admitted and zero
// suppressed is the vacuous-predicate case this seam's unit was rewritten to catch.
func TestObserveTotalsCanReportStarved(t *testing.T) {
	r := NewYieldRegister()
	r.ObserveTotals(SeamSuppression, 5000, 0, yieldNow)
	findings, _ := r.Report(yieldNow)
	f, ok := yieldFindingFor(t, findings, SeamSuppression)
	if !ok || f.State != YieldStarved {
		t.Fatalf("5000 admitted and 0 suppressed did not report starved (found=%v state=%v) — a configured "+
			"chain that matches nothing is the defect this unit exists to name", ok, f.State)
	}
}
