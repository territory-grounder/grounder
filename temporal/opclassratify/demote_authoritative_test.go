package opclassratify

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// TestDemoteSavesUnconditionally guards the operator-demote path against the TG-146 S3/S4 compare-and-set the
// durable graduation store gained. The demote verb's Save must be UNCONDITIONAL (version 0), NOT the optimistic
// guard: this activity runs MaximumAttempts=1 (no engine retry — opclassratify.go ~line 466), so a CAS miss on
// a benign race with a worker Record would fail the operator's demotion outright and leave the class at the
// autonomous rung the operator just judged unsafe. An operator demotion is authoritative and must win, exactly
// like the ratify reset.
//
// KILLING MUTATION: drop `st.Version = 0` from demote — the loaded positive version (7) flows into Save, this
// test sees it, and goes RED (and the real demote becomes CAS-rejectable with no retry to save it).
func TestDemoteSavesUnconditionally(t *testing.T) {
	const op = "tg146-overlay-to-demote"
	// Seed the class at auto with a POSITIVE durable version — a promoted class the operator now demotes.
	a, _, _, lad := ratifyFixture(op, map[string]policy.ClassState{
		op: {Level: policy.LevelAuto, Version: 7},
	})

	res, err := a.demote(context.Background(), Request{
		Verb:      VerbDemote,
		OpClass:   op,
		Rationale: "operator judged it unsafe",
		Approver:  "kyriakosp",
	})
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if res.Level != policy.LevelApprove.String() {
		t.Fatalf("demote result level = %q, want approve", res.Level)
	}
	saved, ok := lad.lastSave()
	if !ok {
		t.Fatalf("demote wrote nothing to the ladder")
	}
	if saved.Level != policy.LevelApprove {
		t.Fatalf("demote saved level = %v, want approve", saved.Level)
	}
	if saved.Version != 0 {
		t.Fatalf("demote saved version = %d, want 0 (unconditional) — an authoritative demote must not be "+
			"compare-and-set-rejectable; this activity has no engine retry", saved.Version)
	}
}
