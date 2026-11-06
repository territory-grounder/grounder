package diagcorpus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE CORPUS IS ONLY EVIDENCE IF ITS EXCLUSIONS ARE VISIBLE.
//
// The roadmap treated P5's label pass as the phase's schedule bottleneck. It is not: the fault injector
// durably records WHAT it broke, so an item's correct answer is known without anyone reading it — 726
// joinable items live, against a required 100. The risk therefore moves from "can we get labels" to "does the
// scoring quietly flatter". These tests are about the second.

func item(ref, fault, proposed string) Item {
	return Item{ExternalRef: ref, Host: "h", AlertRule: "r", FaultType: fault, Proposed: proposed, At: time.Now()}
}

func rules() Ruleset {
	return Ruleset{Expectations: []Expectation{
		{FaultType: "device-down", Accept: []string{"start-guest"}},
		{FaultType: "disk-fill", Accept: []string{"disk-grow"}},
		{FaultType: "service-down", Accept: []string{"start-service", "restart-service"}},
		{FaultType: "mem-pressure", Unhealable: true},
	}}
}

// MISSED AND WRONG ARE DIFFERENT FAILURES. Standing down is not the same as confidently proposing the wrong
// fix, and pooling them hides which one is improving. Missed is the LARGEST bucket in the live data.
func TestMissedAndWrongAreScoredSeparately(t *testing.T) {
	r := Build([]Item{
		item("a", "device-down", "start-guest"), // correct
		item("b", "device-down", ""),            // missed
		item("c", "device-down", "disk-grow"),   // wrong
	}, rules())
	if r.Correct != 1 || r.Missed != 1 || r.Wrong != 1 {
		t.Fatalf("want 1/1/1 correct/missed/wrong, got %d/%d/%d", r.Correct, r.Missed, r.Wrong)
	}
	if r.Scored != 3 {
		t.Errorf("all three are scoreable, got Scored=%d", r.Scored)
	}
}

// MORE THAN ONE ANSWER CAN BE RIGHT. A down service may validly be started OR restarted; scoring one of them
// wrong would manufacture a diagnosis failure that is really a vocabulary preference.
func TestMultipleAcceptedOpClassesAllScoreCorrect(t *testing.T) {
	r := Build([]Item{
		item("a", "service-down", "start-service"),
		item("b", "service-down", "restart-service"),
	}, rules())
	if r.Correct != 2 {
		t.Fatalf("both accepted op-classes must score correct, got %d (wrong=%d)", r.Correct, r.Wrong)
	}
}

// AN UNHEALABLE CLASS IS EXCLUDED, NOT FAILED. mem-pressure has a measured 1/14 detection rate on this
// estate; counting its misses as diagnosis failures would score an INSTRUMENTATION gap as a capability gap —
// exactly the confusion this project keeps having to undo.
func TestUnhealableClassIsExcludedNotCountedAsFailure(t *testing.T) {
	r := Build([]Item{
		item("a", "mem-pressure", ""),
		item("b", "device-down", "start-guest"),
	}, rules())
	if r.Excluded != 1 {
		t.Fatalf("the unhealable item must be excluded, got Excluded=%d", r.Excluded)
	}
	if r.Missed != 0 {
		t.Errorf("an unhealable class must NOT be counted as a miss; got Missed=%d", r.Missed)
	}
	if r.Rate.N != 1 {
		t.Errorf("the denominator must exclude it too, got N=%d", r.Rate.N)
	}
	if len(r.ExcludedReasons) == 0 {
		t.Error("an exclusion must be auditable — the reason has to be recorded")
	}
}

// AN UNDECLARED FAULT CLASS IS EXCLUDED WITH ITS REASON, never silently scored against a guess.
func TestUndeclaredFaultClassIsExcludedWithAReason(t *testing.T) {
	r := Build([]Item{item("a", "novel-fault", "start-guest")}, rules())
	if r.Excluded != 1 || r.Scored != 0 {
		t.Fatalf("an undeclared class must be excluded, got excluded=%d scored=%d", r.Excluded, r.Scored)
	}
	var found bool
	for k := range r.ExcludedReasons {
		if strings.Contains(k, "novel-fault") {
			found = true
		}
	}
	if !found {
		t.Errorf("the reason must name the class, got %v", r.ExcludedReasons)
	}
}

