package db

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/estate"
	"github.com/territory-grounder/grounder/core/learn"
)

// seedLearner accumulates n co-occurrences of root->cons starting at base, each iteration an hour apart (well
// past the cascade window, so no cross-incident pairing WITHIN the call). Callers must pass FAR-APART bases
// for different pairs, or the shared `recent` working set cross-contaminates them.
func seedLearner(l *learn.CoOccurrenceLearner, base time.Time, root, cons string, n int) {
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		l.Observe(learn.AlertObservation{Host: root, At: at})
		l.Observe(learn.AlertObservation{Host: cons, At: at.Add(2 * time.Second)})
	}
}

// TestCoOccurrenceStoreRoundTrip is the DB-gated fidelity proof for TG-388 face (c): the learner's raw
// decay-state floats survive Save->Load->Restore exactly, and Save REPLACES (a decayed-out pair is deleted,
// not left to be reloaded). Runs only with a migrated Postgres.
func TestCoOccurrenceStoreRoundTrip(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the co-occurrence store test")
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
	defer func() {
		for _, q := range []string{`DELETE FROM co_occurrence`, `DELETE FROM co_occurrence_host`} {
			if _, err := p.Exec(ctx, q); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	}()

	store := NewCoOccurrenceStore(p)

	l := learn.NewCoOccurrenceLearner(0)
	d0 := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	seedLearner(l, d0, "A", "B", 8) // distinct FAR-APART bases so the shared `recent`
	// TG-188 organic recovery: one attributed clear for B before the next far-apart base prunes its onset —
	// the DeepEqual identity below then proves the recovery evidence survives Save→Load→Restore too.
	l.ObserveClear(learn.ClearObservation{Host: "B", At: d0.Add(7*time.Hour + 30*time.Minute)})
	seedLearner(l, d0.AddDate(0, 0, 100), "A", "E", 5) // working set does not cross-contaminate the pairs
	seedLearner(l, d0.AddDate(0, 0, 200), "C", "D", 3)
	l.DecayOnDisproof([]estate.DisproofPath{{Target: "A", Surprised: []string{"B"}}}, 0.3) // (A,B) → fractional

	orig := l.Snapshot()
	if err := store.Save(ctx, orig); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	restored := learn.NewCoOccurrenceLearner(0)
	restored.Restore(loaded)
	if got := restored.Snapshot(); !reflect.DeepEqual(got, orig) {
		t.Errorf("DB round-trip is not identity:\n orig=%+v\n got =%+v", orig, got)
	}

	// A pair decays OUT, then re-save: the reconciliation must DELETE the stale row, not leave it to reload.
	for i := 0; i < 3; i++ {
		l.DecayOnDisproof([]estate.DisproofPath{{Target: "C", Surprised: []string{"D"}}}, 0.5) // 3→1.5→0.75→0.375 pruned
	}
	if err := store.Save(ctx, l.Snapshot()); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	loaded2, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	for _, pr := range loaded2.Pairs {
		if pr.Primary == "C" && pr.Dependent == "D" {
			t.Errorf("decayed-out pair C->D still persisted after re-save — Save must replace, not append")
		}
	}
	if len(loaded2.Pairs) == 0 {
		t.Errorf("re-save wiped everything — A->B and A->E should remain")
	}
}
