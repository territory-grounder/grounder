package estate

import (
	"context"
	"fmt"
	"time"
)

// SourceSnapshotRelay is the provenance slug of the RELAY ITSELF — it appears only in SourceError
// reporting. The edges a relay returns keep the sources they were built with (netbox, librenms, pve,
// declared, …): re-stamping them would erase exactly the provenance TG-391 needs rendered and would
// make every relayed edge fight the confidence ratchet under one label.
const SourceSnapshotRelay Source = "snapshot-relay"

// RestoreEdges converts a serialized snapshot back into graph edges — the read half Export never had
// (TG-346). Lossless for what SnapshotEdge carries; ExpectedAlerts is not serialized, so relayed edges
// have none (they influence blast-radius reach and confidence, not alert expectation).
func (s Snapshot) RestoreEdges() []Edge {
	out := make([]Edge, 0, len(s.Edges))
	for _, e := range s.Edges {
		out = append(out, Edge{
			From:         Entity{Type: EntityType(e.FromType), Name: e.FromName},
			To:           Entity{Type: EntityType(e.ToType), Name: e.ToName},
			Rel:          RelType(e.Rel),
			Confidence:   e.Confidence,
			Source:       Source(e.Source),
			ValidUntil:   e.ValidUntil,
			DelaySeconds: e.DelaySeconds,
		})
	}
	return out
}

// SnapshotRelaySource feeds one plane's persisted graph to ANOTHER plane's composer, through the shared
// database rather than through credentials (TG-346).
//
// The actuation plane's graph was 17 edges against the triage plane's 392+ because the estate readers
// need read-triage credentials the credential-plane split (TG-153, REQ-2203) rightly refuses to hand the
// actuation process — my first fix tried exactly that and the boot guard failed it closed, twice. The
// GRAPH is not a credential: the triage plane already persists it to estate_snapshot on every refresh,
// and the actuation plane already holds a database identity. Relaying the snapshot gives the
// blast-radius gate the same estate the triage plane reasons over while each plane keeps only its own
// secrets.
type SnapshotRelaySource struct {
	// Load returns the newest snapshot for the RELAYED plane (triage) plus its write time.
	Load func(ctx context.Context) (Snapshot, time.Time, error)
	// MaxAge bounds staleness. The triage plane snapshots every few minutes; a relay serving an
	// hours-old graph would hide a decommission (or a collapse, TG-375) from the gate. On a stale or
	// failed read the source ERRORS — Build/Refresh isolate that per-source, so the composer keeps the
	// prior graph and the pve source still contributes, and the failure is loud rather than a silent
	// shrink to 17 edges.
	MaxAge time.Duration
	// Now is injectable for the oracles; nil means time.Now.
	Now func() time.Time
}

func (s SnapshotRelaySource) Source() Source { return SourceSnapshotRelay }

func (s SnapshotRelaySource) Edges(ctx context.Context) ([]Edge, error) {
	if s.Load == nil {
		return nil, fmt.Errorf("snapshot relay: no loader wired")
	}
	snap, at, err := s.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot relay: %w", err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if s.MaxAge > 0 {
		if age := now().Sub(at); age > s.MaxAge {
			return nil, fmt.Errorf("snapshot relay: newest relayed snapshot is %s old (bound %s) — refusing to serve a stale estate to the gate; the prior graph is kept and this failure is the signal", age.Round(time.Second), s.MaxAge)
		}
	}
	// VACUITY FLOOR. A 0-edge snapshot is not a graph, it is an outage artifact — installing it would
	// blank the relayed tier during exactly the incident where the gate needs it (TG-375's shape).
	if len(snap.Edges) == 0 {
		return nil, fmt.Errorf("snapshot relay: newest relayed snapshot carries 0 edges — refusing to relay an empty estate")
	}
	return snap.RestoreEdges(), nil
}
