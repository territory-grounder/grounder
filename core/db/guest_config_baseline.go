package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GuestConfigBaselineStore is the pgx-backed per-guest PVE config-hash baseline (TG-466 slice 1,
// migration 0091): the confighash sweep Records each guest's current config hash here, and the store
// answers whether the CONFIG — not the lifecycle state — changed since the last sweep. It is the
// persistence half of the grounded positive observed-mutation signal: a changed config is a
// deliberate act by construction (the sweep's hash excludes every machine-managed key), so slice 2
// can thread ChangedWithin into AttributeInput.Observation without minting INV-09 spurious suspicion
// from organic events. Mutable latest-wins projection like guest_liveness — single writer (the estate
// sweep), parameters always bound, no string-built SQL.
type GuestConfigBaselineStore struct{ p *Pool }

// NewGuestConfigBaselineStore returns the Postgres-backed baseline store.
func NewGuestConfigBaselineStore(p *Pool) *GuestConfigBaselineStore { return &GuestConfigBaselineStore{p: p} }

// GuestConfigObservation is one sweep observation. The baseline is KEYED per vmid (the stable PVE
// identity — a destroyed-and-recreated vmid diffs against its predecessor, which is itself a
// deliberate act); guest/node/kind ride along so slice 2 can resolve by the attribution subject.
type GuestConfigObservation struct {
	VMID  int64
	Guest string
	Node  string
	Kind  string // "qemu" | "lxc", verbatim from the sweep
	Hash  string
}

// GuestConfigOutcome is Record's answer: exactly one of first-sighting, unchanged, or changed.
type GuestConfigOutcome struct {
	FirstSighting bool
	Changed       bool
	PreviousHash  string
}

// Record persists one observation atomically (SELECT … FOR UPDATE + write, one transaction) and
// reports which shape occurred:
//
//   - no baseline row  → INSERT, FirstSighting=true (a first sighting is a BASELINE, never a change —
//     there was nothing to diff against, and flagging it would fire on every new guest and every
//     fresh deployment)
//   - hash unchanged   → refresh observed_at (the sweep keeps vouching), Changed=false
//   - hash differs     → roll the baseline (prev_hash, changed_at=now()), Changed=true
//
// A blank hash, a non-positive vmid, or a blank guest is REFUSED with an error rather than stored:
// an empty-hash baseline would read as "changed" against every later real hash — a fabricated
// mutation signal, the exact thing the fail-closed contract forbids.
func (s *GuestConfigBaselineStore) Record(ctx context.Context, obs GuestConfigObservation) (GuestConfigOutcome, error) {
	if obs.VMID <= 0 || obs.Guest == "" || obs.Hash == "" {
		return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline: refusing malformed observation (vmid=%d guest=%q hash blank=%v) — an empty baseline fabricates a future mutation signal", obs.VMID, obs.Guest, obs.Hash == "")
	}
	tx, err := s.p.Pool.Begin(ctx)
	if err != nil {
		return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var stored string
	err = tx.QueryRow(ctx, `SELECT config_hash FROM guest_config_baseline WHERE vmid = $1 FOR UPDATE`, obs.VMID).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO guest_config_baseline (vmid, guest, node, kind, config_hash)
			VALUES ($1, $2, $3, $4, $5)`,
			obs.VMID, obs.Guest, obs.Node, obs.Kind, obs.Hash); err != nil {
			return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline insert %q: %w", obs.Guest, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline commit: %w", err)
		}
		return GuestConfigOutcome{FirstSighting: true}, nil
	case err != nil:
		return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline read %q: %w", obs.Guest, err)
	}

	if stored == obs.Hash {
		if _, err := tx.Exec(ctx, `
			UPDATE guest_config_baseline
			SET guest = $2, node = $3, kind = $4, observed_at = now()
			WHERE vmid = $1`,
			obs.VMID, obs.Guest, obs.Node, obs.Kind); err != nil {
			return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline refresh %q: %w", obs.Guest, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline commit: %w", err)
		}
		return GuestConfigOutcome{PreviousHash: stored}, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE guest_config_baseline
		SET guest = $2, node = $3, kind = $4,
		    prev_hash = config_hash, config_hash = $5,
		    observed_at = now(), changed_at = now()
		WHERE vmid = $1`,
		obs.VMID, obs.Guest, obs.Node, obs.Kind, obs.Hash); err != nil {
		return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline roll %q: %w", obs.Guest, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return GuestConfigOutcome{}, fmt.Errorf("db: guest config baseline commit: %w", err)
	}
	return GuestConfigOutcome{Changed: true, PreviousHash: stored}, nil
}

// ChangedWithin answers the slice-2 read: was a CONFIG change observed for this guest (by attribution
// subject name) within the window? It is fail-closed at both edges: a guest never observed, never
// seen to change, or changed only before the window answers false; a non-positive window asks
// nothing and answers false — the caller must supply the attribution lookback deliberately, and a
// zero default silently widening to "ever" would fabricate in-window mutations.
func (s *GuestConfigBaselineStore) ChangedWithin(ctx context.Context, guest string, window time.Duration) (bool, error) {
	if guest == "" || window <= 0 {
		return false, nil
	}
	var changed bool
	err := s.p.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM guest_config_baseline
			WHERE guest = $1
			  AND changed_at IS NOT NULL
			  AND changed_at >= now() - make_interval(secs => $2)
		)`, guest, window.Seconds()).Scan(&changed)
	if err != nil {
		return false, fmt.Errorf("db: guest config changed-within read: %w", err)
	}
	return changed, nil
}
