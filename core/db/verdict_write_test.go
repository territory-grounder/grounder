package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/predict"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/schema"
	"github.com/territory-grounder/grounder/core/verify"
)

// TestVerdictSingleWriterSkipsExecuted proves the TG-184 single-writer fix over the REAL pgx path: once the
// interceptor has written an EXECUTED action_verdict for an action_id, the async falsifiability scorer's
// DueForScoring anti-join EXCLUDES that prediction — so the scorer can never re-verdict an executed action
// with its divergent (no-baseline) algorithm and win the ON CONFLICT DO NOTHING race. A never-executed
// prediction stays due (the scorer is authoritative for it). Gated on TG_TEST_POSTGRES_DSN (CI has no Postgres).
func TestVerdictSingleWriterSkipsExecuted(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database to run the verdict single-writer test")
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

	preds := NewPredictionStore(p)
	verdicts := NewVerdictStore(p)
	falsifyStore := NewFalsifiabilityStore(p)

	pid := os.Getpid()
	aExec := fmt.Sprintf("act-exec-%d", pid)   // will get an executed verdict
	aNever := fmt.Sprintf("act-never-%d", pid) // stays never-executed
	phExec := fmt.Sprintf("plan-exec-%d", pid)
	phNever := fmt.Sprintf("plan-never-%d", pid)
	defer func() {
		_, _ = p.Exec(ctx, "DELETE FROM action_verdict WHERE action_id IN ($1,$2)", aExec, aNever)
		_, _ = p.Exec(ctx, "DELETE FROM infragraph_prediction WHERE action_id IN ($1,$2)", aExec, aNever)
	}()

	mk := func(actionID, planHash string) predict.PredictionRecord {
		return predict.PredictionRecord{
			Prediction: verify.Prediction{
				ActionID: actionID, PlanHash: planHash, TargetHost: "web01", Site: "nl",
				PredictedHosts: map[string]struct{}{}, PredictedRules: map[string]struct{}{},
			},
			ControlHosts:   map[string]struct{}{},
			SchemaVersion:  schema.Version(1),
			PredictionHash: "hash-" + actionID,
			ExternalRef:    "verdict-single-writer-it",
		}
	}
	if err := preds.Commit(ctx, mk(aExec, phExec)); err != nil {
		t.Fatalf("commit executed-path prediction: %v", err)
	}
	if err := preds.Commit(ctx, mk(aNever, phNever)); err != nil {
		t.Fatalf("commit never-executed prediction: %v", err)
	}

	older := time.Now().Add(time.Hour) // both predictions committed just now ⇒ older than this cutoff
	contains := func(id string) bool {
		due, derr := falsifyStore.DueForScoring(ctx, older, 1000)
		if derr != nil {
			t.Fatalf("DueForScoring: %v", derr)
		}
		for _, d := range due {
			if d.Record.Prediction.ActionID == id {
				return true
			}
		}
		return false
	}

	// (1) Before any verdict, BOTH never-verdicted predictions are due for scoring.
	if !contains(aExec) || !contains(aNever) {
		t.Fatalf("both never-verdicted predictions must be due before any executed verdict")
	}

	// (2) The interceptor writes an EXECUTED verdict for aExec (the synchronous, baseline-aware writer).
	if err := verdicts.Commit(ctx, aExec, phExec, "web01", "nl", safety.VerdictMatch); err != nil {
		t.Fatalf("commit executed verdict: %v", err)
	}

	// (3) The executed action's prediction is now EXCLUDED from scoring (single-writer): the scorer can never
	//     overwrite/duplicate an executed verdict. The never-executed prediction is STILL due.
	if contains(aExec) {
		t.Fatalf("an executed action_verdict must EXCLUDE its prediction from DueForScoring (single-writer, TG-184)")
	}
	if !contains(aNever) {
		t.Fatalf("a never-executed prediction must remain due — the scorer is authoritative only for those")
	}
}
