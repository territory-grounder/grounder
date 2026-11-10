package suppression

import (
	"context"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
)

// ScheduleRegistry is the in-memory discovered_scheduled_reboots registry the suppression domain reads
// and the discovery/promotion writers update. The pgx-backed store wraps it under compose; the oracle
// drives this directly. Reads and writes are authority-checked under RBAC at the boundary (INV-12).
// It is CONCURRENCY-SAFE — the discovery and promotion writers run as separate scheduled activities that
// share this registry, so every method holds the mutex (Promote is the sole mutator of a row's promotion
// state and holds it across its whole read-modify sequence, so a concurrent promote cannot lose an
// observed boot or half-apply a lifecycle transition).
//
// DURABILITY (TG-225): with a durable store attached (WithDurableStore), every mutation is MIRRORED to it and
// LoadFromStore rehydrates it at boot, so a learned lesson survives a restart. The in-memory map stays the
// AUTHORITY for the running process — the store is a best-effort mirror. The mirror write runs under the
// registry mutex, and the SAME mutex serves the per-alert read path (Live/MatchWindow/Get feed the
// ScheduledStage), so the mirror Save is bounded by mirrorSaveTimeout: a stalled durable store delays a
// mutation (and any concurrent read) by AT MOST that long, then the mirror is dropped-and-logged and the
// in-memory decision proceeds — never an unbounded stall, never a corrupted decision. Without a store (the
// default) the registry is in-memory only, exactly as before.
type ScheduleRegistry struct {
	mu           sync.Mutex
	rows         map[ScheduleKey]*Schedule
	store        persist.ScheduledRebootStore // optional durable mirror; nil = in-memory only
	onPersistErr func(error)                  // optional: called on a mirror-write failure (never fatal)
}

// NewScheduleRegistry returns an empty registry.
func NewScheduleRegistry() *ScheduleRegistry {
	return &ScheduleRegistry{rows: map[ScheduleKey]*Schedule{}}
}

// WithDurableStore attaches a durable store: learned-schedule mutations are mirrored to it (best-effort) and
// LoadFromStore rehydrates from it at boot (TG-225). onErr, if set, is called on a mirror-write failure so the
// worker can log a durability blip. Returns the registry for chaining.
func (r *ScheduleRegistry) WithDurableStore(store persist.ScheduledRebootStore, onErr func(error)) *ScheduleRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store, r.onPersistErr = store, onErr
	return r
}

// toScheduledReboot / fromScheduledReboot map the suppression Schedule to the durable persist row. The durable
// SRState has only observing/live, so SchDisabled maps to SRObserving on the way out — the SAFE direction (a
// disabled row re-observes rather than suppressing), and its KillSwitch still persists. ObservedBoots is NOT
// carried (the durable row has no column for it): an observing schedule therefore re-earns its promotion after
// a restart, which is safe because Promote never demotes a live row below threshold.
func toScheduledReboot(sc Schedule) persist.ScheduledReboot {
	st := persist.SRObserving
	if sc.Status == SchLive {
		st = persist.SRLive
	}
	return persist.ScheduledReboot{
		Host: sc.Host, Kind: sc.Kind, Cron: sc.Cron, Timezone: sc.Timezone,
		State: st, Observations: sc.ObservedCount, KillSwitch: sc.KillSwitch,
		ValidFrom: sc.ValidFrom, ValidUntil: sc.ValidUntil, LastVerifiedAt: sc.LastVerifiedAt,
	}
}

func fromScheduledReboot(sr persist.ScheduledReboot) Schedule {
	st := SchObserving
	if sr.State == persist.SRLive {
		st = SchLive
	}
	return Schedule{
		Host: sr.Host, Kind: sr.Kind, Cron: sr.Cron, Timezone: sr.Timezone,
		Source: SourceLearned, Status: st, ObservedCount: sr.Observations, KillSwitch: sr.KillSwitch,
		ValidFrom: sr.ValidFrom, ValidUntil: sr.ValidUntil, LastVerifiedAt: sr.LastVerifiedAt,
	}
}

