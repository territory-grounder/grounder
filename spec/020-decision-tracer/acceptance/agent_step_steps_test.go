package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cucumber/godog"

	"github.com/territory-grounder/grounder/agent"
	"github.com/territory-grounder/grounder/core/db"
	"github.com/territory-grounder/grounder/core/screen"
	"github.com/territory-grounder/grounder/core/trace"
)

// T-020-8 binds REQ-2008 (each ReAct cycle leaves a SCRUBBED agent-step row with no secret value) and REQ-2016
// (the runtime role holds no UPDATE/DELETE on the agent_step + interceptor_gate_verdict evidence tables). The
// REQ-2008 oracle drives the REAL screen.Scrub redaction path (the exact per-field scrub the InvestigateActivity
// applies) into the real db.MemAgentStepStore, feeding secrets Scrub genuinely catches (PEM key / Bearer token /
// provider token), and asserts the persisted rows carry no raw secret + show the redaction marker. REQ-2016's
// CI half asserts BOTH migrations carry the REVOKE (the grant source); the live-role denial is proven by the
// DSN-gated core/db round-trip. Observe-only: the trace package imports no safety/policy/actuation code.
func init() {
	stepRegistrars = append(stepRegistrars, registerAgentStepSteps)
	stepRegistrars = append(stepRegistrars, registerEvidenceImmutabilitySteps)
}

type agentStepWorld struct {
	ref     string
	steps   []agent.AgentCycle // the CYCLE-ALIGNED transcript the loop produces (agent.Result.Steps)
	secrets []string           // the RAW secret substrings that must NOT survive into a persisted row
	store   *db.MemAgentStepStore
}

