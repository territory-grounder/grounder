// Package persist holds Territory Grounder's governed persistence contracts as typed Go entities with
// oracle-testable in-memory stores. The pgx-backed stores (append-only grants, CHECK/enum integrity)
// wrap these under compose; the types here are the single canonical source the DDL/JSON-Schema
// generation (INV-15) derives from.
//
// Provenance: [F] spec/006 · [O] INV-15/INV-16, spec/006 REQ-506/REQ-507 · [R] ADR-0010 (org-global,
// no tenant_id).
package persist

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/schema"
)

// SRState is the observe-before-live lifecycle of a learned reboot schedule. The zero value is
// SRObserving — a freshly discovered schedule only observes; it never suppresses until deliberately
// promoted to SRLive (safe default). [F] self-learning bounds.
type SRState int

const (
	SRObserving SRState = iota // zero value — learning; does not yet suppress
	SRLive                     // promoted; may inform suppression, subject to the kill switch
)

func (s SRState) String() string {
	if s == SRLive {
		return "live"
	}
	return "observing"
}

// ScheduledReboot is a bi-temporal discovered_scheduled_reboots row: a learned reboot schedule with a
// validity window, an observe/live state, an observation count, and a kill switch. Bi-temporal =
// carries both a validity window (valid_from/valid_until) and mutable operational state. [O] REQ-506.
type ScheduledReboot struct {
	Host          string
	Cron          string
	Kind          string
	State         SRState
	Observations  int
	KillSwitch    bool
	ValidFrom     time.Time
	ValidUntil    time.Time
	SchemaVersion schema.Version
}

// ErrInvalidWindow is returned when a schedule's validity window is empty or inverted.
var ErrInvalidWindow = errors.New("persist: scheduled_reboots validity window is empty or inverted")

// ScheduledRebootStore is the discovered_scheduled_reboots persistence contract.
type ScheduledRebootStore interface {
	Register(ctx context.Context, sr ScheduledReboot) (ScheduledReboot, error)
	Get(ctx context.Context, host, kind string) (ScheduledReboot, bool, error)
}

// MemScheduledReboots is the in-memory oracle implementation of ScheduledRebootStore.
type MemScheduledReboots struct {
	mu   sync.Mutex // the registry is shared across the suppression discovery/promote writers — guard the map
	rows map[string]ScheduledReboot
	born map[string]uint64 // first-registration order per key (the oracle of the pgx twin's created_at)
	next uint64
}

// NewMemScheduledReboots returns an empty in-memory registry.
func NewMemScheduledReboots() *MemScheduledReboots {
	return &MemScheduledReboots{rows: map[string]ScheduledReboot{}, born: map[string]uint64{}}
}

// srKey identifies a schedule by (host, kind, cron) — the SAME key the pgx twin (PRIMARY KEY host,kind,cron,
// migration 0004) and the predecessor (uq_dsr_host_expr_kind) use. cron is part of the IDENTITY, not a
// mutable attribute: a discovery sweep that finds the SAME cron re-registers the same row (promotion state
// preserved), but a sweep that finds a SHIFTED cron is a NEW, unverified schedule that must observe before it
// suppresses — keying only on (host, kind) would silently carry a promotion onto an unverified new time.
func srKey(host, kind, cron string) string { return host + "\x00" + kind + "\x00" + cron }

// Register stamps the schema version, validates the validity window, and stores the row keyed (host, kind,
// cron). A NEW (host, kind, cron) is registered in its supplied state (SRObserving by default). On a
// re-registration of an EXISTING (host, kind, cron) — a periodic discovery sweep re-finding the SAME schedule
// — the promotion state is PRESERVED: State, Observations, and KillSwitch are kept, and only the validity
// window / schema are refreshed. This mirrors the predecessor's `ON CONFLICT (host,kind,cron) … DO UPDATE`
// (which "deliberately does NOT touch status/observed_count/kill_switch"): a re-discovery must never silently
// demote a schedule that promoted to live, nor clear an operator's kill switch. A missing/inverted window is
// rejected (fail closed).
func (m *MemScheduledReboots) Register(_ context.Context, sr ScheduledReboot) (ScheduledReboot, error) {
	if sr.ValidFrom.IsZero() || sr.ValidUntil.IsZero() || !sr.ValidUntil.After(sr.ValidFrom) {
		return ScheduledReboot{}, ErrInvalidWindow
	}
	v, err := schema.Stamp(schema.TableDiscoveredScheduledReboots)
	if err != nil {
		return ScheduledReboot{}, err
	}
	sr.SchemaVersion = v
	m.mu.Lock()
	defer m.mu.Unlock()
	key := srKey(sr.Host, sr.Kind, sr.Cron)
	if existing, ok := m.rows[key]; ok {
		sr.State = existing.State // preserve promotion state — a re-discovery of the SAME cron never demotes
		sr.Observations = existing.Observations
		sr.KillSwitch = existing.KillSwitch // preserve the operator's kill switch — a re-discovery never clears it
	} else {
		m.next++
		m.born[key] = m.next // first-registration order, preserved across re-registers (like the pgx created_at)
	}
	m.rows[key] = sr
	return sr, nil
}

// Get returns the registered schedule for (host, kind). When several crons exist for the same (host, kind),
// it returns the MOST-RECENTLY first-registered one, matching the pgx twin's `ORDER BY created_at DESC LIMIT 1`.
func (m *MemScheduledReboots) Get(_ context.Context, host, kind string) (ScheduledReboot, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		best    ScheduledReboot
		bestSeq uint64
		found   bool
	)
	for key, row := range m.rows {
		if row.Host == host && row.Kind == kind && (!found || m.born[key] > bestSeq) {
			best, bestSeq, found = row, m.born[key], true
		}
	}
	return best, found, nil
}
