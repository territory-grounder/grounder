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
	var count, noticeCount int
	var version int64
	err := s.p.Pool.QueryRow(ctx, `
		SELECT level, clean_run_count, notice_run_count, last_outcome, version FROM policy_graduation WHERE op_class = $1`, opClass).
		Scan(&level, &count, &noticeCount, &lastOutcome, &version)
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
		OpClass:        opClass,
		Level:          lvl,
		CleanRunCount:  count,
		NoticeRunCount: noticeCount,
		LastOutcome:    parseOutcome(lastOutcome),
		Version:        version,
	}, nil
}

// Save upserts st keyed by its op-class with an OPTIMISTIC-CONCURRENCY guard (TG-146 S3/S4). A caller that
// read the row first passes st.Version (> 0); the DO UPDATE then lands ONLY WHERE the durable version still
// equals it, bumping it by one — so a stale writer whose row moved underneath it (a peer worker recorded a
// demotion since the read) matches zero rows and gets policy.ErrConcurrentModification, a signal to reload +
// re-decide, never a blind clobber that resurrects withdrawn autonomy. A caller passing version 0 performs an
// UNCONDITIONAL upsert: a genuinely-new class's first write (no row to guard), or the ratify verb's
// authoritative reset-to-approve (temporal/opclassratify), which must WIN over inherited trust rather than
// lose a CAS. The stored version is always >= 1 (a fresh row starts at 1; every update bumps); version 0 is
// only ever an in-hand sentinel, never persisted.
//
// The shape mirrors the durable mutation breaker's CompareAndOpen (core/db/breaker_write.go): RETURNING yields
// a row only when the write actually lands, and pgx.ErrNoRows means "the guard failed — not this call". The
// 0067 promotion-requires-credit trigger still fires on an advancing DO UPDATE (it keys on level rank, not
// version) and surfaces as an ordinary save error, exactly as before this guard was added.
//
// ASSUMES policy_graduation rows are never DELETED (Forget evicts only the in-process cache, not the row; no
// code path deletes here). If a delete path is ever added, a stale positive-version write would find no
// conflict and land as a fresh INSERT — resurrecting the row and bypassing the guard — so revisit this then.
func (s *PolicyGraduationStore) Save(ctx context.Context, st policy.ClassState) error {
	var newVersion int64
	err := s.p.Pool.QueryRow(ctx, `
		INSERT INTO policy_graduation (op_class, level, clean_run_count, notice_run_count, last_outcome, version, updated_at)
		VALUES ($1, $2, $3, $4, $5, 1, now())
		ON CONFLICT (op_class) DO UPDATE SET
			level            = EXCLUDED.level,
			clean_run_count  = EXCLUDED.clean_run_count,
			notice_run_count = EXCLUDED.notice_run_count,
			last_outcome     = EXCLUDED.last_outcome,
			version          = policy_graduation.version + 1,
			updated_at       = now()
		WHERE $6 = 0 OR policy_graduation.version = $6
		RETURNING version`,
		st.OpClass, st.Level.String(), st.CleanRunCount, st.NoticeRunCount, st.LastOutcome.String(), st.Version).
		Scan(&newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		// The DO UPDATE's version guard failed: a concurrent writer moved the row since st was read. This is the
		// cross-process CAS miss the per-instance mutex cannot catch — the caller reloads and re-decides.
		return fmt.Errorf("%w: %q (expected version %d)", policy.ErrConcurrentModification, st.OpClass, st.Version)
	}
	if err != nil {
		return fmt.Errorf("db: policy_graduation save (%q): %w", st.OpClass, err)
	}
	return nil
}

// parseLevel maps a persisted level spelling to a policy.Level. An unknown spelling fails closed (approve).
//
// THE UNKNOWN→APPROVE ARM IS THE ROLLOUT CONTRACT, not merely defensive coding. Migration 0050 widens the
// `level` CHECK to admit 'auto_notice', and during a rolling deploy an OLD worker will read rows a NEW worker
// wrote. The old binary has no auto_notice arm, so it parses the value as unknown and resolves the class to
// approve — routing to a human vote. That is the safe direction, and it is why the rung could be added by
// widening the CHECK rather than by a coordinated stop-the-world upgrade (the 0040 precedent).
func parseLevel(s string) (policy.Level, bool) {
	switch s {
	case "auto":
		return policy.LevelAuto, true
	case "auto_notice":
		return policy.LevelAutoNotice, true
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
	case "seeded":
		return policy.OutcomeSeeded
	default:
		return policy.OutcomeUnverified
	}
}

// compile-time proof the pgx store satisfies the policy.GraduationStore interface.
var _ policy.GraduationStore = (*PolicyGraduationStore)(nil)
