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
type PolicyModeStore struct {
	p     *Pool
	raise *Pool // TG-661: the tg_mode_admin pool a RAISE is written through; nil ⇒ the plane pool (the guard refuses)
}

// NewPolicyModeStore returns the Postgres-backed active-mode store.
func NewPolicyModeStore(p *Pool) *PolicyModeStore { return &PolicyModeStore{p: p} }

// WithRaisePool routes writes that RAISE the mode into an actuating one (Semi-auto / Full-auto) through a
// separate pool — the dyndb-leased tg_mode_admin login (TG-661): migration 0135's policy_mode_raise_guard refuses a
// raise from every runtime role, so without this pool a transition into an actuating mode fails at persist (the
// ledger keeps the attempted-but-unactivated record, the mode stays). Lowering (Shadow / HITL — the safety kill,
// the resync re-persist) ALWAYS goes through the plane pool, so the kill path never depends on a second
// credential being leasable. A nil pool is the documented unarmed state.
func (s *PolicyModeStore) WithRaisePool(p *Pool) *PolicyModeStore {
	s.raise = p
	return s
}

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

// Save upserts the single active-mode row to m's canonical spelling (latest-wins on the singleton PK). A write
// INTO an actuating mode goes through the raise pool when one is wired (TG-661); everything else through the
// plane pool.
func (s *PolicyModeStore) Save(ctx context.Context, m policy.Mode) error {
	pool := s.p
	if m.MayAutoActuate() && s.raise != nil {
		pool = s.raise
	}
	_, err := pool.Pool.Exec(ctx, `
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
