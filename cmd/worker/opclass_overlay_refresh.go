package main

// THE RATIFIED OVERLAY, FINALLY LOADED (TG-227 blockers 2+3).
//
// Before this file, ratification wrote to opclass_ratified and appended to the ledger — and the RUNNING
// worker never read either. opschema.SetOverlay existed, verified hashes, swapped atomically, and had zero
// production callers; Ladder.WithPerClassThreshold existed and no composition root installed it. A class
// ratified at N=10 would have graduated after the compiled 5, and a ratified capability was live nowhere.
// Code present, correct, tested, never called — the repository's own documented disease, in the one
// subsystem whose whole point is that autonomy must be EARNED.
//
// This file is deliberately thin: ONE store read feeding TWO consumers in lockstep.
//
//   opclass_ratified ──LiveOverlay──▶ overlayRefresher ──▶ opschema.SetOverlay   (the composed registry)
//                                          │
//                                          └─────────────▶ ThresholdFor          (the graduation ladder)
//
// LOCKSTEP MATTERS. The registry snapshot and the threshold map are derived from the SAME rows in the
// same pass, and a row the registry REJECTED (tampered hash, shadowing slug, invalid spec) contributes no
// threshold. Deriving the two from separate reads would open a window where a class graduates under a
// threshold whose registry entry was dropped — a capability acting on provenance the registry refused.
//
// FAILURE POSTURE. Every failure direction is "fewer capabilities, loudly": an unreadable store keeps the
// last good snapshot and says so; a tampered row is dropped by SetOverlay and named in the log; a store
// that returns zero rows installs an EMPTY overlay (a revoke must actually revoke — keeping the last good
// snapshot on an empty read would leave a revoked class actuating until restart).

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/territory-grounder/grounder/core/actuate/opschema"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/policy"
)

// liveOverlayReader is the single store read the refresher needs, as a seam so the oracles can drive it
// with plain rows and injected failures.
type liveOverlayReader interface {
	LiveOverlay(ctx context.Context) ([]db.RatifiedRow, error)
}

type overlayRefresher struct {
	store liveOverlayReader
	logf  func(string, ...any)
	// thresholds is atomic for the same reason opschema's snapshot is: ThresholdFor is consulted on the
	// graduation path while the refresh loop swaps underneath it. Readers take the whole map by pointer.
	thresholds atomic.Pointer[map[string]int]
	// kick lets the ratify/revoke verbs request an immediate pass instead of waiting out the TTL, so an
	// operator's grant is live in seconds. Buffered size 1: coalescing concurrent kicks into one pass is
	// correct — the pass reads the whole table.
	kick chan struct{}
	// evict drops an op-class's cached graduation state in the live enforcement ladder. Every pass evicts each
	// overlay class it serves, so the next enforcement read reloads the AUTHORITATIVE durable row: the ratify
	// verb resets a re-ratified/renamed slug's durable graduation to approve (TG-177), but that store write
	// bypasses the ladder's per-process cache. Every enforcement process runs this refresher on its TTL loop,
	// so evicting here carries the reset to the gate that actuates — in THIS process immediately on the ratify
	// kick, and in every other within one interval. Evicting unconditionally (not only on a detected change)
	// is deliberate: a revoke-then-re-ratify of an identical spec produces the same entry_hash, so
	// change-detection could miss it if the two verbs coalesce into one pass. Reloading a stably-graduated
	// class re-reads the same durable row (Record writes through before returning), so eviction never loses
	// earned progress — it only refuses to serve a level the store no longer backs. Nil until wired (the boot
	// pass runs before the ladder exists; nothing is cached to evict then).
	evict func(opClass string)
}

func newOverlayRefresher(store liveOverlayReader, logf func(string, ...any)) *overlayRefresher {
	r := &overlayRefresher{store: store, logf: logf, kick: make(chan struct{}, 1)}
	empty := map[string]int{}
	r.thresholds.Store(&empty)
	return r
}

// WithLadderEvict wires the enforcement ladder's Forget so a (re)admitted overlay class's cached graduation
// is dropped and reloaded from the store — the coherence half of TG-177's fail-closed reset. Set ONCE at the
// composition root AFTER the ladder is built and BEFORE the refresh loop starts, so no pass reads it racily.
func (r *overlayRefresher) WithLadderEvict(f func(opClass string)) *overlayRefresher {
	r.evict = f
	return r
}

