package policy

// Active-ruleset persistence — spec/015 task T-015-12 (REQ-1503, REQ-1518). The operator's policy is DATA,
// never Rego (REQ-1503): the durable form is the SAME validated JSON document ParseRuleSet accepts. This file
// adds the store seam + an in-memory fake for the oracles; the durable pgx impl (policy_ruleset, migration
// 0019) lives in core/db and satisfies THIS interface. It is the ruleset sibling of the ModeStore /
// GraduationStore / AuditSink seams the earlier leaves left for this persistence leaf.
//
// FAIL-CLOSED BY CONSTRUCTION (INV-09): Save re-validates the document through ParseRuleSet, so a malformed
// document is REFUSED and never persisted as the active policy — the engine keeps its prior known-good rules
// rather than falling open. Load returns ErrRulesetAbsent when no ruleset has been persisted; the caller
// resolves that to the empty RuleSet (every action → the fail-closed default `approve`, never `auto`).
//
// The document is NON-SECRET rules-as-data (op-classes, host globs, verdicts, params) — it carries no
// credential, key material, or secret, so it is safe to persist and surface on the read-only /v1/policy/rules.
//
// Provenance: [R] paradigm-rule 2 (rules-as-data) · [O] INV-09 (fail closed), INV-19 (durable policy). See
// spec/015-policy-engine requirements.md REQ-1503/1518 and design.md (`policy.Engine`).

import (
	"context"
	"errors"
	"sync"
)

// ErrRulesetAbsent is returned by a RulesetStore whose active ruleset has never been persisted. The caller
// resolves it to the empty RuleSet (no rules ⇒ the fail-closed default `approve` for every action).
var ErrRulesetAbsent = errors.New("policy: active ruleset absent")

// RulesetStore persists the operator's single ACTIVE rules-as-data policy document (REQ-1503). Save validates
// the document via ParseRuleSet before persisting (fail-closed — a malformed document is refused, never
// stored) and returns the parsed RuleSet; Load returns the persisted document parsed to a RuleSet PLUS the
// raw non-secret JSON (so the console can render the document verbatim), or ErrRulesetAbsent when none. The
// durable pgx impl (policy_ruleset, migration 0019) is core/db; MemRulesetStore drives the CI oracles.
type RulesetStore interface {
	Load(ctx context.Context) (RuleSet, []byte, error)
	Save(ctx context.Context, document []byte, updatedBy string) (RuleSet, error)
}

// MemRulesetStore is the in-memory RulesetStore fake for oracle tests (CI has no Postgres). It reports
// ErrRulesetAbsent until a document is saved, validates every Save through ParseRuleSet exactly like the pgx
// impl (so a malformed document is refused in tests too), and can be primed with a load error to exercise the
// "unreadable → empty RuleSet" fail-closed path. Concurrency-safe.
type MemRulesetStore struct {
	mu        sync.Mutex
	document  []byte
	rs        RuleSet
	set       bool
	updatedBy string
	loadErr   error
}

// NewMemRulesetStore returns an empty in-memory store (no ruleset persisted → Load fails closed to absent).
func NewMemRulesetStore() *MemRulesetStore { return &MemRulesetStore{} }

// WithLoadError primes the store to fail every Load with err (to test the "unreadable → empty" path).
func (s *MemRulesetStore) WithLoadError(err error) *MemRulesetStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadErr = err
	return s
}

// Load returns the persisted RuleSet + its raw document, or ErrRulesetAbsent when none / a primed error.
func (s *MemRulesetStore) Load(_ context.Context) (RuleSet, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return RuleSet{}, nil, s.loadErr
	}
	if !s.set {
		return RuleSet{}, nil, ErrRulesetAbsent
	}
	return s.rs, append([]byte(nil), s.document...), nil
}

// Save validates the document through ParseRuleSet (fail-closed — a malformed document is refused and NOT
// persisted) and stores it as the active ruleset. It returns the parsed RuleSet on success.
func (s *MemRulesetStore) Save(_ context.Context, document []byte, updatedBy string) (RuleSet, error) {
	rs, err := ParseRuleSet(document)
	if err != nil {
		return RuleSet{}, err // malformed — the prior active ruleset stands (fail closed).
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.document = append([]byte(nil), document...)
	s.rs, s.set, s.updatedBy = rs, true, updatedBy
	return rs, nil
}

// compile-time proof the in-memory fake satisfies the store interface.
var _ RulesetStore = (*MemRulesetStore)(nil)