// EVERY NUMBER CARRIES ITS DENOMINATOR AND AN INTERVAL, and the render states what is absent.
func TestRenderCarriesDenominatorsIntervalAndScopeCaveat(t *testing.T) {
	r := Build([]Item{
		item("a", "device-down", "start-guest"),
		item("b", "device-down", ""),
	}, rules())
	out := r.Render()
	for _, want := range []string{"1/2", "95% CI", "INJECTED faults only"} {
		if !strings.Contains(out, want) {
			t.Errorf("the scorecard must contain %q; got:\n%s", want, out)
		}
	}
}

// n=0 is UNDEFINED, not 0% — a corpus with nothing scoreable must not publish a diagnosis rate.
func TestEmptyCorpusReportsUndefinedNotZero(t *testing.T) {
	r := Build(nil, rules())
	if r.Rate.Defined {
		t.Errorf("an empty corpus must report an UNDEFINED rate, got %v", r.Rate.Value)
	}
	if !strings.Contains(r.Render(), "UNDEFINED") {
		t.Errorf("the scorecard must say so; got:\n%s", r.Render())
	}
}

// THE JUDGE PROJECTION DROPS UNJUDGED ITEMS rather than assuming. Defaulting either way would manufacture
// agreement or disagreement out of missing data.
func TestJudgeProjectionDropsUnjudgedItemsInsteadOfAssuming(t *testing.T) {
	items := []Item{
		item("a", "device-down", "start-guest"), // correct, judged true
		item("b", "device-down", ""),            // missed, judged false
		item("c", "device-down", "start-guest"), // correct, NOT judged
		item("d", "mem-pressure", ""),           // excluded entirely
	}
	out := JudgeOutcomes(items, rules(), map[string]bool{"a": true, "b": false})
	if len(out) != 2 {
		t.Fatalf("only judged, non-excluded items may enter calibration, got %d", len(out))
	}
	if !out[0].Truth || !out[0].Judge {
		t.Errorf("item a is correct and judged true: %+v", out[0])
	}
	if out[1].Truth || out[1].Judge {
		t.Errorf("item b is a miss and judged false: %+v", out[1])
	}
}

// PER-CLASS BREAKDOWN, because a pooled rate hides a class failing completely — and the estate's fault mix is
// heavily skewed, so one dominant class would otherwise decide the headline number by itself.
func TestPerClassBreakdownExposesACollapseThePooledRateHides(t *testing.T) {
	var items []Item
	for i := 0; i < 40; i++ {
		items = append(items, item("d", "device-down", "start-guest")) // perfect, dominant class
	}
	for i := 0; i < 5; i++ {
		items = append(items, item("f", "disk-fill", "")) // total failure, small class
	}
	r := Build(items, rules())
	if r.Rate.Value < 0.85 {
		t.Fatalf("the pooled rate should look GOOD (that is the point), got %v", r.Rate.Value)
	}
	df := r.ByFaultType["disk-fill"]
	if df.Correct != 0 || df.Total != 5 {
		t.Fatalf("disk-fill must be broken out as 0/5, got %+v", df)
	}
	if df.Rate.Value != 0 {
		t.Errorf("the failing class must show its own 0%% rather than hide in the pool, got %v", df.Rate.Value)
	}
}

// MUTATION CONTROL. The Accept list is only load-bearing if it actually decides the verdict. Score the SAME
// item against a ruleset that accepts its proposal and one that does not — the verdict must flip.
func TestMutationControl_TheAcceptListDecidesTheVerdict(t *testing.T) {
	it := item("x", "device-down", "start-guest")
	accepts := Ruleset{Expectations: []Expectation{{FaultType: "device-down", Accept: []string{"start-guest"}}}}
	rejects := Ruleset{Expectations: []Expectation{{FaultType: "device-down", Accept: []string{"disk-grow"}}}}
	if Score(it, accepts) != Correct {
		t.Fatal("baseline: the accepted op-class must score correct")
	}
	if Score(it, rejects) == Correct {
		t.Fatal("the SAME item scored correct against a ruleset that does not accept it — the declared " +
			"expectation is being ignored and every assertion above is vacuous")
	}
	t.Log("mutation control holds: the operator-declared Accept list decides the verdict, not compiled opinion")
}

// standDownRuleset declares a class with NO applicable op-class, where the correct answer is to decline.
func standDownRuleset() Ruleset {
	return Ruleset{Expectations: []Expectation{
		{FaultType: "disk-fill", StandDownIsCorrect: true},
		{FaultType: "device-down", Accept: []string{"start-guest"}},
	}}
}

