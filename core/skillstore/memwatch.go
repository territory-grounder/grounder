package skillstore

import (
	"context"
	"sync"
)

// MemWatchStore backs the oracles.
type MemWatchStore struct {
	mu      sync.Mutex
	watches map[int64]WatchState
	closed  map[int64]string
}

func NewMemWatchStore() *MemWatchStore {
	return &MemWatchStore{watches: map[int64]WatchState{}, closed: map[int64]string{}}
}

func (m *MemWatchStore) PutWatch(_ context.Context, w WatchState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watches[w.VersionID] = w
	return nil
}

func (m *MemWatchStore) OpenWatches(_ context.Context) ([]WatchState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []WatchState
	for _, w := range m.watches {
		out = append(out, w)
	}
	return out, nil
}

func (m *MemWatchStore) UpdateWatch(_ context.Context, w WatchState) error { return m.PutWatch(nil, w) }

func (m *MemWatchStore) CloseWatch(_ context.Context, id int64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watches, id)
	m.closed[id] = reason
	return nil
}
