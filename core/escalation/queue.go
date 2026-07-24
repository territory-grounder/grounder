// Package escalation implements Territory Grounder's unanswered-poll requeue lane: it schedules a
// delayed re-check when an approval poll goes unanswered, fires it through an authenticated internal
// Temporal signal (never a bare re-trigger), re-escalates or defers on the live condition, and stands
// down to a human at the per-incident cap.
//
// Provenance: [F] spec/003 (BEH-3), the predecessor reconcile requeue lane · [O] INV-01/INV-12 (a
// re-check re-enters only via the authenticated signal path, keyed by session id) · [R] paradigm-rule 2
// (stand down to the fallback approver / next on-call tier, not one named person).
package escalation

import (
	"context"
	"errors"
	"time"

	"github.com/territory-grounder/grounder/core/persist"
)

// Outcome is the result of scheduling or firing a re-check.
type Outcome int

const (
	// Scheduled: a delayed re-check row was appended (pending).
	Scheduled Outcome = iota
	// ReEscalated: on fire the condition was still active — the approver graph was paged.
	ReEscalated
	// Deferred: on fire the condition had recovered — closure deferred to the autocloser.
	Deferred
	// StoodDown: the per-incident cap was reached — escalated to the fallback approver / next tier.
	StoodDown
)

func (o Outcome) String() string {
	switch o {
	case ReEscalated:
		return "re_escalated"
	case Deferred:
		return "deferred"
	case StoodDown:
		return "stood_down"
	default:
		return "scheduled"
	}
}

// ConditionChecker reports whether an incident's alert condition is still active (a live post-condition
// check). It is never the acting agent's asserted success.
type ConditionChecker interface {
	StillActive(ctx context.Context, externalRef string) (bool, error)
}

// Pager pages an approver tier (the approver graph, or the fallback approver / next on-call tier).
type Pager interface {
	Page(ctx context.Context, externalRef, tier string) error
}

// ReCheckResult records the outcome of one fired/scheduled re-check for observability.
type ReCheckResult struct {
	ExternalRef string
	Attempts    int
	Outcome     Outcome
}

// Store is the durable seam the requeue controller drives (the spec/006 escalation_queue contract): an
// append-only queue that BOTH the in-memory oracle (persist.EscalationQueue) and the pgx store
// (db.EscalationStore) satisfy — so an operator with a database gets a requeue lane that survives a restart
// behind the SAME controller, while CI runs the controller over the in-memory twin. The controller depends
// only on this interface, never a concrete store. The store exposes the due batch (DuePending) and the
// append-only fired transition (MarkFired); the authenticated re-entry SIGNAL is the controller's (INV-01).
type Store interface {
	Enqueue(ctx context.Context, ref string, attempts int, eligibleAt time.Time) (persist.EscalationItem, error)
	DuePending(ctx context.Context, now time.Time) ([]persist.EscalationItem, error)
	MarkFired(ctx context.Context, seq int64) error
}

// Controller drives the escalation_queue requeue lane over the spec/006 persistence contract. It owns the
// authenticated re-entry signal (SignalRequeue), so each due row re-enters the gated pipeline THROUGH this
// path (REQ-207), never a bare re-trigger.
type Controller struct {
	Store     Store
	Condition ConditionChecker
	Pager     Pager
	Cap       int // per-incident unanswered-poll cap (REQ-208)

	results []ReCheckResult
}

// NewController builds a requeue controller over the given store (in-memory oracle or durable pgx twin).
func NewController(store Store, cond ConditionChecker, pager Pager, cap int) *Controller {
	return &Controller{Store: store, Condition: cond, Pager: pager, Cap: cap}
}

// FireDue fires every due re-check through the authenticated signal path: it reads the due batch from the
// store, marks each row CONSUMED, then re-enters it via SignalRequeue (the ONLY re-entry primitive —
// INV-01/INV-12). Append-only: a fired row is transitioned in place, never deleted.
//
// Two safety properties the naive "signal then mark, abort on first error" shape lacked:
//   - BOUNDED (no page storm): the row is marked fired BEFORE it is re-entered, so a persistently-failing
//     MarkFired (a stuck store) produces NO page at all — no mark, no signal — instead of a paged-but-unmarked
//     row that re-pages the approver graph every tick forever. A dropped re-entry is not lost: the reconcile
//     loop schedules a fresh re-check (ScheduleReCheck), bounded by the per-incident Cap, so escalation still
//     converges to a human.
//   - ISOLATED (no head-of-line block): a failure on one row records the error and CONTINUES to the next, so
//     one poisoned incident can never starve every later incident's due re-check. Errors are joined and returned.
func (c *Controller) FireDue(ctx context.Context, now time.Time) (int, error) {
	due, err := c.Store.DuePending(ctx, now)
	if err != nil {
		return 0, err
	}
	fired := 0
	var errs []error
	for _, it := range due {
		if err := c.Store.MarkFired(ctx, it.Seq); err != nil {
			errs = append(errs, err)
			continue // no mark ⇒ no page; the row stays pending and is retried next tick (no storm)
		}
		if err := c.SignalRequeue(ctx, it.ExternalRef); err != nil {
			errs = append(errs, err)
			continue // per-row isolation: a poisoned row never blocks the rest of the batch
		}
		fired++
	}
	return fired, errors.Join(errs...)
}

// Results returns the recorded re-check outcomes (schedule + fire), in order.
func (c *Controller) Results() []ReCheckResult { return c.results }