// mirrorSaveTimeout bounds the durable mirror write. It runs under r.mu, which also serves the per-alert read
// path (Live/MatchWindow/Get), so an UNBOUNDED Save on a wedged Postgres would hold the mutex for the whole
// outage and stall live suppression decisions. Bounding it means a stalled store costs a mutation (and any
// concurrent read) at most this long, then the mirror is dropped-and-logged and the in-memory decision — which
// is authoritative — proceeds. It is generous relative to a healthy write (single-digit ms) so it only bites a
// genuinely stalled store, and the suppression path fails OPEN (a delayed suppression investigates the alert).
const mirrorSaveTimeout = 500 * time.Millisecond

// mirrorLocked persists a learned schedule's current state to the durable store — called while holding r.mu
// after a mutation. A nil store, a non-learned row, or an invalid window is a no-op (the store fails closed on
// a bad window, and a discovery/declared row is not this store's). The Save is bounded by mirrorSaveTimeout so
// a wedged store cannot hold r.mu (and thus the live read path) for an unbounded time. A write error (including
// a timeout) is surfaced via onPersistErr and NEVER fails the in-memory mutation (the registry is authoritative
// for the running process).
func (r *ScheduleRegistry) mirrorLocked(sc *Schedule) {
	if r.store == nil || sc == nil || sc.Source != SourceLearned {
		return
	}
	if sc.ValidFrom.IsZero() || sc.ValidUntil.IsZero() || !sc.ValidUntil.After(sc.ValidFrom) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), mirrorSaveTimeout)
	defer cancel()
	if err := r.store.Save(ctx, toScheduledReboot(*sc)); err != nil && r.onPersistErr != nil {
		r.onPersistErr(err)
	}
}

// LoadFromStore rehydrates the registry from the durable store at boot (TG-225). Each stored row becomes a
// learned Schedule under its (host, kind, cron) identity; ObservedBoots are not restored, so an OBSERVING
// schedule re-earns its promotion (the safe direction) while a LIVE schedule stays live. A store read error is
// returned; the caller decides whether to proceed in-memory-only.
func (r *ScheduleRegistry) LoadFromStore(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	rows, err := r.store.List(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[ScheduleKey]*Schedule{}
	}
	for _, sr := range rows {
		sc := fromScheduledReboot(sr)
		cp := sc
		r.rows[KeyOf(sc)] = &cp
	}
	return nil
}

// ScheduleKey is the registry IDENTITY of a schedule: host, kind, and the SCHEDULE SIGNATURE (its cron —
// the observed time-of-day + cadence for a learned row). The signature is part of the identity, not a
// mutable attribute: keying on (host, kind) alone let a SHIFTED schedule inherit the previous schedule's
// promotion state, so a reboot moved from 03:00 to 05:00 would suppress LIVE on its very first sighting
// with nothing verified about the new time (port-fidelity P1-10). With the signature in the key, a changed
// schedule is a NEW row that re-enters OBSERVING and must earn promotion again, while the old row ages out
// through drift/expiry. It is the same identity the durable twins use (persist.srKey and migration 0004's
// PRIMARY KEY (host, kind, cron)) and the same the predecessor used (uq_dsr_host_expr_kind).
type ScheduleKey struct {
	Host string
	Kind string
	Cron string
}

// KeyOf returns a schedule's registry identity.
func KeyOf(sc Schedule) ScheduleKey { return ScheduleKey{Host: sc.Host, Kind: sc.Kind, Cron: sc.Cron} }

// getLocked returns a row assuming the caller holds r.mu (used by Promote to keep read-modify atomic).
func (r *ScheduleRegistry) getLocked(k ScheduleKey) (*Schedule, bool) {
	if r.rows == nil {
		return nil, false
	}
	s, ok := r.rows[k]
	return s, ok
}

