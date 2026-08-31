// Package diagcorpus turns the fault injector's own record into a labelled diagnosis corpus.
//
// P5's exit criterion needs >=100 labelled items to calibrate the judge, and the roadmap treated that label
// pass as the phase's schedule bottleneck — "queue the human labelling from P0". It is not, because the
// ground truth already exists: the injector durably records WHAT it broke, on WHICH host, and WHEN. Joining
// a triage session to the fault that was live on its host at the time yields an item whose correct answer is
// known without anyone reading it. Measured when this shipped: 726 joinable items, against a required 100.
//
// WHAT THIS DOES AND DOES NOT MEASURE. It measures whether TG proposed the op-class that actually addresses
// the injected fault — a DIRECT diagnosis measurement, computed from ground truth rather than from a judge.
// That is strictly stronger evidence than a judged score for the classes it covers, and it is also what makes
// the judge calibratable: the same items can be scored by the judge and compared against this truth.
//
// It does NOT cover organic incidents (no injector record ⇒ no truth), and it inherits the injector's class
// coverage. Both are stated with every report rather than left for a reader to discover.
//
// Provenance: [O] INV-22 (a number without its population is not evidence) · spec/025 REQ-2502 (report the
// denominator and what was excluded) / REQ-2505 (evidence carries its provenance).
package diagcorpus

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/calibrate"
)

// Item is one triage session paired with the fault that was live on its host when it ran.
type Item struct {
	ExternalRef string
	Host        string
	AlertRule   string
	FaultType   string // the injector's record — GROUND TRUTH
	Proposed    string // the op-class TG proposed ("" = it proposed nothing)
	// Conclusion + Diagnosis are the session's own diagnostic TEXT (session_triage.conclusion is the free-text
	// read; diagnosis is the typed field, migration 0056). They carry NO weight in the action-policy Score
	// path — they exist for the diagnosis-NAMING ground truth (TG-542): does the diagnosis identify the
	// injected fault's mechanism, the commensurable calibration for the correct_diagnosis judge dimension
	// (a quality axis) that the op-class oracle cannot measure. Either may be empty on an old session.
	Conclusion string
	Diagnosis  string
	At         time.Time
	// FaultID groups every session that ran against the SAME injected fault. It is what makes the fault, not
	// the session, the unit of diagnosis — see ScoreFault.
	FaultID int64
}

// Expectation declares which op-classes correctly address a fault class. It is OPERATOR-DECLARED DATA, not
// compiled judgement: what counts as the right answer for "disk-fill" is an estate policy question (grow the
// filesystem? vacuum the journal? page a human?), and burying it in code would make the corpus score TG
// against this author's opinion rather than against the operator's.
//
// Accept lists EVERY op-class that legitimately addresses the fault, because more than one can be correct —
// a down service may be validly started OR restarted, and scoring one of those wrong would manufacture a
// diagnosis failure that is really a vocabulary preference.
type Expectation struct {
	FaultType string   `json:"fault_type"`
	Accept    []string `json:"accept"`
	// Unhealable marks a fault class TG is not expected to fix — mem-pressure has a measured 1/14 detection
	// rate on this estate, so counting its misses as diagnosis failures would score an instrumentation gap as
	// a capability gap. Items in an unhealable class are EXCLUDED from the score and reported separately.
	Unhealable bool `json:"unhealable"`
	// StandDownIsCorrect INVERTS the polarity: for this fault class no declared op-class can actually address
	// the fault on this estate, so the correct diagnosis outcome is a REASONED STAND-DOWN, and proposing
	// anything is the error.
	//
	// This exists because the first live run of this corpus scored disk-fill at 31.6% and called 63 items
	// "missed" — and the sessions behind them read: "Disk usage on / is 97% (371M free of 9.8G), with /var/tmp
	// consuming 5.9G. No actuatable op-class is applicable to a loop-mounted root filesystem in an LXC." TG had
	// diagnosed the fault, named the consuming path, and correctly concluded that disk-grow cannot grow a
	// loopback device. Every guest in the pool is an LXC on /dev/loopN, so the 30 items that DID propose
	// disk-grow were the wrong ones. The instrument had the sign backwards, and would have published a
	// capability gap as a reasoning failure.
	//
	// Deliberately NOT the same as Unhealable. Unhealable stops measuring (we cannot even detect it, so a miss
	// says nothing). StandDownIsCorrect keeps measuring and flips which answer is right — because "did TG
	// correctly recognise it has no legal move?" is a real and demanding diagnosis question, and one TG can
	// still fail by confidently proposing an inapplicable remedy. Setting both, or setting this alongside a
	// non-empty Accept, is contradictory and is rejected at load time.
	StandDownIsCorrect bool `json:"stand_down_is_correct"`
}

