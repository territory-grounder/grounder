package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/judge"
)

// TG-670 (F18), migration 0131: a judgment row records the instrument that produced it — the gateway-reported
// model and the sampling sent — and a re-judge refreshes it with the rest of the row. REAL Postgres: the upsert
// and the two new columns are what a fake cannot exercise.
func TestWriteJudgmentRecordsAndRefreshesProvenance(t *testing.T) {
	dsn := skipWithoutDB(t)
	ctx := context.Background()
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer p.Close()
	st := NewTriageStore(p)
	ref := fmt.Sprintf("gold-prov-%d", time.Now().UnixNano())
	read := func() (string, string) {
		t.Helper()
		var m, s string
		if err := p.QueryRow(ctx, `SELECT judge_model, judge_sampling FROM session_judgment WHERE external_ref = $1 AND dimension = 'correct_diagnosis'`, ref).Scan(&m, &s); err != nil {
			t.Fatalf("read provenance: %v", err)
		}
		return m, s
	}
	if err := st.WriteJudgment(ctx, ref, "correct_diagnosis", 4, "c", "rv-prov", judge.Provenance{Model: "upstream-opus", Sampling: "temperature=0 seed=0"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if m, s := read(); m != "upstream-opus" || s != "temperature=0 seed=0" {
		t.Fatalf("provenance = %q/%q, want the reported model and the sent sampling", m, s)
	}
	// A re-judge by a completer without the seam leaves the provenance UNRECORDED — never the stale value.
	if err := st.WriteJudgment(ctx, ref, "correct_diagnosis", 3, "c2", "rv-prov", judge.Provenance{}); err != nil {
		t.Fatalf("re-judge: %v", err)
	}
	if m, s := read(); m != "" || s != "" {
		t.Fatalf("a re-judge must refresh the provenance, got %q/%q", m, s)
	}
	// A Go-scored dimension records that no model answered.
	if err := st.WriteJudgment(ctx, ref, judge.DimDiagnosisGrounded, 5, "", "rv-prov", judge.Provenance{Model: judge.ProvenanceDeterministic}); err != nil {
		t.Fatalf("deterministic: %v", err)
	}
	var m string
	if err := p.QueryRow(ctx, `SELECT judge_model FROM session_judgment WHERE external_ref = $1 AND dimension = $2`, ref, judge.DimDiagnosisGrounded).Scan(&m); err != nil || m != judge.ProvenanceDeterministic {
		t.Fatalf("deterministic provenance = %q (%v)", m, err)
	}
}
