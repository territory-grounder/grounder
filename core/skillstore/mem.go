package skillstore

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
)

// ErrNotFound is returned for an unknown skill or version id.
var ErrNotFound = errors.New("skillstore: not found")

// MemStore is the in-memory Store used by the CI oracles and unit tests (constraint D5: acceptance is
// pure-Go; the pgx implementation in core/db is integration-tested under compose). It enforces the same
// structural invariants the schema does — one production per skill, unique (name, version) — so a test
// passing here means the state machine logic holds, not that the fake is looser.
type MemStore struct {
	mu       sync.Mutex
	skills   map[string]Skill
	versions map[int64]Version
	nextID   int64
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{skills: map[string]Skill{}, versions: map[int64]Version{}, nextID: 1}
}

// PutSkill registers a skill identity row.
func (m *MemStore) PutSkill(s Skill) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skills[s.Name] = s
}

// CreateVersion validates and inserts a draft row, returning it with its assigned id.
func (m *MemStore) CreateVersion(ctx context.Context, v Version) (Version, error) {
	if err := ValidateDraft(ctx, m, v); err != nil {
		return Version{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.versions {
		if ex.SkillName == v.SkillName && ex.Version == v.Version {
			return Version{}, errors.New("skillstore: duplicate (skill, version)")
		}
	}
	v.ID = m.nextID
	m.nextID++
	v.Status = StatusDraft
	m.versions[v.ID] = v
	return v, nil
}

// GetVersion implements Store.
func (m *MemStore) GetVersion(_ context.Context, id int64) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[id]
	if !ok {
		return Version{}, ErrNotFound
	}
	return v, nil
}

// GetSkill implements Store.
func (m *MemStore) GetSkill(_ context.Context, name string) (Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.skills[name]
	if !ok {
		return Skill{}, ErrNotFound
	}
	return s, nil
}

// ProductionVersion implements Store.
func (m *MemStore) ProductionVersion(_ context.Context, skillName string) (Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.versions {
		if v.SkillName == skillName && v.Status == StatusProduction {
			return v, true, nil
		}
	}
	return Version{}, false, nil
}

// UpdateVersion implements Store, enforcing the one-production structural invariant like the partial
// unique index does (a second production row for the same skill is an error, not a silent overwrite).
func (m *MemStore) UpdateVersion(_ context.Context, v Version) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.versions[v.ID]; !ok {
		return ErrNotFound
	}
	if v.Status == StatusProduction {
		for id, ex := range m.versions {
			if id != v.ID && ex.SkillName == v.SkillName && ex.Status == StatusProduction {
				return errors.New("skillstore: one-production invariant violated")
			}
		}
	}
	m.versions[v.ID] = v
	return nil
}

// SetOfflineEval stores an offline eval blob on a version (OfflineEvalWriter).
func (m *MemStore) SetOfflineEval(_ context.Context, id int64, blob json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[id]
	if !ok {
		return ErrNotFound
	}
	v.OfflineEval = blob
	m.versions[id] = v
	return nil
}

// ProductionVersions lists the current production version of every skill (FlywheelStore), oldest id
// first for deterministic iteration.
func (m *MemStore) ProductionVersions(_ context.Context) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, v := range m.versions {
		if v.Status == StatusProduction {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// OpenCandidates counts open (draft|trial) flywheel candidates parented on a production version
// (FlywheelStore) — the generator's dedup.
func (m *MemStore) OpenCandidates(_ context.Context, parentVersionID int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.versions {
		if v.ParentVersionID == parentVersionID && v.Author == AuthorFlywheel &&
			(v.Status == StatusDraft || v.Status == StatusTrial) {
			n++
		}
	}
	return n, nil
}

// FlywheelDrafts lists open flywheel DRAFT versions awaiting the offline gate (FlywheelStore).
func (m *MemStore) FlywheelDrafts(_ context.Context) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, v := range m.versions {
		if v.Status == StatusDraft && v.Author == AuthorFlywheel {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AdmittedCandidates lists the flywheel trial-status versions parented on a production version
// (FlywheelStore) — offline-passed, awaiting an active trial.
func (m *MemStore) AdmittedCandidates(_ context.Context, parentVersionID int64) ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, v := range m.versions {
		if v.ParentVersionID == parentVersionID && v.Author == AuthorFlywheel && v.Status == StatusTrial {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// VersionsOf lists a skill's versions, newest id first (test/introspection helper).
func (m *MemStore) VersionsOf(skillName string) []Version {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Version
	for _, v := range m.versions {
		if v.SkillName == skillName {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
