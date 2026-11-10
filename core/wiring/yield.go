package wiring

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	observability "github.com/territory-grounder/grounder/adapters/observability"
)

// THE PAIRED-YIELD REGISTER — did a WIRED seam actually produce anything?
//
// The manifest answers one question, at boot, once: was this seam BOUND? That question has caught real
// defects (a whole discovery lane imported by nothing; a notifier that delivered to nobody), and it is
// blind to the larger half of the class it was built for.
//
// Measured on 2026-08-01 against nine defects fixed that day, the manifest could not have seen six of
// them, because every one was BOUND and RUNNING:
//
//   - an estate outage guard whose predicate was arithmetically unsatisfiable — dead code that ran on
//     every refresh and could never fire once
//   - two discovery probes that were wired but drafted zero entities of their only kind
//   - a knowledge corpus that "loaded" with zero rows and answered "never seen" about the whole estate
//   - an MTTR correlation that silently dropped the commonest incident class from its DENOMINATOR
//   - a badge that counted a different population than the list beside it in the same response
//   - a console row that discarded a verdict the API had already delivered
//
// Two shapes, and the register is built for both:
//
//	STARVED     — the seam was offered work and produced NOTHING. The alarm.
//	DISAGREEMENT — the seam produced FEWER than it was offered. Usually correct (filtering is the job),
//	              so it is published rather than alarmed: both numbers side by side, and the difference
//	              is left visible instead of hidden inside a predicate.
//
// The second is the predecessor's pattern, learned from its own scar tissue: it emits `window_30d`
// beside `window_30d_gated`, each carrying its own count, and an incident-unit rate beside a full
// event-based sibling annotated "never use this as a headline". An exclusion you can SUBTRACT is an
// exclusion someone can argue with. An exclusion buried in a WHERE clause is one nobody can see.
//
// WHY UNOBSERVED IS A FINDING. A seam that declares a unit and that nothing ever calls Observe for
// reports UNOBSERVED — never "fine". This is the same closed-set property that makes the manifest work,
// and it is the whole reason this register can be trusted: without it, a register instrumented at zero
// seams would report a clean estate, which is precisely the failure it exists to detect.
type YieldRegister struct {
	mu   sync.Mutex
	flow map[Seam]*flow
}

type flow struct {
	offered  int64
	produced int64
	// lastProducedAt is when produced last advanced — the difference between "has never produced" and
	// "produced once at boot and nothing since", which a bare counter cannot express.
	lastProducedAt time.Time
	firstSeenAt    time.Time
}

// NewYieldRegister returns an empty register. Every seam in the closed set is reported, whether or not
// anything ever observed it.
func NewYieldRegister() *YieldRegister {
	return &YieldRegister{flow: map[Seam]*flow{}}
}

// Observe records one pass of a seam: how many units were OFFERED to it and how many it PRODUCED.
//
// Both numbers are required, and that is the point. A seam that reported only what it produced could not
// distinguish "nothing arrived" from "everything arrived and was dropped" — which are an idle estate and
// a broken lane, and the whole value here is telling them apart.
//
// Negative counts are clamped to zero rather than rejected: a miscounting caller must not be able to
// drive a total backwards and manufacture a clean report.
func (r *YieldRegister) Observe(s Seam, offered, produced int, now time.Time) {
	if r == nil {
		return
	}
	if offered < 0 {
		offered = 0
	}
	if produced < 0 {
		produced = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flow[s]
	if !ok {
		f = &flow{firstSeenAt: now}
		r.flow[s] = f
	}
	f.offered += int64(offered)
	if produced > 0 {
		f.produced += int64(produced)
		f.lastProducedAt = now
	}
}

// ObserveTotals records ABSOLUTE running totals rather than an increment.
//
// Both shapes exist because both shapes exist in the code. A discovery pass naturally yields a per-pass
// increment (this pass saw 40, drafted 3); a suppression gate naturally exposes a cumulative counter
// (5,000 decisions since boot, 12 of them suppressions). Forcing the second through Observe would make
// every reporting tick re-add the whole history, and the totals would climb quadratically — a metric that
// looks healthier the longer it runs, which is the direction this register exists to refuse.
//
// Totals only ever move FORWARD. A caller reporting a lower total than last time (a counter reset, a
// restarted sub-component) leaves the recorded total where it was rather than winding it back: evidence
// of past starvation must not be erasable by a component that forgot.
func (r *YieldRegister) ObserveTotals(s Seam, offered, produced int64, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flow[s]
	if !ok {
		f = &flow{firstSeenAt: now}
		r.flow[s] = f
	}
	if offered > f.offered {
		f.offered = offered
	}
	if produced > f.produced {
		f.produced = produced
		f.lastProducedAt = now
	}
}

// YieldState is a seam's runtime standing.
type YieldState int

const (
	// YieldUnobserved — the seam declares a unit and NOTHING ever called Observe. The register's own
	// vacuity floor: an uninstrumented seam must read as uncovered, never as healthy.
	YieldUnobserved YieldState = iota
	// YieldIdle — the seam was observed but nothing was ever offered to it. Not a defect: an estate with
	// no incidents offers no work. Reported so it stays distinguishable from starvation.
	YieldIdle
	// YieldStarved — work was offered and the seam produced NOTHING. The alarm.
	YieldStarved
	// YieldFlowing — the seam has produced.
	YieldFlowing
)

func (y YieldState) String() string {
	switch y {
	case YieldIdle:
		return "idle"
	case YieldStarved:
		return "starved"
	case YieldFlowing:
		return "flowing"
	default:
		return "unobserved"
	}
}

// YieldFinding is one seam's runtime standing, carrying the unit prose so an operator reads what was
// supposed to happen rather than a bare pair of integers.
type YieldFinding struct {
	Seam     Seam
	State    YieldState
	Offered  int64
	Produced int64
	// Unit is the seam's declared vocabulary ("alerts" -> "suppression decisions").
	Unit Unit
	// Consequence is the SeamSpec's prose, reused verbatim: a starved seam costs an operator the same
	// thing an unbound one does, so it should read the same way.
	Consequence string
	// LastProducedAt is zero when the seam has never produced.
	LastProducedAt time.Time
}

// Reason renders a finding as one report line.
func (f YieldFinding) Reason() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", f.Seam, f.State)
	switch f.State {
	case YieldUnobserved:
		fmt.Fprintf(&b, " — nothing reports this seam's yield, so it is UNCOVERED: it could be producing "+
			"nothing and this report would not know")
	case YieldStarved:
		fmt.Fprintf(&b, " — %d %s offered, %d %s produced: the seam is wired and running and has emitted "+
			"NOTHING", f.Offered, f.Unit.Offered, f.Produced, f.Unit.Produced)
	case YieldIdle:
		fmt.Fprintf(&b, " — no %s have been offered yet (an idle estate, not a broken lane)", f.Unit.Offered)
	default:
		fmt.Fprintf(&b, " — %d %s offered, %d %s produced", f.Offered, f.Unit.Offered, f.Produced, f.Unit.Produced)
	}
	// COST ONLY, and now by construction rather than by framing. This renderer used to wrap a Consequence
	// that opened with its own cause ("no tier-1 suppression chain is configured: …") — a true frame around
	// a false sentence, since for a STARVED seam a chain IS configured and matches nothing. The audit on
	// 2026-08-06 found five of six such texts naming a state the deployment was not in.
	//
	// SeamSpec.Cause now carries the dark-state reason and is rendered ONLY by the dark report
	// (core/wiring/report.go). Nothing here reads it: an operator-facing line must not assert a reason the
	// register did not establish, and now it cannot.
	if f.Consequence != "" {
		switch f.State {
		case YieldStarved:
			fmt.Fprintf(&b, " [the cost is the same as if it had never been wired — %s]", f.Consequence)
		case YieldUnobserved:
			fmt.Fprintf(&b, " [what it would cost if it were producing nothing — %s]", f.Consequence)
		}
	}
	return b.String()
}

