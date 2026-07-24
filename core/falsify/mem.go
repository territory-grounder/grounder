package falsify

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
)

// MemStore is the in-memory oracle twin of the pgx falsifiability stores: it satisfies UnscoredReader,
// ScoreWriter, VerdictWriter and CascadeStatsWriter so a test (and the no-DB oracle path) drives the whole
// verify-time writeback pure-Go, with no Postgres. It mirrors the durable semantics exactly: the score write
// is idempotent first-wins (tp-null-only), the verdict is append-only first-wins per action_id, and the
// cascade windows are append-only. Guarded by a mutex — a scoring pass may run concurrently with seeding.
type MemStore struct {
	mu       sync.Mutex
	preds    []*memPred
	byKey    map[string]int // plan_hash → index
	verdicts map[string]safety.Verdict
	windows  []CascadeWindow
}

type memPred struct {
	rec         predict.PredictionRecord
	committedAt time.Time
	scored      bool
	score       Score
}

// NewMemStore returns an empty in-memory falsifiability store.
func NewMemStore() *MemStore {
	return &MemStore{byKey: map[string]int{}, verdicts: map[string]safety.Verdict{}}
}

// compile-time proof the fake satisfies every seam the Scorer depends on.
var (
	_ UnscoredReader     = (*MemStore)(nil)
	_ ScoreWriter        = (*MemStore)(nil)
	_ VerdictWriter      = (*MemStore)(nil)
	_ CascadeStatsWriter = (*MemStore)(nil)
)

// Seed stages a committed (unscored) prediction, mirroring the append-only prediction store: a duplicate
// plan_hash is ignored (first-wins). This is the oracle's stand-in for predict.PredictionStore.Commit + the
// row's committed_at, so a test can stage predictions and then score them. (Named Seed, not Commit, so it
// does not clash with the VerdictWriter Commit below.)
func (m *MemStore) Seed(rec predict.PredictionRecord, committedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byKey[rec.Prediction.PlanHash]; ok {
		return
	}
	m.byKey[rec.Prediction.PlanHash] = len(m.preds)
	m.preds = append(m.preds, &memPred{rec: rec, committedAt: committedAt})
}

// DueForScoring returns unscored predictions committed before olderThan, oldest first, up to limit.
func (m *MemStore) DueForScoring(_ context.Context, olderThan time.Time, limit int) ([]DuePrediction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	due := make([]DuePrediction, 0, len(m.preds))
	for _, p := range m.preds {
		if p.scored || !p.committedAt.Before(olderThan) {
			continue
		}
		due = append(due, DuePrediction{Record: p.rec, CommittedAt: p.committedAt})
	}
	sort.Slice(due, func(i, j int) bool { return due[i].CommittedAt.Before(due[j].CommittedAt) })
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

// WriteScore records the score iff the row is still unscored (idempotent first-wins), returning whether it
// updated — exactly the pgx `WHERE tp IS NULL` semantics.
func (m *MemStore) WriteScore(_ context.Context, planHash string, s Score) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.byKey[planHash]
	if !ok || m.preds[i].scored {
		return false, nil
	}
	m.preds[i].scored = true
	m.preds[i].score = s
	return true, nil
}

// Commit records the verdict append-only, first-wins per action_id (the action_verdict PK semantics) —
// satisfying VerdictWriter.
func (m *MemStore) Commit(_ context.Context, actionID, _, _, _ string, v safety.Verdict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.verdicts[actionID]; ok {
		return nil
	}
	m.verdicts[actionID] = v
	return nil
}

// AppendWindow records a cascade-stats window (append-only).
func (m *MemStore) AppendWindow(_ context.Context, w CascadeWindow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.windows = append(m.windows, w)
	return nil
}

// --- read accessors for assertions ---

// ScoreOf returns the written score for a plan_hash and whether it has been scored.
func (m *MemStore) ScoreOf(planHash string) (Score, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.byKey[planHash]
	if !ok || !m.preds[i].scored {
		return Score{}, false
	}
	return m.preds[i].score, true
}

// VerdictOf returns the persisted verdict for an action_id.
func (m *MemStore) VerdictOf(actionID string) (safety.Verdict, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.verdicts[actionID]
	return v, ok
}

// Windows returns a copy of the appended cascade-stats windows.
func (m *MemStore) Windows() []CascadeWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]CascadeWindow(nil), m.windows...)
}
