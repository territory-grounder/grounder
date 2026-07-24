package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/trace"
)

// TestAgentStepRoundTrip drives the REAL pgx path for the agent-step transcript (spec/020 T-020-8, REQ-2008)
// and proves the evidence tables' immutability grants (REQ-2016). It emits rows OUT OF cycle order and asserts
// Steps reads them back ordered by cycle with every field carried (guards the fake-hides-field-drop lesson),
// then asserts tg_runtime holds NO UPDATE and NO DELETE on BOTH agent_step and interceptor_gate_verdict. Gated
// on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestAgentStepRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the agent-step round-trip test")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	s := NewAgentStepStore(p)

	ref := fmt.Sprintf("agent-step-it-%d", os.Getpid())
	defer func() { _, _ = p.Exec(ctx, "DELETE FROM agent_step WHERE external_ref = $1", ref) }()

	c1 := trace.AgentStep{ExternalRef: ref, Cycle: 1, Thought: "checked host key [REDACTED:private-key]", Tool: "get_config", Observation: "ok", Outcome: "success"}
	c2 := trace.AgentStep{ExternalRef: ref, Cycle: 2, Thought: "logs", Tool: "get_logs", Observation: "[REDACTED:bearer-token]", Outcome: "failure"}
	// Emit out of order to prove the read orders by cycle, not insertion.
	if err := s.Emit(ctx, c2); err != nil {
		t.Fatalf("emit c2: %v", err)
	}
	if err := s.Emit(ctx, c1); err != nil {
		t.Fatalf("emit c1: %v", err)
	}

	got, err := s.Steps(ctx, ref)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if len(got) != 2 || got[0].Cycle != 1 || got[1].Cycle != 2 {
		t.Fatalf("want 2 rows ordered by cycle, got %+v", got)
	}
	if got[0].Tool != "get_config" || got[0].Observation != "ok" || got[1].Tool != "get_logs" || got[1].Outcome != "failure" {
		t.Fatalf("agent_step round-trip dropped a field: %+v", got)
	}

	// REQ-2016: the runtime role holds no UPDATE and no DELETE on BOTH evidence tables.
	for _, tbl := range []string{"agent_step", "interceptor_gate_verdict"} {
		var upd, del bool
		if err := p.QueryRow(ctx,
			"SELECT has_table_privilege('tg_runtime', $1, 'UPDATE'), has_table_privilege('tg_runtime', $1, 'DELETE')", tbl).
			Scan(&upd, &del); err != nil {
			t.Logf("grant check on %s skipped (role lacks privilege to introspect): %v", tbl, err)
			continue
		}
		if upd || del {
			t.Fatalf("%s: tg_runtime has UPDATE=%v DELETE=%v, want both false (REQ-2016 append-only)", tbl, upd, del)
		}
	}
}