// YieldReport ranges over the CLOSED seam set — never over what was observed — so a seam nothing
// instruments is reported as UNOBSERVED rather than being invisible. This mirrors Manifest.Report and is
// the property that makes the register's silence mean something.
//
// It returns findings for the states worth acting on (starved, unobserved) and a sample for EVERY seam
// carrying BOTH numbers, always. Publishing the pair is not decoration: it is the only way a filter that
// stops matching becomes visible from outside the code.
func (r *YieldRegister) Report(now time.Time) ([]YieldFinding, []observability.Sample) {
	specs := All()
	findings := make([]YieldFinding, 0, len(specs))
	samples := make([]observability.Sample, 0, len(specs)*3)

	for _, sp := range specs {
		var f flow
		observed := false
		if r != nil {
			r.mu.Lock()
			if got, ok := r.flow[sp.ID]; ok {
				f, observed = *got, true
			}
			r.mu.Unlock()
		}

		st := YieldUnobserved
		switch {
		case !observed:
			st = YieldUnobserved
		case f.produced > 0:
			st = YieldFlowing
		case f.offered > 0:
			st = YieldStarved
		default:
			st = YieldIdle
		}

		fin := YieldFinding{
			Seam: sp.ID, State: st, Offered: f.offered, Produced: f.produced,
			Unit: sp.Unit, Consequence: sp.Consequence, LastProducedAt: f.lastProducedAt,
		}
		if st == YieldStarved || st == YieldUnobserved {
			findings = append(findings, fin)
		}

		lbl := map[string]string{"seam": string(sp.ID), "state": st.String()}
		samples = append(samples,
			// BOTH numbers, on every seam, every scrape. The difference between them is the signal a
			// WHERE clause would otherwise swallow.
			observability.Sample{Name: "tg_wiring_seam_offered_total", Value: float64(f.offered), Stamped: now, Labels: lbl},
			observability.Sample{Name: "tg_wiring_seam_produced_total", Value: float64(f.produced), Stamped: now, Labels: lbl},
			observability.Sample{Name: "tg_wiring_seam_starved", Value: boolValue(st == YieldStarved), Stamped: now, Labels: lbl},
			// Uncovered is its own gauge: "we are not measuring this" must not be readable as "this is fine".
			observability.Sample{Name: "tg_wiring_seam_yield_unobserved", Value: boolValue(st == YieldUnobserved), Stamped: now, Labels: lbl},
		)
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Seam < findings[j].Seam })
	return findings, samples
}

// YieldReportText renders the findings as one operator-readable block, or "" when nothing is worth
// saying. Empty output means every seam is flowing or idle — never that nothing was checked, because an
// unchecked seam reports UNOBSERVED and is therefore in the findings.
func YieldReportText(findings []YieldFinding) string {
	if len(findings) == 0 {
		return ""
	}
	starved, uncovered := 0, 0
	for _, f := range findings {
		switch f.State {
		case YieldStarved:
			starved++
		case YieldUnobserved:
			uncovered++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "seam yield: %d starved, %d uncovered:\n", starved, uncovered)
	for _, f := range findings {
		fmt.Fprintf(&b, "  - %s\n", f.Reason())
	}
	return strings.TrimRight(b.String(), "\n")
}