// Ruleset is the declared expectation set.
type Ruleset struct {
	Expectations []Expectation `json:"expectations"`
}

func (r Ruleset) find(faultType string) (Expectation, bool) {
	for _, e := range r.Expectations {
		if strings.EqualFold(strings.TrimSpace(e.FaultType), strings.TrimSpace(faultType)) {
			return e, true
		}
	}
	return Expectation{}, false
}

// Verdict is how one item scored.
type Verdict string

const (
	// Correct — TG proposed an op-class the ruleset accepts for the injected fault.
	Correct Verdict = "correct"
	// CorrectStandDown — TG proposed nothing for a class where nothing is the right answer. Counted as correct
	// in the rate, but kept as its own verdict so a class carried by declining is never rendered as though it
	// were carried by healing. Both are right; they are not the same achievement, and pooling them would let
	// "TG diagnoses 68% correctly" quietly mean "TG correctly stayed still".
	CorrectStandDown Verdict = "correct-stand-down"
	// Missed — TG proposed NOTHING for a fault it was expected to address. Deliberately distinguished from
	// Wrong: standing down is a different failure from confidently proposing the wrong fix, and conflating
	// them hides which one is getting better.
	Missed Verdict = "missed"
	// Wrong — TG proposed an op-class that does not address the injected fault, INCLUDING proposing anything
	// at all where the ruleset says the correct answer is to stand down.
	Wrong Verdict = "wrong"
	// Excluded — the fault class is declared unhealable, or no expectation covers it. Never scored.
	Excluded Verdict = "excluded"
)

// Score assigns a verdict to one item.
func Score(it Item, rs Ruleset) Verdict {
	exp, ok := rs.find(it.FaultType)
	if !ok || exp.Unhealable {
		return Excluded
	}
	if exp.StandDownIsCorrect {
		// Polarity inverted: nothing is the right answer, so anything is the wrong one.
		if strings.TrimSpace(it.Proposed) == "" {
			return CorrectStandDown
		}
		return Wrong
	}
	if strings.TrimSpace(it.Proposed) == "" {
		return Missed
	}
	for _, a := range exp.Accept {
		if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(it.Proposed)) {
			return Correct
		}
	}
	return Wrong
}

// ScoreFault scores ONE INJECTED FAULT from every session that ran while it was live. The fault — not the
// session — is the unit of diagnosis, because a fault is one opportunity to get it right and a session is
// merely one alert about it. Measured live: 737 sessions over 297 faults, a mean of 2.48 sessions each.
//
// WHY PER-SESSION WAS WRONG, in the data's own words. Scoring per session put device-down at 73.7% with 163
// "misses", and those sessions read: "Device dc1wallos01 is currently UP (uptime 1h41m) — the Device-Down
// alert is stale. No action is needed." They are duplicate alerts for a fault TG had ALREADY HEALED in an
// earlier session; declining is the only correct answer once the guest is back up. Per-session scoring
// therefore punished TG for being right, and inflated the denominator 2.48x with opportunities that were
// never independent. Per fault, device-down is 86.8% (165/190).
//
// THE AGGREGATION IS DELIBERATELY ASYMMETRIC, and the asymmetry is grounded in consequence, not convenience:
//
//   - healable class — correct if ANY session proposed an accepted op-class. One right answer discharges the
//     fault; the later stand-downs are correct precisely BECAUSE the first proposal worked.
//   - stand-down class — correct only if EVERY session declined. One inappropriate proposal has already
//     surfaced an inapplicable action for approval, and nothing a later session says retracts it.
//
// A fault with no sessions at all cannot be scored here; Build only ever sees faults that produced one.
func ScoreFault(sessions []Item, rs Ruleset) Verdict {
	if len(sessions) == 0 {
		return Excluded
	}
	exp, ok := rs.find(sessions[0].FaultType)
	if !ok || exp.Unhealable {
		return Excluded
	}
	if exp.StandDownIsCorrect {
		for _, s := range sessions {
			if Score(s, rs) == Wrong {
				return Wrong // one inapplicable proposal is not undone by later restraint
			}
		}
		return CorrectStandDown
	}
	sawWrong := false
	for _, s := range sessions {
		switch Score(s, rs) {
		case Correct:
			return Correct // the fault was correctly diagnosed at least once — that is the opportunity taken
		case Wrong:
			sawWrong = true
		}
	}
	if sawWrong {
		return Wrong
	}
	return Missed
}

