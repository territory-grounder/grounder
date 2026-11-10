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

// Admit claims the RESOURCE PATH target for ref (TG-664: actuate.ResourcePath — the guest, or the unit or container
// on its host). It refuses while ANY overlapping path (actuate.Overlaps: the same resource, one that contains it, or
// one it contains) holds a live claim or a cooldown, so two spellings of one guest are one claim and a host-level
// action waits for a unit heal on the same host. Every claim on one host is serialized by a transaction-scoped
// advisory lock on that host, so two processes cannot both pass the overlap check.
//
// CRASH TAKEOVER (TG-664): a claim older than the claim TTL is a crashed worker's leftover. Its effect may have
// landed, so the resource is in an unknown state: the takeover clears the claim, STARTS the cooldown, and refuses —
// the next hand waits out the dust exactly as after a disturbed effect, instead of being admitted at once.
func (s *ActuationTargetStore) Admit(ctx context.Context, target, ref string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		// An unkeyed target shares one estate-wide bucket rather than bypassing admission — the same
		// no-exemption posture as the in-process limiter.
		target = actuate.ResourceKeyNoTarget
	}
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: target admission for %q: %w", target, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('tg-target-admission:' || $1))`, actuate.ResourceHost(target)); err != nil {
		return fmt.Errorf("db: target admission lock for %q: %w", target, err)
	}
	// The overlap candidates: every row on the same host (starts_with, never LIKE — a unit name may hold '_' or
	// '%'). Overlaps is applied in Go, the same function the commit-confirm superseded check uses.
	rows, err := tx.Query(ctx, `
		SELECT target, claimed_by, claimed_at, cooldown_until, (claimed_at < now() - $2::interval) AS stale, now()
		  FROM actuation_target_state
		 WHERE target = $1 OR starts_with(target, $1 || '/') OR starts_with($1, target || '/')`,
		target, s.claimTTL.String())
	if err != nil {
		return fmt.Errorf("db: target admission for %q: %w", target, err)
	}
	type held struct {
		target, by    string
		cooldownUntil *time.Time
		stale         bool
	}
	var hits []held
	var now time.Time
	for rows.Next() {
		var h held
		var claimedAt *time.Time
		var stale *bool
		if err := rows.Scan(&h.target, &h.by, &claimedAt, &h.cooldownUntil, &stale, &now); err != nil {
			rows.Close()
			return fmt.Errorf("db: target admission for %q: %w", target, err)
		}
		if !actuate.Overlaps(target, h.target) {
			continue
		}
		h.stale = stale != nil && *stale
		if claimedAt == nil {
			h.by = ""
		}
		hits = append(hits, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("db: target admission for %q: %w", target, err)
	}
	for _, h := range hits {
		if h.cooldownUntil != nil && h.cooldownUntil.After(now) {
			return fmt.Errorf("resource %q is cooling down until %s after a disturbed effect on %q", target, h.cooldownUntil.UTC().Format(time.RFC3339), h.target)
		}
	}
	for _, h := range hits {
		if h.by == "" {
			continue
		}
		if !h.stale {
			return fmt.Errorf("resource %q overlaps %q, claimed by in-flight session %q", target, h.target, h.by)
		}
		// A crashed worker's leftover: clear it, start the cooldown, refuse (committed — the takeover must stick).
		if _, err := tx.Exec(ctx, `
			UPDATE actuation_target_state
			   SET claimed_by = '', claimed_at = NULL, cooldown_until = now() + $2::interval,
			       last_error = 'crash takeover: claim of session ' || $3 || ' outlived the claim TTL'
			 WHERE target = $1`, h.target, s.cooldown.String(), h.by); err != nil {
			return fmt.Errorf("db: target admission takeover for %q: %w", h.target, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("db: target admission takeover for %q: %w", h.target, err)
		}
		return fmt.Errorf("resource %q overlaps %q, whose claim by session %q outlived the claim TTL (a crashed worker): taken over and cooling down for %s before any new effect",
			target, h.target, h.by, s.cooldown)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO actuation_target_state (target, claimed_by, claimed_at)
		VALUES ($1, $2, now())
		ON CONFLICT (target) DO UPDATE SET claimed_by = EXCLUDED.claimed_by, claimed_at = EXCLUDED.claimed_at`,
		target, ref); err != nil {
		return fmt.Errorf("db: target admission for %q: %w", target, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: target admission for %q: %w", target, err)
	}
	return nil
}

// Release drops ref's claim; disturbed=true stamps the cooldown window. Scoped to ref so a stale sibling
// release cannot clear a newer claim. Best-effort by contract: the error is returned for logging but a
// lost release only refuses (claim TTL), never admits.
func (s *ActuationTargetStore) Release(ctx context.Context, target, ref string, disturbed bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		target = actuate.ResourceKeyNoTarget
	}
	_, _ = s.p.Exec(ctx, `
		UPDATE actuation_target_state
		   SET claimed_by = '', claimed_at = NULL,
		       cooldown_until = CASE WHEN $3 THEN now() + $4::interval ELSE cooldown_until END,
		       last_error = CASE WHEN $3 THEN 'effect disturbed by session ' || $2 ELSE last_error END
		 WHERE target = $1 AND claimed_by = $2`,
		target, ref, disturbed, s.cooldown.String())
}
