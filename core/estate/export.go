package estate

import "time"

// SnapshotEdge is a serializable projection of one graph edge for publication to the read API (REQ-516).
type SnapshotEdge struct {
	FromType   string    `json:"from_type"`
	FromName   string    `json:"from_name"`
	ToType     string    `json:"to_type"`
	ToName     string    `json:"to_name"`
	Rel        string    `json:"rel"`
	Confidence float64   `json:"confidence"`
	Source     string    `json:"source"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
}

// Snapshot is the serializable projection of the whole graph — the edge set plus a derived node set.
type Snapshot struct {
	Edges []SnapshotEdge `json:"edges"`
	Nodes []Entity       `json:"nodes"`
}

// Export projects the graph to a serializable snapshot: every stored edge, plus the de-duplicated set
// of entities that appear as an endpoint. It reads the graph without mutating it.
func (g *Graph) Export() Snapshot {
	snap := Snapshot{}
	seen := map[string]bool{}
	addNode := func(e Entity) {
		if !seen[e.key()] {
			seen[e.key()] = true
			snap.Nodes = append(snap.Nodes, e)
		}
	}
	for _, e := range g.edges {
		snap.Edges = append(snap.Edges, SnapshotEdge{
			FromType: string(e.From.Type), FromName: e.From.Name,
			ToType: string(e.To.Type), ToName: e.To.Name,
			Rel: string(e.Rel), Confidence: e.Confidence, Source: string(e.Source),
			ValidUntil: e.ValidUntil,
		})
		addNode(e.From)
		addNode(e.To)
	}
	return snap
}
