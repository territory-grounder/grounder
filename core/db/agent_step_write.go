package db

import (
	"context"
	"fmt"
	"sync"

	"github.com/territory-grounder/grounder/core/trace"
)

// AgentStepStore is the pgx-backed writer/reader for agent_step (migration 0031) — the APPEND-ONLY per-ReAct-
// cycle transcript for the decision tracer (spec/020 T-020-8, REQ-2008). It persists ONLY the scrubbed,
// non-secret projection the caller passes (the Runner runs every field through core/screen.Scrub BEFORE Emit).
// Parameterized SQL only; the runtime role holds no UPDATE/DELETE (0031 REVOKE), so an appended step is
// tamper-resistant (REQ-2016).
type AgentStepStore struct{ p *Pool }

// NewAgentStepStore returns the Postgres-backed agent-step transcript store.
func NewAgentStepStore(p *Pool) *AgentStepStore { return &AgentStepStore{p: p} }

// compile-time proof it satisfies the observe-only sink the Runner emits into.
var _ trace.AgentStepSink = (*AgentStepStore)(nil)

// Emit appends one scrubbed agent-step row (INSERT-only — the table is append-only). A write error is returned
// so the caller can log it, but the caller treats the emit as best-effort: it never changes the investigation.
func (s *AgentStepStore) Emit(ctx context.Context, st trace.AgentStep) error {
	if st.ExternalRef == "" {
		return fmt.Errorf("db: agent_step with empty external_ref refused")
	}
	_, err := s.p.Exec(ctx, `
		INSERT INTO agent_step (external_ref, cycle, thought, tool, observation, outcome, schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		st.ExternalRef, st.Cycle, st.Thought, st.Tool, st.Observation, st.Outcome)
	if err != nil {
		return fmt.Errorf("db: agent_step insert %s#%d: %w", st.ExternalRef, st.Cycle, err)
	}
	return nil
}

// Steps reads back the ordered agent-step transcript for one session (by cycle) — the tracer join/read surface.
func (s *AgentStepStore) Steps(ctx context.Context, externalRef string) ([]trace.AgentStep, error) {
	rows, err := s.p.Query(ctx, `
		SELECT external_ref, cycle, thought, tool, observation, outcome
		FROM agent_step WHERE external_ref = $1 ORDER BY cycle ASC`, externalRef)
	if err != nil {
		return nil, fmt.Errorf("db: agent_step read %s: %w", externalRef, err)
	}
	defer rows.Close()
	var out []trace.AgentStep
	for rows.Next() {
		var a trace.AgentStep
		if err := rows.Scan(&a.ExternalRef, &a.Cycle, &a.Thought, &a.Tool, &a.Observation, &a.Outcome); err != nil {
			return nil, fmt.Errorf("db: scan agent_step: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MemAgentStepStore is the in-memory twin for the CI oracle (no Postgres): it records the scrubbed rows per
// session so an acceptance test can prove the emit→read seam carries no secret WITHOUT a database. The pgx
// round-trip (agent_step_write_test.go, DSN-gated) proves the real SQL. Concurrency-safe.
type MemAgentStepStore struct {
	mu    sync.Mutex
	steps map[string][]trace.AgentStep
}

// NewMemAgentStepStore returns an empty in-memory agent-step twin.
func NewMemAgentStepStore() *MemAgentStepStore {
	return &MemAgentStepStore{steps: map[string][]trace.AgentStep{}}
}

// compile-time proof the twin satisfies the same sink.
var _ trace.AgentStepSink = (*MemAgentStepStore)(nil)

// Emit records one scrubbed step (append, order preserved).
func (m *MemAgentStepStore) Emit(_ context.Context, st trace.AgentStep) error {
	if st.ExternalRef == "" {
		return fmt.Errorf("db: agent_step with empty external_ref refused")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps[st.ExternalRef] = append(m.steps[st.ExternalRef], st)
	return nil
}

// Steps reads back the recorded steps for a ref, in emit order.
func (m *MemAgentStepStore) Steps(_ context.Context, externalRef string) ([]trace.AgentStep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]trace.AgentStep(nil), m.steps[externalRef]...), nil
}
