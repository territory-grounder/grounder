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

// LearnedSuppressionReason is the recorded reason for the EVIDENCE-driven demotion: a LEARNED suppression
// pattern was proven to have silenced an incident that then needed action. Distinct from the repeat-offender
// reason because the trigger is different in kind — that one counts recurrences, this one holds proof.
const LearnedSuppressionReason = "learned_suppression_silenced_real_incident"

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

// ---- the evidence lane: unlearning a learned suppression (spec/005 REQ-411) ----

// SuppressionEvidence is PROOF that a suppression silenced an incident that then needed action: the
// tuple, the incident, and what reversed it. Today the sole producer is the two-phase boot verify — a
// LEARNED scheduled-reboot suppression whose boot came back NOT clean, i.e. the pattern hid a crash.
type SuppressionEvidence struct {
	Tuple       Tuple
	ExternalRef string
	Detail      string
	ObservedAt  time.Time
}

// EvidenceStore holds suppression-miss evidence for the scheduled demote pass to act on.
type EvidenceStore interface {
	Record(ctx context.Context, ev SuppressionEvidence) error
	// Since returns the evidence observed at or after cutoff.
	Since(ctx context.Context, cutoff time.Time) ([]SuppressionEvidence, error)
}

// MemEvidenceStore is the in-memory implementation. Bounded: it retains the most recent evidenceCap rows,
// which is ample — one proven miss is already enough to demote, so extra history is only context.
type MemEvidenceStore struct {
	mu   sync.Mutex
	rows []SuppressionEvidence
}

const evidenceCap = 500

// NewMemEvidenceStore returns an empty evidence store.
func NewMemEvidenceStore() *MemEvidenceStore { return &MemEvidenceStore{} }

// Record appends one piece of evidence.
func (s *MemEvidenceStore) Record(_ context.Context, ev SuppressionEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, ev)
	if len(s.rows) > evidenceCap {
		s.rows = s.rows[len(s.rows)-evidenceCap:]
	}
	return nil
}

// Since returns the evidence observed at or after cutoff (a copy).
func (s *MemEvidenceStore) Since(_ context.Context, cutoff time.Time) ([]SuppressionEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SuppressionEvidence
	for _, r := range s.rows {
		if !r.ObservedAt.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out, nil
}

// EvaluateEvidence demotes every tuple carrying PROOF that a learned suppression silenced a real incident,
// and returns the rows written. It is the unlearning half of the learning lane (spec/005 REQ-411).
//
// Two deliberate differences from the repeat-offender lane (Evaluate above):
//
//   - The threshold is ONE. That lane counts recurrences to tell a genuine offender from noise; here the
//     input is not a count but a demonstrated miss — a suppression that hid an incident needing action.
//     Waiting for a second proof means silencing a second real incident to earn the right to stop.
//   - The known-transient carve-out (REQ-303) does NOT apply. That carve-out exists because a tuple whose
//     RECURRENCE is by design should not be demoted for recurring. It says nothing about a tuple whose
//     suppression provably hid a crash — and honoring it here would let a "known transient" tag disable the
//     escape on exactly the patterns most likely to need it.
//
// Each demotion writes the same org-global analysis-only row the repeat-offender lane writes and appends to
// the hash-chained audit spine (INV-19), so the suppression chain's existing consult sees it with no second
// mechanism, and it auto-expires at DemotionTTL like any other (REQ-304 — reversible, no human action).
func (d *Demoter) EvaluateEvidence(ctx context.Context, evidence []SuppressionEvidence, now time.Time) ([]DemotionRow, error) {
	var demoted []DemotionRow
	seen := map[Tuple]bool{}
	for _, ev := range evidence {
		if ev.Tuple.Host == "" && ev.Tuple.AlertRule == "" {
			continue
		}
		if seen[ev.Tuple] {
			continue
		}
		seen[ev.Tuple] = true
		if _, live, err := d.Store.LiveFor(ctx, ev.Tuple, now); err != nil {
			return demoted, err
		} else if live {
			continue // already demoted; do not double-write
		}
		ver, err := schema.Stamp(schema.TableSessionRiskAudit)
		if err != nil {
			return demoted, err
		}
		row := DemotionRow{Tuple: ev.Tuple, Reason: LearnedSuppressionReason, ValidFrom: now, ValidUntil: now.Add(DemotionTTL), SchemaVersion: ver}
		if err := d.Store.Write(ctx, row); err != nil {
			return demoted, err
		}
		if d.Ledger != nil {
			if _, err := d.Ledger.Append(audit.GovDecision{
				Decision: "demote:analysis-only",
				Reason:   row.Reason + ": " + ev.Detail,
				ActionID: "demote:" + ev.Tuple.Host + "/" + ev.Tuple.AlertRule,
				Withheld: true, // demotion withholds suppression eligibility for the tuple
			}); err != nil {
				return demoted, err
			}
		}
		demoted = append(demoted, row)
	}
	return demoted, nil
}

// DemotionLookup adapts a DemotionStore to the (host, alert_rule) question the suppression chain asks
// before honoring a LEARNED pattern. It satisfies core/suppression.DemotionLookup structurally, so the
// suppression domain never imports this package (and this package never imports that one).
type DemotionLookup struct{ Store DemotionStore }

// DemotionLookupOf builds the lookup over a store.
func DemotionLookupOf(store DemotionStore) DemotionLookup { return DemotionLookup{Store: store} }

// Demoted answers whether the tuple is currently analysis-only. A nil store answers "not demoted" with no
// error: an unwired governance plane must not, by itself, block suppression — the caller's own fail-safe
// direction (a nil lookup means the consult is simply absent) is the one that governs.
func (l DemotionLookup) Demoted(ctx context.Context, host, alertRule string, now time.Time) (bool, error) {
	if l.Store == nil {
		return false, nil
	}
	return Demoted(ctx, l.Store, Tuple{Host: host, AlertRule: alertRule}, now)
}
