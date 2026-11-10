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
	// DelaySeconds is the learned mean propagation delay along this edge (TG-188 slice 2). Serialized (unlike
	// ExpectedAlerts) because it is a single scalar and its whole point is to be OBSERVABLE in the published
	// snapshot + carried across the relay; 0 = unlearned, omitted.
	DelaySeconds float64 `json:"delay_seconds,omitempty"`
	// RecoverySeconds is the learned mean recovery time / MTTR along this edge (TG-188 slice 2c). Serialized for
	// the same reason as DelaySeconds — a single scalar whose point is to be OBSERVABLE in the snapshot + carried
	// across the relay; 0 = unlearned, omitted.
	RecoverySeconds float64 `json:"recovery_seconds,omitempty"`
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
			ValidUntil: e.ValidUntil, DelaySeconds: e.DelaySeconds, RecoverySeconds: e.RecoverySeconds,
		})
		addNode(e.From)
		addNode(e.To)
	}
	return snap
}
