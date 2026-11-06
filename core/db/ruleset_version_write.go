package db

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

// RulesetVersionStore is the pgx-backed writer/reader for the IMMUTABLE, versioned ruleset archive
// (policy_ruleset_version, migration 0029) — the decision tracer's join from a past policy_decision.bundle_version
// to the EXACT rules-as-data document in force when it decided (spec/020 T-020-6, REQ-2018). Every ruleset
// version is kept, keyed by its bundle_version (the content fingerprint, REQ-1522), so a historical decision
// resolves the doc that produced it rather than the singleton latest-wins state (policy_ruleset). Parameters are
// always bound ($1); the document is NON-SECRET rules-as-data (op-classes/host-globs/verdicts/params — no
// credential or key material). The runtime role holds no UPDATE/DELETE (0029 REVOKE): a version, once recorded,
// is immutable, so the archive is tamper-resistant.
type RulesetVersionStore struct{ p *Pool }

// NewRulesetVersionStore returns the Postgres-backed versioned-ruleset store.
func NewRulesetVersionStore(p *Pool) *RulesetVersionStore { return &RulesetVersionStore{p: p} }

// Save records one ruleset version — FIRST-WINS on bundle_version (ON CONFLICT DO NOTHING). A re-save of the
// same version is a no-op by construction (the same bundle_version is the same content), so the archive is
// append-only and idempotent. An empty bundle_version is refused.
func (s *RulesetVersionStore) Save(ctx context.Context, bundleVersion string, document []byte, ruleCount int, savedBy string) error {
	if bundleVersion == "" {
		return errors.New("db: ruleset version with empty bundle_version refused")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO policy_ruleset_version (bundle_version, document, rule_count, saved_by, schema_version)
		VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (bundle_version) DO NOTHING`,
		bundleVersion, document, ruleCount, savedBy)
	if err != nil {
		return fmt.Errorf("db: ruleset version save %s: %w", bundleVersion, err)
	}
	return nil
}

// Get resolves a bundle_version to its exact immutable ruleset document — the decision-tracer JOIN
// (policy_decision.bundle_version -> policy_ruleset_version.document). Read-only, bound $1. ok=false when the
// version was never archived (e.g. a decision made before this table existed): the caller falls back to "the
// exact doc is not retained", never to the current singleton.
func (s *RulesetVersionStore) Get(ctx context.Context, bundleVersion string) ([]byte, bool, error) {
	var document []byte
	err := s.p.QueryRow(ctx,
		`SELECT document FROM policy_ruleset_version WHERE bundle_version = $1`, bundleVersion).Scan(&document)
	switch {
	case err == nil:
		return document, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("db: ruleset version get %s: %w", bundleVersion, err)
	}
}

// MemRulesetVersionStore is the in-memory twin for the CI oracles (no Postgres): first-wins Save keyed by
// bundle_version + Get, mirroring the pgx immutability so an acceptance test proves a past decision resolves
// the EXACT archived doc without a database. The pgx round-trip (ruleset_version_write_test.go, DSN-gated)
// proves the real SQL. Concurrency-safe.
type MemRulesetVersionStore struct {
	mu   sync.Mutex
	docs map[string][]byte
}

// NewMemRulesetVersionStore returns an empty in-memory versioned-ruleset twin.
func NewMemRulesetVersionStore() *MemRulesetVersionStore {
	return &MemRulesetVersionStore{docs: map[string][]byte{}}
}

// Save records a version first-wins (a re-save of the same bundle_version is a no-op), mirroring the pgx
// ON CONFLICT DO NOTHING.
func (m *MemRulesetVersionStore) Save(_ context.Context, bundleVersion string, document []byte, _ int, _ string) error {
	if bundleVersion == "" {
		return errors.New("db: ruleset version with empty bundle_version refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.docs[bundleVersion]; !ok {
		cp := make([]byte, len(document))
		copy(cp, document)
		m.docs[bundleVersion] = cp
	}
	return nil
}

// Get resolves a bundle_version to its archived doc (ok=false when never saved).
func (m *MemRulesetVersionStore) Get(_ context.Context, bundleVersion string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.docs[bundleVersion]
	if !ok {
		return nil, false, nil
	}
	cp := make([]byte, len(doc))
	copy(cp, doc)
	return cp, true, nil
}
