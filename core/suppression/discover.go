package suppression

import "sync"

// ScheduleRegistry is the in-memory discovered_scheduled_reboots registry the suppression domain reads
// and the discovery/promotion writers update. The pgx-backed store wraps it under compose; the oracle
// drives this directly. Reads and writes are authority-checked under RBAC at the boundary (INV-12).
// It is CONCURRENCY-SAFE — the discovery and promotion writers run as separate scheduled activities that
// share this registry, so every method holds the mutex (Promote is the sole mutator of a row's promotion
// state and holds it across its whole read-modify sequence, so a concurrent promote cannot lose an
// observed boot or half-apply a lifecycle transition).
type ScheduleRegistry struct {
	mu   sync.Mutex
	rows map[string]*Schedule
}

// NewScheduleRegistry returns an empty registry.
func NewScheduleRegistry() *ScheduleRegistry {
	return &ScheduleRegistry{rows: map[string]*Schedule{}}
}

func regKey(host, kind string) string { return host + "\x00" + kind }

// getLocked returns a row assuming the caller holds r.mu (used by Promote to keep read-modify atomic).
func (r *ScheduleRegistry) getLocked(host, kind string) (*Schedule, bool) {
	if r.rows == nil {
		return nil, false
	}
	s, ok := r.rows[regKey(host, kind)]
	return s, ok
}

// RegisterObserving registers a discovered schedule. A NEW schedule starts OBSERVING with a zero boot count,
// so a freshly discovered (or reactively classified) schedule never suppresses until the promoter confirms
// it (observe-before-live, REQ-404). Re-discovery of an EXISTING schedule updates its descriptive fields but
// PRESERVES its promotion state (status, observed count, kill switch) — a weekly re-scan must never demote a
// promoted-to-live schedule back to observing (the predecessor's ON CONFLICT preserves these; TG previously
// force-reset them, un-promoting every live schedule on each sweep).
func (r *ScheduleRegistry) RegisterObserving(sc Schedule) *Schedule {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[string]*Schedule{}
	}
	k := regKey(sc.Host, sc.Kind)
	if existing, ok := r.rows[k]; ok {
		sc.Status = existing.Status
		sc.ObservedCount = existing.ObservedCount
		sc.ObservedBoots = existing.ObservedBoots
		sc.KillSwitch = existing.KillSwitch
		cp := sc
		r.rows[k] = &cp
		return &cp
	}
	sc.Status = SchObserving
	sc.ObservedCount = 0
	cp := sc
	r.rows[k] = &cp
	return &cp
}

// Get returns a registered schedule by (host, kind).
func (r *ScheduleRegistry) Get(host, kind string) (*Schedule, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLocked(host, kind)
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
