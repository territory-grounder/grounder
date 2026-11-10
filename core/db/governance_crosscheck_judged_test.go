package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// THE FALSE JUDGE-DEATH (TG-686, live 2026-11-08 01:00 NL). The frontier cross-check sampled the newest sessions and
// read every one the local judge had not REACHED yet as "scored not at all" — after the PVE04 reset the local judge
// had not run, so DEATH fired on a live judge. And a deterministic Go-scored axis (diagnosis_grounded,
// estate_grounded) counted as "locally scored", so a dead judge MODEL could read alive.
//
// Seeded over real SQL, one session per case:
//
//	fresh   — not yet judged (judged=false), no rows                     ⇒ NOT in the sample
//	alive   — judged, model dimensions scored                            ⇒ in the sample, LocalScored
//	masked  — judged, ONLY a deterministic axis scored (the model wrote nothing) ⇒ in the sample, NOT LocalScored
//	silent  — judged, no rows at all                                     ⇒ in the sample, NOT LocalScored
//
// KILLING MUTATIONS: drop `AND t.judged` (fresh enters the sample) and drop the dimension filter (masked reads scored).
func TestTheCrossCheckSamplesOnlyJudgedSessionsAndCountsOnlyModelAxes(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()

	pfx := fmt.Sprintf("tg686-%d-", time.Now().UnixNano())
	refs := map[string]string{"fresh": pfx + "fresh", "alive": pfx + "alive", "masked": pfx + "masked", "silent": pfx + "silent"}
	cleanup := func() {
		for _, r := range refs {
			_, _ = p.Exec(ctx, `DELETE FROM session_judgment WHERE external_ref = $1`, r)
			_, _ = p.Exec(ctx, `DELETE FROM session_triage WHERE external_ref = $1`, r)
		}
	}
	cleanup()
	defer cleanup()

	ts := NewTriageStore(p)
	for _, r := range refs {
		if _, err := p.Exec(ctx, `INSERT INTO session_triage (external_ref, confidence, schema_version) VALUES ($1, 0.5, 1)`, r); err != nil {
			t.Fatalf("seed triage %s: %v", r, err)
		}
	}
	if len(judge.Dimensions) == 0 {
		t.Fatal("control: the rubric declares no model dimensions")
	}
	for _, dim := range judge.Dimensions {
		if err := ts.WriteJudgment(ctx, refs["alive"], dim, 4, "tg686", judge.RubricVersion(), judge.Provenance{Model: "tg686-model"}); err != nil {
			t.Fatalf("seed model judgment: %v", err)
		}
	}
	if err := ts.WriteJudgment(ctx, refs["masked"], judge.DimDiagnosisGrounded, 5, "tg686", judge.RubricVersion(),
		judge.Provenance{Model: judge.ProvenanceDeterministic}); err != nil {
		t.Fatalf("seed deterministic judgment: %v", err)
	}
	for _, k := range []string{"alive", "masked", "silent"} {
		if err := ts.MarkJudged(ctx, refs[k]); err != nil {
			t.Fatalf("mark judged %s: %v", k, err)
		}
	}

	rows, err := NewGovernanceReadStore(p, time.Hour, 20).RecentForCrossCheck(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	present := map[string]bool{}
	for _, r := range rows {
		if strings.HasPrefix(r.ExternalRef, pfx) {
			present[strings.TrimPrefix(r.ExternalRef, pfx)] = true
			got[strings.TrimPrefix(r.ExternalRef, pfx)] = r.LocalScored
		}
	}
	if present["fresh"] {
		t.Error("a session the local judge has not reached yet must not be sampled — it is 'not yet judged', not 'unscored'")
	}
	for _, k := range []string{"alive", "masked", "silent"} {
		if !present[k] {
			t.Errorf("the judged session %q must be in the sample (the left join keeps unscored ones — they ARE the death signal)", k)
		}
	}
	if !got["alive"] {
		t.Error("a judged session with model-scored dimensions must read LocalScored")
	}
	if got["masked"] {
		t.Error("a judged session where ONLY a deterministic axis scored must NOT read LocalScored — the model said nothing")
	}
	if got["silent"] {
		t.Error("a judged session with no judgment rows must NOT read LocalScored")
	}
}
