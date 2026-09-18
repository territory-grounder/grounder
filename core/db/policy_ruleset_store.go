package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/policy"
)

// PolicyRulesetStore is the pgx-backed policy.RulesetStore: it persists the operator's single ACTIVE
// rules-as-data policy DOCUMENT in the singleton policy_ruleset row (spec/015 REQ-1503, migration 0019).
// Parameters are always bound ($1) — no string-built SQL. Save re-validates the document through
// ParseRuleSet BEFORE persisting (fail-closed — a malformed document is refused, never stored as the active
// policy, so the engine keeps its prior known-good rules). Load returns policy.ErrRulesetAbsent when none.
// The document is NON-SECRET rules-as-data (op-classes, host globs, verdicts, params) — it carries no
// credential or key material, so it is safe to persist and surface on the read-only /v1/policy/rules.
type PolicyRulesetStore struct{ p *Pool }

// NewPolicyRulesetStore returns the Postgres-backed active-ruleset store.
func NewPolicyRulesetStore(p *Pool) *PolicyRulesetStore { return &PolicyRulesetStore{p: p} }

// Load reads the single active ruleset document and parses it to a RuleSet. An empty table returns
// policy.ErrRulesetAbsent (→ the caller resolves it to the empty RuleSet: every action → the fail-closed
// default). A persisted document that no longer parses fails closed with the ParseRuleSet error surfaced.
func (s *PolicyRulesetStore) Load(ctx context.Context) (policy.RuleSet, []byte, error) {
	var document []byte
	err := s.p.Pool.QueryRow(ctx, `SELECT document FROM policy_ruleset WHERE singleton = true`).Scan(&document)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.RuleSet{}, nil, policy.ErrRulesetAbsent
	}
	if err != nil {
		return policy.RuleSet{}, nil, fmt.Errorf("db: policy_ruleset load: %w", err)
	}
	rs, perr := policy.ParseRuleSet(document)
	if perr != nil {
		return policy.RuleSet{}, document, fmt.Errorf("db: policy_ruleset corrupt document: %w", perr)
	}
	return rs, document, nil
}

// Save validates document via ParseRuleSet (fail-closed — a malformed document is refused and NOT persisted),
// upserts it as the single active ruleset (latest-wins on the singleton PK), AND archives it as an IMMUTABLE
// version keyed by its bundle_version (spec/020 T-020-6, REQ-2018) — both in ONE transaction so a ruleset
// change atomically updates the active doc and records the exact version a later decision can join back to.
// The version archive is first-wins (ON CONFLICT DO NOTHING): re-saving an identical ruleset re-archives
// nothing. Returns the parsed RuleSet.
func (s *PolicyRulesetStore) Save(ctx context.Context, document []byte, updatedBy string) (policy.RuleSet, error) {
	rs, err := policy.ParseRuleSet(document)
	if err != nil {
		return policy.RuleSet{}, err // malformed — the prior active ruleset stands (fail closed).
	}
	bv := policy.BundleVersion(rs) // the content fingerprint (REQ-1522) — the version archive + join key.
	tx, err := s.p.Pool.Begin(ctx)
	if err != nil {
		return policy.RuleSet{}, fmt.Errorf("db: policy_ruleset save begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit
	if _, err = tx.Exec(ctx, `
		INSERT INTO policy_ruleset (singleton, document, rule_count, updated_by, updated_at)
		VALUES (true, $1, $2, $3, now())
		ON CONFLICT (singleton) DO UPDATE SET
			document   = EXCLUDED.document,
			rule_count = EXCLUDED.rule_count,
			updated_by = EXCLUDED.updated_by,
			updated_at = now()`,
		document, len(rs.Rules), updatedBy); err != nil {
		return policy.RuleSet{}, fmt.Errorf("db: policy_ruleset save: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO policy_ruleset_version (bundle_version, document, rule_count, saved_by, schema_version)
		VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (bundle_version) DO NOTHING`,
		bv, document, len(rs.Rules), updatedBy); err != nil {
		return policy.RuleSet{}, fmt.Errorf("db: policy_ruleset_version save %s: %w", bv, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return policy.RuleSet{}, fmt.Errorf("db: policy_ruleset save commit: %w", err)
	}
	return rs, nil
}

// compile-time proof the pgx store satisfies the policy.RulesetStore interface.
var _ policy.RulesetStore = (*PolicyRulesetStore)(nil)