// RefreshOnce performs one load: read the live rows, install the registry snapshot, and derive the
// per-class thresholds from exactly the rows the registry accepted.
func (r *overlayRefresher) RefreshOnce(ctx context.Context) error {
	rows, err := r.store.LiveOverlay(ctx)
	if err != nil {
		// KEEP THE LAST GOOD SNAPSHOT. A config-plane outage must not strip capabilities mid-flight —
		// but it must be loud, because an operator who just ratified is watching for their grant to go
		// live and it has not.
		r.logf("opclass overlay: refresh FAILED (%v) — composed registry keeps its last good snapshot; "+
			"a grant ratified since the last successful load is NOT live yet", err)
		return err
	}
	entries := make([]opschema.OverlayEntry, 0, len(rows))
	byKey := make(map[string]db.RatifiedRow, len(rows))
	for _, row := range rows {
		entries = append(entries, opschema.OverlayEntry{Spec: row.Spec, Hash: row.EntryHash})
		byKey[normalizeOpClass(row.OpClass)] = row
	}
	// SetOverlay is the authority on admission: it re-verifies each row's hash against the canonical
	// spec, refuses shadowing and malformed rows, and swaps atomically. This caller adds nothing to that
	// judgment — it only reports it.
	accepted, rejected := opschema.SetOverlay(entries)
	for _, why := range rejected {
		r.logf("opclass overlay: DROPPED row — %s", why)
	}
	// Thresholds in LOCKSTEP with the snapshot just installed: a class contributes its ratified
	// promote_threshold ONLY if the composed registry now actually serves it from the overlay. The
	// !IsEmbedded guard closes the one gap Lookup alone would leave: a row dropped for shadowing an
	// embedded class still resolves via Lookup (embedded wins), and without the guard the REJECTED row's
	// threshold would ride in on the embedded class's back.
	th := make(map[string]int, accepted)
	for key, row := range byKey {
		if _, ok := opschema.Lookup(key); !ok || opschema.IsEmbedded(key) {
			continue
		}
		// TG-177 COHERENCE: this pass serves `key` from the overlay, so drop its cached graduation in the
		// enforcement ladder and force the next read to reload the authoritative durable row. This is what
		// carries the ratify verb's fail-closed reset (a re-ratified/renamed class reset to approve in the
		// store) into the gate that actuates — the store write bypasses the per-process cache, and this
		// eviction runs in every enforcement process on its refresh loop. Scoped to admitted, NON-embedded
		// overlay classes: the guard above excludes embedded classes, so their hot-path cache is untouched.
		if r.evict != nil {
			r.evict(key)
		}
		if row.PromoteThreshold > 0 {
			th[key] = row.PromoteThreshold
		}
	}
	r.thresholds.Store(&th)
	r.logf("opclass overlay: %d ratified class(es) live in the composed registry (%d dropped), "+
		"%d per-class promote threshold(s) armed", accepted, len(rejected), len(th))
	return nil
}

// Run is the refresh loop: an immediate pass, then one per interval, plus on-demand passes when the
// ratify/revoke verbs kick. Errors are logged inside RefreshOnce; the loop itself is the retry.
func (r *overlayRefresher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	_ = r.RefreshOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
		_ = r.RefreshOnce(ctx)
	}
}

// Kick requests an immediate refresh without blocking the caller (the ratify activity must not stall on
// the loop). A pending kick already covers the request — the pass reads the whole table.
func (r *overlayRefresher) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// ThresholdFor is the Ladder.WithPerClassThreshold resolver: the ratified promote_threshold for opClass,
// if the composed registry currently serves that class from the overlay. The ladder ignores values at or
// below its compiled bar, so this can only ever RAISE the requirement — a hostile row buys nothing.
func (r *overlayRefresher) ThresholdFor(opClass string) (int, bool) {
	th := *r.thresholds.Load()
	n, ok := th[normalizeOpClass(opClass)]
	return n, ok
}

// normalizeOpClass mirrors the slug normalization the registry applies (lowercase, trimmed), so the
// threshold map and the registry snapshot cannot disagree about a class's identity by case alone.
func normalizeOpClass(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// buildGraduationLadder is the ONE production ladder construction, named so an oracle can hold the chain
// together: the compiled default bar, the durable store, and the ratified per-class resolver. Before
// TG-227 the .WithPerClassThreshold call existed nowhere in any composition root — the TG-248 defect —
// so a class ratified at N=10 would have graduated at the compiled bar.
func buildGraduationLadder(store policy.GraduationStore, perClass func(string) (int, bool)) *policy.Ladder {
	l := policy.NewLadder(policy.DefaultPromoteThreshold, store, log.Printf)
	if perClass != nil {
		l = l.WithPerClassThreshold(perClass)
	}
	return l
}