// Report is the corpus result: the overall diagnosis rate with its interval, the per-fault-class breakdown,
// and — always — what was excluded and why.
type Report struct {
	Total       int // INJECTED FAULTS — the unit of diagnosis
	Sessions    int // triage sessions behind them; Sessions/Total is the duplicate-alert cost
	Scored      int
	Correct     int
	StoodDown   int // correct BY DECLINING — a subset of the numerator, always reported separately
	Missed      int
	Wrong       int
	Excluded    int
	Rate        calibrate.Rate // (correct + stood-down) / scored, with a Wilson interval
	ByFaultType map[string]ClassReport
	FaultTypes  []string // sorted, for stable rendering
	// ExcludedReasons names why items were dropped, so the exclusion is auditable rather than a bare count.
	ExcludedReasons map[string]int
}

// ClassReport is the same breakdown for one fault class. Per-class matters because a pooled rate hides a
// class that is failing completely — the estate's fault mix is not uniform, and one class dominating the
// corpus would otherwise decide the headline number on its own.
type ClassReport struct {
	Total     int // faults
	Sessions  int // sessions behind them
	Correct   int
	StoodDown int
	Missed    int
	Wrong     int
	Rate      calibrate.Rate
	// StandDownExpected records that this class's correct answer IS the stand-down, so the renderer can label
	// it rather than leaving a reader to assume a high rate means a high heal rate.
	StandDownExpected bool
}

