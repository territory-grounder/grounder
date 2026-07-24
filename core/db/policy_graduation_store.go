package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/territory-grounder/grounder/core/policy"
)

// PolicyGraduationStore is the pgx-backed policy.GraduationStore: it persists per-op-class earned-autonomy
// ladder state in policy_graduation, one latest-wins row per op-class (spec/015 REQ-1514, migration 0019).
// Parameters are always bound ($1) — no string-built SQL. Load returns policy.ErrClassAbsent for an unknown
// class, which the Ladder resolves fail-closed to a fresh LevelApprove state (never LevelAuto) — a class is
// NEVER loaded straight into auto from an absent/corrupt store. A corrupt persisted level spelling is treated
// as absent (fail closed to approve). The op-class label + level are non-secret; no secret can land here.
type PolicyGraduationStore struct{ p *Pool }

// NewPolicyGraduationStore returns the Postgres-backed per-op-class graduation store.
func NewPolicyGraduationStore(p *Pool) *PolicyGraduationStore { return &PolicyGraduationStore{p: p} }

// Load reads the persisted ladder state for opClass. An absent row returns policy.ErrClassAbsent (→ fresh
// approve at the Ladder); a corrupt persisted level fails closed to ErrClassAbsent so the class is never
// loaded as auto from a bad row.
func (s *PolicyGraduationStore) Load(ctx context.Context, opClass string) (policy.ClassState, error) {
	var level, lastOutcome string
	var count int
	err := s.p.Pool.QueryRow(ctx, `
		SELECT level, clean_run_count, last_outcome FROM policy_graduation WHERE op_class = $1`, opClass).
		Scan(&level, &count, &lastOutcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.ClassState{}, fmt.Errorf("%w: %q", policy.ErrClassAbsent, opClass)
	}
	if err != nil {
		return policy.ClassState{}, fmt.Errorf("db: policy_graduation load (%q): %w", opClass, err)
	}
	lvl, ok := parseLevel(level)
	if !ok {
		// A corrupt persisted level fails closed to approve — treat as absent so the Ladder never loads auto.
		return policy.ClassState{}, fmt.Errorf("%w: %q (corrupt level %q)", policy.ErrClassAbsent, opClass, level)
	}
	return policy.ClassState{
		OpClass:       opClass,
		Level:         lvl,
		CleanRunCount: count,
		LastOutcome:   parseOutcome(lastOutcome),
	}, nil
}

// Save upserts st keyed by its op-class (latest-wins on the op_class PK).
func (s *PolicyGraduationStore) Save(ctx context.Context, st policy.ClassState) error {
	_, err := s.p.Pool.Exec(ctx, `
		INSERT INTO policy_graduation (op_class, level, clean_run_count, last_outcome, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (op_class) DO UPDATE SET
			level           = EXCLUDED.level,
			clean_run_count = EXCLUDED.clean_run_count,
			last_outcome    = EXCLUDED.last_outcome,
			updated_at      = now()`,
		st.OpClass, st.Level.String(), st.CleanRunCount, st.LastOutcome.String())
	if err != nil {
		return fmt.Errorf("db: policy_graduation save (%q): %w", st.OpClass, err)
	}
	return nil
}

// parseLevel maps a persisted level spelling to a policy.Level. An unknown spelling fails closed (approve).
func parseLevel(s string) (policy.Level, bool) {
	switch s {
	case "auto":
		return policy.LevelAuto, true
	case "approve":
		return policy.LevelApprove, true
	default:
		return policy.LevelApprove, false
	}
}

// parseOutcome maps a persisted outcome spelling to a policy.RunOutcome (unknown → unverified, fail safe).
func parseOutcome(s string) policy.RunOutcome {
	switch s {
	case "verified_clean":
		return policy.OutcomeVerifiedClean
	case "deviated":
		return policy.OutcomeDeviated
	default:
		return policy.OutcomeUnverified
	}
}

// compile-time proof the pgx store satisfies the policy.GraduationStore interface.
var _ policy.GraduationStore = (*PolicyGraduationStore)(nil)
