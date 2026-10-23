package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/territory-grounder/grounder/adapters/model"
)

// stateKeyedModel is deterministic w.r.t. the CONVERSATION, not a call counter — essential for a resume test:
// a full run and a run resumed from a mid checkpoint make DIFFERENT numbers of calls, so a call-indexed script
// (scriptedModel) would desync. This one keys on how many OBSERVATION envelopes are already in msgs: it issues a
// distinct-arg read (distinct args dodge the repeated-call trajectory veto) until `investigate` of them exist,
// then a grounded proposal citing tr-1 (the id readTool captures). So the response at "2 observations gathered"
// is identical whether reached fresh or by resume.
type stateKeyedModel struct {
	investigate int
	calls       int // model calls made — the resumed run must make FEWER (it skips the pre-crash cycles)
}

func (m *stateKeyedModel) Complete(_ context.Context, _, _ string, msgs []model.Message) (string, error) {
	m.calls++
	obs := 0
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "OBSERVATION[") {
			obs++
		}
	}
	if obs < m.investigate {
		// distinct host per state so the trajectory veto (repeated identical tool calls) never fires.
		return `{"action":"tool","tool":"get-logs","args":{"host":"web0` + string(rune('1'+obs)) + `"},"confidence":0.8}`, nil
	}
	return proposeHigh, nil
}

// reconSpy records the session ids the read-lane budget is metered on — so a test can assert a resumed run
// keys on the SAME session identity the crashed attempt used (TG-297 restore), not a freshly-minted one.
type reconSpy struct{ sessions map[string]bool }

func (r *reconSpy) Admit(session string) error  { r.see(session); return nil }
func (r *reconSpy) Record(session, _, _ string) { r.see(session) }
func (r *reconSpy) see(session string) {
	if r.sessions == nil {
		r.sessions = map[string]bool{}
	}
	r.sessions[session] = true
}
func (r *reconSpy) only(t *testing.T) string {
	t.Helper()
	if len(r.sessions) != 1 {
		t.Fatalf("expected exactly one session keyed, got %d: %v", len(r.sessions), r.sessions)
	}
	for s := range r.sessions {
		return s
	}
	return ""
}

// TestDurableCheckpointResumeReproducesFullRun is the correctness oracle for TG-47 durable checkpointing: a run
// resumed from a mid-investigation checkpoint (round-tripped through JSON, exactly as a Temporal heartbeat would
// serialize it) produces a Result byte-identical to the run that never crashed. KILLING MUTATION: drop any field
// from the restore in loop.go (e.g. don't restore res, or start the loop at cycle 1) → the resumed Result loses
// the pre-crash cycles' Steps/ToolResults and this DeepEqual fails.
func TestDurableCheckpointResumeReproducesFullRun(t *testing.T) {
	lim := Limits{HandoffHalt: 10, HandoffPoll: 5}

	// FULL run — capture every cycle-boundary checkpoint, JSON-serialized at emit time (the loop mutates the
	// live slices afterwards, so a retained reference would not be a snapshot; serializing is what the activity
	// does too).
	var snaps [][]byte
	full := newAgent(nil, lim)
	fullModel := &stateKeyedModel{investigate: 3}
	full.Model = fullModel
	fullRecon := &reconSpy{}
	full.Recon = fullRecon
	full.Checkpoint = &CheckpointHooks{Emit: func(cp Checkpoint) {
		b, err := json.Marshal(cp)
		if err != nil {
			t.Fatalf("checkpoint is not JSON-serializable (Temporal heartbeat would reject it): %v", err)
		}
		snaps = append(snaps, b)
	}}
	seed := []model.Message{{Role: "user", Content: "web01 NginxDown — investigate"}}
	rFull, err := full.Run(context.Background(), seed)
	if err != nil {
		t.Fatalf("full run: %v", err)
	}
	if rFull.Outcome != OutcomeProposed {
		t.Fatalf("full run expected a proposal, got outcome %v (reason %q)", rFull.Outcome, rFull.Reason)
	}
	if len(snaps) < 3 {
		t.Fatalf("expected >=3 cycle checkpoints, got %d", len(snaps))
	}

	// RESUME from the SECOND checkpoint (cycle 2, one observation already gathered) — the mid-investigation
	// point a crash-restart would land on.
	var resume Checkpoint
	if err := json.Unmarshal(snaps[1], &resume); err != nil {
		t.Fatalf("checkpoint did not round-trip through JSON: %v", err)
	}
	resumed := newAgent(nil, lim)
	resumedModel := &stateKeyedModel{investigate: 3} // fresh, state-keyed — no counter to reset
	resumed.Model = resumedModel
	resumedRecon := &reconSpy{}
	resumed.Recon = resumedRecon
	resumed.Checkpoint = &CheckpointHooks{Resume: &resume}
	rResumed, err := resumed.Run(context.Background(), seed)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}

	// TG-297 identity restore: the resumed run must meter its read-lane budget on the SAME session the crashed
	// attempt used (carried in the checkpoint), not a freshly-minted one — else every crash-resume grants a new
	// recon/tool budget. KILLING MUTATION: drop the SessionID restore in loop.go → the resumed run keys on a new
	// id and this fails.
	if fs, rs := fullRecon.only(t), resumedRecon.only(t); fs != rs {
		t.Fatalf("resume did not restore the session identity: full keyed on %q, resumed on %q", fs, rs)
	}

	// It must genuinely RESUME, not silently re-run from scratch: resuming from the cycle-2 checkpoint skips
	// exactly cycle 1, so the resumed run makes one FEWER model call than the full run. Without this, a no-op
	// restore would pass the DeepEqual below (a fresh re-run reaches the same proposal).
	if resumedModel.calls != fullModel.calls-1 {
		t.Fatalf("resume did not skip the pre-checkpoint cycle: full made %d model calls, resumed made %d (want full-1)", fullModel.calls, resumedModel.calls)
	}

	if !reflect.DeepEqual(rFull, rResumed) {
		t.Fatalf("resumed Result diverged from the full run:\n full:    outcome=%v cycles=%d steps=%d toolresults=%d ref=%s\n resumed: outcome=%v cycles=%d steps=%d toolresults=%d ref=%s",
			rFull.Outcome, rFull.Cycles, len(rFull.Steps), len(rFull.ToolResults), rFull.Proposal.ExternalRef,
			rResumed.Outcome, rResumed.Cycles, len(rResumed.Steps), len(rResumed.ToolResults), rResumed.Proposal.ExternalRef)
	}
}

// TestDurableCheckpointNilHooksIsByteIdentical proves the OFF path: a nil Checkpoint hooks pointer (every
// non-activity caller, and the worker with the flag unset) runs exactly as before — no restore, no emit.
func TestDurableCheckpointNilHooksIsByteIdentical(t *testing.T) {
	lim := Limits{HandoffHalt: 10, HandoffPoll: 5}
	a := newAgent(nil, lim)
	a.Model = &stateKeyedModel{investigate: 2}
	if a.Checkpoint != nil {
		t.Fatal("precondition: Checkpoint must default nil")
	}
	r, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "web01 down"}})
	if err != nil {
		t.Fatalf("nil-hooks run: %v", err)
	}
	if r.Outcome != OutcomeProposed {
		t.Fatalf("nil-hooks run should still reach a proposal, got %v", r.Outcome)
	}
}
