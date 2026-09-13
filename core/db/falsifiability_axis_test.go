package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// ★ THE VACUOUS PASS (TG-192).
//
// Falsifiable() is Ratio() <= 0.5 where Ratio() is ControlTP/max(RealTP,1). A window in which the real arm
// found NOTHING and the control found nothing computes 0/1 = 0 and PASSES — both arms found nothing, and
// the row records "the real graph beat its structural control".
//
// Measured live 2026-08-06: 150 of 173 windows passed, and 123 of those had real_tp=0. Publishing the naive
// rate would put 86.7% in an exceed-proof for a model that made a claim in 44 windows and won 27 of them.
// This drives the real pgx aggregate, because a FILTER clause that quietly re-admits no-claim rows is
// exactly what a fake cannot catch.
func TestNoClaimWindowsAreNotCountedAsPasses(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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
	if _, err := p.Exec(ctx, "DELETE FROM infragraph_cascade_stats"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	now := time.Now().UTC()
	seed := func(realTP, ctrlTP int, ratio float64, fals bool) {
		if _, err := p.Exec(ctx,
			`INSERT INTO infragraph_cascade_stats
			   (window_start, window_end, real_tp, control_tp, control_ratio, falsifiable, computed_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			now.Add(-time.Hour), now, realTP, ctrlTP, ratio, fals, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(0, 0, 0, true)      // NO CLAIM — passes arithmetically, proves nothing
	seed(0, 0, 0, true)      // NO CLAIM
	seed(0, 0, 0, false)     // NO CLAIM, and not even marked falsifiable
	seed(10, 2, 0.2, true)   // a real win
	seed(10, 11, 1.1, false) // a real loss: the shuffle beat it
	// NO CLAIM, but the SHUFFLE found something. This row is what distinguishes a filtered sum from an
	// unfiltered one: real_tp=0 with control_tp=7 means the degree-preserving shuffle named alerting hosts
	// the real graph never did. Folding it into the headline "real TP vs control TP" credits the control arm
	// on a window the model never entered. Without this row every no-claim window is 0/0, an unfiltered sum
	// is numerically identical, and the mutation that drops the FILTER survives — it did, once.
	seed(0, 7, 7.0, false)

	got, err := NewAxisReadStore(p).Falsifiability(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Falsifiability: %v", err)
	}

	if got.Windows != 6 {
		t.Fatalf("Windows = %d, want 6", got.Windows)
	}
	if got.NoClaim != 4 {
		t.Errorf("NoClaim = %d, want 4 — real_tp=0 means the model made no claim", got.NoClaim)
	}
	if got.Claimed != 2 {
		t.Errorf("Claimed = %d, want 2 — the only honest denominator", got.Claimed)
	}
	if got.ClaimedPassed != 1 {
		t.Errorf("ClaimedPassed = %d, want 1", got.ClaimedPassed)
	}
	if r := got.Rate(); r != 0.5 {
		t.Errorf("Rate = %v, want 0.5 (1 of 2 claims won). Counting the four no-claim windows would "+
			"publish 3/6 or 5/6 and overstate a model that won one of the two claims it made.", r)
	}
	// Passed is the MEASURED naive count, not ClaimedPassed+NoClaim — one no-claim window here is not
	// marked falsifiable, exactly as 6 of 129 were not in production.
	if got.Passed != 3 {
		t.Errorf("Passed = %d, want 3 (every falsifiable row). Deriving it as ClaimedPassed+NoClaim would "+
			"give 4 and overstate the overstatement — a figure quoted to shame a wrong number must be right.",
			got.Passed)
	}
	// Sums must cover CLAIMED windows only, or no-claim zeros dilute nothing but look like coverage.
	if got.RealTP != 20 || got.ControlTP != 13 {
		t.Errorf("RealTP/ControlTP = %d/%d, want 20/13 over CLAIMED windows only. An unfiltered sum picks up "+
			"the no-claim window where the shuffle scored 7, publishing control TP 20 and making the control "+
			"arm look level with the real one on windows the model never entered.", got.RealTP, got.ControlTP)
	}
	if got.LosingRatio < 1.0 {
		t.Errorf("LosingRatio = %v, want >=1.0 — the one losing claim had the shuffle at 1.1", got.LosingRatio)
	}
}

// An empty table must yield a zero RATE without asserting a zero result: no windows means no claim either
// way, and Rate() must not divide by zero into a confident 0%.
func TestAnEmptyFalsifiabilityTableAssertsNothing(t *testing.T) {
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to a migrated database")
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
	if _, err := p.Exec(ctx, "DELETE FROM infragraph_cascade_stats"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := NewAxisReadStore(p).Falsifiability(ctx, time.Time{})
	if err != nil {
		t.Fatalf("Falsifiability: %v", err)
	}
	if got.Windows != 0 || got.Claimed != 0 || got.Rate() != 0 {
		t.Errorf("empty table gave %+v with rate %v; want all zero", got, got.Rate())
	}
}