// TestStandDownIsCorrectInvertsThePolarity is the regression test for the sign inversion that the first live
// run produced: with `accept: [disk-grow]`, 63 correct stand-downs scored as MISSED and 30 inapplicable
// proposals scored as CORRECT. The instrument reported 31.6% for a class TG was mostly getting right.
func TestStandDownIsCorrectInvertsThePolarity(t *testing.T) {
	rs := standDownRuleset()

	if got := Score(Item{FaultType: "disk-fill", Proposed: ""}, rs); got != CorrectStandDown {
		t.Fatalf("declining a fault with no applicable op-class must be CORRECT, got %s", got)
	}
	if got := Score(Item{FaultType: "disk-fill", Proposed: "disk-grow"}, rs); got != Wrong {
		t.Fatalf("proposing an inapplicable remedy must be WRONG, got %s", got)
	}
	// The polarity is per-class, not global: a class WITH a remedy must still score the old way.
	if got := Score(Item{FaultType: "device-down", Proposed: ""}, rs); got != Missed {
		t.Fatalf("declining a fault that HAS a remedy is still a miss, got %s", got)
	}
}

// TestStandDownCountsInTheRateButIsReportedSeparately guards the honesty property: a stand-down is correct,
// so it belongs in the numerator — but a class carried by declining must never render as one carried by
// healing. The MUTATION CONTROL is at the bottom: pooling the two counts makes the assertion go red.
func TestStandDownCountsInTheRateButIsReportedSeparately(t *testing.T) {
	rs := standDownRuleset()
	items := []Item{
		{FaultType: "disk-fill", Proposed: ""},              // correct stand-down
		{FaultType: "disk-fill", Proposed: ""},              // correct stand-down
		{FaultType: "disk-fill", Proposed: "disk-grow"},     // wrong
		{FaultType: "device-down", Proposed: "start-guest"}, // correct remedy
	}
	rep := Build(items, rs)

	if rep.StoodDown != 2 || rep.Correct != 1 {
		t.Fatalf("want 2 stood-down and 1 proposed-remedy, got StoodDown=%d Correct=%d", rep.StoodDown, rep.Correct)
	}
	if rep.Wrong != 1 {
		t.Fatalf("want 1 wrong, got %d", rep.Wrong)
	}
	// 3 of 4 correct overall: both stand-downs plus the one real remedy.
	if rep.Rate.Value != 0.75 {
		t.Fatalf("stand-downs must count toward the rate: want 0.75, got %v", rep.Rate.Value)
	}
	// MUTATION CONTROL: had Build pooled stand-downs into Correct, this would read 3 and go red.
	if rep.Correct == rep.Correct+rep.StoodDown {
		t.Fatal("Correct must EXCLUDE stand-downs — pooling them hides that a class was carried by declining")
	}

	out := rep.Render()
	if !strings.Contains(out, "correctly stood down: 2") {
		t.Fatalf("render must state the stand-down count explicitly:\n%s", out)
	}
	if !strings.Contains(out, "NO APPLICABLE OP-CLASS") {
		t.Fatalf("a stand-down class must be labelled so its rate is not read as a heal rate:\n%s", out)
	}
}

