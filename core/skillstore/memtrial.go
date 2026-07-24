package skillstore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// MemTrialStore backs the CI oracles (pgx integration under compose, constraint D5). Assign enforces
// read-before-hash: a stored row wins over any recomputation.
type MemTrialStore struct {
	mu          sync.Mutex
	trials      map[int64]Trial
	assignments map[string]int // "ref|trial" → variant
	scores      map[int64]map[int][]float64
	safety      map[int64]map[int][]float64
	rate        float64
	nextID      int64
	malformed   int
}

func NewMemTrialStore(rate float64) *MemTrialStore {
	return &MemTrialStore{trials: map[int64]Trial{}, assignments: map[string]int{},
		scores: map[int64]map[int][]float64{}, safety: map[int64]map[int][]float64{}, rate: rate, nextID: 1}
}

func (m *MemTrialStore) ActiveTrialFor(_ context.Context, name string) (Trial, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.trials {
		if t.SkillName == name && t.Status == "active" {
			return t, true, nil
		}
	}
	return Trial{}, false, nil
}

func (m *MemTrialStore) ActiveTrials(_ context.Context) ([]Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Trial
	for _, t := range m.trials {
		if t.Status == "active" {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *MemTrialStore) Assign(_ context.Context, ref string, trialID int64, variant int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s|%d", ref, trialID)
	if v, ok := m.assignments[key]; ok {
		return v, nil // the stored row wins (read-before-hash)
	}
	m.assignments[key] = variant
	return variant, nil
}

func (m *MemTrialStore) ArmScores(_ context.Context, trialID int64) (map[int][]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scores[trialID], nil
}

func (m *MemTrialStore) SafetyArmScores(_ context.Context, trialID int64) (map[int][]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.safety[trialID], nil
}

func (m *MemTrialStore) FinalizeTrial(_ context.Context, trialID int64, status string, winID int64, winMean, winP float64, note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.trials[trialID]
	t.Status = status
	t.Note = t.Note + "\n" + note
	m.trials[trialID] = t
	return nil
}

func (m *MemTrialStore) JudgedSessionRate(_ context.Context, _ time.Duration) (float64, error) {
	return m.rate, nil
}

func (m *MemTrialStore) CreateTrial(_ context.Context, t Trial) (Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.ID = m.nextID
	m.nextID++
	m.trials[t.ID] = t
	return t, nil
}

// SetScores seeds a trial's per-arm target-dimension scores (test/oracle helper).
func (m *MemTrialStore) SetScores(trialID int64, scores map[int][]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scores[trialID] = scores
}

// SetSafety seeds a trial's per-arm safety-dimension scores (test/oracle helper).
func (m *MemTrialStore) SetSafety(trialID int64, scores map[int][]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.safety[trialID] = scores
}

// CountMalformed increments the malformed-ref counter (called by AssignArm on a rejected key).
func (m *MemTrialStore) CountMalformed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.malformed++
}

// Malformed reports the malformed-ref rejection count (the dead-man metric's input).
func (m *MemTrialStore) Malformed() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.malformed
}
