package db

// EXACTLY-ONCE LADDER CREDIT (TG-266, REQ-2804, migration 0050).
//
// The table shipped with migration 0050 and was referenced by NO code: `grep -rn graduation_credit` over
// core/ cmd/ temporal/ returned nothing outside a migration test. Its own comment states the contract it
// was never given — "Consulted BEFORE any streak increment: credit is claimed by (op_class, external_ref)
// or it is not claimed at all" — so one incident could advance a class once per re-run of the session that
// healed it, and the ladder's whole premise is that autonomy is earned per CONSECUTIVE verified run.
//
// ONLY THE PROMOTING OUTCOME IS DEDUPED. A claim gates a streak INCREMENT and nothing else: an outcome
// that breaks a streak is a safety action and must never be blocked by a bookkeeping key. That asymmetry
// is the fail-safe direction — the worst a lost claim can do is withhold autonomy, and the worst an
// un-deduped demotion can do is nothing at all.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrCreditUngrounded is returned when the database REFUSED the credit because the incident it names has no
// recorded action_execution (migration 0064, TG-321).
//
// It is a distinct error rather than a bare failure because the two mean opposite things to a caller. A
// store outage is transient and worth retrying; an ungrounded credit is a permanent, correct refusal, and
// retrying it forever would turn a safety control into a hot loop. Both still yield claimed=false — the
// fail-safe direction is unchanged — but only one of them is a defect to page about.
var ErrCreditUngrounded = errors.New("db: graduation credit refused — the incident has no recorded action_execution (TG-321)")

// GraduationCreditStore is the pgx-backed exactly-once credit ledger.
type GraduationCreditStore struct{ p *Pool }

// NewGraduationCreditStore returns the store over the shared pool.
func NewGraduationCreditStore(p *Pool) *GraduationCreditStore { return &GraduationCreditStore{p: p} }

// Claim attempts to claim the promotion credit for (opClass, externalRef). It returns true exactly once
// per pair: the first caller wins and every later attempt — a workflow replay, a resumed session, a
// re-run of the same incident — returns false and must NOT advance the streak.
//
// INSERT … ON CONFLICT DO NOTHING is the whole mechanism, and it is deliberately an INSERT: migration
// 0050 REVOKEs UPDATE and DELETE on this table from tg_runtime ("credit that can be rewritten is not
// credit"), so a DO UPDATE form would fail at the grant. The claim is therefore append-only by the
// database's own permissions, not by this code's good intentions.
//
// A STORE ERROR RETURNS false. Unclaimable means uncredited: a class whose credit could not be recorded
// does not climb on that run. Failing the other way would let a database blip mint the very double-credit
// the table exists to forbid.
func (s *GraduationCreditStore) Claim(ctx context.Context, opClass, externalRef, outcome string) (bool, error) {
	if opClass == "" || externalRef == "" {
		// No key, no claim. A creditable run always knows both; an empty one is a wiring defect, and
		// granting credit for it would make the key meaningless for every other run.
		return false, fmt.Errorf("db: graduation credit needs both op_class and external_ref (got %q/%q)", opClass, externalRef)
	}
	tag, err := s.p.Exec(ctx, `
		INSERT INTO graduation_credit (op_class, external_ref, outcome)
		VALUES ($1, $2, $3)
		ON CONFLICT (op_class, external_ref) DO NOTHING`,
		opClass, externalRef, outcome)
	if err != nil {
		// 23514 is the check_violation the grounding trigger raises. Matching on the SQLSTATE rather than on
		// the message text keeps this from breaking the first time the message is reworded — a string match
		// here would silently reclassify a refusal as an outage.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			return false, fmt.Errorf("%w: op_class=%s external_ref=%s", ErrCreditUngrounded, opClass, externalRef)
		}
		return false, fmt.Errorf("db: claim graduation credit %s/%s: %w", opClass, externalRef, err)
	}
	return tag.RowsAffected() == 1, nil
}