// Build scores a corpus against the declared expectations, ONE FAULT AT A TIME.
//
// Items are grouped by FaultID first, so every session that ran against the same injected fault contributes
// to a single verdict. Sessions are still counted and reported, because the ratio of sessions to faults is
// itself a finding — it is the cost of duplicate alerts, and at 2.48 sessions per fault most of TG's runs are
// re-answering a question it has already answered correctly.
//
// Items carrying FaultID 0 (no grouping key — a caller that built Items by hand) each become their own fault,
// which degrades exactly to the old per-session behaviour rather than silently collapsing unrelated sessions
// into one bogus fault.
func Build(items []Item, rs Ruleset) Report {
	rep := Report{
		Total:           len(items),
		Sessions:        len(items),
		ByFaultType:     map[string]ClassReport{},
		ExcludedReasons: map[string]int{},
	}
	type group struct {
		key      string
		sessions []Item
	}
	var order []string
	groups := map[string]*group{}
	for i, it := range items {
		key := fmt.Sprintf("%d", it.FaultID)
		if it.FaultID == 0 {
			key = fmt.Sprintf("ungrouped-%d", i)
		}
		g := groups[key]
		if g == nil {
			g = &group{key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.sessions = append(g.sessions, it)
	}
	rep.Total = len(order)

	byClass := map[string]*ClassReport{}
	for _, key := range order {
		g := groups[key]
		ft := g.sessions[0].FaultType
		v := ScoreFault(g.sessions, rs)
		if v == Excluded {
			rep.Excluded++
			exp, ok := rs.find(ft)
			switch {
			case !ok:
				rep.ExcludedReasons["no declared expectation for "+ft]++
			case exp.Unhealable:
				rep.ExcludedReasons[ft+" declared unhealable"]++
			}
			continue
		}
		c := byClass[ft]
		if c == nil {
			exp, _ := rs.find(ft)
			c = &ClassReport{StandDownExpected: exp.StandDownIsCorrect}
			byClass[ft] = c
		}
		rep.Scored++
		c.Total++
		c.Sessions += len(g.sessions)
		switch v {
		case Correct:
			rep.Correct++
			c.Correct++
		case CorrectStandDown:
			rep.StoodDown++
			c.StoodDown++
		case Missed:
			rep.Missed++
			c.Missed++
		case Wrong:
			rep.Wrong++
			c.Wrong++
		}
	}
	rep.Rate = ratio(rep.Correct+rep.StoodDown, rep.Scored)
	for k, c := range byClass {
		c.Rate = ratio(c.Correct+c.StoodDown, c.Total)
		rep.ByFaultType[k] = *c
		rep.FaultTypes = append(rep.FaultTypes, k)
	}
	sort.Strings(rep.FaultTypes)
	return rep
}

// ratio reuses the calibration package's Wilson-bounded Rate so a corpus number is reported on the same terms
// as every other published proportion: with its denominator, and undefined rather than zero at n=0.
func ratio(num, den int) calibrate.Rate {
	c := calibrate.Confusion{TP: num, FN: den - num}
	return calibrate.TPR(c)
}

// JudgeOutcomes projects the corpus into calibration outcomes for the JUDGE: ground truth is whether the
// item was correctly diagnosed, and the judge's call is whatever the judge said about the same session. This
// is what makes the judge calibratable at all — before this there was no ground truth to calibrate against.
//
// judged maps an external_ref to the judge's binary call. An item the judge never scored is DROPPED rather
// than assumed: an unjudged session is missing data, and defaulting it either way would manufacture agreement
// or disagreement out of nothing.
// DiagNaming is the diagnosis-NAMING verdict (TG-542): did the session's own diagnostic text identify the
// injected fault's MECHANISM? This is the ground truth COMMENSURABLE with the correct_diagnosis judge
// dimension — a diagnosis-QUALITY axis — where Score's op-class-acceptance oracle measures action POLICY, a
// different construct that cannot validate a diagnosis judge (the finding that TNR 0.060 was a construct
// mismatch, not a broken judge: correct_diagnosis / sensible_proposal / appropriate_band all returned the
// identical confusion against the action oracle).
type DiagNaming string

const (
	// Named — the diagnosis names the injected fault's mechanism, OR correctly reads a since-recovered state
	// (a stand-down whose grounded conclusion is "the guest is back up, the alert is stale" is a correct
	// diagnosis of the alert too — the rubric rewards it).
	Named DiagNaming = "named"
	// Unnamed — diagnostic text is present but does not identify the mechanism.
	Unnamed DiagNaming = "unnamed"
	// NoDiagText — no conclusion/diagnosis to score; excluded from the naming calibration (nothing to name).
	NoDiagText DiagNaming = "no-text"
)

// mechanismTerms maps each injector fault class to the vocabulary a CORRECT diagnosis of it uses. It is
// PROVISIONAL (TG-542): the tool and its plumbing are correct now, but this MAP is validated + tuned against
// the campaign-#3 fresh typed-diagnosis population before §5.4 relies on it — the historical corpus carries
// only 11 typed diagnoses, too few to tune against. Kept HERE in Go, deliberately NOT in expectations.json,
// which is campaign-frozen (§6 accrual reads it). Terms are lowercased substrings; a hit on ANY marks Named,
// so the list errs toward Named — a wrong diagnosis must AVOID all of them to score Unnamed, which is the
// conservative direction for a judge-over-generosity test (it under-counts, never invents, judge errors).
//
// Tuned 2026-08-26 against the real historical CONCLUSIONS (2,538 populated): the initial narrow terms
// FALSE-Unnamed'd 32 correct diagnoses (a guest "stopped" not "is down"; "down per librenms"; a "disabled"
// device correctly declined; "already resolved"/"stale or ..."), which the judge had correctly scored high.
// The domain word for a *-down fault ("down") legitimately Names a correct diagnosis — a WRONG one (e.g.
// "the disk is full" for a device-down) does not use it — so the per-class domain vocabulary is the right
// grain. Still validated afresh against the campaign-#3 typed-diagnosis population before §5.4 relies on it.
var mechanismTerms = map[string][]string{
	"device-down":    {"down", "unreachable", "stopped", "not responding", "no icmp"},
	"container-down": {"container", "exited", "not running", "stopped", "down", "crash"},
	"service-down":   {"service", "unit", "inactive", "failed", "not running", "stopped", "down", "systemd"},
	"disk-fill":      {"disk", "filesystem", "file system", "space", "full", "% in use", "root fs"},
}

// recoveredTerms mark a correct read of a since-resolved OR intentionally-out-of-service fault (the fault
// self-cleared, the alert is stale, or the device is disabled/planned — each a correct grounded stand-down
// the rubric rewards). Broadened from the same real-conclusion tuning.
var recoveredTerms = []string{"back up", "recovered", "self-resolved", "stale", "already", "no action", "no recovery", "no remediation", "no repair", "disabled", "out of monitoring", "intentionally", "planned", "condition self", "resolved"}

// ScoreDiagnosisNaming decides whether the session's diagnostic text named the injected mechanism. Pure,
// deterministic, case-insensitive substring matching over conclusion + typed diagnosis.
func ScoreDiagnosisNaming(it Item) DiagNaming {
	text := strings.ToLower(strings.TrimSpace(it.Conclusion + " " + it.Diagnosis))
	if text == "" {
		return NoDiagText
	}
	for _, t := range recoveredTerms {
		if strings.Contains(text, t) {
			return Named
		}
	}
	for _, t := range mechanismTerms[it.FaultType] {
		if strings.Contains(text, t) {
			return Named
		}
	}
	return Unnamed
}

// JudgeDiagnosisOutcomes pairs the correct_diagnosis judge call with the diagnosis-NAMING ground truth — the
// commensurable §5.4 calibration (TG-542). It EXCLUDES the same unhealable/no-expectation classes JudgeOutcomes
// drops (via Score==Excluded) and any item with no diagnostic text to name. Mirrors JudgeOutcomes exactly but
// for the Truth source.
func JudgeDiagnosisOutcomes(items []Item, rs Ruleset, judged map[string]bool) []calibrate.Outcome {
	var out []calibrate.Outcome
	for _, it := range items {
		if Score(it, rs) == Excluded {
			continue
		}
		v := ScoreDiagnosisNaming(it)
		if v == NoDiagText {
			continue
		}
		call, ok := judged[it.ExternalRef]
		if !ok {
			continue
		}
		out = append(out, calibrate.Outcome{Truth: v == Named, Judge: call})
	}
	return out
}

func JudgeOutcomes(items []Item, rs Ruleset, judged map[string]bool) []calibrate.Outcome {
	var out []calibrate.Outcome
	for _, it := range items {
		v := Score(it, rs)
		if v == Excluded {
			continue
		}
		call, ok := judged[it.ExternalRef]
		if !ok {
			continue
		}
		out = append(out, calibrate.Outcome{Truth: v == Correct || v == CorrectStandDown, Judge: call})
	}
	return out
}

// Render writes the report as a human-readable scorecard. Every number carries its denominator, and the
// exclusions are printed rather than summarised away.
func (r Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "DIAGNOSIS CORPUS — %d injected fault(s), %d scored, %d excluded  [%d triage session(s), %.2f per fault]\n",
		r.Total, r.Scored, r.Excluded, r.Sessions, float64(r.Sessions)/math.Max(1, float64(r.Total)))
	if r.Rate.Defined {
		// The numerator MUST be the same quantity the rate was computed from. Printing r.Correct here while
		// Rate covered correct+stood-down showed "471/730 = 73.2%" — a fraction that does not equal its own
		// percentage, and the kind of internal contradiction a reader is entitled to never see.
		fmt.Fprintf(&b, "  correct diagnosis: %d/%d = %.1f%% (95%% CI %.1f%%–%.1f%%)\n",
			r.Correct+r.StoodDown, r.Scored, r.Rate.Value*100, r.Rate.Lo*100, r.Rate.Hi*100)
	} else {
		b.WriteString("  correct diagnosis: UNDEFINED — nothing scored\n")
	}
	// The split is printed unconditionally, including at zero. A reader must never have to infer from a missing
	// line that none of the correct answers were stand-downs.
	fmt.Fprintf(&b, "    of which proposed a remedy: %d    correctly stood down: %d\n", r.Correct, r.StoodDown)
	fmt.Fprintf(&b, "  missed (proposed nothing): %d    wrong class: %d\n", r.Missed, r.Wrong)
	for _, ft := range r.FaultTypes {
		c := r.ByFaultType[ft]
		if !c.Rate.Defined {
			continue
		}
		fmt.Fprintf(&b, "  - %-16s %d/%d faults = %.1f%% (CI %.1f–%.1f)  missed=%d wrong=%d  [%d sessions]\n",
			ft, c.Correct+c.StoodDown, c.Total, c.Rate.Value*100, c.Rate.Lo*100, c.Rate.Hi*100, c.Missed, c.Wrong, c.Sessions)
		if c.StandDownExpected {
			fmt.Fprintf(&b, "      ^ NO APPLICABLE OP-CLASS on this estate — correct = a reasoned stand-down (%d), "+
				"and the %d proposal(s) are the errors. This class cannot be healed, only recognised.\n", c.StoodDown, c.Wrong)
		}
	}
	if len(r.ExcludedReasons) > 0 {
		b.WriteString("  excluded:\n")
		keys := make([]string, 0, len(r.ExcludedReasons))
		for k := range r.ExcludedReasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "    %d × %s\n", r.ExcludedReasons[k], k)
		}
	}
	b.WriteString("  UNIT: one INJECTED FAULT = one diagnosis opportunity. Scoring per SESSION instead counted duplicate\n" +
		"        alerts for an already-healed fault as fresh misses, and inflated the denominator ~2.5x.\n")
	b.WriteString("  NOTE: covers INJECTED faults only — organic incidents carry no ground truth and are absent.\n")
	return b.String()
}