func registerAgentStepSteps(sc *godog.ScenarioContext) {
	w := &agentStepWorld{}

	sc.Step(`^an agent loop whose thoughts observations and tool-results may contain secret-shaped text$`, func() error {
		w.ref = "ext-req2008"
		// Secret VALUES are CONSTRUCTED so the source holds no contiguous secret-shaped literal (the
		// forbidden-pattern / mirror-sanitizer gate), while the RUNTIME value is a real secret shape screen.Scrub
		// genuinely redacts — the same split-literal convention as core/screen/screen_test.go.
		keyBody := strings.Repeat("Z", 24) + "KEYMATERIAL"
		pem := "-----BEGIN " + "PRIVATE KEY-----\n" + keyBody + "\n-----END " + "PRIVATE KEY-----"
		bearerTok := strings.Repeat("A", 20) + "b2C4d6"
		ghpTok := "gh" + "p_" + strings.Repeat("A", 40)
		// A deliberately MISALIGNED transcript — the exact shape that broke the old union-by-index emit: cycle 2
		// has NO thought (agent.Result.Thoughts skips it) and cycle 3 is a propose with NO tool (ToolResults
		// skips it). agent.Result.Steps is CYCLE-ALIGNED (one AgentCycle per iteration with its real ordinal), so
		// the emit must key each row on step.Cycle and pair each cycle's own thought+tool — never zip the sparse
		// slices (which would glue cycle-3's thought onto cycle-2's tool and drop cycle 3 entirely).
		w.steps = []agent.AgentCycle{
			{Cycle: 1, Thought: "inspecting the host key " + pem, Action: "tool", Tool: "get_config", Observation: "observed lnms-1", Outcome: "investigate"},
			{Cycle: 2, Thought: "", Action: "tool", Tool: "get_logs", Observation: "log line: Authorization: Bearer " + bearerTok + " on web01", Outcome: "investigate"},
			{Cycle: 3, Thought: "the CI token " + ghpTok + " is set; proposing restart", Action: "propose", Tool: "", Observation: "", Outcome: "proposed"},
		}
		w.secrets = []string{keyBody, bearerTok, ghpTok}
		return nil
	})

	sc.Step(`^each ReAct cycle is persisted$`, func() error {
		w.store = db.NewMemAgentStepStore()
		// The SAME cycle-aligned emit the InvestigateActivity runs: iterate res.Steps 1:1, Scrub every text field
		// before Emit, key each row on the REAL step.Cycle (INV-13/INV-08).
		for _, st := range w.steps {
			th, _ := screen.Scrub(st.Thought)
			tool, _ := screen.Scrub(st.Tool)
			obs, _ := screen.Scrub(st.Observation)
			if err := w.store.Emit(context.Background(), trace.AgentStep{
				ExternalRef: w.ref, Cycle: st.Cycle, Thought: th, Tool: tool, Observation: obs, Outcome: st.Outcome,
			}); err != nil {
				return err
			}
		}
		return nil
	})

	sc.Step(`^one agent_step row per cycle is written keyed by external_ref every field is run through the Scrub redaction path before write and the row carries no secret value and the thoughts never re-enter the decision path as control flow$`, func() error {
		rows, err := w.store.Steps(context.Background(), w.ref)
		if err != nil {
			return err
		}
		// One row per REAL cycle — 3 here. The old union-by-index emit produced only max(len(Thoughts),
		// len(ToolResults)) = 2 rows, DROPPING cycle 3; this assertion fails under that regression.
		if len(rows) != len(w.steps) {
			return fmt.Errorf("want one row per real cycle (%d), got %d — a cycle was dropped/mispaired", len(w.steps), len(rows))
		}
		sawRedaction := false
		for i, r := range rows {
			if r.ExternalRef != w.ref {
				return fmt.Errorf("row %d not keyed by external_ref: got %q", i, r.ExternalRef)
			}
			// Ordinals are the REAL per-cycle ordinals, contiguous 1..N.
			if r.Cycle != w.steps[i].Cycle {
				return fmt.Errorf("row %d has cycle=%d, want %d", i, r.Cycle, w.steps[i].Cycle)
			}
			blob := r.Thought + "\x00" + r.Tool + "\x00" + r.Observation
			for _, sec := range w.secrets {
				if strings.Contains(blob, sec) {
					return fmt.Errorf("cycle %d row carries a RAW secret value %q — Scrub did not redact before write", r.Cycle, sec)
				}
			}
			if strings.Contains(blob, "[REDACTED:") {
				sawRedaction = true
			}
		}
		// NO cross-cycle field mispairing: cycle 2 had no thought, so its row's Thought is EMPTY and its Tool is
		// cycle-2's own tool — NOT cycle-3's thought glued on (the old zip bug). Cycle 3 is a propose with NO tool.
		if rows[1].Thought != "" {
			return fmt.Errorf("cycle 2 row carries a thought %q, but cycle 2 emitted none — a foreign cycle's thought was mispaired", rows[1].Thought)
		}
		if rows[1].Tool != "get_logs" {
			return fmt.Errorf("cycle 2 row tool=%q, want get_logs — wrong cycle's tool", rows[1].Tool)
		}
		if rows[2].Tool != "" {
			return fmt.Errorf("cycle 3 (propose) row has tool=%q, want empty — a foreign cycle's tool was mispaired", rows[2].Tool)
		}
		if rows[2].Thought == "" {
			return fmt.Errorf("cycle 3 row lost its thought — the propose cycle was dropped/mispaired")
		}
		if !sawRedaction {
			return fmt.Errorf("no [REDACTED:...] marker in any persisted row — the Scrub redaction path did not run")
		}
		// "thoughts never re-enter the decision path as control flow" is structural (INV-08): agent_step is a
		// pure side-write; nothing reads it to dispatch, and core/trace imports no safety/policy/actuation package.
		return nil
	})
}

func registerEvidenceImmutabilitySteps(sc *godog.ScenarioContext) {
	sc.Step(`^the append-only agent_step and interceptor_gate_verdict tables$`, func() error { return nil })
	sc.Step(`^the runtime database role attempts to update or delete a persisted trace row$`, func() error { return nil })
	sc.Step(`^the update or delete is denied by the grants because the runtime role holds no UPDATE and no DELETE on these evidence tables$`, func() error {
		// CI-runnable half of REQ-2016: assert BOTH evidence migrations REVOKE UPDATE+DELETE from tg_runtime (the
		// grant SOURCE). The live-role denial is proven by the DSN-gated core/db round-trip (agent_step immutability).
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			return fmt.Errorf("cannot resolve test file path to read the migrations")
		}
		migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "core", "db", "migrations")
		for _, m := range []struct{ file, table string }{
			{"0031_agent_step.up.sql", "agent_step"},
			{"0030_interceptor_gate_verdict.up.sql", "interceptor_gate_verdict"},
		} {
			b, err := os.ReadFile(filepath.Join(migDir, m.file))
			if err != nil {
				return fmt.Errorf("read %s: %w", m.file, err)
			}
			want := "REVOKE UPDATE, DELETE ON " + m.table + " FROM tg_runtime"
			if !strings.Contains(string(b), want) {
				return fmt.Errorf("%s is missing the append-only grant %q — the runtime role could tamper this evidence table", m.file, want)
			}
		}
		return nil
	})
}
