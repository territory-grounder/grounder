package db

import (
	"context"
	"fmt"
	"sync"

	"github.com/territory-grounder/grounder/core/trace"
)

// GateVerdictStore is the pgx-backed APPEND-ONLY writer for interceptor_gate_verdict (migration 0030, spec/020
// T-020-7, REQ-2007/REQ-2016): one ordered NON-SECRET row per interceptor gate. Parameters are always bound
// ($1) — no string-built SQL. The runtime role holds no UPDATE/DELETE on the table (0030 REVOKE), so an emitted
// gate row is tamper-resistant like the rest of the accountability spine. Only non-secret projection fields are
// written (gate/verdict/reason + the two correlation keys) — no argv/host/credential can land here (INV-13).
type GateVerdictStore struct{ p *Pool }

// NewGateVerdictStore returns the Postgres-backed gate-verdict trail writer.
func NewGateVerdictStore(p *Pool) *GateVerdictStore { return &GateVerdictStore{p: p} }

// compile-time proof it satisfies the observe-only sink the interceptor emits into.
var _ trace.GateVerdictSink = (*GateVerdictStore)(nil)

// Emit appends one gate-verdict row. Called observe-only from the interceptor; a returned error is swallowed by
// the caller (it never changes a gate outcome), but the failure is surfaced here rather than silently dropped.
func (s *GateVerdictStore) Emit(ctx context.Context, gv trace.GateVerdict) error {
	if gv.ActionID == "" {
		return fmt.Errorf("db: gate verdict with empty action_id refused")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO interceptor_gate_verdict
			(action_id, external_ref, ordinal, gate, verdict, reason, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		gv.ActionID, gv.ExternalRef, gv.Ordinal, gv.Gate, gv.Verdict, gv.Reason)
	if err != nil {
		return fmt.Errorf("db: gate verdict emit %s/%d (%s): %w", gv.ActionID, gv.Ordinal, gv.Gate, err)
	}
	return nil
}

// GateRows reads the ordered gate-verdict trail for one action (the tracer join / round-trip read). Read-only,
// bound, ordered by ordinal — the ordered walk the detail endpoint projects.
func (s *GateVerdictStore) GateRows(ctx context.Context, actionID string) ([]trace.GateVerdict, error) {
	rows, err := s.p.Query(ctx, `
		SELECT action_id, external_ref, ordinal, gate, verdict, reason
		FROM interceptor_gate_verdict
		WHERE action_id = $1
		ORDER BY ordinal ASC`, actionID)
	if err != nil {
		return nil, fmt.Errorf("db: gate verdict rows %s: %w", actionID, err)
	}
	defer rows.Close()
	var out []trace.GateVerdict
	for rows.Next() {
		var g trace.GateVerdict
		if err := rows.Scan(&g.ActionID, &g.ExternalRef, &g.Ordinal, &g.Gate, &g.Verdict, &g.Reason); err != nil {
			return nil, fmt.Errorf("db: scan gate verdict: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MemGateVerdictStore is the in-memory GateVerdictSink twin for the CI oracles (no Postgres): it records every
// emitted gate row in emit order so an acceptance test can assert the ordered trail (and that a refusal leaves
// no phantom pass rows past it) WITHOUT a database. The pgx round-trip (gate_verdict_write_test.go, DSN-gated)
// proves the real SQL persists the order. Concurrency-safe.
type MemGateVerdictStore struct {
	mu   sync.Mutex
	rows []trace.GateVerdict
}

// NewMemGateVerdictStore returns an empty in-memory gate-verdict twin.
func NewMemGateVerdictStore() *MemGateVerdictStore { return &MemGateVerdictStore{} }

// compile-time proof the mem twin satisfies the sink.
var _ trace.GateVerdictSink = (*MemGateVerdictStore)(nil)

// Emit records the row (append, in emit order).
func (m *MemGateVerdictStore) Emit(_ context.Context, gv trace.GateVerdict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, gv)
	return nil
}

// GateRows returns the recorded rows for one action in emit order.
func (m *MemGateVerdictStore) GateRows(actionID string) []trace.GateVerdict {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []trace.GateVerdict
	for _, g := range m.rows {
		if g.ActionID == actionID {
			out = append(out, g)
		}
	}
	return out
}