// RegisterObserving registers a discovered schedule. A NEW schedule starts OBSERVING with a zero boot count,
// so a freshly discovered (or reactively classified) schedule never suppresses until the promoter confirms
// it (observe-before-live, REQ-404). Re-discovery of an EXISTING schedule — the SAME (host, kind, signature)
// — updates its descriptive fields but PRESERVES its promotion state (status, observed count, kill switch):
// a weekly re-scan must never demote a promoted-to-live schedule back to observing (the predecessor's
// ON CONFLICT preserves these; TG previously force-reset them, un-promoting every live schedule on each
// sweep). A schedule whose SIGNATURE changed is a different key, so it is a NEW observing row rather than an
// inherited promotion.
func (r *ScheduleRegistry) RegisterObserving(sc Schedule) *Schedule {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[ScheduleKey]*Schedule{}
	}
	k := KeyOf(sc)
	if existing, ok := r.rows[k]; ok {
		sc.Status = existing.Status
		sc.ObservedCount = existing.ObservedCount
		sc.ObservedBoots = existing.ObservedBoots
		sc.KillSwitch = existing.KillSwitch
		cp := sc
		r.rows[k] = &cp
		r.mirrorLocked(&cp)
		return &cp
	}
	sc.Status = SchObserving
	sc.ObservedCount = 0
	sc.ObservedBoots = nil
	cp := sc
	r.rows[k] = &cp
	r.mirrorLocked(&cp)
	return &cp
}

// Get returns a registered schedule by its full identity.
func (r *ScheduleRegistry) Get(k ScheduleKey) (*Schedule, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLocked(k)
}

// Live returns a snapshot of the currently-live schedules (what the ScheduledStage matches against). The
// entries are COPIES, so a caller iterating them never races a concurrent Promote mutating a row.
func (r *ScheduleRegistry) Live() []Schedule {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Schedule
	for _, s := range r.rows {
		if s.Status == SchLive {
			out = append(out, *s)
		}
	}
	return out
}

// MatchWindow returns the identity of an existing, non-disabled LEARNED row on the host whose window
// contains t. It is how a recurring reboot keeps ONE stable identity under normal jitter: an observation a
// few minutes off the anchor lands inside the registered window and accrues to the SAME row, rather than
// minting a fresh signature every time and never accumulating the two occurrences promotion needs. An
// observation OUTSIDE every registered window is a genuinely different schedule and gets its own key.
func (r *ScheduleRegistry) MatchWindow(host string, w WindowEvaluator, t time.Time) (ScheduleKey, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, s := range r.rows {
		if s.Host != host || s.Source != SourceLearned || s.Status == SchDisabled {
			continue
		}
		if w.Contains(*s, t) {
			return k, true
		}
	}
	return ScheduleKey{}, false
}

// RenewOnMatch extends a matched row's validity and re-stamps its freshness (the predecessor's
// renew_on_match), so a schedule that is demonstrably still firing does not expire mid-life. It touches
// NEITHER the promotion state nor the kill switch, and it never resurrects a disabled row.
func (r *ScheduleRegistry) RenewOnMatch(k ScheduleKey, until, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sc, ok := r.getLocked(k)
	if !ok || sc.Status == SchDisabled {
		return
	}
	if until.After(sc.ValidUntil) {
		sc.ValidUntil = until
	}
	sc.LastVerifiedAt = now
	r.mirrorLocked(sc)
}

// Demote drives a LIVE learned row back to OBSERVING and CLEARS its accumulated boot evidence, so the
// lesson must be re-earned from scratch rather than re-promoting on the same evidence that produced a
// wrong suppression (spec/005 REQ-411 — the unlearning half of the learning lane). It is safe-direction
// only: it can never move a row toward suppressing, and it never touches a DECLARED row (an operator
// declaration is not TG's to revoke). It returns whether a row was demoted.
func (r *ScheduleRegistry) Demote(k ScheduleKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	sc, ok := r.getLocked(k)
	if !ok || sc.Source != SourceLearned || sc.Status != SchLive {
		return false
	}
	sc.Status = SchObserving
	sc.ObservedCount = 0
	sc.ObservedBoots = nil
	r.mirrorLocked(sc)
	return true
}

// DemoteLearned demotes EVERY live learned row on a host and returns how many were demoted. The scheduled
// demote pass uses it to re-assert a demotion durably: the in-path demote already stopped the exact row
// that misfired, and this catches any sibling row on the same host that re-promoted since.
func (r *ScheduleRegistry) DemoteLearned(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, sc := range r.rows {
		if sc.Host != host || sc.Source != SourceLearned || sc.Status != SchLive {
			continue
		}
		sc.Status = SchObserving
		sc.ObservedCount = 0
		sc.ObservedBoots = nil
		r.mirrorLocked(sc)
		n++
	}
	return n
}
