package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/territory-grounder/grounder/core/falsify"
	"github.com/territory-grounder/grounder/core/safety"
	"github.com/territory-grounder/grounder/core/verify"
)

// gated on TG_TEST_POSTGRES_DSN (an empty database; it calls Migrate() itself), like the other core/db oracles.
func discoveryFixture(t *testing.T) (context.Context, *Pool) {
	t.Helper()
	dsn := os.Getenv("TG_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TG_TEST_POSTGRES_DSN to an empty database to run the discovery-corpus durability oracle")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return ctx, p
}

func devRec(target, site string, surprise []string, observedAt time.Time) falsify.DiscoveryRecord {
	return falsify.DiscoveryRecord{
		ActionID: fmt.Sprintf("act-%d", observedAt.UnixNano()), TargetHost: target, Site: site,
		Verdict: safety.Verdict("deviation"), SurpriseHosts: surprise,
		Mismatches: []verify.RuleMismatch{}, Observed: []verify.ObservedAlert{},
		Score: falsify.Score{TP: 1, FP: 2}, ObservedAt: observedAt,
	}
}

// TG-206: MemDiscoveryCorpus lost the entire discovery corpus on restart, resetting the "reproduces >= N"
// promotion signal to zero. DiscoveryStore persists it to Postgres. This oracle proves the corpus SURVIVES a
// restart: a fresh store over a fresh pool (the post-restart worker, in-memory buffer gone) reads back the
// captured deviation with its reproduction count intact, and the typed breakdown round-trips through jsonb.
// Killing mutation: make Capture a no-op (return true without the INSERT) → the post-restart read is empty → RED.
func TestDiscoveryCorpusSurvivesRestart(t *testing.T) {
	ctx, p := discoveryFixture(t)
	target := fmt.Sprintf("host-%d-restart", os.Getpid()) // unique per run — isolated from residue
	s := NewDiscoveryStore(p)
	t0 := time.Now().UTC().Truncate(time.Second)

	// A FIRST sighting of the signature is newly captured.
	newCap, err := s.Capture(ctx, devRec(target, "nllei", []string{"g1", "g2"}, t0))
	if err != nil {
		t.Fatalf("capture 1: %v", err)
	}
	if !newCap {
		t.Fatal("the first sighting of a signature must be reported as newly captured (true)")
	}
	// A reproduction of the SAME signature (sorted surprise set is identical; different action_id/time) is NOT
	// new and increments the durable count.
	repro, err := s.Capture(ctx, devRec(target, "nllei", []string{"g2", "g1"}, t0.Add(time.Minute)))
	if err != nil {
		t.Fatalf("capture 2: %v", err)
	}
	if repro {
		t.Fatal("a reproduction of an existing signature must be reported as NOT new (false)")
	}

	// RESTART: a brand-new store over a fresh pool = the worker after a restart with its in-memory buffer gone.
	p2, err := Connect(ctx, os.Getenv("TG_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer p2.Close()
	got, err := NewDiscoveryStore(p2).Deviations(ctx, 1)
	if err != nil {
		t.Fatalf("read after restart: %v", err)
	}
	var found *falsify.CapturedDeviation
	for i := range got {
		if got[i].Record.TargetHost == target {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatal("the captured deviation did NOT survive the restart — the corpus is still in-process only (TG-206)")
	}
	if found.Reproductions != 2 {
		t.Fatalf("reproductions=%d after two captures of one signature, want 2 — the durable count IS the promotion signal", found.Reproductions)
	}
	if len(found.Record.SurpriseHosts) != 2 {
		t.Errorf("the typed surprise-host breakdown must round-trip through jsonb, got %v", found.Record.SurpriseHosts)
	}
}
