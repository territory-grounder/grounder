package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/territory-grounder/grounder/core/judge"
	"github.com/territory-grounder/grounder/core/trace"
)

// TestRecordTriageDerivesStepCountFromTranscript is the TG-398 guard.
//
// step_count (axis A6a) was written ONLY on the investigation success path, so all 135 failed:investigate
// sessions recorded 0 while their agent loop had already persisted real ReAct cycles to agent_step before
// failing — the "134 sessions did ZERO steps" outage headline was a measurement artifact. RecordTriage now
// derives the count from the durable per-session agent_step transcript when the incoming value is zero.
//
// Runs against a REAL Postgres (TG_TEST_DSN, goldtest is migrated by the harness job): the whole mechanism is
// the SQL count over agent_step and the ON CONFLICT insert, which a pgx fake would not exercise.
//
//	Killing mutation: delete the `if stepCount == 0 { ...derive... }` block in RecordTriage (so the insert
//	binds row.StepCount unchanged) — the failed:investigate assertion goes RED at step_count 0.
//	Vacuity guard: the test asserts 3 agent_step rows were persisted BEFORE RecordTriage, so a run that wrote
//	no transcript cannot pass by coincidence (an empty transcript would derive 0 and look identical to the bug).
func TestRecordTriageDerivesStepCountFromTranscript(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	steps := NewAgentStepStore(p)
	triage := NewTriageStore(p)

	uniq := fmt.Sprintf("tg398-stepcount-%d", os.Getpid())
	failedRef := uniq + "-failed"   // investigation errored: step_count arrives 0, transcript has rows
	standDownRef := uniq + "-stand" // genuine zero-step stand-down: no transcript, must stay 0
	knownRef := uniq + "-known"     // count already known (>0): must be recorded verbatim, never overwritten
	cleanup := func() {
		for _, ref := range []string{failedRef, standDownRef, knownRef} {
			_, _ = p.Exec(ctx, `DELETE FROM agent_step WHERE external_ref = $1`, ref)
			_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, ref)
		}
	}
	cleanup()
	defer cleanup()

	// The failed session ran three read-only cycles and persisted each to agent_step, then the activity
	// returned an error — so the transcript is durable but the triage row arrives with step_count 0.
	const ranSteps = 3
	for c := 1; c <= ranSteps; c++ {
		if err := steps.Emit(ctx, trace.AgentStep{ExternalRef: failedRef, Cycle: c, Tool: "get_logs", Outcome: "success"}); err != nil {
			t.Fatalf("emit agent_step #%d: %v", c, err)
		}
	}
	// VACUITY GUARD: the transcript the derivation reads must exist before we record, or the assertion below
	// would pass on an empty database exactly as it would on the bug.
	var seeded int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM agent_step WHERE external_ref = $1`, failedRef).Scan(&seeded); err != nil {
		t.Fatalf("count seeded transcript: %v", err)
	}
	if seeded != ranSteps {
		t.Fatalf("vacuity guard: seeded %d agent_step rows, want %d — the derivation assertion would be meaningless", seeded, ranSteps)
	}

	if err := triage.RecordTriage(ctx, judge.TriageRow{ExternalRef: failedRef, Outcome: "failed:investigate", StepCount: 0}); err != nil {
		t.Fatalf("record failed:investigate triage: %v", err)
	}
	if got := readStepCount(ctx, t, p, failedRef); got != ranSteps {
		t.Fatalf("failed:investigate recorded step_count=%d, want %d derived from the agent_step transcript — "+
			"a zero here is the TG-398 artifact: the session ran the steps and the row denies it", got, ranSteps)
	}

	// A genuine stand-down proposed nothing and ran no cycles: no transcript, so the derivation must leave the
	// zero alone. This is what keeps 0 meaning exactly "no cycle ran" rather than being overwritten by noise.
	if err := triage.RecordTriage(ctx, judge.TriageRow{ExternalRef: standDownRef, Outcome: "no-proposal:stop", StepCount: 0}); err != nil {
		t.Fatalf("record stand-down triage: %v", err)
	}
	if got := readStepCount(ctx, t, p, standDownRef); got != 0 {
		t.Fatalf("stand-down recorded step_count=%d, want 0 — a session with no transcript must stay zero, "+
			"otherwise the derivation invents investigation that never happened", got)
	}

	// A session that already carried its count keeps it verbatim: the derivation only fills a zero, it never
	// second-guesses a recorded count (even if the transcript disagrees, the success-path value is authoritative).
	if err := steps.Emit(ctx, trace.AgentStep{ExternalRef: knownRef, Cycle: 1, Tool: "get_logs", Outcome: "success"}); err != nil {
		t.Fatalf("emit known-ref agent_step: %v", err)
	}
	const declared = 7
	if err := triage.RecordTriage(ctx, judge.TriageRow{ExternalRef: knownRef, Outcome: "proposed", StepCount: declared}); err != nil {
		t.Fatalf("record known-count triage: %v", err)
	}
	if got := readStepCount(ctx, t, p, knownRef); got != declared {
		t.Fatalf("known-count recorded step_count=%d, want %d — a non-zero count must be recorded verbatim, "+
			"never overwritten by the transcript count", got, declared)
	}
}

func readStepCount(ctx context.Context, t *testing.T, p *Pool, externalRef string) int {
	t.Helper()
	var n int
	if err := p.QueryRow(ctx, `SELECT step_count FROM session_triage WHERE external_ref = $1`, externalRef).Scan(&n); err != nil {
		t.Fatalf("read step_count for %s: %v", externalRef, err)
	}
	return n
}
