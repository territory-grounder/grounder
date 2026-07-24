package lessons

// Recency/decay discipline for the teacher's corpus (spec/018, design-wisdom #11; Gulli ch14 — periodic
// reconciliation). A lesson now carries a PROVENANCE timestamp (ResolvedIncident.ResolvedAt), so its AGE is
// known and its influence can decay: HalfLifeWeight quantifies the down-weighting (a lesson at one half-life
// counts half), and Reconcile + PruneStaleFromCorpus realize the pruning (a lesson older than the retention
// horizon leaves the retrieval corpus, so its influence decays to zero). This ages LEARNED state only — it
// never touches the estate, never actuates, and never gates. Mutation stays OFF.

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// HalfLifeWeight returns a lesson's recency weight in [0,1] from its provenance age: 1.0 at age 0, 0.5 at one
// half-life, decaying exponentially (2^(-age/halfLife)). It is the influence quantifier the retrieval plane
// (or an operator dashboard) can multiply a lesson's relevance by so an ancient precedent no longer counts as
// much as a fresh one. A zero resolvedAt (no provenance) or a non-positive halfLife yields 1.0 — an undatable
// lesson is never down-weighted on an age we cannot prove; a future observation (negative age) is also 1.0.
func HalfLifeWeight(resolvedAt, now time.Time, halfLife time.Duration) float64 {
	if resolvedAt.IsZero() || halfLife <= 0 {
		return 1.0
	}
	age := now.Sub(resolvedAt)
	if age <= 0 {
		return 1.0
	}
	return math.Exp2(-age.Seconds() / halfLife.Seconds())
}

// ReconcileResult is one lessons reconciliation pass: the lessons still within the retention horizon (Fresh)
// and the external_refs of the lessons pruned for exceeding it (StaleRefs, sorted).
type ReconcileResult struct {
	Fresh     []ResolvedIncident
	StaleRefs []string
}

// Reconcile partitions a resolved-incident feed by PROVENANCE AGE: a lesson whose ResolvedAt is older than
// maxAge is STALE — its influence has decayed to zero and it is pruned; a lesson within maxAge, or one with no
// provenance timestamp (undatable), is KEPT. maxAge <= 0 disables pruning (every lesson kept). This is the
// periodic reconciliation pass: the corpus tracks recent, still-relevant precedent instead of accumulating
// stale advice forever. Pure over its inputs — the caller persists the effect with PruneStaleFromCorpus.
func Reconcile(resolved []ResolvedIncident, now time.Time, maxAge time.Duration) ReconcileResult {
	res := ReconcileResult{Fresh: make([]ResolvedIncident, 0, len(resolved))}
	for _, ri := range resolved {
		if maxAge > 0 && !ri.ResolvedAt.IsZero() && now.Sub(ri.ResolvedAt) > maxAge {
			res.StaleRefs = append(res.StaleRefs, strings.TrimSpace(ri.ExternalRef))
			continue
		}
		res.Fresh = append(res.Fresh, ri)
	}
	sort.Strings(res.StaleRefs)
	return res
}

// PruneStaleFromCorpus removes the corpus incidents whose external_ref is in staleRefs — the durable
// counterpart of Reconcile: a lesson the reconciliation aged out loses its influence by LEAVING the retrieval
// corpus. It returns the surviving corpus and how many precedents were pruned. Order-preserving; a nil/empty
// staleRefs is a no-op (the corpus is returned unchanged). Pure — the caller persists the survivors.
func PruneStaleFromCorpus(corpus []knowledge.Incident, staleRefs []string) ([]knowledge.Incident, int) {
	if len(staleRefs) == 0 {
		return corpus, 0
	}
	stale := make(map[string]struct{}, len(staleRefs))
	for _, r := range staleRefs {
		stale[strings.TrimSpace(r)] = struct{}{}
	}
	kept := make([]knowledge.Incident, 0, len(corpus))
	removed := 0
	for _, inc := range corpus {
		if _, ok := stale[strings.TrimSpace(inc.ExternalRef)]; ok {
			removed++
			continue
		}
		kept = append(kept, inc)
	}
	return kept, removed
}
