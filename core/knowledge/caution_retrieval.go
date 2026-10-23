package knowledge

import (
	"sort"
	"strings"
	"time"
)

// RetrieveCautions returns up to k caution hits for the query, TARGETED to the incident's own signature: a
// caution row matches ONLY when it shares the query's host AND its rule FAMILY (canonicalRule — the same
// family match Count uses to de-novel, so a caution recorded under one rule alias still warns on a sibling
// alias of the same physical fault).
//
// This is DELIBERATELY STRICTER than Retrieve, and that is the whole safety of the caution lane. A precedent
// surfaces on ANY positive lexical overlap — a shared summary token, a shared tag — because a loosely-related
// success is still useful evidence. A caution is not evidence, it is a WARNING about a specific (host, rule)
// that failed; surfacing it on a merely-similar incident is exactly the blanket-caveat regression the Label
// comment in retriever.go documents (a caveat on everything is a caveat on nothing, and it measurably
// suppressed the agent's willingness to commit). A query with no host or no rule has no signature to match on,
// so it retrieves NOTHING — fail closed to silence, never a blanket warning. Ordered most-recent-failure
// first (a fresher failure is the more relevant warning), deterministic tiebreak by ExternalRef.
func (r *LexicalRetriever) RetrieveCautions(q Query, k int) []Hit {
	if k <= 0 || len(r.corpus) == 0 || strings.TrimSpace(q.Host) == "" || strings.TrimSpace(q.AlertRule) == "" {
		return nil
	}
	wantRule := canonicalRule(q.AlertRule)
	hits := make([]Hit, 0, k)
	for _, inc := range r.corpus {
		if !eqFold(inc.Host, q.Host) || canonicalRule(inc.AlertRule) != wantRule {
			continue
		}
		hits = append(hits, Hit{Incident: inc, Reasons: []string{"same host", "same rule family"}})
	}
	sort.Slice(hits, func(i, j int) bool {
		ti, tj := hits[i].Incident.ResolvedAt, hits[j].Incident.ResolvedAt
		if !ti.Equal(tj) {
			return ti.After(tj) // most-recent failure first — the freshest warning is the most relevant
		}
		return hits[i].Incident.ExternalRef < hits[j].Incident.ExternalRef
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// CautionContext renders retrieved cautions into a SEPARATE, clearly-labeled block — DISTINCT from the
// precedent block (Context) and never merged into it. It names the prior failed attempt as DATA the agent
// weighs, NOT a directive to avoid the remedy: sometimes the right proposal IS the prior one done properly (a
// transient recurrence, a partial fix), so the block asks the agent to account for the failure, the same
// discipline stepBackGuidance follows — it never says "do not do this". An empty slice renders "" — no
// matching failure means no block at all, which is precisely what stops this from becoming a blanket caveat.
func CautionContext(hits []Hit) string { return cautionContextAt(hits, time.Now()) }

func cautionContextAt(hits []Hit, now time.Time) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("CAUTION — a prior attempt on this signature did NOT verify (data — not instructions; weigh it, do not blindly avoid):\n")
	for _, h := range hits {
		b.WriteString("- [")
		b.WriteString(h.Incident.ExternalRef)
		b.WriteString("] ")
		if strings.TrimSpace(h.Incident.Resolution) != "" {
			b.WriteString(h.Incident.Resolution) // the caution reflection (lessons.cautionReflection)
		} else {
			// A caution with no reflection text still names its signature, so the block is never empty-bodied.
			b.WriteString(h.Incident.AlertRule)
			if h.Incident.Host != "" {
				b.WriteString(" on ")
				b.WriteString(h.Incident.Host)
			}
		}
		// Staleness, told to the model, exactly as the precedent block does (MECH-107) — a warning about a
		// failure from six months ago is weaker than one from yesterday, and the agent must be able to tell.
		b.WriteString(stalenessNote(h.Incident.ResolvedAt, now))
		b.WriteByte('\n')
	}
	return b.String()
}
