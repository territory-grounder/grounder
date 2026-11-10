package db

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/territory-grounder/grounder/core/instrumentid"
)

// InstrumentIdentityStore persists the components behind each instrument identity (TG-672, migration 0132) —
// the readable record behind session_triage.instrument_id, as harness_render is behind harness_fp. One INSERT
// per identity per process: the first session under a configuration writes it, every later one is a cache hit.
type InstrumentIdentityStore struct {
	p    *Pool
	mu   sync.Mutex
	seen map[string]bool
}

// NewInstrumentIdentityStore returns the Postgres-backed identity sink.
func NewInstrumentIdentityStore(p *Pool) *InstrumentIdentityStore {
	return &InstrumentIdentityStore{p: p, seen: map[string]bool{}}
}

var _ instrumentid.Sink = (*InstrumentIdentityStore)(nil)

// Ensure records rec once. The row carries every component and the names of the unrecorded ones; a second
// writer of the same id (another worker, a retry) is a no-op, never a conflict.
func (s *InstrumentIdentityStore) Ensure(ctx context.Context, rec instrumentid.Record) error {
	if rec.ID == "" {
		return fmt.Errorf("db: instrument identity: empty id refused")
	}
	s.mu.Lock()
	if s.seen[rec.ID] {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	_, err := s.p.Exec(ctx, `
		INSERT INTO instrument_identity (instrument_id, build_sha, harness_fp, opschema_hash, policy_bundle, model_alias_hash,
		                                 rubric_version, skill_set_hash, knowledge_hash, unrecorded)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (instrument_id) DO NOTHING`,
		rec.ID, rec.BuildSHA, rec.HarnessFP, rec.OpSchemaHash, rec.PolicyBundle, rec.ModelAliasHash,
		rec.RubricVersion, rec.SkillSetHash, rec.KnowledgeHash, strings.Join(rec.Unrecorded(), ","))
	if err != nil {
		return fmt.Errorf("db: insert instrument_identity %s: %w", rec.ID[:12], err)
	}
	s.mu.Lock()
	s.seen[rec.ID] = true
	s.mu.Unlock()
	return nil
}
