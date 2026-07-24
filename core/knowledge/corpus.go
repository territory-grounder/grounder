package knowledge

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ParseCorpus reads a JSON array of prior Incident records — the knowledge corpus the retriever ranks over.
// It is the config-not-code feed (an operator-exported incident history) until a knowledge store feeds the
// retriever automatically. An entry with no external_ref is rejected loudly (a corpus row with no identity
// cannot be cited as precedent), never silently dropped.
func ParseCorpus(r io.Reader) ([]Incident, error) {
	var corpus []Incident
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&corpus); err != nil {
		return nil, fmt.Errorf("knowledge: malformed corpus JSON: %w", err)
	}
	for i, inc := range corpus {
		if strings.TrimSpace(inc.ExternalRef) == "" {
			return nil, fmt.Errorf("knowledge: corpus entry %d has no external_ref (cannot be cited as precedent)", i)
		}
	}
	return corpus, nil
}

// MergeCorpus merges new incidents into an existing corpus, deduplicated by ExternalRef with the NEW record
// winning (a re-resolved incident updates its precedent). Deterministically ordered by ExternalRef so the
// serialized corpus is stable and diff-friendly. This is the write-side of the lessons loop: a resolved
// incident distilled by core/lessons is merged into the corpus the retriever reloads.
func MergeCorpus(existing, added []Incident) []Incident {
	byRef := make(map[string]Incident, len(existing)+len(added))
	order := make([]string, 0, len(existing)+len(added))
	seen := map[string]struct{}{}
	remember := func(inc Incident) {
		ref := strings.TrimSpace(inc.ExternalRef)
		if ref == "" {
			return
		}
		if _, ok := seen[ref]; !ok {
			seen[ref] = struct{}{}
			order = append(order, ref)
		}
		byRef[ref] = inc // last write wins → the newer record updates precedent
	}
	for _, inc := range existing {
		remember(inc)
	}
	for _, inc := range added {
		remember(inc)
	}
	sort.Strings(order)
	out := make([]Incident, 0, len(order))
	for _, ref := range order {
		out = append(out, byRef[ref])
	}
	return out
}

// WriteCorpus serializes a corpus as the JSON array ParseCorpus reads back — a round-trippable, stable form.
func WriteCorpus(w io.Writer, corpus []Incident) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(corpus); err != nil {
		return fmt.Errorf("knowledge: write corpus: %w", err)
	}
	return nil
}
