package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/territory-grounder/grounder/core/actuate"
)

var _ actuate.TargetAdmission = (*ActuationTargetStore)(nil)

// ActuationTargetStore is the pgx-backed durable per-target admission + cooldown store (TG-81 borrow 2,
// migration 0107) behind the interceptor's gate 4h2. It satisfies actuate.TargetAdmission. All three
// admission questions are answered by ONE atomic conditional upsert, so two processes racing for a target
// serialize on the row: a target is admitted only when it is unclaimed (or its claim is older than the
// claim TTL — a crashed worker's leftover) AND not inside a cooldown window.
//
// FAIL-CLOSED by contract: every error return from Admit — a held claim, an active cooldown, an
// unreachable store — is a refusal at the gate. Release is best-effort; a lost release ages out through
// the claim TTL (refusing in the meantime, never admitting).
type ActuationTargetStore struct {
	p *Pool
	// claimTTL bounds how long a claim can outlive its worker before takeover; cooldown parks a target
	// after a disturbed effect. Compiled defaults (no knobs): the TTL comfortably exceeds the longest
	// pre-effect sequence, the cooldown is the h-ssh-derived 120s.
	claimTTL time.Duration
	cooldown time.Duration
}

// NewActuationTargetStore returns the Postgres-backed target admission (satisfies actuate.TargetAdmission).
func NewActuationTargetStore(p *Pool) *ActuationTargetStore {
	return &ActuationTargetStore{p: p, claimTTL: 10 * time.Minute, cooldown: 120 * time.Second}
}

// Admit atomically claims target for ref. The conditional upsert claims when — and only when — no live
// claim and no live cooldown stand; otherwise no row comes back and the follow-up read names the holder
// for the refusal (best-effort: the race between the two reads can only make the reason vaguer, never
// admit).
func (s *ActuationTargetStore) Admit(ctx context.Context, target, ref string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		// An unkeyed target shares one estate-wide bucket rather than bypassing admission — the same
		// no-exemption posture as the in-process limiter.
		target = "(no-target)"
	}
	var claimed string
	err := s.p.QueryRow(ctx, `
		INSERT INTO actuation_target_state (target, claimed_by, claimed_at)
		VALUES ($1, $2, now())
		ON CONFLICT (target) DO UPDATE
		  SET claimed_by = EXCLUDED.claimed_by, claimed_at = EXCLUDED.claimed_at
		  WHERE (actuation_target_state.claimed_by = ''
		         OR actuation_target_state.claimed_at IS NULL
		         OR actuation_target_state.claimed_at < now() - $3::interval)
		    AND (actuation_target_state.cooldown_until IS NULL
		         OR actuation_target_state.cooldown_until < now())
		RETURNING claimed_by`,
		target, ref, s.claimTTL.String()).Scan(&claimed)
	if err == nil {
		return nil
	}
	// No row: refused by a standing claim or cooldown — say which, so the ledger reason is adjudicable.
	var holder string
	var cooldownUntil *time.Time
	if derr := s.p.QueryRow(ctx,
		`SELECT claimed_by, cooldown_until FROM actuation_target_state WHERE target = $1`,
		target).Scan(&holder, &cooldownUntil); derr == nil {
		if cooldownUntil != nil && cooldownUntil.After(time.Now()) {
			return fmt.Errorf("target %q is cooling down until %s after a disturbed effect", target, cooldownUntil.UTC().Format(time.RFC3339))
		}
		if holder != "" {
			return fmt.Errorf("target %q is claimed by in-flight session %q", target, holder)
		}
	}
	return fmt.Errorf("db: target admission for %q: %w", target, err)
}

// Release drops ref's claim; disturbed=true stamps the cooldown window. Scoped to ref so a stale sibling
// release cannot clear a newer claim. Best-effort by contract: the error is returned for logging but a
// lost release only refuses (claim TTL), never admits.
func (s *ActuationTargetStore) Release(ctx context.Context, target, ref string, disturbed bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = "(no-target)"
	}
	_, _ = s.p.Exec(ctx, `
		UPDATE actuation_target_state
		   SET claimed_by = '', claimed_at = NULL,
		       cooldown_until = CASE WHEN $3 THEN now() + $4::interval ELSE cooldown_until END,
		       last_error = CASE WHEN $3 THEN 'effect disturbed by session ' || $2 ELSE last_error END
		 WHERE target = $1 AND claimed_by = $2`,
		target, ref, disturbed, s.cooldown.String())
}
