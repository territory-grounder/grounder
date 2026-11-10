package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GuestLivenessStore is the pgx-backed guest power-state projection (TG-378, migration 0079): each guest's
// hypervisor-reported status from the SAME /cluster/resources fetch the placement edges come from, so "is
// guest X running?" becomes answerable estate-wide. It mirrors runtime_posture's shape — a mutable,
// latest-wins projection the reader measures staleness against. Parameters are always bound — no
// string-built SQL.
//
// MULTI-WRITER, latest-OBSERVED-wins (TG-496): two writers now feed it — the 5-min estate sweep (all
// guests, the backstop) and the ~37s pve-liveness detector (the watched guests only, the fast feed that
// keeps the observed-stopped read fresh enough for the deterministic guest-down heal + the seal gate). With
// plain latest-WRITE-wins that would flap: the slow sweep fetches a guest RUNNING at T, the detector
// fetches it STOPPED at T+2s and writes first, then the sweep's LATER write (stamped newer) clobbers the
// fresh STOPPED with its stale RUNNING — the projection regressing to stale truth exactly during the
// down-transition. The Upsert therefore keys the winner on OBSERVATION time (the client-supplied fetch
// time, GuestLivenessState.ObservedAt), not write time, and guards the update monotone.
type GuestLivenessStore struct{ p *Pool }

// NewGuestLivenessStore returns the Postgres-backed guest-liveness projection.
func NewGuestLivenessStore(p *Pool) *GuestLivenessStore { return &GuestLivenessStore{p: p} }

// GuestLivenessState is one guest's observation as written by a feed.
type GuestLivenessState struct {
	Guest  string
	Node   string
	Status string // the hypervisor's word, verbatim; "" = listed without a status (observed, state unknown)
	// ObservedAt is the hypervisor FETCH time (when the writer read /cluster/resources), NOT the write
	// time. The monotone Upsert guard keys the winner on it so a stale writer cannot clobber a fresher one —
	// the down-transition hazard once two writers feed this projection (the 5-min sweep + the ~37s
	// pve-liveness detector, TG-496). A zero value ⇒ the writer supplied no observation time (a legacy/test
	// caller only, now that both real writers stamp it): the Upsert falls back to the column DEFAULT now()
	// so a zero can never win — or lose — the monotone guard with a bogus 0001-01-01 stamp.
	ObservedAt time.Time
}

// Upsert records one feed's states, latest-OBSERVED-wins per guest, in a single transaction. An empty feed
// is a deliberate no-op, not an error: a cluster with zero listed guests writes nothing, and the guests
// that VANISHED simply age out past the reader's freshness bound (absent is unknown, never stopped — the
// pve03 shape is a dead node's guests dropping off /cluster/resources while very much mid-incident).
//
// MONOTONE ON OBSERVATION TIME (TG-496): observed_at is the writer's client-supplied FETCH time, not now(),
// and the ON CONFLICT UPDATE fires only WHERE the stored observation is no newer than the incoming one. So
// the fast ~37s detector's fresh STOPPED (observed at T+2s) is NOT clobbered by the slow 5-min sweep's
// stale RUNNING (observed at T) even though the sweep's WRITE lands later — the exact multi-writer flap the
// deterministic heal would otherwise suffer. A zero ObservedAt falls back to the column DEFAULT now() via
// COALESCE, so a legacy/test caller never writes a monotone-losing 0001-01-01 stamp. tg_runtime keeps
// UPDATE on this table (migration 0079, no 0015-style REVOKE), so the guarded conditional UPDATE is
// permitted.
func (s *GuestLivenessStore) Upsert(ctx context.Context, states []GuestLivenessState) error {
	// Nil-receiver safe (TG-496): a typed-nil *GuestLivenessStore reaches here if a caller boxes a nil store
	// into a guestLivenessSink interface (the no-DSN degrade-honestly config, where guestLivenessStore.Load()
	// is nil). The interface is then non-nil and slips past a `sink == nil` guard; without this the first
	// non-empty feed panics on s.p and crash-loops the worker. No pool ⇒ no projection ⇒ write nothing (the
	// honest degrade), never a crash. Call sites should still pass a genuine nil interface (feedLiveness idiom).
	if s == nil {
		return nil
	}
	if len(states) == 0 {
		return nil
	}
	tx, err := s.p.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: guest liveness begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, st := range states {
		// Client observation time, or NULL → column DEFAULT now() for a zero-stamp legacy/test caller. A
		// typed nil *time.Time encodes as SQL NULL; the ::timestamptz cast disambiguates it for COALESCE.
		var observedAt *time.Time
		if !st.ObservedAt.IsZero() {
			at := st.ObservedAt
			observedAt = &at
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO guest_liveness (guest, node, status, observed_at)
			VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()))
			ON CONFLICT (guest) DO UPDATE SET
				node        = EXCLUDED.node,
				status      = EXCLUDED.status,
				observed_at = EXCLUDED.observed_at
			WHERE guest_liveness.observed_at <= EXCLUDED.observed_at`,
			st.Guest, st.Node, st.Status, observedAt); err != nil {
			return fmt.Errorf("db: guest liveness upsert %q: %w", st.Guest, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: guest liveness commit: %w", err)
	}
	return nil
}

// Running answers "is this guest observed running?" with a CLOSED status vocabulary and a fail-closed
// unknown. ok=true is returned ONLY for the two states the hypervisor names unambiguously:
//
//	"running" → (true,  true)
//	"stopped" → (false, true)
//
// Everything else is ok=false — no row (never observed, or vanished and aged out), a row older than
// staleAfter (the sweep stopped vouching), or any OTHER status ("paused", "suspended", "" …): a paused
// guest is neither safely "running" nor safely "not running" for a start precondition, and inventing a
// side is exactly the guess TG-378 exists to forbid. A non-positive staleAfter disables the age check
// (tests only).
func (s *GuestLivenessStore) Running(ctx context.Context, guest string, staleAfter time.Duration) (running bool, ok bool, err error) {
	var status string
	var observedAt time.Time
	qerr := s.p.Pool.QueryRow(ctx, `
		SELECT status, observed_at FROM guest_liveness WHERE guest = $1`, guest).
		Scan(&status, &observedAt)
	if errors.Is(qerr, pgx.ErrNoRows) {
		return false, false, nil
	}
	if qerr != nil {
		return false, false, fmt.Errorf("db: guest liveness read: %w", qerr)
	}
	if staleAfter > 0 && time.Since(observedAt) > staleAfter {
		return false, false, nil
	}
	switch status {
	case "running":
		return true, true, nil
	case "stopped":
		return false, true, nil
	default:
		return false, false, nil
	}
}
