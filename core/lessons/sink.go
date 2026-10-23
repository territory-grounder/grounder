package lessons

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/territory-grounder/grounder/core/knowledge"
)

// ParseResolved reads a JSON array of ResolvedIncident records — the resolved-incident history the teacher
// distills. Today this is an operator-exported feed (config-not-code, the same shape as the knowledge
// corpus); in Phase 2 the close-out path emits the same records. An entry with no external_ref is rejected
// loudly (a resolved incident with no identity cannot be keyed as precedent), never silently dropped — the
// same fail-loud discipline knowledge.ParseCorpus applies to the corpus it will be merged into.
func ParseResolved(r io.Reader) ([]ResolvedIncident, error) {
	var resolved []ResolvedIncident
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resolved); err != nil {
		return nil, fmt.Errorf("lessons: malformed resolved-incident JSON: %w", err)
	}
	for i, ri := range resolved {
		if strings.TrimSpace(ri.ExternalRef) == "" {
			return nil, fmt.Errorf("lessons: resolved incident %d has no external_ref (cannot be keyed as precedent)", i)
		}
	}
	return resolved, nil
}

// Merge is the persistence hop that closes the learn→retrieve loop: it distills the resolved incidents to
// their confirmed-clean lessons (lessons.Distill — a deviation/partial/unconfirmed outcome is dropped, never
// poisoning the corpus) and merges those into the existing corpus by external_ref (knowledge.MergeCorpus, new
// record wins). It returns the merged corpus and the count of NET-NEW external_refs it contributed (a lesson
// whose ref was already present updates that precedent in place but counts as 0 new) — so the caller persists
// and reloads the corpus only when something actually changed, and an idempotent re-import is a no-op.
func Merge(existing []knowledge.Incident, resolved []ResolvedIncident) ([]knowledge.Incident, int) {
	priorRefs := make(map[string]struct{}, len(existing))
	for _, inc := range existing {
		priorRefs[strings.TrimSpace(inc.ExternalRef)] = struct{}{}
	}
	distilled := Distill(resolved)
	added := 0
	for _, l := range distilled {
		if _, ok := priorRefs[strings.TrimSpace(l.ExternalRef)]; !ok {
			added++
			priorRefs[strings.TrimSpace(l.ExternalRef)] = struct{}{} // a batch with the same new ref twice counts once
		}
	}
	return knowledge.MergeCorpus(existing, distilled), added
}
