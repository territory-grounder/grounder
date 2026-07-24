package breaker

import (
	"context"
	"sort"
	"sync"
)

// MemStore is the in-memory, process-shared Store used as the oracle (and a valid single-process backend).
// It is safe for concurrent use, so many Breaker values sharing one *MemStore coordinate exactly as sibling
// processes coordinate through the pgx row — which is what lets the tests prove cross-instance behavior.
type MemStore struct {
	mu   sync.Mutex
	rows map[string]Record
}

// NewMemStore returns an empty in-memory breaker store.
func NewMemStore() *MemStore { return &MemStore{rows: map[string]Record{}} }

// Load returns the stored record for name, or ok=false if the breaker has never been saved.
func (m *MemStore) Load(_ context.Context, name string) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.rows[name]
	return rec, ok, nil
}

// Save upserts the record by name.
func (m *MemStore) Save(_ context.Context, rec Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[rec.Name] = rec
	return nil
}

// List returns every stored breaker record, ordered by name (for the metrics exporter).
func (m *MemStore) List(_ context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
