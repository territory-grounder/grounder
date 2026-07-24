package persist

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// The pending_decisions projection (spec/006 REQ-519): the POLL_PAUSE decisions currently awaiting a
// human vote, so the console can LIST them and the operator can act (the vote itself is REQ-518/INV-12).
//
// This is a READ PROJECTION, never an authority. The Runner workflow is the sole authority on whether an
// action is paused, released, or denied; this table only mirrors "a poll is open, here is what it says" so
// a separate process (the grounder HTTP surface) can display it — the workflow runs in the worker, the
// console reads from the grounder, so the projection MUST be shared state (Postgres), not in-process memory.
// A row here can never release an action: releasing goes through /v1/vote → the waiting workflow, which
// accepts the vote ONLY when action_id names its sealed action (INV-12). A stale/duplicate projection row
// is harmless — the workflow is the gate.
//
// Lifecycle: the workflow OPENS a row when it enters POLL_PAUSE (upsert keyed by external_ref — a session
// holds one open decision at a time) and RESOLVES it when the poll is decided (approved / denied / timeout).
// Resolved rows are retained (the recent decision history), never deleted.

// PendingStatus is the lifecycle of a pending_decision row. The zero value is DecisionOpen.
type PendingStatus int

const (
	DecisionOpen     PendingStatus = iota // the poll is open, awaiting a human vote
	DecisionResolved                      // the poll was decided (approved / denied / timeout)
)

func (s PendingStatus) String() string {
	if s == DecisionResolved {
		return "resolved"
	}
	return "open"
}

// PendingDecision is one POLL_PAUSE decision projected for the console. It carries exactly what a human
// needs to decide — the sealed action it binds (action_id), the candidate approaches, the committed
// prediction, reversibility — plus the correlation key the vote binds to (external_ref). It holds NO
// authority, NO secret, and NO caller-supplied control: the workflow populates it from its own sealed poll.
//
// It is OPERATIONAL projection state (like operator_sessions), not a governed audit row — the governance
// ledger remains the authoritative decision record — so it carries no schema_version.
type PendingDecision struct {
	ExternalRef string
	ActionID    string
	Band        string // always "POLL_PAUSE" for a paused decision — the only band that waits for a human
	Approaches  []string
	Prediction  string
	Reversible  bool
	Site        string
	OpenedAt    time.Time
	Status      PendingStatus
	Outcome     string // "" while open; "approved" / "denied" / "timeout" once resolved
	ResolvedAt  time.Time
}

// ErrEmptyDecisionKey fails closed on a decision with no correlation key or no bound action.
var ErrEmptyDecisionKey = errors.New("persist: pending_decision row missing external_ref or action_id")

// PendingWriter records and resolves open decisions. The Runner workflow (worker process) drives it via an
// activity; the durable pgx twin (db.PendingStore) and the in-memory oracle both satisfy it.
type PendingWriter interface {
	// OpenDecision upserts an open decision keyed by external_ref (idempotent on Temporal activity retry —
	// re-opening the same ref+action is a no-op refresh, not a duplicate).
	OpenDecision(ctx context.Context, d PendingDecision) error
	// ResolveDecision marks the open decision for external_ref resolved with the given outcome, but ONLY
	// when actionID names the action that row bound (a vote/timeout for a different action never resolves
	// it). Idempotent: an unknown, already-resolved, or mismatched row is a no-op, never an error.
	ResolveDecision(ctx context.Context, externalRef, actionID, outcome string, resolvedAt time.Time) error
}

// PendingReader lists open decisions for the console read surface. The grounder process drives it.
type PendingReader interface {
	OpenDecisions(ctx context.Context) ([]PendingDecision, error) // open rows, oldest first
	CountOpen(ctx context.Context) (int, error)
}

// MemPendingDecisions is the in-memory oracle (CI has no Postgres) AND a single-process fallback. Keyed by
// external_ref: a session holds at most one open decision. It is NOT a cross-process store — production
// wires the pgx twin so the worker's write is visible to the grounder's read.
type MemPendingDecisions struct {
	mu   sync.Mutex
	rows map[string]PendingDecision // external_ref → latest decision
}

// NewMemPendingDecisions returns an empty in-memory projection.
func NewMemPendingDecisions() *MemPendingDecisions {
	return &MemPendingDecisions{rows: map[string]PendingDecision{}}
}

// OpenDecision upserts an open decision. It fails closed if the correlation key or the bound action is
// missing — a decision with no ref can never be voted on.
func (m *MemPendingDecisions) OpenDecision(_ context.Context, d PendingDecision) error {
	if d.ExternalRef == "" || d.ActionID == "" {
		return ErrEmptyDecisionKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d.Band = "POLL_PAUSE"
	d.Status = DecisionOpen
	d.Outcome = ""
	d.ResolvedAt = time.Time{}
	m.rows[d.ExternalRef] = d
	return nil
}

// ResolveDecision marks the row for external_ref resolved, only if it is open and actionID matches.
func (m *MemPendingDecisions) ResolveDecision(_ context.Context, externalRef, actionID, outcome string, resolvedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[externalRef]
	if !ok || row.Status != DecisionOpen || row.ActionID != actionID {
		return nil // idempotent no-op: unknown, already resolved, or a different action
	}
	row.Status = DecisionResolved
	row.Outcome = outcome
	row.ResolvedAt = resolvedAt
	m.rows[externalRef] = row
	return nil
}

// OpenDecisions returns the open rows, oldest first (OpenedAt then external_ref for a stable order).
func (m *MemPendingDecisions) OpenDecisions(_ context.Context) ([]PendingDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PendingDecision
	for _, r := range m.rows {
		if r.Status == DecisionOpen {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAt.Equal(out[j].OpenedAt) {
			return out[i].ExternalRef < out[j].ExternalRef
		}
		return out[i].OpenedAt.Before(out[j].OpenedAt)
	})
	return out, nil
}

// CountOpen returns the number of open decisions.
func (m *MemPendingDecisions) CountOpen(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.Status == DecisionOpen {
			n++
		}
	}
	return n, nil
}
