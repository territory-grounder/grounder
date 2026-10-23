package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/policy"
)

// PolicyModeStore is the pgx-backed policy.ModeStore: it persists the SINGLE active global autonomy mode in
// the singleton policy_mode row (spec/015 REQ-1500/1519, migration 0019). Parameters are always bound ($1) —
// no string-built SQL. Load returns policy.ErrModeAbsent when no mode has been persisted, which the
// ModeController resolves fail-closed to Shadow (REQ-1519); a persisted mode text that is not one of the four
// canonical spellings is treated as absent (fail closed, never an actuating mode). Mode TRANSITIONS are
// audited as immutable governance_ledger records by the ModeController — this store holds only the current
// mode. The mode name is a non-secret label; no secret can land here.
type PolicyModeStore struct{ p *Pool }

// NewPolicyModeStore returns the Postgres-backed active-mode store.
func NewPolicyModeStore(p *Pool) *PolicyModeStore { return &PolicyModeStore{p: p} }

// Load reads the single persisted active mode. An empty table (no mode ever saved) returns
// policy.ErrModeAbsent (→ fail-closed Shadow at the controller); an unparseable persisted spelling also fails
// closed to (Shadow, error) so a corrupt row is never read as an actuating mode.
func (s *PolicyModeStore) Load(ctx context.Context) (policy.Mode, error) {
	var name string
	err := s.p.Pool.QueryRow(ctx, `SELECT mode FROM policy_mode WHERE singleton = true`).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.ModeShadow, policy.ErrModeAbsent
	}
	if err != nil {
		return policy.ModeShadow, fmt.Errorf("db: policy_mode load: %w", err)
	}
	m, perr := policy.ParseMode(name)
	if perr != nil {
		// A corrupt persisted spelling fails closed to Shadow with the error surfaced (never an actuating mode).
		return policy.ModeShadow, fmt.Errorf("db: policy_mode corrupt spelling %q: %w", name, perr)
	}
	return m, nil
}

// Save upserts the single active-mode row to m's canonical spelling (latest-wins on the singleton PK).
func (s *PolicyModeStore) Save(ctx context.Context, m policy.Mode) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO policy_mode (singleton, mode, updated_at)
		VALUES (true, $1, now())
		ON CONFLICT (singleton) DO UPDATE SET
			mode       = EXCLUDED.mode,
			updated_at = now()`,
		m.String())
	if err != nil {
		return fmt.Errorf("db: policy_mode save (%s): %w", m, err)
	}
	return nil
}

// compile-time proof the pgx store satisfies the policy.ModeStore interface.
var _ policy.ModeStore = (*PolicyModeStore)(nil)
