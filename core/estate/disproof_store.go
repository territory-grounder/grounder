package estate

import (
	"context"
	"sync"
	"time"
)

// EdgeDisproofStore durably records the per-edge disproofs a decay-on-disproof pass produced (TG-206a,
// spec/018). It is the "attach the contradiction to the edge" substrate: rather than lowering a confidence and
// discarding the DecayReport, each decayed learned edge is retained as an attributable disproof so a later
// verdict can vindicate or refute it and the learned-tier lifecycle (TG-388) has a durable disproof history to
// consult. Append-only by contract (a disproof is evidence, never rewritten), and best-effort at the call site
// — a persistence failure NEVER changes the in-memory decay (the graph swap is authoritative), matching the
// competence-plane discipline (this ages learned read-model state only; it never actuates).
type EdgeDisproofStore interface {
	// Record appends the disproofs of ONE decay pass, stamped with the observation time. Returns rows written.
	Record(ctx context.Context, at time.Time, rows []EdgeDisproof) (int, error)
	// List returns every recorded disproof, most-recent pass first — the inspection/rehydration source.
	List(ctx context.Context) ([]RecordedEdgeDisproof, error)
}

// RecordedEdgeDisproof is a persisted EdgeDisproof with the pass observation time it was stamped at.
type RecordedEdgeDisproof struct {
	EdgeDisproof
	ObservedAt time.Time
}

// MemEdgeDisproofStore is the in-memory twin of the durable store: the ORACLE fake (a decay test asserts a
// pass's disproofs round-trip through it) and a self-contained EdgeDisproofStore any in-process caller can
// hold. It is deliberately NOT wired as a production fallback — a worker with no db pool skips durable
// persistence (best-effort; the decay itself is unaffected), because an in-process store nothing reads back
// would only accumulate. Append-only and concurrency-safe.
type MemEdgeDisproofStore struct {
	mu   sync.Mutex
	rows []RecordedEdgeDisproof
}

// NewMemEdgeDisproofStore returns an empty in-memory disproof store.
func NewMemEdgeDisproofStore() *MemEdgeDisproofStore { return &MemEdgeDisproofStore{} }

// compile-time proof the in-memory twin satisfies the seam the worker records through.
var _ EdgeDisproofStore = (*MemEdgeDisproofStore)(nil)

// Record appends each disproof stamped at the pass time. It never mutates an existing row (append-only).
func (m *MemEdgeDisproofStore) Record(_ context.Context, at time.Time, rows []EdgeDisproof) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		m.rows = append(m.rows, RecordedEdgeDisproof{EdgeDisproof: r, ObservedAt: at})
	}
	return len(rows), nil
}

// List returns a copy of every recorded disproof, most-recent pass first.
func (m *MemEdgeDisproofStore) List(_ context.Context) ([]RecordedEdgeDisproof, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RecordedEdgeDisproof, len(m.rows))
	for i := range m.rows {
		out[len(m.rows)-1-i] = m.rows[i] // most-recent first
	}
	return out, nil
}
