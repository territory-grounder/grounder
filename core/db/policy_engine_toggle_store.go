package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/policy"
)

// PolicyEngineToggleStore is the pgx-backed policy.ToggleStore: it persists the SINGLE current admin engine
// override in the singleton policy_engine_toggle row (spec/015 REQ-1519, migration 0030) — the durable sibling
// of PolicyModeStore. So an EngineToggle.Override on the grounder (the authenticated admin plane) reaches the
// worker (the decision plane) through this row. Parameters are always bound ($1/$2) — no string-built SQL. The
// override column is nullable: NULL ⇒ no override (follow the per-mode default). The immutable audit of every
// change is the governance_ledger (Override appends there before it takes effect); this store holds only the
// current state. actor is a non-secret label; no secret can land here.
type PolicyEngineToggleStore struct{ p *Pool }

// NewPolicyEngineToggleStore returns the Postgres-backed engine-toggle override store.
func NewPolicyEngineToggleStore(p *Pool) *PolicyEngineToggleStore { return &PolicyEngineToggleStore{p: p} }

// Load reads the single persisted override. An empty table (no override ever saved) returns (nil, nil) — no
// override, so the toggle follows the per-mode default. A persisted SQL NULL likewise yields nil (an explicit
// "cleared" override). A read error is surfaced so the caller keeps its last-known override rather than
// silently flipping the engine's effective state.
func (s *PolicyEngineToggleStore) Load(ctx context.Context) (*bool, error) {
	var ov *bool
	err := s.p.Pool.QueryRow(ctx, `SELECT override FROM policy_engine_toggle WHERE singleton = true`).Scan(&ov)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: policy_engine_toggle load: %w", err)
	}
	return ov, nil
}

// Save upserts the single override row (latest-wins on the singleton PK). A nil override persists SQL NULL,
// clearing the override so the toggle follows the per-mode default.
func (s *PolicyEngineToggleStore) Save(ctx context.Context, override *bool, actor string) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO policy_engine_toggle (singleton, override, actor, updated_at)
		VALUES (true, $1, $2, now())
		ON CONFLICT (singleton) DO UPDATE SET
			override   = EXCLUDED.override,
			actor      = EXCLUDED.actor,
			updated_at = now()`,
		override, actor)
	if err != nil {
		return fmt.Errorf("db: policy_engine_toggle save: %w", err)
	}
	return nil
}

// compile-time proof the pgx store satisfies the policy.ToggleStore interface.
var _ policy.ToggleStore = (*PolicyEngineToggleStore)(nil)
