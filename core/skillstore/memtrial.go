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
	proposals   map[int64]map[int][]float64
	pinned      map[string]bool
	rate        float64
	dimRate     map[string]float64
	nextID      int64
	malformed   int
}

// SetPinned marks a skill pinned for the oracles (TG-218).
func (m *MemTrialStore) SetPinned(name string, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pinned == nil {
		m.pinned = map[string]bool{}
	}
	m.pinned[name] = v
}

// IsPinned implements TrialStore.
func (m *MemTrialStore) IsPinned(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pinned[name], nil
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

// ProposalArmRates implements the anti-abstention guard's read for the in-memory oracle store.
func (m *MemTrialStore) ProposalArmRates(_ context.Context, trialID int64) (map[int][]float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proposals[trialID], nil
}

// SetProposals seeds a trial's per-arm proposal indicators (test/oracle helper; 1.0 proposed / 0.0 stood down).
func (m *MemTrialStore) SetProposals(trialID int64, rates map[int][]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proposals == nil {
		m.proposals = map[int64]map[int][]float64{}
	}
	m.proposals[trialID] = rates
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

// ArmFillRate answers per DIMENSION. dimRate wins when the test declares one; m.rate remains the
// fallback so existing single-dimension cases keep their meaning.
func (m *MemTrialStore) ArmFillRate(_ context.Context, dimension string, _ time.Duration) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.dimRate[dimension]; ok {
		return r, nil
	}
	return m.rate, nil
}

// SetDimensionRate declares the observed filling-sample supply for one dimension.
func (m *MemTrialStore) SetDimensionRate(dimension string, perDay float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dimRate == nil {
		m.dimRate = map[string]float64{}
	}
	m.dimRate[dimension] = perDay
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