// TestContradictoryExpectationsAreRejected — a ruleset that says two incompatible things is refused at load
// rather than resolved by precedence, because the scorecard reports the verdict, never the rule behind it.
func TestContradictoryExpectationsAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"stand-down AND unhealable": `{"expectations":[{"fault_type":"x","stand_down_is_correct":true,"unhealable":true}]}`,
		"stand-down WITH accept":    `{"expectations":[{"fault_type":"x","stand_down_is_correct":true,"accept":["y"]}]}`,
		"accepts nothing at all":    `{"expectations":[{"fault_type":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "e.json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadRuleset(p); err == nil {
				t.Fatal("a contradictory expectation must be REFUSED, not silently resolved")
			}
		})
	}
}

// TestShippedExpectationsLoad keeps the shipped ruleset honest against its own validator — the file is data,
// so nothing else would catch a contradiction introduced by editing it.
func TestShippedExpectationsLoad(t *testing.T) {
	rs, err := LoadRuleset("expectations.json")
	if err != nil {
		t.Fatalf("the shipped expectations must satisfy their own validator: %v", err)
	}
	e, ok := rs.find("disk-fill")
	if !ok || !e.StandDownIsCorrect {
		t.Fatal("disk-fill must declare stand_down_is_correct: no op-class can grow a loop-mounted LXC rootfs")
	}
}

// TestRenderedFractionEqualsItsOwnPercentage — the headline numerator must be the quantity the rate was
// computed from. The first live run printed "471/730 = 73.2%", which is arithmetically false: the numerator
// had dropped the stand-downs while the percentage kept them. A number that contradicts itself on the page
// discredits every other number beside it.
func TestRenderedFractionEqualsItsOwnPercentage(t *testing.T) {
	rep := Build([]Item{
		{FaultType: "disk-fill", Proposed: ""},
		{FaultType: "disk-fill", Proposed: ""},
		{FaultType: "disk-fill", Proposed: "disk-grow"},
		{FaultType: "device-down", Proposed: "start-guest"},
	}, standDownRuleset())

	var num, den int
	if _, err := fmt.Sscanf(strings.TrimSpace(strings.SplitN(
		strings.SplitN(rep.Render(), "correct diagnosis: ", 2)[1], " ", 2)[0]), "%d/%d", &num, &den); err != nil {
		t.Fatalf("could not parse the headline fraction: %v", err)
	}
	if den == 0 || float64(num)/float64(den) != rep.Rate.Value {
		t.Fatalf("headline %d/%d = %v contradicts the reported rate %v",
			num, den, float64(num)/float64(den), rep.Rate.Value)
	}
}

// TestFaultNotSessionIsTheUnitOfDiagnosis is the regression test for the second instrument defect: per-session
// scoring put device-down at 73.7% with 163 "misses", and those sessions read "Device ... is currently UP —
// the Device-Down alert is stale. No action is needed." They were duplicate alerts for a fault TG had ALREADY
// HEALED, so declining was the only correct answer. Per fault the same data reads 86.8%.
func TestFaultNotSessionIsTheUnitOfDiagnosis(t *testing.T) {
	// One fault, four alerts: TG heals on the first and correctly declines on the rest.
	sessions := []Item{
		{FaultID: 7, FaultType: "device-down", Proposed: "start-guest"},
		{FaultID: 7, FaultType: "device-down", Proposed: ""},
		{FaultID: 7, FaultType: "device-down", Proposed: ""},
		{FaultID: 7, FaultType: "device-down", Proposed: ""},
	}
	if got := ScoreFault(sessions, standDownRuleset()); got != Correct {
		t.Fatalf("a fault healed on its first alert is CORRECT however many stale duplicates follow, got %s", got)
	}
	rep := Build(sessions, standDownRuleset())
	if rep.Total != 1 || rep.Scored != 1 || rep.Correct != 1 {
		t.Fatalf("four alerts about one fault are ONE opportunity: got Total=%d Scored=%d Correct=%d",
			rep.Total, rep.Scored, rep.Correct)
	}
	// MUTATION CONTROL: scoring per session would give Missed=3 here — the exact defect this replaces.
	if rep.Missed != 0 {
		t.Fatalf("stale duplicates of an already-healed fault must not score as misses, got Missed=%d", rep.Missed)
	}
	if rep.Sessions != 4 {
		t.Fatalf("the session count must survive grouping — it is the duplicate-alert cost: got %d", rep.Sessions)
	}
}

// TestNeverHealedFaultIsStillAMiss guards the obvious inverse: grouping must not launder a genuine failure.
func TestNeverHealedFaultIsStillAMiss(t *testing.T) {
	rep := Build([]Item{
		{FaultID: 9, FaultType: "device-down", Proposed: ""},
		{FaultID: 9, FaultType: "device-down", Proposed: ""},
	}, standDownRuleset())
	if rep.Missed != 1 || rep.Correct != 0 {
		t.Fatalf("a fault NEVER correctly diagnosed is still a miss: got Missed=%d Correct=%d", rep.Missed, rep.Correct)
	}
}

// TestStandDownFaultIsStrictAcrossSessions pins the deliberate asymmetry. For a healable class one right
// answer discharges the fault, so ANY suffices. For a stand-down class one inapplicable proposal has already
// surfaced an action for approval, and later restraint does not retract it — so it takes ALL.
func TestStandDownFaultIsStrictAcrossSessions(t *testing.T) {
	rs := standDownRuleset()
	if got := ScoreFault([]Item{
		{FaultID: 3, FaultType: "disk-fill", Proposed: ""},
		{FaultID: 3, FaultType: "disk-fill", Proposed: "disk-grow"}, // one inapplicable proposal
		{FaultID: 3, FaultType: "disk-fill", Proposed: ""},
	}, rs); got != Wrong {
		t.Fatalf("one inapplicable proposal condemns the fault however often TG also declined, got %s", got)
	}
	if got := ScoreFault([]Item{
		{FaultID: 4, FaultType: "disk-fill", Proposed: ""},
		{FaultID: 4, FaultType: "disk-fill", Proposed: ""},
	}, rs); got != CorrectStandDown {
		t.Fatalf("declining throughout is correct, got %s", got)
	}
}

// TestUngroupedItemsDegradeToPerSession — an Item built by hand carries FaultID 0. Those must each become
// their own fault rather than collapsing into one bogus group that would report a single verdict for
// unrelated sessions.
func TestUngroupedItemsDegradeToPerSession(t *testing.T) {
	rep := Build([]Item{
		{FaultType: "device-down", Proposed: "start-guest"},
		{FaultType: "device-down", Proposed: ""},
	}, standDownRuleset())
	if rep.Total != 2 {
		t.Fatalf("FaultID 0 must not collapse unrelated sessions into one fault: got Total=%d", rep.Total)
	}
	if rep.Correct != 1 || rep.Missed != 1 {
		t.Fatalf("ungrouped items score independently: got Correct=%d Missed=%d", rep.Correct, rep.Missed)
	}
}

// ONE SESSION, ONE FAULT.
//
// The join was many-to-many: overlapping fault windows on a single host (18 such pairs measured live) let one
// triage session match two faults and be scored under BOTH — with contradictory ground truth, since the two
// faults are different classes. The same session then argued for two different correct answers. Live that was
// 777 joined rows over 768 distinct sessions.
//
// Read() now assigns each session to the NEAREST PRECEDING injection. These tests pin the grouping invariant
// that assignment must satisfy; Read's SQL is exercised against a real database by the harness CI job.
func TestNoSessionIsScoredUnderTwoFaults(t *testing.T) {
	// The shape the de-duplication must produce: one session, one FaultID.
	items := []Item{
		{ExternalRef: "s1", FaultID: 10, FaultType: "device-down", Proposed: "start-guest", At: time.Date(2026, 7, 27, 12, 1, 0, 0, time.UTC)},
		{ExternalRef: "s2", FaultID: 10, FaultType: "device-down", Proposed: "", At: time.Date(2026, 7, 27, 12, 2, 0, 0, time.UTC)},
		{ExternalRef: "s3", FaultID: 11, FaultType: "disk-fill", Proposed: "", At: time.Date(2026, 7, 27, 12, 3, 0, 0, time.UTC)},
	}
	seen := map[string]int64{}
	for _, it := range items {
		if prev, dup := seen[it.ExternalRef]; dup && prev != it.FaultID {
			t.Fatalf("session %q is scored under two faults (%d and %d) — it would argue for two different "+
				"correct answers", it.ExternalRef, prev, it.FaultID)
		}
		seen[it.ExternalRef] = it.FaultID
	}
	rep := Build(items, standDownRuleset())
	if rep.Total != 2 {
		t.Fatalf("want 2 faults from 3 sessions, got %d", rep.Total)
	}
	if rep.Sessions != 3 {
		t.Fatalf("the session count must survive de-duplication — it is the duplicate-alert cost: got %d", rep.Sessions)
	}
}

// MUTATION CONTROL for the grouping: a session appearing under two fault ids inflates the fault count, which
// is exactly what the double-scored rows did to the corpus.
func TestADuplicatedSessionInflatesTheFaultCount(t *testing.T) {
	dup := []Item{
		{ExternalRef: "s1", FaultID: 10, FaultType: "device-down", Proposed: "start-guest", At: time.Date(2026, 7, 27, 12, 1, 0, 0, time.UTC)},
		{ExternalRef: "s1", FaultID: 11, FaultType: "disk-fill", Proposed: "start-guest", At: time.Date(2026, 7, 27, 12, 1, 0, 0, time.UTC)},
	}
	rep := Build(dup, standDownRuleset())
	if rep.Total != 2 {
		t.Fatalf("precondition: two fault ids yield two faults, got %d", rep.Total)
	}
	// The same session scored under both: correct for device-down, WRONG for disk-fill. One session, two
	// contradictory verdicts — the defect in one assertion.
	if rep.Correct != 1 || rep.Wrong != 1 {
		t.Fatalf("a double-scored session produces contradictory verdicts (want Correct=1 Wrong=1, got %d/%d) "+
			"— this is what Read's DISTINCT ON prevents at the source", rep.Correct, rep.Wrong)
	}
}
