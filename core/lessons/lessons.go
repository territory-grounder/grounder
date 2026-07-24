// Package lessons is Territory Grounder's teacher: it distills a RESOLVED incident's outcome into a reusable
// lesson — a knowledge.Incident the retrieval plane surfaces for future similar incidents — closing the
// outcome-labelled memory loop (observe → resolve → learn → retrieve).
//
// The load-bearing discipline: a lesson is recorded ONLY from a CONFIRMED CLEAN outcome (a mechanical
// verdict of `match` AND an orchestrator-confirmed clear). A deviation, a partial, or an unconfirmed session
// never becomes precedent, so the corpus is never poisoned with advice from a session where reality diverged
// from the model or the fix was never verified. Learning from your successes is safe; learning from your
// near-misses as if they were successes is how an autonomous system compounds its own mistakes.
package lessons

import (
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// ResolvedIncident is a closed-out incident with its verified outcome — the input the teacher labels. The
// json tags let an operator-exported resolved-incident history (or, in Phase 2, the close-out path) round-trip
// through ParseResolved into the persistence hop.
type ResolvedIncident struct {
	ExternalRef    string         `json:"external_ref"`
	Host           string         `json:"host,omitempty"`
	AlertRule      string         `json:"alert_rule,omitempty"`
	Site           string         `json:"site,omitempty"`
	Summary        string         `json:"summary,omitempty"`
	Action         string         `json:"action,omitempty"`  // what was done (the ActionManifest op) — becomes the lesson's Resolution
	Verdict        safety.Verdict `json:"verdict,omitempty"` // the mechanical verdict (spec/002)
	ConfirmedClear bool           `json:"confirmed_clear"`   // an orchestrator-captured confirmation the condition actually cleared (INV-11)
	Tags           []string       `json:"tags,omitempty"`
	// ResolvedAt is the lesson's PROVENANCE timestamp — when the incident was resolved and the precedent
	// became true (spec/018, Gulli ch14). It lets the recency/decay discipline know a lesson's AGE so a stale
	// precedent can be down-weighted (HalfLifeWeight) or pruned from the corpus (Reconcile). Zero = undatable:
	// the reconciliation never ages out a lesson whose age it cannot prove (fail toward retention).
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// Lesson distills a resolved incident into a knowledge.Incident, or (_, false) when the outcome is not a
// trustworthy precedent. Both gates must hold: the mechanical verdict is a clean `match` (reality matched the
// prediction) AND the condition was confirmed clear (the fix is verified, not merely asserted). An incident
// with no external_ref or no action is also not a citable lesson.
func Lesson(ri ResolvedIncident) (knowledge.Incident, bool) {
	if ri.Verdict != safety.VerdictMatch || !ri.ConfirmedClear {
		return knowledge.Incident{}, false
	}
	if strings.TrimSpace(ri.ExternalRef) == "" || strings.TrimSpace(ri.Action) == "" {
		return knowledge.Incident{}, false
	}
	return knowledge.Incident{
		ExternalRef: ri.ExternalRef,
		Host:        ri.Host,
		AlertRule:   ri.AlertRule,
		Site:        ri.Site,
		Summary:     ri.Summary,
		Resolution:  ri.Action,
		Tags:        ri.Tags,
	}, true
}

// Distill maps a batch of resolved incidents to the lessons worth keeping — the confirmed-clean subset. It is
// the teacher's corpus-building pass: the survivors are exactly the incidents the retriever should surface.
func Distill(resolved []ResolvedIncident) []knowledge.Incident {
	out := make([]knowledge.Incident, 0, len(resolved))
	for _, ri := range resolved {
		if lesson, ok := Lesson(ri); ok {
			out = append(out, lesson)
		}
	}
	return out
}
