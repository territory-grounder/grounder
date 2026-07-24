package cost

import (
	"context"
	"sync"
)

// MemStore is the in-memory, process-shared Store used as the oracle (and a valid single-process backend
// when no DB is configured). It is safe for concurrent use, so many Accountant values sharing one *MemStore
// coordinate exactly as sibling processes coordinate through the pgx rows — which is what lets the tests
// prove the cross-instance spend kill (a trip through one Accountant is read as OPEN by another).
type MemStore struct {
	mu      sync.Mutex
	buckets map[string]float64 // (kind|key) -> running USD total
	open    bool
	reason  string
}

// NewMemStore returns an empty in-memory cost store.
func NewMemStore() *MemStore { return &MemStore{buckets: map[string]float64{}} }

func bucketKey(kind, key string) string { return kind + "\x00" + key }

// Accrue adds usd to the (kind,key) bucket and returns the new running total.
func (m *MemStore) Accrue(_ context.Context, kind, key string, usd float64) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[bucketKey(kind, key)] += usd
	return m.buckets[bucketKey(kind, key)], nil
}

// Total reads the running total for a (kind,key) bucket (0 if unseen).
func (m *MemStore) Total(_ context.Context, kind, key string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buckets[bucketKey(kind, key)], nil
}

// BreakerOpen reports the shared breaker state (open + reason).
func (m *MemStore) BreakerOpen(_ context.Context) (bool, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open, m.reason, nil
}

// TripBreaker sets the shared breaker state OPEN (latest-wins).
func (m *MemStore) TripBreaker(_ context.Context, reason string, _ float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.open = true
	m.reason = reason
	return nil
}

// compile-time proof the in-memory twin satisfies the Store interface.
var _ Store = (*MemStore)(nil)
