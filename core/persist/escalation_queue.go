package persist

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/territory-grounder/grounder/core/schema"
)

// EscalStatus is the lifecycle of an escalation_queue row. The zero value is EscalPending.
type EscalStatus int

const (
	EscalPending   EscalStatus = iota // enqueued, awaiting its eligible time
	EscalFired                        // re-entered the gated pipeline via an authenticated signal
	EscalStoodDown                    // superseded / resolved before firing
)

func (s EscalStatus) String() string {
	switch s {
	case EscalFired:
		return "fired"
	case EscalStoodDown:
		return "stood_down"
	default:
		return "pending"
	}
}

// EscalationItem is one append-only escalation_queue row: a dropped-escalation requeue keyed by
// external_ref, with an attempt count and an eligible time. [O] REQ-507.
type EscalationItem struct {
	Seq           int64
	ExternalRef   string
	Attempts      int
	Status        EscalStatus
	EligibleAt    time.Time
	SchemaVersion schema.Version
}

// ErrEmptyRef fails closed on an escalation with no correlation key.
var ErrEmptyRef = errors.New("persist: escalation_queue row missing external_ref")

// EscalationQueue is the append-only, rate-capped requeue lane (escalation_queue). Append-only means
// rows are NEVER deleted — the requeue history is preserved; marking a row fired transitions its status but
// leaves it in place. This is the in-memory oracle core; the pgx store (db.EscalationStore) is its durable
// twin, and both satisfy the same store seam (Enqueue/DuePending/MarkFired with ctx) that the requeue
// controller (spec/003) drives — so the controller runs over either store unchanged. The re-entry SIGNAL
// itself is the controller's, not the store's: the store only exposes the due batch and the fired transition.
type EscalationQueue struct {
	mu    sync.Mutex // the queue is shared — Enqueue derives seq from len(items) and MarkFired mutates status, so guard both
	items []EscalationItem
}

// NewEscalationQueue returns an empty queue.
func NewEscalationQueue() *EscalationQueue { return &EscalationQueue{} }

// Enqueue appends a new pending requeue row (stamped with the schema version). Append-only: it never
// mutates or removes a prior row. The ctx matches the durable twin (db.EscalationStore.Enqueue) so both
// satisfy the same seam; the in-memory oracle does not use it.
func (q *EscalationQueue) Enqueue(_ context.Context, ref string, attempts int, eligibleAt time.Time) (EscalationItem, error) {
	if ref == "" {
		return EscalationItem{}, ErrEmptyRef
	}
	v, err := schema.Stamp(schema.TableEscalationQueue)
	if err != nil {
		return EscalationItem{}, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	item := EscalationItem{
		Seq:           int64(len(q.items)) + 1,
		ExternalRef:   ref,
		Attempts:      attempts,
		Status:        EscalPending,
		EligibleAt:    eligibleAt,
		SchemaVersion: v,
	}
	q.items = append(q.items, item)
	return item, nil
}

// DuePending returns every pending row whose eligible time has arrived, oldest-first (Seq order) — the batch
// the requeue controller fires. A copy is returned; callers never alias the queue. Mirrors
// db.EscalationStore.DuePending so the controller drives either store through the same seam.
func (q *EscalationQueue) DuePending(_ context.Context, now time.Time) ([]EscalationItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []EscalationItem
	for _, it := range q.items {
		if it.Status == EscalPending && !it.EligibleAt.After(now) {
			out = append(out, it)
		}
	}
	return out, nil
}

// MarkFired transitions the pending row with the given seq to fired after it re-entered the gated pipeline.
// Append-only: it never deletes a row. Idempotent — a seq that is missing or already fired is a no-op (no
// error), matching db.EscalationStore.MarkFired's `AND status = 'pending'` guard.
func (q *EscalationQueue) MarkFired(_ context.Context, seq int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].Seq == seq && q.items[i].Status == EscalPending {
			q.items[i].Status = EscalFired
			return nil
		}
	}
	return nil
}

// Items returns a copy of every row (fired and pending) — the preserved, append-only history.
func (q *EscalationQueue) Items() []EscalationItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]EscalationItem, len(q.items))
	copy(out, q.items)
	return out
}

// Len returns the number of rows ever enqueued (never decreases — append-only).
func (q *EscalationQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
