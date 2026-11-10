package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/skillstore"
)

// errRealWrite is the shadow store's one refusal: a write aimed at a row that exists in the database. The dry run
// may create and move its OWN rows (negative ids) freely; it may never touch a live one. The proposers and the
// composer only ever update rows they created, so this path is unreachable in a correct run — and if a future
// change makes it reachable, the operator sees a refusal instead of a silent write to production.
var errRealWrite = errors.New("proposerdryrun: refusing to write a LIVE skill_version row — the dry run persists nothing")

// liveReader is the read side the shadow store delegates to (core/db.SkillStore in production).
type liveReader interface {
	GetVersion(ctx context.Context, id int64) (skillstore.Version, error)
	GetSkill(ctx context.Context, name string) (skillstore.Skill, error)
	ProductionVersion(ctx context.Context, skillName string) (skillstore.Version, bool, error)
}

// shadowStore satisfies the store surface the proposers (GenerateCandidatesFromEvidence), the composer
// (ComposeCandidates → Transition) and the prober (OfflineRunner.Store) use: reads come from the live store, every
// write lands in memory under a negative id. The same validation the real CreateVersion runs (ValidateDraft) runs
// here, so a draft the database would refuse (no rationale, wrong content hash, pinned skill, body bounds) is refused
// by the rehearsal too.
type shadowStore struct {
	live liveReader
	now  func() time.Time

	mu     sync.Mutex
	next   int64
	rows   map[int64]skillstore.Version
	writes []string // the audit trail printed at the end: what WOULD have been written
}

func newShadowStore(live liveReader) *shadowStore {
	return &shadowStore{live: live, now: func() time.Time { return time.Now().UTC() }, rows: map[int64]skillstore.Version{}}
}

func (s *shadowStore) GetSkill(ctx context.Context, name string) (skillstore.Skill, error) {
	return s.live.GetSkill(ctx, name)
}

func (s *shadowStore) ProductionVersion(ctx context.Context, skillName string) (skillstore.Version, bool, error) {
	return s.live.ProductionVersion(ctx, skillName)
}

func (s *shadowStore) GetVersion(ctx context.Context, id int64) (skillstore.Version, error) {
	if id < 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		v, ok := s.rows[id]
		if !ok {
			return skillstore.Version{}, fmt.Errorf("proposerdryrun: shadow version %d does not exist", id)
		}
		return v, nil
	}
	return s.live.GetVersion(ctx, id)
}

// CreateVersion mirrors core/db.SkillStore.CreateVersion minus the database: validate, stamp status/timestamps,
// compute the content hash the chain would record, assign the next negative id.
func (s *shadowStore) CreateVersion(ctx context.Context, v skillstore.Version) (skillstore.Version, error) {
	if err := skillstore.ValidateDraft(ctx, s, v); err != nil {
		return skillstore.Version{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next--
	v.ID = s.next
	v.Status = skillstore.StatusDraft
	v.CreatedAt = s.now()
	v.StatusChangedAt = v.CreatedAt
	// The content hash is the CALLER's (the proposers compute it, ValidateDraft above checked it) — exactly as the
	// real store, which never fills one in; a draft without it was refused above, never silently repaired.
	s.rows[v.ID] = v
	s.writes = append(s.writes, fmt.Sprintf("would create version %s v%s (%d bytes, author %s, source %s) → shadow id %d",
		v.SkillName, v.Version, len(v.Body), v.Author, v.Source, v.ID))
	return v, nil
}

func (s *shadowStore) UpdateVersion(ctx context.Context, v skillstore.Version) error {
	if v.ID >= 0 {
		return fmt.Errorf("%w (id %d, status %s)", errRealWrite, v.ID, v.Status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.rows[v.ID]
	if !ok {
		return fmt.Errorf("proposerdryrun: shadow version %d does not exist", v.ID)
	}
	s.rows[v.ID] = v
	s.writes = append(s.writes, fmt.Sprintf("would move shadow id %d: %s → %s", v.ID, prev.Status, v.Status))
	return nil
}

// Shadow returns the rehearsal's rows in creation order (most negative id first = oldest).
func (s *shadowStore) Shadow() []skillstore.Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]skillstore.Version, 0, len(s.rows))
	for id := int64(-1); id >= s.next; id-- {
		if v, ok := s.rows[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

// Writes returns the would-have-been-written audit trail.
func (s *shadowStore) Writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// memLedger is the governance ledger the composer's Transition calls append to — a list, never a table.
type memLedger struct {
	mu      sync.Mutex
	entries []audit.LedgerEntry
}

func (l *memLedger) Append(d audit.GovDecision) (audit.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := audit.LedgerEntry{Seq: int64(len(l.entries) + 1), Decision: d.Decision, Reason: d.Reason, ActionID: d.ActionID, Withheld: d.Withheld}
	l.entries = append(l.entries, e)
	return e, nil
}

func (l *memLedger) Entries() []audit.LedgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]audit.LedgerEntry(nil), l.entries...)
}

// describeComposition names what the composer did, from its return values and the shadow's rows.
func describeComposition(composed skillstore.Version, kept bool, drafts []skillstore.Version) string {
	switch {
	case kept:
		return "KEEP_BASE — the composer rejected every proposer draft (nothing to admit)"
	case composed.ID == 0:
		return "no composition — fewer than two proposer drafts, or two invalid composer replies (drafts untouched)"
	case strings.Contains(composed.Version, "-composed-"):
		return "MERGED — one composed candidate " + composed.Version + "; the proposals were withdrawn"
	default:
		for _, d := range drafts {
			if d.ID == composed.ID {
				return "ADOPT-ONE — the composition equals proposal " + d.Version + " byte for byte; the other proposal was withdrawn"
			}
		}
		return "candidate " + composed.Version
	}
}
