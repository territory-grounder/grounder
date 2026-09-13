// Reflexion caution lane (TG-52). TG's teacher (Lesson) records a precedent ONLY from a confirmed-clean
// success; every deviation, partial, or unverified trajectory is dropped and its lesson is lost. So TG learns
// exclusively from what worked and never from what failed — the exact non-reflectiveness TG-52 names. Caution
// is the COMPLEMENT of Lesson: it captures a trajectory where an action was ATTEMPTED but the outcome was NOT
// a confirmed-clean resolution, distilling it into a verbal self-reflection kept in a STRICTLY SEPARATE
// caution lane (Source == knowledge.ProvenanceCaution), never the precedent corpus.
//
// THE HYGIENE, which is the whole reason this is non-trivial: a failed trajectory must NEVER become a
// learned-match precedent (core/lessons/lessons.go:5-9). Caution and Lesson are mutually exclusive BY
// CONSTRUCTION — Caution's gate is the exact negation of Lesson's success gate — so no session is ever both,
// and the caution corpus is a store distinct from the precedent corpus that the precedent Merge never touches.
// A caution is not advice; it is a record of what did NOT verify, kept so the loop can be cautious about it.
package lessons

import (
	"fmt"
	"strings"

	"github.com/territory-grounder/grounder/core/knowledge"
	"github.com/territory-grounder/grounder/core/safety"
)

// Caution distills a resolved incident whose outcome is NOT a confirmed-clean precedent into a caution — or
// (_, false) when the incident IS a clean success (that is a precedent, Lesson's job, never a caution) or when
// no action was attempted / it has no identity (nothing to be cautious about). The gate is the exact NEGATION
// of Lesson's success gate, so Caution and Lesson partition the outcomes: a session is at most one of
// {precedent, caution}, never both. The Source is ProvenanceCaution — NEVER ProvenanceVerifiedResolution — so
// a caution can never render or merge as a verified precedent. Free text is input-screened on the way in,
// exactly as Lesson screens it.
func Caution(ri ResolvedIncident) (knowledge.Incident, bool) {
	// The complement of Lesson's gate: a clean match+ConfirmedClear is a PRECEDENT, never a caution.
	if ri.Verdict == safety.VerdictMatch && ri.ConfirmedClear {
		return knowledge.Incident{}, false
	}
	// An action must have been ATTEMPTED and the incident must have an identity — a grounded stand-down with
	// no action, or a record with no external_ref, carries no caution worth keeping.
	if strings.TrimSpace(ri.ExternalRef) == "" || strings.TrimSpace(ri.Action) == "" {
		return knowledge.Incident{}, false
	}
	summary, summaryCats := scrubField(ri.Summary)
	action, actionCats := scrubField(ri.Action)
	return knowledge.Incident{
		ExternalRef: ri.ExternalRef,
		Host:        ri.Host,
		AlertRule:   ri.AlertRule,
		Site:        ri.Site,
		Summary:     summary,
		Resolution:  cautionReflection(ri.Host, ri.AlertRule, action, ri.Verdict, ri.ConfirmedClear),
		ResolvedAt:  ri.ResolvedAt,
		// NEVER ProvenanceVerifiedResolution — the killing invariant. A caution is a record of a trajectory
		// that did not verify; stamping it verified would be the exact corpus poisoning TG-52 exists to avoid.
		Source: knowledge.ProvenanceCaution,
		Tags:   withScreenedTags(ri.Tags, summaryCats, actionCats),
	}, true
}

// cautionReflection is the verbal self-reflection a caution carries. It STATES A FACT and gives no
// instruction — the same eval-measured discipline knowledge.Provenance.Label follows (a blanket "do not
// trust" caveat measurably suppressed the agent's willingness to commit, 2026-08-05). It names what was
// attempted and the specific way the outcome fell short, so a reader can weigh it — not a directive.
func cautionReflection(host, rule, action string, verdict safety.Verdict, confirmedClear bool) string {
	shortfall := "the outcome was never confirmed clear"
	switch {
	case verdict == safety.VerdictDeviation:
		shortfall = "the post-state DEVIATED from the prediction (a surprise the fix did not account for)"
	case verdict == safety.VerdictPartial:
		shortfall = "the fix was only PARTIAL"
	case !confirmedClear:
		shortfall = "the condition was never confirmed clear (the fix was asserted, not verified)"
	}
	where := strings.Trim(strings.TrimSpace(host)+"/"+strings.TrimSpace(rule), "/")
	if strings.TrimSpace(where) == "" {
		where = "this incident"
	}
	return fmt.Sprintf("a prior attempt on %s ran %q, and %s — it was not a confirmed-clean resolution.", where, action, shortfall)
}

// DistillCautions maps a batch of resolved incidents to their cautions — the complement of Distill's
// confirmed-clean subset. A clean success yields a precedent (Distill) and no caution; a failed/deviated/
// unverified trajectory with an attempted action yields a caution here and no precedent; a no-action record
// yields neither. So Distill and DistillCautions never both emit for the same incident.
func DistillCautions(resolved []ResolvedIncident) []knowledge.Incident {
	out := make([]knowledge.Incident, 0, len(resolved))
	for _, ri := range resolved {
		if c, ok := Caution(ri); ok {
			out = append(out, c)
		}
	}
	return out
}

// CautionMerge is the persistence hop for the caution lane, the mirror of Merge for the FAILURE outcomes: it
// distills the resolved incidents to their cautions and merges them into the EXISTING CAUTION STORE — never
// the precedent corpus — by external_ref, newest wins. `existing` is the caution lane, a store distinct from
// the precedent corpus: the caller keeps the two separate, which is the structural half of the hygiene. It
// returns the merged caution lane and the count of net-new external_refs contributed (an idempotent re-merge
// adds 0).
func CautionMerge(existing []knowledge.Incident, resolved []ResolvedIncident) ([]knowledge.Incident, int) {
	priorRefs := make(map[string]struct{}, len(existing))
	for _, inc := range existing {
		priorRefs[strings.TrimSpace(inc.ExternalRef)] = struct{}{}
	}
	distilled := DistillCautions(resolved)
	added := 0
	for _, c := range distilled {
		if _, ok := priorRefs[strings.TrimSpace(c.ExternalRef)]; !ok {
			added++
			priorRefs[strings.TrimSpace(c.ExternalRef)] = struct{}{} // a batch with the same new ref twice counts once
		}
	}
	return knowledge.MergeCorpus(existing, distilled), added
}
