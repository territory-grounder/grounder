package governance

import (
	"context"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/audit"
	"github.com/territory-grounder/grounder/core/schema"
)

// DemotionTTL is the auto-expiry of a demotion: a demotion is a circuit-breaker of a metric, an audit
// record, and this expiry — the tuple is eligible again 30 days later with no human action (REQ-304).
const DemotionTTL = 30 * 24 * time.Hour

// DemotionReason is the recorded reason for an auto-demotion.
const DemotionReason = "pattern_repeat_3plus"

// DemotionRow is an org-global analysis-only policy row. Tier-1 suppression (spec/005) reads a LIVE row
// and escalates the tuple instead of suppressing or auto-resolving it (REQ-301). Org-global (ADR-0010).
type DemotionRow struct {
	Tuple         Tuple
	Reason        string
	ValidFrom     time.Time
	ValidUntil    time.Time
	SchemaVersion schema.Version
}

// Live reports whether the demotion is currently in force at now — the read path treats an expired
// demotion as absent (REQ-304).
func (r DemotionRow) Live(now time.Time) bool {
	return !now.Before(r.ValidFrom) && now.Before(r.ValidUntil)
}

// KnownTransientStore reports whether a tuple is tagged an intentional known-transient for the org — its
// recurrence is by design and must be excluded from demotion (REQ-303).
type KnownTransientStore interface {
	IsKnownTransient(ctx context.Context, t Tuple) bool
}

// DemotionStore is the org-global policy store the demotion rows live in.
type DemotionStore interface {
	Write(ctx context.Context, row DemotionRow) error
	// LiveFor returns the currently-in-force demotion for a tuple, if any (an expired one is absent).
	LiveFor(ctx context.Context, t Tuple, now time.Time) (DemotionRow, bool, error)
}

// MemDemotionStore is the in-memory oracle implementation of DemotionStore.
type MemDemotionStore struct {
	mu   sync.Mutex // shared across the governance demote/read activities — guard the map
	rows map[Tuple]DemotionRow
}

// NewMemDemotionStore returns an empty store.
func NewMemDemotionStore() *MemDemotionStore { return &MemDemotionStore{rows: map[Tuple]DemotionRow{}} }

// Write records a demotion row (latest wins per tuple).
func (s *MemDemotionStore) Write(_ context.Context, row DemotionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.Tuple] = row
	return nil
}

// LiveFor returns the tuple's demotion only if it is currently in force (REQ-304 read-path expiry).
func (s *MemDemotionStore) LiveFor(_ context.Context, t Tuple, now time.Time) (DemotionRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[t]
	if !ok || !row.Live(now) {
		return DemotionRow{}, false, nil
	}
	return row, true, nil
}

// Demoter runs the governance-metrics worker's demotion decision.
type Demoter struct {
	Store      DemotionStore
	Transients KnownTransientStore
	Ledger     *audit.Ledger
}

// Evaluate demotes every genuine repeat-offender tuple that is not a known-transient and not already
// carrying a live demotion. Each demotion writes an org-global analysis-only policy row and appends the
// decision to the hash-chained audit spine (INV-19). It returns the rows written. No manual review step
// exists — the circuit-breaker is the metric, the record, and the expiry (REQ-301..304).
func (d *Demoter) Evaluate(ctx context.Context, counts map[Tuple]int, now time.Time) ([]DemotionRow, error) {
	var demoted []DemotionRow
	for t, c := range counts {
		if !IsDemoteCandidate(c) { // REQ-302
			continue
		}
		if d.Transients != nil && d.Transients.IsKnownTransient(ctx, t) { // REQ-303
			continue
		}
		if _, live, err := d.Store.LiveFor(ctx, t, now); err != nil {
			return demoted, err
		} else if live {
			continue // already demoted; do not double-write
		}
		ver, err := schema.Stamp(schema.TableSessionRiskAudit) // audit-spine schema family
		if err != nil {
			return demoted, err
		}
		row := DemotionRow{Tuple: t, Reason: DemotionReason, ValidFrom: now, ValidUntil: now.Add(DemotionTTL), SchemaVersion: ver}
		if err := d.Store.Write(ctx, row); err != nil {
			return demoted, err
		}
		if d.Ledger != nil {
			if _, err := d.Ledger.Append(audit.GovDecision{
				Decision: "demote:analysis-only",
				Reason:   row.Reason,
				ActionID: "demote:" + t.Host + "/" + t.AlertRule,
				Withheld: true, // demotion withholds suppression/auto-resolve eligibility
			}); err != nil {
				return demoted, err
			}
		}
		demoted = append(demoted, row)
	}
	return demoted, nil
}

// Demoted reports whether a tuple currently carries a live demotion — the signal Tier-1 suppression
// reads to escalate the tuple instead of suppressing it (REQ-301).
func Demoted(ctx context.Context, store DemotionStore, t Tuple, now time.Time) (bool, error) {
	_, live, err := store.LiveFor(ctx, t, now)
	return live, err
}
