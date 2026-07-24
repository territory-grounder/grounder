package estate

import "context"

// EdgeSource is one contributor of estate edges — a NetBox topology reader, a LibreNMS dependency-parent
// reader, a PVE placement reader, the operator-declared table, or a learned pass. Each yields edges already
// stamped with its own confidence + provenance (Source); the builder merges them with the MAX-ratchet.
type EdgeSource interface {
	// Source is the provenance slug this reader stamps (e.g. SourcePVE, SourceNetbox).
	Source() Source
	// Edges returns the edges this source currently contributes. An error is reported per-source and does
	// not abort the others.
	Edges(ctx context.Context) ([]Edge, error)
}

// SourceError records that one source failed to contribute — surfaced loudly rather than swallowed, so a
// dead source degrades to "its edges are missing (and will expire)", never a silent gap presented as truth.
type SourceError struct {
	Source Source
	Err    error
}

func (e SourceError) Error() string { return string(e.Source) + ": " + e.Err.Error() }

// Build seeds a Graph from the ordered sources, PER-SOURCE-ISOLATED: a source that errors is reported in the
// returned []SourceError but the others still commit (the predecessor's infragraph-seed.py design — a
// failing source rolls back only its own contribution). Because Upsert ratchets confidence upward on the
// edge key, source ORDER does not change the final confidence — only which edges are present if a source is
// down. Every edge a source stamps carries that source's confidence + provenance already.
func Build(ctx context.Context, sources []EdgeSource, opts ...Option) (*Graph, []SourceError) {
	g := NewGraph(opts...)
	var errs []SourceError
	for _, s := range sources {
		if s == nil {
			continue
		}
		edges, err := s.Edges(ctx)
		if err != nil {
			errs = append(errs, SourceError{Source: s.Source(), Err: err})
			continue // isolate the failure — the other sources still seed
		}
		for _, e := range edges {
			// A source may leave Confidence unset (0) to accept its policy default; stamp it here so every
			// edge carries a confidence and the ratchet is meaningful.
			if e.Confidence == 0 {
				if c, ok := SourceConfidence[e.Source]; ok {
					e.Confidence = c
				}
			}
			g.Upsert(e)
		}
	}
	return g, errs
}
