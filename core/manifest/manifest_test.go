package manifest

import (
	"testing"

	"github.com/territory-grounder/grounder/core/safety"
)

func sampleAction() Action {
	return Action{
		Target: "web01", OpClass: "restart-service", Op: "systemctl restart nginx",
		Params: map[string]string{"unit": "nginx", "site": "nl"}, Reversible: true,
	}
}

// action_id must be deterministic and stable across equal actions.
func TestActionIDStable(t *testing.T) {
	a := sampleAction()
	id1, _ := a.ID()
	id2, _ := a.ID()
	if id1 == "" || id1 != id2 {
		t.Fatalf("action id not stable: %q vs %q", id1, id2)
	}
}

// Changing ANY field must change the action_id (identity is what the gate protects). [O] INV-07.
func TestActionIDChangesOnMutation(t *testing.T) {
	base, _ := sampleAction().ID()
	mutations := []func(a *Action){
		func(a *Action) { a.Target = "web02" },
		func(a *Action) { a.Op = "systemctl restart apache" },
		func(a *Action) { a.Params["unit"] = "apache" },
		func(a *Action) { a.Reversible = false },
		func(a *Action) { a.OpClass = "reboot" },
	}
	for i, mut := range mutations {
		a := sampleAction()
		mut(&a)
		got, _ := a.ID()
		if got == base {
			t.Errorf("mutation %d did not change action_id", i)
		}
	}
}

// A stage receiving a tampered manifest or a mismatched expected id must fail closed.
// Rehydrate reconstructs a SEALED manifest from persisted fields and re-asserts the content hash. A durable
// store (core/db) must use it — a cross-package struct literal cannot set the unexported `sealed` flag, so a
// hand-built manifest fails Assert on every row. An untampered row rehydrates and asserts; a tampered stored
// action (id no longer matches the re-derived hash) is rejected.
func TestManifestRehydrate(t *testing.T) {
	m, err := New(sampleAction(), safety.BandAutoNotice, "plan#7", "pred#7")
	if err != nil {
		t.Fatal(err)
	}
	// round-trip: rehydrate from the persisted fields → a SEALED manifest that asserts.
	rh, err := Rehydrate(m.ActionID, m.Action, m.Band, m.PlanHash, m.PredictionHash)
	if err != nil {
		t.Fatalf("an untampered persisted manifest must rehydrate: %v", err)
	}
	if err := rh.Assert(m.ActionID); err != nil {
		t.Fatalf("a rehydrated manifest must be sealed and assert (not '!sealed'): %v", err)
	}
	// a tampered stored action (its id no longer matches the re-derived hash) is rejected at rehydrate.
	bad := m.Action
	bad.Target = "attacker-host"
	if _, err := Rehydrate(m.ActionID, bad, m.Band, m.PlanHash, m.PredictionHash); err == nil {
		t.Fatal("rehydrate must reject a stored action whose id no longer matches its content hash")
	}
}

func TestManifestAssert(t *testing.T) {
	m, err := New(sampleAction(), safety.BandPollPause, "plan#1", "pred#1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Assert(m.ActionID); err != nil {
		t.Fatalf("assert with correct id must pass: %v", err)
	}
	if err := m.Assert("deadbeefdeadbeef"); err == nil {
		t.Fatal("assert with a different expected id must fail closed")
	}
	// Tamper the bound action after sealing → derived id no longer matches the sealed id.
	m.Action.Target = "attacker-host"
	if err := m.Assert(m.ActionID); err == nil {
		t.Fatal("assert must detect a tampered bound action")
	}
}

// Record builds an append-only chain where every stage binds the one action_id; VerifyChain accepts it.
func TestRecordBuildsBoundChain(t *testing.T) {
	m, err := New(sampleAction(), safety.BandAutoNotice, "plan#1", "pred#1")
	if err != nil {
		t.Fatal(err)
	}
	m.WithProvenance(Provenance{IncidentRef: "TG-2041", ContextSnapshotID: "ctx-9", ModelRef: "zai", PromptHash: "ph1"})
	if err := m.Record(StagePredicted, map[string]string{"prediction": "pred#1"}); err != nil {
		t.Fatalf("record predicted: %v", err)
	}
	if err := m.Record(StageApproved, map[string]string{"choice": "approve"}); err != nil {
		t.Fatalf("record approved: %v", err)
	}
	if err := m.Record(StageVerified, map[string]string{"verdict": "match"}); err != nil {
		t.Fatalf("record verified: %v", err)
	}
	if err := m.VerifyChain(); err != nil {
		t.Fatalf("a well-formed chain must verify: %v", err)
	}
	for i, s := range m.Stages {
		if s.ActionID != m.ActionID {
			t.Fatalf("stage %d not bound to the action_id", i)
		}
		if s.PayloadHash == "" {
			t.Fatalf("stage %d missing payload hash", i)
		}
	}
	if m.Provenance.IncidentRef != "TG-2041" {
		t.Fatalf("provenance must be bound: %+v", m.Provenance)
	}
}

// The core guarantee: a stage bound to a DIFFERENT action_id (a prediction/approval/verdict for some other
// action passed off as this one's) is caught — the predecessor's "prediction not bound to the executed
// action" defect made structurally impossible.
func TestVerifyChainCatchesForeignActionStage(t *testing.T) {
	m, err := New(sampleAction(), safety.BandPollPause, "plan#1", "pred#1")
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Record(StagePredicted, "p")
	// Splice in a stage that binds a DIFFERENT action id — the attack/bug the chain exists to catch.
	m.Stages = append(m.Stages, Stage{Stage: StageApproved, ActionID: "deadbeefdeadbeef", PayloadHash: "x", Seq: 1})
	if err := m.VerifyChain(); err == nil {
		t.Fatal("VerifyChain must reject a stage bound to a foreign action_id")
	}
}

// Stages are append-only in lifecycle order; a backwards stage is refused.
func TestRecordRefusesOutOfOrder(t *testing.T) {
	m, _ := New(sampleAction(), safety.BandAuto, "p", "pr")
	if err := m.Record(StageApproved, nil); err != nil {
		t.Fatalf("first stage: %v", err)
	}
	if err := m.Record(StagePredicted, nil); err == nil {
		t.Fatal("recording an earlier stage after a later one must be refused (append-only, ordered)")
	}
	if err := m.Record(StageApproved, nil); err == nil {
		t.Fatal("recording the same stage twice must be refused")
	}
}

// Provenance is frozen once the first stage is recorded — it is part of the immutable record.
func TestProvenanceFrozenAfterFirstStage(t *testing.T) {
	m, _ := New(sampleAction(), safety.BandAuto, "p", "pr")
	m.WithProvenance(Provenance{IncidentRef: "first"})
	_ = m.Record(StagePredicted, nil)
	m.WithProvenance(Provenance{IncidentRef: "rewritten"})
	if m.Provenance.IncidentRef != "first" {
		t.Fatalf("provenance must freeze after the first stage, got %q", m.Provenance.IncidentRef)
	}
}
