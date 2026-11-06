// Package governance implements Territory Grounder's two self-monitoring controls: the governance-metrics
// worker that auto-demotes a genuine repeat-offender (host, alert_rule) tuple to analysis-only, and the
// judge-liveness monitor that measures whether the local LLM judge is actually scoring recent sessions —
// computed from tables the judge does not write, so a dead judge cannot certify itself alive.
//
// Provenance: [F] spec/004 (BEH-4), the predecessor write-governance-metrics.py + judge-death signal,
// re-expressed under the typed spine · [O] INV-12 (RBAC authority), INV-15 (one generated source),
// INV-19 (audit spine), INV-22 (judge-independence tested on the real path) · [R] paradigm-rules 1/4/5
// (org-global policy rows, no host-local flag; the retention split; ADR-0010 single-org).
package governance

import "time"

// Tuple is a (host, alert_rule) identity — the unit a demotion circuit-breaker acts on.
type Tuple struct {
	Host      string
	AlertRule string
}

// Incident is a per-incident close-out row (produced by the BEH-3 reconciler, spec/003). Recurrences
// are counted per incident (by external_ref), never per event, so an alert storm cannot inflate the
// offender count.
type Incident struct {
	Tuple       Tuple
	ExternalRef string
	ClosedAt    time.Time
}

const (
	// RepeatOffenderThreshold: a tuple with this many or more incidents in the window is a candidate.
	RepeatOffenderThreshold = 3
	// RepeatWindow is the rolling window over which recurrences are counted (REQ-302).
	RepeatWindow = 30 * 24 * time.Hour
)

// CountByTuple counts DISTINCT incidents (by external_ref) per tuple within the rolling window ending
// at now. Counting incidents rather than events realizes the count-incidents-not-events discipline
// carried from BEH-3.
func CountByTuple(incidents []Incident, now time.Time) map[Tuple]int {
	refs := map[Tuple]map[string]struct{}{}
	for _, i := range incidents {
		if now.Sub(i.ClosedAt) > RepeatWindow || i.ClosedAt.After(now) {
			continue
		}
		if refs[i.Tuple] == nil {
			refs[i.Tuple] = map[string]struct{}{}
		}
		refs[i.Tuple][i.ExternalRef] = struct{}{}
	}
	out := make(map[Tuple]int, len(refs))
	for t, set := range refs {
		out[t] = len(set)
	}
	return out
}

// IsDemoteCandidate reports whether an incident count crosses the repeat-offender threshold (REQ-302).
func IsDemoteCandidate(incidentCount int) bool {
	return incidentCount >= RepeatOffenderThreshold
}
