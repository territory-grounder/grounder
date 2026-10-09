package groundnet

import (
	"context"
	"testing"

	"github.com/territory-grounder/grounder/core/policy"
)

// testReGrad returns a re-graduator plus the node's OWN graduation ladder over the same store. The
// production ReGrad never writes the ladder (TG-436); the ladder here stands in for the consuming
// node's EXISTING grounded graduation flow, and the test drives it directly to model a LOCAL verified
// run (test files carry no ladder-writer obligation).
func testReGrad(t *testing.T) (*ReGrad, *policy.Ladder) {
	t.Helper()
	store := policy.NewMemGraduationStore()
	ladder := policy.NewLadder(1, store, nil) // N=1: one local verified-clean run advances off LevelApprove
	return NewReGrad(store), ladder
}

// REQ-2110 / INV-08: a landed foreign chunk earns NO authority from the producer's asserted outcome;
// it graduates only via the node's own LOCAL verified-clean runs, and a poisoned chunk never earns
// authority.
func TestReGradSubordinateNotAuthority(t *testing.T) {
	ctx := context.Background()
	rg, ladder := testReGrad(t)

	w := WisdomV0{
		OpClass: "restart-service", AlertClass: "service-down/http",
		Outcome: WisdomOutcome{Verifier: VerifierMechanical, Verdict: VerdictClean},
	}
	if err := rg.LandCandidate(ctx, w); err != nil {
		t.Fatalf("LandCandidate: %v", err)
	}
	// Landing granted NO authority — the producer's clean outcome did not graduate it.
	if lvl := rg.Level(ctx, "restart-service"); lvl != policy.LevelApprove {
		t.Fatalf("a landed foreign chunk must earn NO authority (LevelApprove), got %v", lvl)
	}
	if _, ok := rg.LandedHint("restart-service"); !ok {
		t.Error("the landed hint should be citable as evidence (REQ-2110)")
	}

	// It earns authority ONLY when the node's OWN grounded flow records a LOCAL verified-clean run
	// (modeled by driving the node's ladder directly).
	if _, err := ladder.Record(ctx, "restart-service", policy.OutcomeVerifiedClean); err != nil {
		t.Fatalf("local verified run: %v", err)
	}
	if rg.Level(ctx, "restart-service") == policy.LevelApprove {
		t.Fatalf("a local verified-clean run must advance the class off LevelApprove")
	}

	// An UNVERIFIED local run earns no authority.
	if _, err := ladder.Record(ctx, "unverified-class", policy.OutcomeUnverified); err != nil {
		t.Fatalf("local unverified run: %v", err)
	}
	if rg.Level(ctx, "unverified-class") != policy.LevelApprove {
		t.Error("an unverified local run must not earn authority")
	}

	// A POISONED foreign chunk that never produces a LOCAL verified-clean run NEVER earns authority.
	poison := WisdomV0{
		OpClass: "delete-everything", AlertClass: "x",
		Outcome: WisdomOutcome{Verifier: VerifierMechanical, Verdict: VerdictClean},
	}
	if err := rg.LandCandidate(ctx, poison); err != nil {
		t.Fatal(err)
	}
	if rg.Level(ctx, "delete-everything") != policy.LevelApprove {
		t.Fatal("a poisoned chunk with no LOCAL verified run must NEVER earn authority")
	}
}

// The FULL in-process e2e (the honest round-trip): producer A emits a signed, receipted Transparent
// Statement; consumer B verifies + de-replays + INGESTS it into its OWN re-graduator, where it lands
// as a subordinate candidate with NO authority — even though A's statement verified — and earns
// authority only when B's OWN local verified run advances it through B's ladder (REQ-2109/2110).
func TestFullE2ERegraduation(t *testing.T) {
	ctx := context.Background()

	// Producer A.
	tlA := NewTranslog()
	aEmit := NewAdapter(testPseudonym(t, testSeedHex), tlA, tlA, nil)
	stmt, err := aEmit.Emit(ctx, NewChunk(testLayer()))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wire, err := stmt.MarshalCBOR()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Consumer B: its own translog + re-graduator over its own graduation store/ladder.
	rg, ladder := testReGrad(t)
	tlB := NewTranslog()
	bIngest := NewAdapter(testPseudonym(t, testSeed2Hex), tlB, tlB, rg)

	out, err := bIngest.Ingest(ctx, wire)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !out.Accepted || out.Disposition != DispositionCandidate {
		t.Fatalf("ingest should land a candidate, got %+v", out)
	}
	// The imported op-class landed as a SUBORDINATE candidate — NO authority yet.
	if lvl := rg.Level(ctx, "restart-service"); lvl != policy.LevelApprove {
		t.Fatalf("an ingested chunk must earn no authority until LOCAL re-graduation, got %v", lvl)
	}
	if _, ok := rg.LandedHint("restart-service"); !ok {
		t.Error("the ingested wisdom should be a citable local hint")
	}
	// It earns authority ONLY when B's OWN local verified run advances it through B's ladder.
	if _, err := ladder.Record(ctx, "restart-service", policy.OutcomeVerifiedClean); err != nil {
		t.Fatalf("B local verified run: %v", err)
	}
	if rg.Level(ctx, "restart-service") == policy.LevelApprove {
		t.Fatal("B's own local verified run must graduate the imported class")
	}
}
